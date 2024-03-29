package nodeagent

import (
	"net"

	"google.golang.org/grpc"
	"github.com/spf13/cobra"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeagent"
)

type dnAgentArgsStruct struct {
	grpcNetwork string
	grpcAddress string
}

var (
	dnAgentCmd = &cobra.Command{
		Use: "dnv_dn_agent",
		Short: "dnv disk node agent",
		Long: `dnv disk node agent`,
		Run: launchDnAgent,
	}
	dnAgentArgs = dnAgentArgsStruct{}
	gLogger = lib.NewPrefixLogger("dn_agent")
)

func init() {
	dnAgentCmd.PersistentFlags().StringVarP(
		&dnAgentArgs.grpcNetwork, "grpc-network", "", "tcp", "grpc network",
	)
	dnAgentCmd.PersistentFlags().StringVarP(
		&dnAgentArgs.grpcAddress, "grpc-address", "", ":9620", "grpc address",
	)
}

func launchDnAgent(cmd *cobra.Command, args []string) {
	gLogger.Info("Launch disk node agent: %v", dnAgentArgs)
	lis, err := net.Listen(dnAgentArgs.grpcNetwork, dnAgentArgs.grpcAddress)
	if err != nil {
		gLogger.Fatal("Listen err: %v", err)
	}

	dnAgent := newDnAgentServer()

	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			lib.UnaryServerPerCtxHelperInterceptor,
		),
	)

	pbnd.RegisterDiskNodeAgentServer(grpcServer, dnAgent)
	if err := grpcServer.Serve(lis); err != nil {
		gLogger.Fatal("Serve err: %v", err)
	}

	gLogger.Info("Exit disk node agent")
}

func DnAgentExecute() {
	if err := dnAgentCmd.Execute(); err != nil {
		gLogger.Info("Cmd execute err: %v", err)
	}
}
