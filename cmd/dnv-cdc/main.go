// Command dnv-cdc is the NVMe-oF central discovery controller (cdc.md §6):
// one cobra root command without subcommands that parses the CM1 flags
// through viper, builds the process's single etcdutil client (EU1) and hands
// off to cdc.Run, which owns the etcd watcher, the per-host view registry and
// the NVMe/TCP listener.
//
// main itself is deliberately thin (layout.md §5): it never touches etcd, it
// never opens a socket, and the "cdc starting" / "cdc stopping" records of §7
// are emitted by cdc.Run, which is why the endpoints travel to it in
// cdc.Config (CM3).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/distributed-nvme/distributed-nvme/cdc"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
)

// envPrefix makes every flag settable as DNV_CDC_<FLAG_WITH_UNDERSCORES>
// (CM1).
const envPrefix = "DNV_CDC"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// newRootCmd builds the CM1 command: one root, no subcommands.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "dnv-cdc",
		Short: "dnv central discovery controller (NVMe/TCP)",
		Long: "dnv-cdc watches the cdc keys in etcd, keeps a per-host " +
			"filtered discovery log and answers hosts on the well-known " +
			"discovery NQN over NVMe/TCP (cdc.md).",
		SilenceUsage: true,
		// main reports the error on stderr and exits 1; without this cobra
		// would print the very same line a second time.
		SilenceErrors: true,
		// No positional arguments: every input is a flag, a config key or an
		// environment variable.
		Args: cobra.NoArgs,
		RunE: run,
	}
	addFlags(root)
	return root
}

// addFlags declares the CM1 flag set. There is deliberately no
// --etcd-op-timeout: every plain etcd operation is bounded by
// common.DefaultEtcdOpTimeout (EU5), exactly as in cmd/dnv-worker.
func addFlags(cmd *cobra.Command) {
	flags := cmd.Flags()
	flags.String("etcd-endpoints", "",
		"comma-separated host:port list of the etcd cluster (required)")
	flags.Int("etcd-dial-timeout", common.DefaultEtcdDialTimeout,
		"seconds to wait for the initial etcd dial (0 = the built-in default)")
	flags.String("range", common.CdcRangeAll,
		"comma-separated hex digits, each claiming the sixteen shard "+
			"codes h0..hf")
	flags.String("tr-type", common.DefaultCdcTrType,
		"transport type of the listen endpoint; only tcp is implemented")
	flags.String("adr-fam", common.DefaultCdcAdrFam,
		"address family of the listen endpoint: ipv4 or ipv6")
	flags.String("tr-addr", "", "listen address (required)")
	flags.String("tr-svc-id", common.DefaultCdcTrSvcId, "listen port")
	flags.String("config", "", "optional viper config file")
}

// bindViper wires flags, config file and environment together exactly as
// cmd/dnv-worker and cmd/dnv-agent do (CM1); every value is read through viper
// afterwards, so a file or the environment satisfies a required value just as
// a flag does.
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

// options is the validated CM1 configuration, ready to become a cdc.Config.
type options struct {
	// endpoints is the parsed --etcd-endpoints list, never empty (CM2).
	endpoints []string
	// dialTimeout bounds the initial etcd dial (EU1, EU5). A non-positive
	// value is passed through: etcdutil.New substitutes
	// common.DefaultEtcdDialTimeout for it.
	dialTimeout time.Duration
	// ranges is the parsed --range list: non-empty, duplicate-free, every
	// element in [0, 15], in the order given (CM2).
	ranges []uint32
	// The validated listen endpoint (CM2).
	trType  string
	adrFam  string
	trAddr  string
	trSvcId string
}

// optionsFromViper reads and validates the CM1 values after the binding above.
// It performs no I/O and logs nothing, so the §8 unit tests can drive every
// CM2 branch directly.
func optionsFromViper() (*options, error) {
	endpoints := splitList(viper.GetString("etcd-endpoints"))
	if len(endpoints) == 0 {
		return nil, errors.New("missing required flag: --etcd-endpoints")
	}
	ranges, err := parseRanges(viper.GetString("range"))
	if err != nil {
		return nil, err
	}
	trType := viper.GetString("tr-type")
	if trType != common.DefaultCdcTrType {
		return nil, fmt.Errorf(
			"--tr-type must be %q, got %q", common.DefaultCdcTrType, trType)
	}
	adrFam := viper.GetString("adr-fam")
	switch adrFam {
	case common.DefaultCdcAdrFam, common.CdcAdrFamIpv6:
	default:
		return nil, fmt.Errorf(
			"--adr-fam must be %q or %q, got %q",
			common.DefaultCdcAdrFam, common.CdcAdrFamIpv6, adrFam)
	}
	trAddr := strings.TrimSpace(viper.GetString("tr-addr"))
	if trAddr == "" {
		return nil, errors.New("missing required flag: --tr-addr")
	}
	if err := checkTrAddr(adrFam, trAddr); err != nil {
		return nil, err
	}
	trSvcId := strings.TrimSpace(viper.GetString("tr-svc-id"))
	if err := checkTrSvcId(trSvcId); err != nil {
		return nil, err
	}
	return &options{
		endpoints: endpoints,
		dialTimeout: time.Duration(viper.GetInt("etcd-dial-timeout")) *
			time.Second,
		ranges:  ranges,
		trType:  trType,
		adrFam:  adrFam,
		trAddr:  trAddr,
		trSvcId: trSvcId,
	}, nil
}

// splitList parses one comma-separated flag value: entries are trimmed and
// empty ones dropped, so "a, b," is the two-element list. Both list flags are
// plain strings rather than pflag string slices on purpose — viper's
// GetStringSlice does not split a single environment or config string on
// commas, and CM1 requires the flag, the file and the environment to mean the
// same thing.
func splitList(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// parseRanges validates --range (CM2): a non-empty, duplicate-free list of
// single hex digits, each claiming the sixteen shard codes h0..hf (DS2). The
// spelling is deliberately strict — "0f" or "F" is a typo for a range, not a
// range — because a mis-parsed digit is a silent coverage gap no instance can
// detect (§0 #3).
func parseRanges(raw string) ([]uint32, error) {
	items := splitList(raw)
	if len(items) == 0 {
		return nil, errors.New(
			"--range must name at least one hex digit in [0-9a-f]")
	}
	seen := make(map[uint32]bool, len(items))
	out := make([]uint32, 0, len(items))
	for _, item := range items {
		if len(item) != 1 {
			return nil, fmt.Errorf(
				"--range: %q is not a single hex digit in [0-9a-f]", item)
		}
		value, err := strconv.ParseUint(item, 16, 8)
		if err != nil || fmt.Sprintf("%x", value) != item {
			return nil, fmt.Errorf(
				"--range: %q is not a single hex digit in [0-9a-f]", item)
		}
		digit := uint32(value)
		if seen[digit] {
			return nil, fmt.Errorf("--range: duplicate digit %q", item)
		}
		seen[digit] = true
		out = append(out, digit)
	}
	return out, nil
}

// checkTrAddr rejects a listen address that is not a literal of the declared
// address family (CM2). A name is refused on purpose: a discovery service
// whose endpoint resolves differently on two hosts is a support call, not a
// feature.
func checkTrAddr(adrFam string, trAddr string) error {
	addr := net.ParseIP(trAddr)
	if addr == nil {
		return fmt.Errorf("--tr-addr: %q is not an IP address", trAddr)
	}
	isV4 := addr.To4() != nil
	if adrFam == common.DefaultCdcAdrFam && !isV4 {
		return fmt.Errorf(
			"--tr-addr: %q is not an %s address", trAddr,
			common.DefaultCdcAdrFam)
	}
	if adrFam == common.CdcAdrFamIpv6 && isV4 {
		return fmt.Errorf(
			"--tr-addr: %q is not an %s address", trAddr, common.CdcAdrFamIpv6)
	}
	return nil
}

// checkTrSvcId rejects a port that is not a decimal number in range (CM2).
func checkTrSvcId(trSvcId string) error {
	if trSvcId == "" {
		return errors.New("--tr-svc-id must not be empty")
	}
	port, err := strconv.ParseUint(trSvcId, 10, 16)
	if err != nil || port == 0 {
		return fmt.Errorf(
			"--tr-svc-id: %q is not a tcp port in [1, 65535]", trSvcId)
	}
	return nil
}

// run is the CM3/CM4 startup path. The default JSON logger is already
// installed by common's init (log.md R3), so the only work here is: bind,
// validate, mint the startup trace id, arm the CM5 signal handling, build the
// etcd client and hand off to cdc.Run.
//
// Startup never fails because etcd is unreachable: etcdutil.New dials lazily
// and dnv-cdc keeps serving its last known state through an outage anyway
// (DS10). A configuration error, or a listen address already in use, is what
// makes the process exit non-zero.
func run(cmd *cobra.Command, args []string) error {
	if err := bindViper(cmd); err != nil {
		return err
	}
	opts, err := optionsFromViper()
	if err != nil {
		return err
	}

	ctx := common.WithTraceId(context.Background(), common.NewTraceId())
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Capacity 2: the first signal starts the CM5 drain, the second aborts
	// it. signal.NotifyContext is not used because it stops consuming after
	// the first signal, which would swallow the second one.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go watchSignals(ctx, cancel, sigCh, os.Exit)

	cli, err := etcdutil.New(ctx, opts.endpoints, opts.dialTimeout)
	if err != nil {
		return err
	}
	// cdc.Run closes the client as the last step of its CM5 drain, so it is
	// deliberately not closed here.
	return cdc.Run(ctx, cli, cdc.Config{
		Ranges:    opts.ranges,
		TrType:    opts.trType,
		AdrFam:    opts.adrFam,
		TrAddr:    opts.trAddr,
		TrSvcId:   opts.trSvcId,
		Endpoints: opts.endpoints,
	})
}

// watchSignals implements CM5: the first SIGINT/SIGTERM cancels ctx, which
// stops the listener, closes every connection and stops the watcher; a second
// signal during that window gives up on the drain and exits immediately with
// status 1.
//
// exit is a parameter so the unit tests can observe the second-signal path
// without terminating the test binary.
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
		slog.String("signal", first.String()),
	)
	cancel()

	second, ok := <-sigCh
	if !ok {
		return
	}
	slog.WarnContext(ctx, "second signal, exiting without a clean drain",
		slog.String("signal", second.String()),
	)
	exit(1)
}
