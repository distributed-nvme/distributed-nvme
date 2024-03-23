package nodeagent

import (
	"net"

	"google.golang.org/grpc"
	"github.com/spf13/cobra"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeapi"
)

type ndArgsStruct struct {
	network     string
	address     string
	role        string
}

var (
	ndCmd = &cobra.Command{
		Use: "dnv_nodeagent",
		Short: "dnv node agent",
		Long: `dnv node agent`,
		Run: launchNd,
	}
	ndArgs = ndArgsStruct{}
	gLogger = lib.NewLogger("nodeagent")
)

func init() {
	ndCmd.PersistentFlags().StringVarP(
		&ndArgs.network, "network", "", "tcp", "grpc network",
	)
	ndCmd.PersistentFlags().StringVarP(
		&ndArgs.address, "address", "", ":9521", "grpc address",
	)
	ndCmd.PersistentFlags().StringVarP(
		&ndArgs.role, "role", "", "", "agent role, valid values: cn, dn",
	)
}

func launchNd(cmd *cobra.Command, args []string) {
	gLogger.Info("ndArgs: %v", ndArgs)
}

func launchCnAgent() {
	gLogger.Info("Launch cn agent")

	lis, err := net.Listen(ndArgs.network, ndArgs.address)
	if err != nil {
		gLogger.Fatal("Listen err: %v", err)
	}

	logger := lib.NewLogger("cnagent")

	cnAgent := newCnAgentServer(logger)

	opts := []logging.Option{
		logging.WithLogOnEvents(logging.StartCall, logging.FinishCall),
	}
	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			logging.UnaryServerInterceptor(lib.InterceptorLogger(logger), opts...),
			lib.UnaryShowReqReplyInterceptor(logger),
		),
		grpc.ChainStreamInterceptor(
			logging.StreamServerInterceptor(lib.InterceptorLogger(logger), opts...),
		),
	)

	pbnd.RegisterCnAgentServer(grpcServer, cnAgent)
	if err := grpcServer.Serve(lis); err != nil {
		gLogger.Fatal("Serve err: %v", err)
	}

	gLogger.Info("Exit cn agent")
}

func launchDnAgent() {
	gLogger.Info("Launch dn agent")

	lis, err := net.Listen(ndArgs.network, ndArgs.address)
	if err != nil {
		gLogger.Fatal("Listen err: %v", err)
	}

	logger := lib.NewLogger("dnagent")

	dnAgent := newDnAgentServer(logger)

	opts := []logging.Option{
		logging.WithLogOnEvents(logging.StartCall, logging.FinishCall),
	}
	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			logging.UnaryServerInterceptor(lib.InterceptorLogger(logger), opts...),
			lib.UnaryShowReqReplyInterceptor(logger),
		),
		grpc.ChainStreamInterceptor(
			logging.StreamServerInterceptor(lib.InterceptorLogger(logger), opts...),
		),
	)

	pbnd.RegisterDnAgentServer(grpcServer, dnAgent)
	if err := grpcServer.Serve(lis); err != nil {
		gLogger.Fatal("Serve err: %v", err)
	}

	gLogger.Info("Exit dn agent")
}

func Execute() {
	if err := ndCmd.Execute(); err != nil {
		gLogger.Info("Cmd execute err: %v", err)
	}
	switch ndArgs.role {
	case "cn":
		launchCnAgent()
	case "dn":
		launchDnAgent()
	default:
		gLogger.Fatal("Invalid role: %v, valid values are cn, dn", ndArgs.role)
	}
}
