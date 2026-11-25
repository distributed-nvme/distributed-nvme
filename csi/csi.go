// Package csi is dnv-csi, the Container Storage Interface driver of dnv
// (csi.md): the identity service, the controller plugin and the node plugin,
// served over a unix socket to the kubelet and the CSI sidecars. The
// controller plugin is a gRPC client of the gateway and of nothing else: it
// never touches etcd and never talks to an agent. The node plugin talks to
// the node's kernel through the OS client and never to the gateway.
package csi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

const (
	// DriverName is the name Kubernetes knows the driver by (csi.md, The
	// driver): the reverse-domain form of the project's NQN domain. Every
	// storage class of dnv carries it.
	DriverName = "csi.dnv.io"
	// DefaultGatewayTimeout bounds one gateway call of the controller
	// plugin. The provisioner sidecar gives a whole CreateVolume about ten
	// seconds, and a create makes several gateway calls, so each is held
	// well below that: a healthy gateway answers in milliseconds, and this
	// bound is for a gateway that does not answer at all.
	DefaultGatewayTimeout = 5 * time.Second
)

// Version is the vendor version the identity service reports and the start
// records carry: the build version of the running binary
// (common.BuildVersion), a release build's tag such as "v0.2.0". The v0.1.0
// driver reported the constant "0.1.0" in its place.
var Version = common.BuildVersion()

// The lifecycle and call records of csi.md, Log records.
const (
	msgControllerStarting = "csi controller starting"
	msgControllerServing  = "csi controller serving"
	msgControllerStopping = "csi controller stopping"
	msgNodeStarting       = "csi node starting"
	msgNodeServing        = "csi node serving"
	msgNodeStopping       = "csi node stopping"
	msgRequest            = "csi request"
	msgReply              = "csi reply"
)

// polledMethods are the calls Kubernetes repeats on a timer: the liveness
// probe's, every few seconds; the provisioner's capacity poll; and the
// kubelet's volume stats of each mounted volume with the capability call it
// makes before them. Each says nothing new while it succeeds, so it is
// logged as a read is: its request record at Debug, and its reply record at
// Debug while it succeeds and at Info when it fails (csi.md CSI8).
var polledMethods = map[string]bool{
	"/csi.v1.Identity/Probe":           true,
	"/csi.v1.Controller/GetCapacity":   true,
	"/csi.v1.Node/NodeGetVolumeStats":  true,
	"/csi.v1.Node/NodeGetCapabilities": true,
}

// Config is what a plugin needs to serve.
type Config struct {
	// Endpoint is the path of the unix socket the plugin serves. A stale
	// socket file is removed before the listen.
	Endpoint string
	// GatewayAddress is the gateway's host:port (the controller plugin).
	GatewayAddress string
	// CdcAddresses are the cdc endpoints, ip:port each: the node plugin's
	// follower follows these and no other, from its start (csi.md CSI15),
	// and the controller plugin puts them in the publish context at attach
	// (csi.md CSI10), where the node plugin does not read them.
	CdcAddresses []string
	// GatewayTimeout bounds one gateway call; zero takes
	// DefaultGatewayTimeout.
	GatewayTimeout time.Duration
	// HostNqnPath and HostIdPath are the node's identity files (the node
	// plugin); empty takes the defaults.
	HostNqnPath string
	HostIdPath  string
	// DeviceWait bounds a stage's wait for the volume's device; zero takes
	// DefaultDeviceWait.
	DeviceWait time.Duration
	// SyncPeriod is the follower's timer; zero takes DefaultSyncPeriod.
	SyncPeriod time.Duration
	// OsClient is the node plugin's one path to the operating system; nil
	// takes a LimitedOsClient. The tests pass a fake.
	OsClient common.OsClient
}

// RunController serves the identity service and the controller plugin on
// cfg.Endpoint until ctx ends, then stops gracefully and returns nil. It
// returns an error when the gateway address cannot be dialed or the socket
// cannot be listened on.
func RunController(ctx context.Context, cfg Config) error {
	slog.InfoContext(ctx, msgControllerStarting,
		slog.String("endpoint", cfg.Endpoint),
		slog.String("gateway_address", cfg.GatewayAddress),
		slog.Any("cdc_addresses", cfg.CdcAddresses),
		slog.String("version", Version),
	)
	conn, err := dialGateway(cfg.GatewayAddress)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	ctl := newController(pb.NewGatewayClient(conn), cfg)
	err = serve(ctx, cfg.Endpoint, msgControllerServing,
		func(s grpc.ServiceRegistrar) {
			csi.RegisterIdentityServer(s, newIdentity())
			csi.RegisterControllerServer(s, ctl)
		})
	slog.InfoContext(ctx, msgControllerStopping)
	return err
}

// RunNode serves the identity service and the node plugin on cfg.Endpoint
// until ctx ends. It refuses to start without the node's identity files,
// and on a kernel with native NVMe multipath off or no NVMe over Fabrics
// device (csi.md CSI13), reads its state back from sysfs, runs the follower
// beside the server, and stops both at the end: the follower drops the
// discovery connections when nothing is staged and leaves every connection
// in place otherwise.
func RunNode(ctx context.Context, cfg Config) error {
	// Two OS clients, each with its own command slots: the stage and
	// unstage commands, a mount or an unmount that waits for IO among them,
	// take the first, and the follower's commands the second, so that the
	// connects that would end such a wait never wait for a slot behind it
	// (csi.md CSI16).
	oc, foc := nodeClients(cfg)
	nqnPath, idPath := cfg.HostNqnPath, cfg.HostIdPath
	if nqnPath == "" {
		nqnPath = DefaultHostNqnPath
	}
	if idPath == "" {
		idPath = DefaultHostIdPath
	}
	id, err := readIdentity(ctx, oc, nqnPath, idPath)
	if err != nil {
		return err
	}
	if err := checkHostKernel(ctx, oc); err != nil {
		return err
	}
	deviceWait := cfg.DeviceWait
	if deviceWait <= 0 {
		deviceWait = DefaultDeviceWait
	}
	var cdcs []endpoint
	for _, raw := range cfg.CdcAddresses {
		ep, err := parseEndpoint(raw)
		if err != nil {
			return err
		}
		cdcs = append(cdcs, ep)
	}
	slog.InfoContext(ctx, msgNodeStarting,
		slog.String("endpoint", cfg.Endpoint),
		slog.String("node_id", id.hostNqn),
		slog.Any("cdc_addresses", cfg.CdcAddresses),
		slog.String("version", Version),
	)
	host := &nvmeHost{oc: foc, id: id}
	follow := newFollower(host, cfg.SyncPeriod, cdcs)
	if err := follow.recover(ctx); err != nil {
		return err
	}
	fctx, fcancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		follow.run(fctx)
	}()
	n := &node{
		id:         id,
		host:       host,
		follow:     follow,
		mnt:        &mounter{oc: oc},
		deviceWait: deviceWait,
	}
	err = serve(ctx, cfg.Endpoint, msgNodeServing,
		func(s grpc.ServiceRegistrar) {
			csi.RegisterIdentityServer(s, newIdentity())
			csi.RegisterNodeServer(s, n)
		})
	fcancel()
	wg.Wait()
	slog.InfoContext(ctx, msgNodeStopping)
	return err
}

// nodeClients are the node plugin's two OS clients, the stage's and the
// follower's, each with command slots of its own, or the one a test passes
// for both.
func nodeClients(cfg Config) (common.OsClient, common.OsClient) {
	if cfg.OsClient != nil {
		return cfg.OsClient, cfg.OsClient
	}
	return common.NewLimitedOsClient(0), common.NewLimitedOsClient(0)
}

// dialGateway opens the one gateway connection a plugin holds: plaintext,
// the shared client chain, which carries the trace id the plugin minted for
// the CSI call, and the shared message limit (grpc.md, Wiring).
func dialGateway(address string) (*grpc.ClientConn, error) {
	return grpc.NewClient(address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(common.GrpcUnaryClientInterceptor()),
		grpc.WithChainStreamInterceptor(common.GrpcStreamClientInterceptor()),
		common.GrpcClientMsgOption(),
	)
}

// serve listens on the unix socket at endpoint, serves the registered CSI
// services with the plugin's own interceptor until ctx ends, and returns
// what Serve returned after a graceful stop: nil. The socket file is
// unlinked when the listener closes.
func serve(
	ctx context.Context,
	endpoint string,
	servingMsg string,
	register func(grpc.ServiceRegistrar),
) error {
	if err := os.Remove(endpoint); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing the stale socket %s: %w", endpoint, err)
	}
	lis, err := net.Listen("unix", endpoint)
	if err != nil {
		return err
	}
	// The shared message limit, as on every server of the tree (grpc.md,
	// Wiring); the chain is the plugin's own (csi.md CSI8).
	srv := grpc.NewServer(append(common.GrpcServerMsgOptions(),
		grpc.ChainUnaryInterceptor(unaryInterceptor()))...)
	register(srv)
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()
	slog.InfoContext(ctx, servingMsg, slog.String("endpoint", endpoint))
	if err := srv.Serve(lis); err != nil &&
		!errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}

// unaryInterceptor is the CSI services' own chain (csi.md, Log records): it
// mints a trace id for every call, since the kubelet and the sidecars send
// none, and it logs the method on the way in and the method, the code and
// the duration on the way out — never a request or a reply, so nothing the
// kubelet may send as a secret reaches a log. The shared server interceptor
// of grpc.md is for dnv-internal services and logs every message, which is
// why it is not chained here.
func unaryInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		ctx = common.WithTraceId(ctx, common.NewTraceId())
		requestLevel := slog.LevelInfo
		if polledMethods[info.FullMethod] {
			requestLevel = slog.LevelDebug
		}
		slog.LogAttrs(ctx, requestLevel, msgRequest,
			slog.String("method", info.FullMethod))
		start := time.Now()
		reply, err := handler(ctx, req)
		replyLevel := requestLevel
		if err != nil {
			replyLevel = slog.LevelInfo
		}
		attrs := []slog.Attr{
			slog.String("method", info.FullMethod),
			slog.String("code", status.Code(err).String()),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		}
		if err != nil {
			attrs = append(attrs, slog.String("error", err.Error()))
		}
		slog.LogAttrs(ctx, replyLevel, msgReply, attrs...)
		return reply, err
	}
}
