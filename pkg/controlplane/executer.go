package controlplane

import (
	"context"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"google.golang.org/grpc"
	"github.com/spf13/cobra"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbCpApi "github.com/distributed-nvme/distributed-nvme/pkg/proto/cpapi"
)

type cpArgsStruct struct {
	etcdEndpoints string
	apiNetwork string
	apiAddress string
	cnInterval int
	dnInterval int
	spInterval int
}

var (
	cpCmd = &cobra.Command{
		Use:   "dnv_cp",
		Short: "dnv control plane",
		Long:  `dnv control plane`,
		Run:   launchCp,
	}
	cpArgs = cpArgsStruct{}
	gLogger = lib.NewLogger("controlplane")
)

func init() {
	cpCmd.PersistentFlags().StringVarP(
		&cpArgs.etcdEndpoints,
		"etcd-endpoints", "", "localhost:2379", "etcd endpoint list")
	cpCmd.PersistentFlags().StringVarP(
		&cpArgs.apiNetwork, "api-network", "", "tcp", "api network")
	cpCmd.PersistentFlags().StringVarP(
		&cpArgs.apiAddress, "api-address", "", ":9520", "api address")
	cpCmd.PersistentFlags().IntVarP(
		&cpArgs.cnInterval, "cn-interval", "", 5, "cn interval")
	cpCmd.PersistentFlags().IntVarP(
		&cpArgs.dnInterval, "dn-interval", "", 5, "dn interval")
	cpCmd.PersistentFlags().IntVarP(
		&cpArgs.spInterval, "sp-interval", "", 5, "sp interval")
}

func launchCpApiServer(wg *sync.WaitGroup, ctx context.Context) {
	defer wg.Done()
	if cpArgs.apiNetwork == "" || cpArgs.apiAddress == "" {
		gLogger.Info("No control plane api server")
	}
	
	gLogger.Info("Launch control plane api server")
	lis, err := net.Listen(cpArgs.apiNetwork, cpArgs.apiAddress)
	if err != nil {
		gLogger.Fatal("Listen err: %v", err)
	}
	etcdEndpoints := strings.Split(cpArgs.etcdEndpoints, ",")
	etcdCli, err := clientv3.New(clientv3.Config{Endpoints: etcdEndpoints})
	if err != nil {
		gLogger.Fatal("Create etcd client err: %v", err)
	}
	cpApi := newCpApiServer(etcdCli)
	grpcServer := grpc.NewServer()
	go func() {
		for {
			select {
			case <-ctx.Done():
				gLogger.Info("Stop control plane api server")
				grpcServer.Stop()
				return
			}
		}
	}()
	pbCpApi.RegisterControlPlaneServer(grpcServer, cpApi)
	if err := grpcServer.Serve(lis); err != nil {
		gLogger.Fatal("Serve err: %v", err)
	}
	gLogger.Info("Exit control plane api server")
}

func launchDnMonitor() {
	gLogger.Info("launchDnMonitor")
}

func launchCnMonitor() {
	gLogger.Info("launchCnMonitor")
}

func launchSpMonitor() {
	gLogger.Info("launchSpMonitor")
}

func launchCp(cmd *cobra.Command, args []string) {
	gLogger.Info("cpArgs: %v", cpArgs)
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	wg.Add(1)
	go launchCpApiServer(&wg, ctx)
	go launchDnMonitor()
	go launchCnMonitor()
	go launchSpMonitor()
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM)
	<-signalCh
	gLogger.Info("Cancel all tasks")
	cancel()
	gLogger.Info("Wait")
	wg.Wait()
	gLogger.Info("Exit")
}

func Execute() {
	if err := cpCmd.Execute(); err != nil {
		gLogger.Info("Cmd execute err: %v", err)
	}
}
