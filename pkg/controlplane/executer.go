package controlplane

import (
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"google.golang.org/grpc"
	"github.com/spf13/cobra"

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
	grpcServer *grpc.Server
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

func launchCpApiServer(wg *sync.WaitGroup) {
	defer wg.Done()
	if cpArgs.apiNetwork == "" || cpArgs.apiAddress == "" {
		gLogger.Info("No control plane api server")
	}
	gLogger.Info("Launch control plane api server")
	lis, err := net.Listen(cpArgs.apiNetwork, cpArgs.apiAddress)
	if err != nil {
		gLogger.Fatal("Listen err: %v", err)
	}

	grpcServer = grpc.NewServer()
	cpApi := newCpApiServer()
	pbCpApi.RegisterControlPlaneServer(grpcServer, cpApi)
	if err := grpcServer.Serve(lis); err != nil {
		gLogger.Fatal("Serve err: %v", err)
	}
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
	wg.Add(1)
	go launchCpApiServer(&wg)
	go launchDnMonitor()
	go launchCnMonitor()
	go launchSpMonitor()
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM)
	<-signalCh
	gLogger.Info("wait")
	if grpcServer != nil {
		grpcServer.Stop()
	}
	wg.Wait()
	gLogger.Info("exit")
}

func Execute() {
	if err := cpCmd.Execute(); err != nil {
		gLogger.Info("Cmd execute err: %v", err)
	}
}
