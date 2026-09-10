// Command dnv-gateway is the control-plane API server (gateway.md §7): one
// cobra root command without subcommands that parses the CM2 flags through
// viper, builds the process's single etcdutil client (EU1) and hands off to
// gateway.Run, which serves the 59 RPCs of `service Gateway`.
//
// It is the only dnv binary that needs both flag families — the gRPC-server
// pair of cmd/dnv-agent and the etcd pair of cmd/dnv-worker (§0 #11).
//
// main itself is deliberately thin (layout.md §5): it never touches etcd, it
// logs only the CM3 signal records, and the "gateway starting" / "gateway
// serving" / "gateway stopping" records of §8 are emitted by gateway.Run,
// which is why the endpoints travel to it in gateway.Config.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/gateway"
)

// envPrefix makes every flag settable as DNV_GATEWAY_<FLAG_WITH_UNDERSCORES>
// (CM1).
const envPrefix = "DNV_GATEWAY"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// newRootCmd builds the CM1 command: one root, no subcommands.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "dnv-gateway",
		Short: "dnv control-plane API server",
		Long: "dnv-gateway serves the Gateway gRPC service to users and " +
			"CLIs. It reads and writes etcd, and calls agents only for " +
			"GetDnSize/GetCnSize, the Get*Info behind its Inspect* RPCs " +
			"and the Get*Bm bitmap reads (gateway.md).",
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

// addFlags declares the CM2 flag set: the two gRPC-server flags of
// cmd/dnv-agent and the two etcd flags of cmd/dnv-worker.
//
// There is deliberately no --etcd-op-timeout — every plain etcd operation and
// every STM attempt is bounded by common.DefaultEtcdOpTimeout (EU5) — and no
// default gRPC port: --grpc-address is required exactly as the agent's is, and
// 29527 stays a documented example rather than a constant (§0 #10).
func addFlags(cmd *cobra.Command) {
	flags := cmd.Flags()
	flags.String("grpc-network", "tcp", "net.Listen network")
	flags.String("grpc-address", "",
		"gRPC endpoint this gateway serves on (required)")
	flags.String("etcd-endpoints", "",
		"comma-separated host:port list of the etcd cluster (required)")
	flags.Int("etcd-dial-timeout", common.DefaultEtcdDialTimeout,
		"seconds to wait for the initial etcd dial (0 = the built-in default)")
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

// options is the validated CM2 configuration, ready to become a
// gateway.Config.
type options struct {
	// grpcNetwork and grpcAddress are the listener; the address is required.
	grpcNetwork string
	grpcAddress string
	// endpoints is the parsed --etcd-endpoints list, never empty.
	endpoints []string
	// dialTimeout bounds the initial etcd dial (EU1, EU5). A non-positive
	// value is passed through: etcdutil.New substitutes
	// common.DefaultEtcdDialTimeout for it.
	dialTimeout time.Duration
}

// optionsFromViper reads and validates the CM2 values after the binding. It
// performs no I/O and logs nothing, so the unit tests can drive every branch
// directly.
func optionsFromViper() (*options, error) {
	grpcAddress := strings.TrimSpace(viper.GetString("grpc-address"))
	if grpcAddress == "" {
		return nil, errors.New("missing required flag: --grpc-address")
	}
	grpcNetwork := strings.TrimSpace(viper.GetString("grpc-network"))
	if grpcNetwork == "" {
		return nil, errors.New("--grpc-network must not be empty")
	}
	endpoints := splitList(viper.GetString("etcd-endpoints"))
	if len(endpoints) == 0 {
		return nil, errors.New("missing required flag: --etcd-endpoints")
	}
	return &options{
		grpcNetwork: grpcNetwork,
		grpcAddress: grpcAddress,
		endpoints:   endpoints,
		dialTimeout: time.Duration(viper.GetInt("etcd-dial-timeout")) *
			time.Second,
	}, nil
}

// splitList parses one comma-separated flag value: entries are trimmed and
// empty ones dropped, so "a, b," is the two-element list. The list flag is a
// plain string rather than a pflag string slice on purpose — viper's
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

// run is the CM3 startup path. The default JSON logger is already installed by
// common's init and the gateway keeps level Info by doing nothing (log.md R6),
// so the only work here is: bind, validate, mint the startup trace id, arm the
// two-signal handling, build the etcd client and hand off to gateway.Run.
//
// Startup never fails because etcd is unreachable: etcdutil.New dials lazily,
// and a gateway with no etcd simply answers ABORTED until one appears. Only a
// configuration error or a listener that cannot be opened returns non-nil
// here, which is what makes the process exit non-zero.
func run(cmd *cobra.Command, args []string) error {
	if err := bindViper(cmd); err != nil {
		return err
	}
	opts, err := optionsFromViper()
	if err != nil {
		return err
	}

	// The startup trace id. Every REQUEST gets its own — the server
	// interceptor adopts the caller's when there is one and gateway.Run's
	// server chain mints one otherwise (gateway/traceid.go; §0 #6, grpc.md
	// T4) — so this id only ever labels the process's own lifecycle records.
	ctx := common.WithTraceId(context.Background(), common.NewTraceId())

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Capacity 2: the first signal starts GracefulStop, the second abandons
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
	// gateway.Run closes the client as the last step of its drain (GW2), so
	// it is deliberately not closed here.
	return gateway.Run(ctx, cli, gateway.Config{
		GrpcNetwork: opts.grpcNetwork,
		GrpcAddress: opts.grpcAddress,
		Endpoints:   opts.endpoints,
	})
}

// watchSignals implements CM3's two-signal handling: the first SIGINT/SIGTERM
// cancels ctx, which makes gateway.Run call GracefulStop and let in-flight
// handlers finish; a second signal during that window gives up and exits 1,
// because a handler waiting on a hung agent may hold the process for up to
// common.DefaultGatewayAgentTimeout.
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
