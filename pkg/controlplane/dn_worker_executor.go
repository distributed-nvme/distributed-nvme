package controlplane

import (
	"context"
	"time"
	// "net"
	"strings"
	"os"
	"os/signal"
	"sync"
	"syscall"

	// "google.golang.org/grpc"
	"github.com/spf13/cobra"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	// pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

type dnWorkerArgsStruct struct {
	etcdEndpoints string
	etcdDialTimeout int
	grpcNetwork string
	grpcAddress string
	grpcTarget string
	prioCodeConf string
}

var (
	dnWorkerCmd = &cobra.Command{
		Use: "dnv_dn_worker",
		Short: "dnv disk node worker",
		Long: `dnv disk node worker`,
		Run: launchDnWorker,
	}
	dnWorkerArgs = dnWorkerArgsStruct{}
	gDnWorkerLogger = lib.NewPrefixLogger("dn_worker")
)

func init() {
	dnWorkerCmd.PersistentFlags().StringVarP(
		&dnWorkerArgs.etcdEndpoints,
		"etcd-endpoints", "", "localhost:2379", "etcd endpoint list",
	)
	dnWorkerCmd.PersistentFlags().IntVarP(
		&dnWorkerArgs.etcdDialTimeout,
		"etcd-dial-timeout", "", 30, "etcd dial timeout",
	)
	dnWorkerCmd.PersistentFlags().StringVarP(
		&dnWorkerArgs.grpcNetwork, "grpc-network", "", "tcp", "grpc network",
	)
	dnWorkerCmd.PersistentFlags().StringVarP(
		&dnWorkerArgs.grpcAddress, "grpc-address", "", ":9521", "grpc address",
	)
	dnWorkerCmd.PersistentFlags().StringVarP(
		&dnWorkerArgs.grpcTarget, "grpc-target", "", "", "grpc target",
	)
	dnWorkerCmd.PersistentFlags().StringVarP(
		&dnWorkerArgs.prioCodeConf, "prio-code-conf", "", "", "priority code configuration",
	)
}

func launchDnWorker(cmd *cobra.Command, args []string) {
	gDnWorkerLogger.Info("Launch disk node worker: %v", dnWorkerArgs)

	prioCode, err := initPrioCode(dnWorkerArgs.prioCodeConf)
	if err != nil {
		gDnWorkerLogger.Fatal("Init prio code err: %v", err)
	}

	endpoints := strings.Split(dnWorkerArgs.etcdEndpoints, ",")
	dialTimeout := time.Duration(dnWorkerArgs.etcdDialTimeout) * time.Second
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints: endpoints,
		DialTimeout: dialTimeout,
	})
	if err != nil {
		gDnWorkerLogger.Fatal("Create etcd client err: %v", err)
	}
	dnWorker := newDnWorkerServer(
		etcdCli,
		lib.SchemaPrefixDefault,
		prioCode,
		dnWorkerArgs.grpcTarget,
	)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go dnMemberWorker(ctx, &wg, dnWorker)
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM)
	<-signalCh
	gDnWorkerLogger.Info("Cancel all tasks")
	cancel()
	gDnWorkerLogger.Info("Wait")
	wg.Wait()
	gDnWorkerLogger.Info("Exit")
}

func DnWorkerExecute() {
	if err := dnWorkerCmd.Execute(); err != nil {
		gDnWorkerLogger.Fatal("Cmd execute err: %v", err)
	}
}
