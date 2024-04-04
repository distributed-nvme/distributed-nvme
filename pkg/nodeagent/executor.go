package nodeagent

import (
	"net"

	"google.golang.org/grpc"
	"github.com/spf13/cobra"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeagent"
)

type agentArgsStruct struct {
	role string
	grpcNetwork string
	grpcAddress string
}

var (
	agentCmd = &cobra.Command{
		Use: "dnvagent",
		Short: "dnv dataplane agent",
		Long: `dnv dataplaine agent`,
		Run: launchAgent,
	}
	agentArgs = agentArgsStruct{}
	gLogger = lib.NewPrefixLogger("agent")
)

func init() {
	agentCmd.PersistentFlags().StringVarP(
		&agentArgs.role, "role", "", "", "agent role, either dn or cn",
	)
	agentCmd.PersistentFlags().StringVarP(
		&agentArgs.grpcNetwork, "grpc-network", "", "tcp", "grpc network",
	)
	agentCmd.PersistentFlags().StringVarP(
		&agentArgs.grpcAddress, "grpc-address", "", ":9620", "grpc address",
	)
}

func launchAgent(cmd *cobra.Command, args []string) {
	gLogger.Info("Launch agent: %v", agentArgs)
	lis, err := net.Listen(agentArgs.grpcNetwork, agentArgs.grpcAddress)
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

func Execute() {
	if err := agentCmd.Execute(); err != nil {
		gLogger.Info("Cmd execute err: %v", err)
	}
}
