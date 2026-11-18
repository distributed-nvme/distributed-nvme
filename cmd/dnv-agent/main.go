// Command dnv-agent is the node-local agent
// (architecture.md, Components: invocation reference). It has
// exactly two subcommands: dn serves DiskNodeAgent, cn serves
// ControllerNodeAgent; both share the bootstrap, --local-store handling and
// the single OsClient of package agent (dnagent.md CM1-CM4).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
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
	// A string flag: pflag's own uint64 parse takes Go integer syntax
	// ("0x100", "1_000", a leading 0 as octal), which the environment and a
	// config file's string would not get, so capacityFromViper holds the
	// three to one decimal rule; a config file's YAML number keeps YAML's own
	// syntax and is taken by its value (CM3).
	cmd.Flags().String("capacity", "0",
		"capacity budget in bytes this CN is willing to host, a plain "+
			"decimal number; GetCnSize replies it verbatim, 0 = use the "+
			"CP default")
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
	flags.Int("nvmet-port-id", common.NvmetPortId,
		"configfs id of the nvmet port this agent converges "+
			"(/sys/kernel/config/nvmet/ports/<id>); agents that need their "+
			"own port take distinct ids, while a dn and a cn on one node "+
			"may share the default")
	flags.Int("nvmet-qid-max", common.NvmetQidMax,
		fmt.Sprintf("IO queue cap (attr_qid_max) written on each nvmet "+
			"subsystem before it is linked to the port, from 1 to %d; "+
			"0 writes none and leaves nvmet's default",
			common.NvmetQidMaxLimit))
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

// nvmetPortIdFromViper resolves --nvmet-port-id and refuses anything below
// 1: configfs has no ports/0, and viper.GetInt turns a non-numeric env or
// config value into 0, so the floor also catches a typo. requireValues
// cannot express this — it is string-only and only tests for emptiness —
// so this is its own check, run after bindViper for the same reason (CM3):
// a config file or DNV_AGENT_NVMET_PORT_ID must be validated too, not just
// the flag.
func nvmetPortIdFromViper() (int, error) {
	portId := viper.GetInt("nvmet-port-id")
	if portId < 1 {
		return 0, fmt.Errorf("--nvmet-port-id must be >= 1, got %d", portId)
	}
	return portId, nil
}

// nvmetQidMaxFromViper resolves --nvmet-qid-max and refuses a value outside
// 0..common.NvmetQidMaxLimit, the range nvmet takes plus 0 for "write none".
// It parses the resolved string itself: viper.GetInt turns a non-numeric env
// or config value into 0, which here is a valid value that would turn the
// cap off without a word (CM3).
func nvmetQidMaxFromViper() (int, error) {
	raw := strings.TrimSpace(viper.GetString("nvmet-qid-max"))
	qidMax, err := strconv.Atoi(raw)
	if err != nil || qidMax < 0 || qidMax > common.NvmetQidMaxLimit {
		return 0, fmt.Errorf("--nvmet-qid-max must be 0 to %d, got %q",
			common.NvmetQidMaxLimit, raw)
	}
	return qidMax, nil
}

// capacityFromViper resolves --capacity and refuses, naming the value as
// given, a value from the flag (a string flag for this), the environment
// or a config file's string that is not a plain unsigned decimal number
// that fits 64 bits: digits alone, with no leading zero but in "0" itself.
// A leading zero is refused, not read as decimal, since the flag's former
// uint64 parse read it as octal, so its meaning would change with no word.
// A config file's YAML number reaches it as YAML decoded it, written out in
// decimal ("1e12" as 1000000000000, "0x100" as 256, "0100" as 64), so it is
// taken by its value, and one that is no whole number, as 1.5 or -1, is
// refused. viper.GetUint64 would turn "500GiB" or "-1" into 0, which the
// control plane reads as no local opinion and replaces with its default, so
// a typo would size the node with no word (CM3). Its range is the control
// plane's to judge (cnagent.md CN3).
func capacityFromViper() (uint64, error) {
	raw := strings.TrimSpace(viper.GetString("capacity"))
	capacity, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || (len(raw) > 1 && raw[0] == '0') {
		return 0, fmt.Errorf("--capacity must be a plain decimal number "+
			"of bytes, got %q", raw)
	}
	return capacity, nil
}

// signalContext is the agent's stop by signal (CM4): the first SIGINT or
// SIGTERM cancels the context, which starts Serve's bounded stop; a second
// one ends the process at once with status 1, as every other dnv program's
// does (watchSignals), skipping the join of background work, so a child of
// it, such as a zeroing batch, outlives the agent (SH27). signal.NotifyContext
// is not used: it stops reading after the first signal, which would swallow
// the second. The returned stop ends the watch.
func signalContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	// Capacity 2: the first signal starts the stop, the second ends it.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go watchSignals(ctx, cancel, sigCh, os.Exit)
	return ctx, func() {
		signal.Stop(sigCh)
		cancel()
	}
}

// watchSignals waits for the first signal on sigCh and cancels ctx, then
// for a second one, on which it calls exit(1). It returns when ctx ends
// before any signal, so a clean stop leaves no watcher behind. exit is a
// parameter so the unit tests can observe the second signal without ending
// the test binary.
func watchSignals(
	ctx context.Context,
	cancel context.CancelFunc,
	sigCh <-chan os.Signal,
	exit func(int),
) {
	var first os.Signal
	select {
	case first = <-sigCh:
	case <-ctx.Done():
		return
	}
	slog.InfoContext(ctx, "signal received",
		slog.String("signal", first.String()))
	cancel()

	second, ok := <-sigCh
	if !ok {
		return
	}
	slog.WarnContext(ctx, "second signal, exiting without a clean stop",
		slog.String("signal", second.String()))
	exit(1)
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
	portId, err := nvmetPortIdFromViper()
	if err != nil {
		return err
	}
	qidMax, err := nvmetQidMaxFromViper()
	if err != nil {
		return err
	}
	ctx, stop := signalContext()
	defer stop()

	localStore := viper.GetString("local-store")
	nf := common.NewNameFmt(localStore)
	// The process's single OsClient, shared by every wrapper (osclient.md).
	oc := common.NewLimitedOsClient(0)
	srv := dnagent.NewDnAgentServer(
		oc, nf, localStore, viper.GetString("disk"), trConfFromViper(),
		portId, qidMax)

	return agent.Serve(ctx,
		viper.GetString("grpc-network"), viper.GetString("grpc-address"),
		srv.Prepare, srv.Reconcile,
		func(grpcServer *grpc.Server) {
			pb.RegisterDiskNodeAgentServer(grpcServer, srv)
		},
		// The dn owns background goroutines with long-running children — the
		// DN9 zeroing loop's `blkdiscard --zeroout` — so shutdown joins them
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
	portId, err := nvmetPortIdFromViper()
	if err != nil {
		return err
	}
	qidMax, err := nvmetQidMaxFromViper()
	if err != nil {
		return err
	}
	capacity, err := capacityFromViper()
	if err != nil {
		return err
	}
	ctx, stop := signalContext()
	defer stop()

	localStore := viper.GetString("local-store")
	nf := common.NewNameFmt(localStore)
	// The process's single OsClient, shared by every wrapper (osclient.md).
	oc := common.NewLimitedOsClient(0)
	srv := cnagent.NewCnAgentServer(oc, nf, localStore,
		capacity, trConfFromViper(), portId, qidMax)

	return agent.Serve(ctx,
		viper.GetString("grpc-network"), viper.GetString("grpc-address"),
		srv.Prepare, srv.Reconcile,
		func(grpcServer *grpc.Server) {
			pb.RegisterControllerNodeAgentServer(grpcServer, srv)
		},
		// The cn passes nil: its CN11 leg probers are stopped by cancelling
		// rootCtx and are deliberately never joined — a probe wedged in an
		// uninterruptible pread would hang shutdown forever, which is the very
		// starvation the probe-IO carve-out exists to prevent. Nor is the child
		// of its sweep's background `nvme disconnect`: it holds nothing a
		// restarted agent needs (cnagent.md CN10).
		nil)
}
