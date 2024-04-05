package exapi

import (
	"net"
	"strings"
	"time"

	"github.com/spf13/cobra"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/prefixlog"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

type exApiArgsStruct struct {
	etcdEndpoints   string
	etcdDialTimeout int
	grpcNetwork     string
	grpcAddress     string
}

var (
	exApiCmd = &cobra.Command{
		Use:   "dnvapi",
		Short: "dnv external api",
		Long:  `dnv external api`,
		Run:   launchExApi,
	}
	exApiArgs    = exApiArgsStruct{}
	gExApiLogger = prefixlog.NewPrefixLogger("ex_api")
)

func init() {
	exApiCmd.PersistentFlags().StringVarP(
		&exApiArgs.etcdEndpoints,
		"etcd-endpoints", "", "localhost:2379", "etcd endpoint list",
	)
	exApiCmd.PersistentFlags().IntVarP(
		&exApiArgs.etcdDialTimeout,
		"etcd-dial-timeout", "", 30, "etcd dial timeout",
	)
	exApiCmd.PersistentFlags().StringVarP(
		&exApiArgs.grpcNetwork, "grpc-network", "", "tcp", "grpc network",
	)
	exApiCmd.PersistentFlags().StringVarP(
		&exApiArgs.grpcAddress, "grpc-address", "", ":9520", "grpc address",
	)
}

func launchExApi(cmd *cobra.Command, args []string) {
	gExApiLogger.Info("Launch external api: %v", exApiArgs)
	lis, err := net.Listen(exApiArgs.grpcNetwork, exApiArgs.grpcAddress)
	if err != nil {
		gExApiLogger.Fatal("Listen err: %v", err)
	}

	endpoints := strings.Split(exApiArgs.etcdEndpoints, ",")
	dialTimeout := time.Duration(exApiArgs.etcdDialTimeout) * time.Second
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: dialTimeout,
	})
	if err != nil {
		gExApiLogger.Fatal("Create etcd client err: %v", err)
	}

	exApi := newExApiServer(
		etcdCli,
		constants.SchemaPrefixDefault,
	)

	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			ctxhelper.UnaryServerPerCtxHelperInterceptor,
		),
	)

	pbcp.RegisterExternalApiServer(grpcServer, exApi)
	if err := grpcServer.Serve(lis); err != nil {
		gExApiLogger.Fatal("Serve err: %v", err)
	}

	gExApiLogger.Info("Exit external api")
}

func Execute() {
	if err := exApiCmd.Execute(); err != nil {
		gExApiLogger.Fatal("Cmd execute err: %v", err)
	}
}
