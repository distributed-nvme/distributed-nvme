package worker

import (
	"context"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/prefixlog"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

type workerArgsStruct struct {
	role            string
	etcdEndpoints   string
	etcdDialTimeout int
	grpcNetwork     string
	grpcAddress     string
	grpcTarget      string
	prioCodeConf    string
	replica         int
}

var (
	workerCmd = &cobra.Command{
		Use:   "dnvworker",
		Short: "dnv worker",
		Long:  `dnv worker`,
		Run:   launchWorker,
	}
	workerArgs = workerArgsStruct{}
	gLogger    = prefixlog.NewPrefixLogger("worker")
)

func init() {
	workerCmd.PersistentFlags().StringVarP(
		&workerArgs.role,
		"role", "", "", "worker role, either dn, cn or sp",
	)
	workerCmd.PersistentFlags().StringVarP(
		&workerArgs.etcdEndpoints,
		"etcd-endpoints", "", "localhost:2379", "etcd endpoint list",
	)
	workerCmd.PersistentFlags().IntVarP(
		&workerArgs.etcdDialTimeout,
		"etcd-dial-timeout", "", 30, "etcd dial timeout",
	)
	workerCmd.PersistentFlags().StringVarP(
		&workerArgs.grpcNetwork, "grpc-network", "", "tcp", "grpc network",
	)
	workerCmd.PersistentFlags().StringVarP(
		&workerArgs.grpcAddress, "grpc-address", "", ":9521", "grpc address",
	)
	workerCmd.PersistentFlags().StringVarP(
		&workerArgs.grpcTarget, "grpc-target", "", "", "grpc target",
	)
	workerCmd.PersistentFlags().StringVarP(
		&workerArgs.prioCodeConf, "prio-code-conf", "", "", "priority code configuration",
	)
	workerCmd.PersistentFlags().IntVarP(
		&workerArgs.replica, "replica", "", 0, "replica",
	)
}

func launchWorker(cmd *cobra.Command, args []string) {
	gLogger.Info("Launch worker: %v", workerArgs)

	lis, err := net.Listen(workerArgs.grpcNetwork, workerArgs.grpcAddress)
	if err != nil {
		gLogger.Fatal("Listen err: %v", err)
	}

	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			ctxhelper.UnaryServerPerCtxHelperInterceptor,
		),
	)

	prioCode, err := initPrioCode(workerArgs.prioCodeConf)
	if err != nil {
		gLogger.Fatal("Init prio code err: %v", err)
	}

	endpoints := strings.Split(workerArgs.etcdEndpoints, ",")
	dialTimeout := time.Duration(workerArgs.etcdDialTimeout) * time.Second
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: dialTimeout,
	})
	if err != nil {
		gLogger.Fatal("Create etcd client err: %v", err)
	}

	var mwkr *memberWorker
	ctx, cancel := context.WithCancel(context.Background())

	switch workerArgs.role {
	case "dn":
		worker := newDnWorkerServer(
			etcdCli,
			constants.SchemaPrefixDefault,
		)
		pbcp.RegisterDiskNodeWorkerServer(grpcServer, worker)
		mwkr = newMemberWorker(
			ctx,
			workerArgs.grpcTarget,
			prioCode,
			uint32(workerArgs.replica),
			constants.GrantTTLDefault,
			worker,
		)
	case "cn":
		worker := newCnWorkerServer(
			etcdCli,
			constants.SchemaPrefixDefault,
		)
		pbcp.RegisterControllerNodeWorkerServer(grpcServer, worker)
		mwkr = newMemberWorker(
			ctx,
			workerArgs.grpcTarget,
			prioCode,
			uint32(workerArgs.replica),
			constants.GrantTTLDefault,
			worker,
		)
	case "sp":
		worker := newSpWorkerServer(
			etcdCli,
			constants.SchemaPrefixDefault,
		)
		pbcp.RegisterStoragePoolWorkerServer(grpcServer, worker)
		mwkr = newMemberWorker(
			ctx,
			workerArgs.grpcTarget,
			prioCode,
			uint32(workerArgs.replica),
			constants.GrantTTLDefault,
			worker,
		)
	default:
		gLogger.Fatal("Unknown role: %s", workerArgs.role)
	}

	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			gLogger.Fatal("Serve err: %v", err)
		}
	}()

	mwkr.run()
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM)
	<-signalCh
	gLogger.Info("Cancel all tasks")
	cancel()
	mwkr.wait()
	gLogger.Info("Exit")
}

func Execute() {
	if err := workerCmd.Execute(); err != nil {
		gLogger.Fatal("Cmd execute err: %v", err)
	}
}
