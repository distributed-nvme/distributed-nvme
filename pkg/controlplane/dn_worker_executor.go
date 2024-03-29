package controlplane

import (
	// "time"
	// "net"
	// "strings"

	// "google.golang.org/grpc"
	"github.com/spf13/cobra"
	// clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	// pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

type dnWorkerArgsStruct struct {
	etcdEndpoints string
	etcdDialTimeout int
	grpcNetwork string
	grpcAddress string
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
}

func launchDnWorker(cmd *cobra.Command, args []string) {
	gDnWorkerLogger.Info("Launch disk node worker: %v", dnWorkerArgs)
}

func DnWorkerExecute() {
	if err := dnWorkerCmd.Execute(); err != nil {
		gDnWorkerLogger.Fatal("Cmd execute err: %v", err)
	}
}
