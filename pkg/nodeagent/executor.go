package nodeagent

import (
	"context"
	"net"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/prefixlog"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeagent"
)

type agentArgsStruct struct {
	role        string
	grpcNetwork string
	grpcAddress string
}

var (
	agentCmd = &cobra.Command{
		Use:   "dnvagent",
		Short: "dnv dataplane agent",
		Long:  `dnv dataplaine agent`,
		Run:   launchAgent,
	}
	agentArgs = agentArgsStruct{}
	gLogger   = prefixlog.NewPrefixLogger("agent")
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

func launchDnAgent() {
	ctx, cancel := context.WithCancel(context.Background())

	lis, err := net.Listen(agentArgs.grpcNetwork, agentArgs.grpcAddress)
	if err != nil {
		gLogger.Fatal("Listen err: %v", err)
	}

	dnAgent := newDnAgentServer(
		ctx,
		constants.LocalDataPathDefault,
		constants.DnAgentBgInterval,
	)

	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			ctxhelper.UnaryServerPerCtxHelperInterceptor,
		),
	)

	pbnd.RegisterDiskNodeAgentServer(grpcServer, dnAgent)
	if err := grpcServer.Serve(lis); err != nil {
		gLogger.Fatal("Serve err: %v", err)
	}

	cancel()
	gLogger.Info("Exit disk node agent")
}

func launchCnAgent() {
	ctx, cancel := context.WithCancel(context.Background())

	lis, err := net.Listen(agentArgs.grpcNetwork, agentArgs.grpcAddress)
	if err != nil {
		gLogger.Fatal("Listen err: %v", err)
	}

	cnAgent := newCnAgentServer(
		ctx,
		constants.LocalDataPathDefault,
		constants.CnAgentBgInterval,
	)

	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			ctxhelper.UnaryServerPerCtxHelperInterceptor,
		),
	)

	pbnd.RegisterControllerNodeAgentServer(grpcServer, cnAgent)
	if err := grpcServer.Serve(lis); err != nil {
		gLogger.Fatal("Serve err: %v", err)
	}

	cancel()
	gLogger.Info("Exit controller node agent")
}

func launchAgent(cmd *cobra.Command, args []string) {
	gLogger.Info("Launch agent: %v", agentArgs)

	switch agentArgs.role {
	case "dn":
		launchDnAgent()
	case "cn":
		launchCnAgent()
	default:
		gLogger.Fatal("Unknow role: %s", agentArgs.role)
	}
}

func Execute() {
	if err := agentCmd.Execute(); err != nil {
		gLogger.Info("Cmd execute err: %v", err)
	}
}
