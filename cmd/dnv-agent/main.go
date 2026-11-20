// Command dnv-agent is the node-local agent
// (architecture.md, Components: invocation reference). Its command tree
// (dnagent.md CM1): dn serves DiskNodeAgent and cn serves
// ControllerNodeAgent, sharing the bootstrap, --local-store handling and the
// single OsClient of package agent (CM2-CM4); check dn and check cn are the
// node check (CM5) and collect the gather (CM6), one-shot tools of package
// nodecheck that start no server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/agent/cnagent"
	"github.com/distributed-nvme/distributed-nvme/agent/dnagent"
	"github.com/distributed-nvme/distributed-nvme/agent/nodecheck"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// envPrefix makes every flag but --version settable as
// DNV_AGENT_<FLAG_WITH_UNDERSCORES>.
const envPrefix = "DNV_AGENT"

// requiredCommon are the values both roles must have. They are validated
// after the viper binding rather than through cobra's required-flag
// machinery, so a config file or an environment variable satisfies them just
// as a flag does (CM3).
var requiredCommon = []string{
	"grpc-address", "tr-type", "adr-fam", "tr-addr", "tr-svc-id",
}

// main runs the command tree. A node check or a gather ends with an exit
// status of its own (nodecheck.ExitError), printing its error, if any, once;
// any other error is printed and exits 1 (CM3).
func main() {
	if err := newRootCmd().Execute(); err != nil {
		var exit *nodecheck.ExitError
		if errors.As(err, &exit) {
			if exit.Err != nil {
				fmt.Fprintln(os.Stderr, "dnv-agent:", exit.Err)
			}
			os.Exit(exit.Code)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// The seams the command tree's tests replace: what serves an agent, what
// the cn agent's start check reads (cnagent.md CN-CM2) and the cn server it
// builds with the cut mode the check decided, and what the node check and
// the gather read the node through.
var (
	serveAgent    = agent.Serve
	hostCutCauses = agent.HostCutCauses
	newCnServer   = cnagent.NewCnAgentServer
	nodeEnv       = func() nodecheck.Env {
		return nodecheck.Env{
			// The process's single OsClient (osclient.md).
			Oc:   common.NewLimitedOsClient(0),
			Euid: os.Geteuid,
			Now:  time.Now,
		}
	}
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "dnv-agent",
		Short:        "dnv node agent (dn / cn roles), node check and gather",
		SilenceUsage: true,
	}
	root.AddCommand(newDnCmd(), newCnCmd(), newCheckCmd(), newCollectCmd())
	addVersionFlag(root)
	return root
}

// addVersionFlag gives the root alone --version (architecture.md,
// Components: invocation reference): cobra prints "<program> <version>" on
// stdout and returns before it binds a flag or runs a command. The flag is
// declared here so cobra adds no -v, which reads as verbose, and on the root
// alone, never inherited, so a subcommand refuses it rather than starting
// what it starts.
func addVersionFlag(root *cobra.Command) {
	root.Version = common.BuildVersion()
	root.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	root.Flags().Bool("version", false, "print this program's build version and exit")
}

func newDnCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dn",
		Short: "serve DiskNodeAgent for this disk node",
		RunE:  runDn,
	}
	addCommonFlags(cmd)
	addDiskFlag(cmd)
	return cmd
}

func newCnCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cn",
		Short: "serve ControllerNodeAgent for this controller node",
		RunE:  runCn,
	}
	addCommonFlags(cmd)
	addCapacityFlag(cmd)
	addIgnoreHostDisconnectionFlag(cmd)
	return cmd
}

// addIgnoreHostDisconnectionFlag adds the cn role's
// --ignore-host-disconnection (cnagent.md CN-CM1): it lets the agent start
// on a kernel that cannot end an nvmet connection, and then a removed host
// keeps its connection; where the kernel can, the agent cuts with it or
// without it. A bool, read through viper, so a word it does not take reads
// false: the agent then refuses on a kernel that cannot cut, naming the
// flag.
func addIgnoreHostDisconnectionFlag(cmd *cobra.Command) {
	cmd.Flags().Bool("ignore-host-disconnection", false,
		"start even where the kernel cannot end the connection of a "+
			"host taken off a subsystem's list (no "+
			"CONFIG_NVME_TARGET_DEBUGFS, debugfs not mounted, or "+
			"lockdown); such a host then keeps its connection")
}

// addDiskFlag adds the dn role's --disk.
func addDiskFlag(cmd *cobra.Command) {
	cmd.Flags().String("disk", "",
		"raw block device that carries the dnv disk format (required)")
}

// addCapacityFlag adds the cn role's --capacity. A string flag: pflag's own
// uint64 parse takes Go integer syntax ("0x100", "1_000", a leading 0 as
// octal), which the environment and a config file's string would not get,
// so capacityFromViper holds the three to one decimal rule; a config file's
// YAML number keeps YAML's own syntax and is taken by its value (CM3).
func addCapacityFlag(cmd *cobra.Command) {
	cmd.Flags().String("capacity", "0",
		"capacity budget in bytes this CN is willing to host, a plain "+
			"decimal number; GetCnSize replies it verbatim, 0 = no "+
			"budget of its own: the gateway takes the largest")
}

// notRunOnFlagError makes a flag a check or a gather cannot parse end it
// with nodecheck.ExitNotRun, as any other way it cannot run does (CM5).
func notRunOnFlagError(cmd *cobra.Command) {
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &nodecheck.ExitError{Code: nodecheck.ExitNotRun, Err: err}
	})
}

// newCheckCmd is the node check (CM5): check dn and check cn, each with its
// role's flags, bound as the role binds them, so the agent's own --config
// serves the check too.
func newCheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "check a node before it joins: dn or cn",
	}
	dn := &cobra.Command{
		Use:   "dn",
		Short: "check this node for a disk node agent",
		RunE: func(c *cobra.Command, _ []string) error {
			return runCheck(c, nodecheck.RoleDn)
		},
		SilenceErrors: true,
	}
	addCommonFlags(dn)
	addDiskFlag(dn)
	cn := &cobra.Command{
		Use:   "cn",
		Short: "check this node for a controller node agent",
		RunE: func(c *cobra.Command, _ []string) error {
			return runCheck(c, nodecheck.RoleCn)
		},
		SilenceErrors: true,
	}
	addCommonFlags(cn)
	addCapacityFlag(cn)
	addIgnoreHostDisconnectionFlag(cn)
	cn.Flags().Bool("md-replay", false,
		"also replay md's start rules on two loop devices (a spare "+
			"machine only: it refuses a node that holds dnv objects)")
	for _, sub := range []*cobra.Command{dn, cn} {
		sub.Flags().String("output", nodecheck.OutputText,
			"report form: text or json")
		notRunOnFlagError(sub)
		cmd.AddCommand(sub)
	}
	return cmd
}

// runCheck runs the node check for role: viper binds the role's flags, and
// no value is required, since the check reads the node, not a cluster (CM5).
// It logs at Warn, dnvctl's default level, whatever --log-level says
// (log.md R6).
func runCheck(cmd *cobra.Command, role string) error {
	common.SetLogLevel(slog.LevelWarn)
	if err := bindViper(cmd); err != nil {
		return &nodecheck.ExitError{Code: nodecheck.ExitNotRun, Err: err}
	}
	ctx, stop := toolSignalContext()
	defer stop()
	opts := nodecheck.CheckOptions{
		Role:                    role,
		Disk:                    viper.GetString("disk"),
		LocalStore:              viper.GetString("local-store"),
		MdReplay:                viper.GetBool("md-replay"),
		IgnoreHostDisconnection: viper.GetBool("ignore-host-disconnection"),
	}
	return nodecheck.RunCheck(ctx, nodeEnv(), opts,
		viper.GetString("output"), cmd.OutOrStdout())
}

// newCollectCmd is the gather (CM6).
func newCollectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "collect",
		Short:         "gather this node's state into one archive",
		RunE:          runCollect,
		SilenceErrors: true,
	}
	flags := cmd.Flags()
	flags.String("log-file", "", "the agent's log file")
	flags.String("journal-unit", "", "the systemd unit the agent's log is "+
		"in, read with journalctl")
	flags.String("local-store", common.DefaultLocalStorPrefix,
		"prefix of the agent's local state files, listed by name, size "+
			"and time")
	flags.Duration("deadline", nodecheck.DefaultGatherDeadline,
		"how long the reads that can wait in the kernel may take, all "+
			"together")
	flags.String("out", "", "the archive to write; by default "+
		"dnv-collect-<host>-<time>.tar.gz in the working directory")
	notRunOnFlagError(cmd)
	return cmd
}

// runCollect runs the gather. It logs at Warn, dnvctl's default level
// (log.md R6).
func runCollect(cmd *cobra.Command, _ []string) error {
	common.SetLogLevel(slog.LevelWarn)
	flags := cmd.Flags()
	var opts nodecheck.GatherOptions
	var err error
	get := func(name string) string {
		value, flagErr := flags.GetString(name)
		err = errors.Join(err, flagErr)
		return value
	}
	opts.LogFile = get("log-file")
	opts.JournalUnit = get("journal-unit")
	opts.LocalStore = get("local-store")
	opts.Out = get("out")
	deadline, flagErr := flags.GetDuration("deadline")
	opts.Deadline = deadline
	if err = errors.Join(err, flagErr); err != nil {
		return &nodecheck.ExitError{Code: nodecheck.ExitNotRun, Err: err}
	}
	ctx, stop := toolSignalContext()
	defer stop()
	return nodecheck.RunGather(ctx, nodeEnv(), opts, cmd.OutOrStdout())
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
	flags.String("log-level", common.LogLevelInfo,
		"log level: debug, info or warn; SIGUSR1 sets debug while the "+
			"agent runs and SIGUSR2 sets this level again (the node "+
			"check takes it and logs at warn)")
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
// given, a value from the flag (a string flag for this), the environment or a
// config file's string that is not a plain unsigned decimal number that fits
// 64 bits: digits alone, with no leading zero but in "0" itself. A leading
// zero is refused, not read as decimal, since Go integer syntax, which pflag
// takes, and a YAML number both read it as octal, so reading it as decimal
// would give one spelling two values. A config file's YAML number reaches it
// as YAML decoded it, written out in decimal ("1e12" as 1000000000000,
// "0x100" as 256, "0100" as 64), so it is taken by its value, and one that is
// no whole number, as 1.5 or -1, is refused. viper.GetUint64 would turn
// "500GiB" or "-1" into 0, which the control plane reads as no local opinion
// and replaces with its default, so a typo would size the node with no word
// (CM3). Its range is the control plane's to judge (cnagent.md CN3).
func capacityFromViper() (uint64, error) {
	raw := strings.TrimSpace(viper.GetString("capacity"))
	capacity, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || (len(raw) > 1 && raw[0] == '0') {
		return 0, fmt.Errorf("--capacity must be a plain decimal number "+
			"of bytes, got %q", raw)
	}
	return capacity, nil
}

// startLogLevel sets the role's --log-level (log.md R6) and arms its change
// by SIGUSR1 and SIGUSR2 (log.md R13) under a trace id of its own, since the
// role's startup id is minted inside agent.Serve; the returned stop ends the
// watch.
func startLogLevel(level slog.Level) (stop func()) {
	common.SetLogLevel(level)
	return common.WatchLogLevel(
		common.WithTraceId(context.Background(), common.NewTraceId()), level)
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

// toolSignals are the signals that stop the node check and the gather (CM5,
// CM6): the two that stop an agent, and the hang-up and quit a terminal or
// an ssh session that goes away sends, which would otherwise end the
// process with no teardown.
var toolSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM,
	syscall.SIGHUP, syscall.SIGQUIT}

// toolSignalContext is the node check's and the gather's stop by signal:
// the first of toolSignals cancels the context, so the tool stops at its
// next step and tears down what it made (the md replay's array and loop
// devices); every later one is ignored, so the teardown runs to its end.
// The returned stop ends the watch.
func toolSignalContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, toolSignals...)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()
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
	slog.InfoContext(ctx, "second signal, exiting without a clean stop",
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
	level, err := common.ParseLogLevel(viper.GetString("log-level"))
	if err != nil {
		return err
	}
	stopLogLevel := startLogLevel(level)
	defer stopLogLevel()
	ctx, stop := signalContext()
	defer stop()

	localStore := viper.GetString("local-store")
	nf := common.NewNameFmt(localStore)
	// The process's single OsClient, shared by every wrapper (osclient.md).
	oc := common.NewLimitedOsClient(0)
	// The start record: the build version and the kernel release; a failed
	// read is logged, and the start goes on (CM4; log.md, Agent start).
	agent.LogStartVersions(ctx, oc, false)
	srv := dnagent.NewDnAgentServer(
		oc, nf, localStore, viper.GetString("disk"), trConfFromViper(),
		portId, qidMax)

	return serveAgent(ctx,
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

// runCn mirrors runDn (CM4/CN-CM2), with the cn-only values: --capacity,
// optional, since 0 means "no local opinion" and the CP substitutes
// DefaultCnCap, and --ignore-host-disconnection, which the start check reads
// (startCut).
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
	level, err := common.ParseLogLevel(viper.GetString("log-level"))
	if err != nil {
		return err
	}
	stopLogLevel := startLogLevel(level)
	defer stopLogLevel()
	ctx, stop := signalContext()
	defer stop()

	localStore := viper.GetString("local-store")
	nf := common.NewNameFmt(localStore)
	// The process's single OsClient, shared by every wrapper (osclient.md).
	oc := common.NewLimitedOsClient(0)
	// The start record: the build version, the kernel release and the
	// version of the mdadm the agent runs; a failed read is logged, and the
	// start goes on (CM4, CN-CM2; log.md, Agent start).
	agent.LogStartVersions(ctx, oc, true)
	strayCut, err := startCut(ctx, oc)
	if err != nil {
		return err
	}
	srv := newCnServer(oc, nf, localStore,
		capacity, trConfFromViper(), portId, qidMax, strayCut)

	return serveAgent(ctx,
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

// startCut is the cn agent's start check (cnagent.md CN-CM2): it reads
// whether the kernel can end an nvmet connection (agent.HostCutCauses, the
// read the node check's host cut row makes) and answers the cut mode
// (cnagent.md CN33). Where the kernel can, the mode is cut, whatever
// --ignore-host-disconnection says. Where it cannot, the start ends with an
// error that names every cause and the flag, printed as a flag's is (CM3),
// before any listener opens; with the flag, the agent logs one warning,
// "agent host cut disabled", with the causes, and runs in no cut mode.
func startCut(ctx context.Context, oc common.OsClient) (bool, error) {
	causes := hostCutCauses(ctx, oc)
	if len(causes) == 0 {
		return true, nil
	}
	if !viper.GetBool("ignore-host-disconnection") {
		return false, fmt.Errorf("this kernel cannot end the nvmet "+
			"connection of a host taken off a subsystem's list: %s; fix "+
			"that, or start with --ignore-host-disconnection, under which "+
			"such a host keeps its connection",
			strings.Join(causes, "; "))
	}
	slog.WarnContext(ctx, msgHostCutDisabled,
		slog.String("causes", strings.Join(causes, "; ")))
	return false, nil
}

// msgHostCutDisabled is the cn agent's start warning where the kernel cannot
// end an nvmet connection and --ignore-host-disconnection let it start
// (log.md, Agent start).
const msgHostCutDisabled = "agent host cut disabled"
