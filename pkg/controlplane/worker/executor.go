package worker

import (
	"context"
	"time"
	// "net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	// "google.golang.org/grpc"
	"github.com/spf13/cobra"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/prefixlog"
	// pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

type workerArgsStruct struct {
	role            string
	etcdEndpoints   string
	etcdDialTimeout int
	grpcNetwork     string
	grpcAddress     string
	grpcTarget      string
	prioCodeConf    string
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
}

func launchWorker(cmd *cobra.Command, args []string) {
	gLogger.Info("Launch worker: %v", workerArgs)

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
	dnWorker := newDnWorkerServer(
		etcdCli,
		constants.SchemaPrefixDefault,
		prioCode,
		workerArgs.grpcTarget,
	)

	ctx, cancel := context.WithCancel(context.Background())
	dnmw := newDnMemberWorker(ctx, dnWorker)
	dnmw.run()
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM)
	<-signalCh
	gLogger.Info("Cancel all tasks")
	cancel()
	dnmw.wait()
	gLogger.Info("Exit")
}

func Execute() {
	if err := workerCmd.Execute(); err != nil {
		gLogger.Fatal("Cmd execute err: %v", err)
	}
}
