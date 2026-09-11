// Command dnv-agent is the node-local agent (architecture.md §13). It has
// exactly two subcommands: dn serves DiskNodeAgent, cn serves
// ControllerNodeAgent; both share the bootstrap, --local-store handling and
// the single OsClient of package agent (dnagent.md §3).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/agent/cnagent"
	"github.com/distributed-nvme/distributed-nvme/agent/dnagent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// envPrefix makes every flag settable as DNV_AGENT_<FLAG_WITH_UNDERSCORES>.
const envPrefix = "DNV_AGENT"

// requiredCommon are the values both roles must have. They are validated
// after the viper binding rather than through cobra's required-flag
// machinery, so a config file or an environment variable satisfies them just
// as a flag does (CM3).
var requiredCommon = []string{
	"grpc-address", "tr-type", "adr-fam", "tr-addr", "tr-svc-id",
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "dnv-agent",
		Short:        "dnv node agent (dn / cn roles)",
		SilenceUsage: true,
	}
	root.AddCommand(newDnCmd(), newCnCmd())
	return root
}

func newDnCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dn",
		Short: "serve DiskNodeAgent for this disk node",
		RunE:  runDn,
	}
	addCommonFlags(cmd)
	cmd.Flags().String("disk", "",
		"raw block device that carries the dnv disk format (required)")
	return cmd
}

func newCnCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cn",
		Short: "serve ControllerNodeAgent for this controller node",
		RunE:  runCn,
	}
	addCommonFlags(cmd)
	cmd.Flags().Uint64("capacity", 0,
		"capacity budget in bytes this CN is willing to host; "+
			"GetCnSize replies it verbatim, 0 = use the CP default")
	return cmd
}

func addCommonFlags(cmd *cobra.Command) {
	flags := cmd.Flags()
	flags.String("grpc-network", "tcp", "net.Listen network")
	flags.String("grpc-address", "",
		"gRPC endpoint the control plane stores as addr_port (required)")
	flags.String("tr-type", "", "nvmet port transport type (required)")
	flags.String("adr-fam", "", "nvmet port address family (required)")
	flags.String("tr-addr", "", "nvmet port transport address (required)")
	flags.String("tr-svc-id", "", "nvmet port service id (required)")
	flags.String("local-store", common.DefaultLocalStorPrefix,
		"prefix of the agent's local state files")
	flags.String("config", "", "optional viper config file")
}

// bindViper wires flags, config file and environment together; every value
// read afterwards goes through viper, so file and env win per viper
// precedence (CM3).
func bindViper(cmd *cobra.Command) error {
	if err := viper.BindPFlags(cmd.Flags()); err != nil {
		return err
	}
	viper.SetEnvPrefix(envPrefix)
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()
	if path := viper.GetString("config"); path != "" {
		viper.SetConfigFile(path)
		if err := viper.ReadInConfig(); err != nil {
			return err
		}
	}
	return nil
}

func requireValues(names ...string) error {
	var missing []string
	for _, name := range names {
		if strings.TrimSpace(viper.GetString(name)) == "" {
			missing = append(missing, "--"+name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required flag(s): %s",
			strings.Join(missing, ", "))
	}
	return nil
}

func trConfFromViper() *pb.NvmeTrConf {
	return &pb.NvmeTrConf{
		TrType:  viper.GetString("tr-type"),
		AdrFam:  viper.GetString("adr-fam"),
		TrAddr:  viper.GetString("tr-addr"),
		TrSvcId: viper.GetString("tr-svc-id"),
	}
}

func runDn(cmd *cobra.Command, args []string) error {
	if err := bindViper(cmd); err != nil {
		return err
	}
	if err := requireValues(append(requiredCommon, "disk")...); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(
		context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	localStore := viper.GetString("local-store")
	nf := common.NewNameFmt(localStore)
	// The process's single OsClient, shared by every wrapper (osclient.md).
	oc := common.NewLimitedOsClient(0)
	srv := dnagent.NewDnAgentServer(
		oc, nf, localStore, viper.GetString("disk"), trConfFromViper())

	return agent.Serve(ctx,
		viper.GetString("grpc-network"), viper.GetString("grpc-address"),
		srv.Reconcile,
		func(grpcServer *grpc.Server) {
			pb.RegisterDiskNodeAgentServer(grpcServer, srv)
		},
		// The dn owns background goroutines with long-running children — the
		// §9.4 zeroing loop's `blkdiscard --zeroout` — so shutdown joins them
		// and no orphan child outlives the agent (dnagent.md SH27).
		srv.WaitBackground)
}

// runCn mirrors runDn (CM4/CN-CM2): --capacity is the one cn-only value and
// is optional, since 0 means "no local opinion" and the CP substitutes
// DefaultCnCap.
func runCn(cmd *cobra.Command, args []string) error {
	if err := bindViper(cmd); err != nil {
		return err
	}
	if err := requireValues(requiredCommon...); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(
		context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	localStore := viper.GetString("local-store")
	nf := common.NewNameFmt(localStore)
	// The process's single OsClient, shared by every wrapper (osclient.md).
	oc := common.NewLimitedOsClient(0)
	srv := cnagent.NewCnAgentServer(oc, nf, localStore,
		viper.GetUint64("capacity"), trConfFromViper())

	return agent.Serve(ctx,
		viper.GetString("grpc-network"), viper.GetString("grpc-address"),
		srv.Reconcile,
		func(grpcServer *grpc.Server) {
			pb.RegisterControllerNodeAgentServer(grpcServer, srv)
		},
		// The cn passes nil: its CN11 leg probers are stopped by cancelling
		// rootCtx and are deliberately never joined — a probe wedged in an
		// uninterruptible pread would hang shutdown forever, which is the very
		// starvation the probe-IO carve-out exists to prevent.
		nil)
}
