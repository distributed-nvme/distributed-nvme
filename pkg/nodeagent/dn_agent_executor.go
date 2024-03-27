package nodeagent

import (
	"net"

	"google.golang.org/grpc"
	"github.com/spf13/cobra"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeagent"
)

type dnAgentArgsStruct {
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
	gLogger = lib.NewLogger("dn_agent")
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

	opts := []logging.Option{
		logging.WithLogOnEvents(logging.StartCall, logging.FinishCall),
	}

	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			logging.UnaryServerInterceptor(lib.InterceptorLogger(&gLogger), opts...),
			lib.UnaryShowReqReplyInterceptor(&gLogger),
		),
		grpc.ChainStreamInterceptor(
			logging.StreamServerInterceptor(lib.InterceptorLogger(&gLogger), opts...),
		),
	)

	pbnd.RegisterDnAgentServer(grpcServer, dnAgent)
	if err := grpcServer.Serve(lis); err != nil {
		gLogger.Fatal("Serve err: %v", err)
	}

	gLogger.Info("Exit dn agent")
}

func DnAgentExecute() {
	if err := dnAgentCmd.Execute(); err != nil {
		gLogger.Info("Cmd execute err: %v", err)
	}
}
