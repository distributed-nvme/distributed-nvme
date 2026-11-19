// Command dnv-worker is the control-plane worker (dnv-worker.md,
// `cmd/dnv-worker`): one cobra root command without subcommands that parses
// the CM1 flags through viper, builds the process's single etcdutil client
// (EU1) and hands off to worker.Run, which owns the ClusterConf cache (RW21),
// the vote worker (VW1 to VW11) and everything below it.
//
// main itself is deliberately thin (layout.md, `cmd/` wiring): it never
// touches etcd, it logs nothing but the CM3 warning, the two CM5 signal
// records and the log level changes of log.md R13, and the "worker
// starting" / "worker stopping" records of dnv-worker.md, Log records, are
// emitted by worker.Run, which is why the endpoints travel to it in
// worker.Config (CM6).
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
	"github.com/distributed-nvme/distributed-nvme/worker"
)

// envPrefix makes every flag but --version settable as
// DNV_WORKER_<FLAG_WITH_UNDERSCORES> (CM2).
const envPrefix = "DNV_WORKER"

// workerRoles are the three legal --roles values, in the canonical order
// that also forms the flag's default. Each role is registered, voted and
// driven independently (VW10).
var workerRoles = []string{
	common.WorkerRoleDn, common.WorkerRoleCn, common.WorkerRoleSp,
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// newRootCmd builds the CM1 command: one root, no subcommands.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "dnv-worker",
		Short: "dnv control-plane worker (dn / cn / sp roles)",
		Long: "dnv-worker watches the revision keys in etcd, shards the work " +
			"by shard code and drives the dn/cn agents through Syncup*, " +
			"Push*Bitmap and the Check* streams (dnv-worker.md).",
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

// addFlags declares the CM1 flag set. There is deliberately no
// --etcd-op-timeout: every plain etcd operation, and every STM transaction as
// a whole with its conflict retries, is bounded by
// common.DefaultEtcdOpTimeout (EU5).
func addFlags(cmd *cobra.Command) {
	flags := cmd.Flags()
	flags.String("etcd-endpoints", "",
		"comma-separated host:port list of the etcd cluster (required)")
	flags.String("roles", strings.Join(workerRoles, ","),
		"comma-separated subset of dn, cn, sp this process carries")
	flags.Int("vote-interval", common.DefaultVoteWorkerInterval,
		"seconds between two registry heartbeats; "+
			"a registration unrefreshed for twice this long is dead")
	flags.Int("vote-grace-time", common.DefaultVoteWorkerGraceTime,
		"seconds an observed join must hold before it is committed")
	flags.Int("vote-leave-grace-time", common.DefaultVoteWorkerLeaveGraceTime,
		"seconds an observed leave must hold before it is committed; "+
			"more than the vote interval plus the etcd operation timeout")
	flags.Int("etcd-dial-timeout", common.DefaultEtcdDialTimeout,
		"seconds to wait for the initial etcd dial (0 = the built-in default)")
	flags.String("log-level", common.LogLevelInfo,
		"log level: debug, info or warn; SIGUSR1 sets debug while the "+
			"process runs and SIGUSR2 sets this level again")
	flags.String("config", "", "optional viper config file")
}

// bindViper wires flags, config file and environment together exactly as
// cmd/dnv-agent does (CM2, dnagent.md CM3); every value is read through viper
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

// options is the validated CM1 configuration, ready to become a
// worker.Config.
type options struct {
	// endpoints is the parsed --etcd-endpoints list, never empty (CM3).
	endpoints []string
	// roles is the parsed --roles list: non-empty, duplicate-free, a subset
	// of workerRoles, in the order given (CM3).
	roles []string
	// voteInterval, graceTime and leaveGraceTime are the vote timers (VW2,
	// VW5): the heartbeat period, a join's grace and a leave's.
	voteInterval   time.Duration
	graceTime      time.Duration
	leaveGraceTime time.Duration
	// dialTimeout bounds the initial etcd dial (EU1, EU5). A non-positive
	// value is passed through: etcdutil.New substitutes
	// common.DefaultEtcdDialTimeout for it.
	dialTimeout time.Duration
	// logLevel is the parsed --log-level, set before the first record
	// (log.md R6).
	logLevel slog.Level
	// graceTooShort records the CM3 SHOULD that was not met — a grace window
	// no longer than the dead threshold (2 x the vote interval) is legal but
	// pointless. The caller logs a warning; it is never a rejection.
	graceTooShort bool
}

// optionsFromViper reads and validates the CM1 values after the CM2 binding.
// It performs no I/O and logs nothing, so the unit tests can drive every CM3
// branch directly.
func optionsFromViper() (*options, error) {
	endpoints := splitList(viper.GetString("etcd-endpoints"))
	if len(endpoints) == 0 {
		return nil, errors.New("missing required flag: --etcd-endpoints")
	}
	roles, err := parseRoles(viper.GetString("roles"))
	if err != nil {
		return nil, err
	}
	logLevel, err := common.ParseLogLevel(viper.GetString("log-level"))
	if err != nil {
		return nil, err
	}
	voteInterval := viper.GetInt("vote-interval")
	if voteInterval < 1 {
		return nil, fmt.Errorf(
			"--vote-interval must be at least 1 second, got %d", voteInterval)
	}
	graceTime := viper.GetInt("vote-grace-time")
	if graceTime < 1 {
		return nil, fmt.Errorf(
			"--vote-grace-time must be at least 1 second, got %d", graceTime)
	}
	// A worker cut off from etcd fences itself by one vote interval past the
	// dead threshold plus one put's etcd budget; a leave grace no longer than
	// the interval plus that budget would let its peers commit it dead
	// before it fences (VW8; dnv-worker.md CM3).
	leaveGraceTime := viper.GetInt("vote-leave-grace-time")
	if floor := voteInterval + common.DefaultEtcdOpTimeout; leaveGraceTime <= floor {
		return nil, fmt.Errorf(
			"--vote-leave-grace-time must exceed the vote interval plus the "+
				"etcd operation timeout, %d seconds, got %d", floor,
			leaveGraceTime)
	}
	return &options{
		endpoints:      endpoints,
		roles:          roles,
		voteInterval:   time.Duration(voteInterval) * time.Second,
		graceTime:      time.Duration(graceTime) * time.Second,
		leaveGraceTime: time.Duration(leaveGraceTime) * time.Second,
		dialTimeout: time.Duration(viper.GetInt("etcd-dial-timeout")) *
			time.Second,
		logLevel:      logLevel,
		graceTooShort: graceTime <= 2*voteInterval,
	}, nil
}

// splitList parses one comma-separated flag value: entries are trimmed and
// empty ones dropped, so "a, b," is the two-element list. Both list flags are
// plain strings rather than pflag string slices on purpose — viper's
// GetStringSlice does not split a single environment or config string on
// commas, and CM2 requires the flag, the file and the environment to mean the
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

// parseRoles validates --roles (CM3): a non-empty, duplicate-free subset of
// dn, cn and sp.
func parseRoles(raw string) ([]string, error) {
	roles := splitList(raw)
	if len(roles) == 0 {
		return nil, fmt.Errorf("--roles must name at least one of %s",
			strings.Join(workerRoles, ", "))
	}
	seen := make(map[string]bool, len(roles))
	for _, role := range roles {
		if !isWorkerRole(role) {
			return nil, fmt.Errorf("--roles: unknown role %q, want a subset of %s",
				role, strings.Join(workerRoles, ", "))
		}
		if seen[role] {
			return nil, fmt.Errorf("--roles: duplicate role %q", role)
		}
		seen[role] = true
	}
	return roles, nil
}

func isWorkerRole(role string) bool {
	for _, known := range workerRoles {
		if role == known {
			return true
		}
	}
	return false
}

// run is the CM4 startup path. The default JSON logger is already installed
// by common's init (log.md R3), so the only work here is: bind, validate,
// set the --log-level (log.md R6), mint the startup trace id, arm the log
// level's signals (log.md R13) and the CM5 signal handling, build the etcd
// client and hand off to worker.Run.
//
// Startup never fails because etcd is unreachable: etcdutil.New dials lazily
// and the vote layer stays fenced until its first successful put (VW8, EU1).
// Only a configuration error returns non-nil here, which is what makes the
// process exit non-zero.
func run(cmd *cobra.Command, args []string) error {
	if err := bindViper(cmd); err != nil {
		return err
	}
	opts, err := optionsFromViper()
	if err != nil {
		return err
	}
	common.SetLogLevel(opts.logLevel)

	// The startup trace id; every unit of work below, and every part of an
	// incarnation that outlives one, mints its own from the seed (RW10).
	ctx := common.WithTraceId(context.Background(), common.NewTraceId())
	// log.md R13: SIGUSR1 sets Debug, SIGUSR2 the level the process started
	// with, until the process ends; the watch has the startup ctx, which no
	// stop signal cancels, so it lasts through the drain.
	stopLogLevel := common.WatchLogLevel(ctx, opts.logLevel)
	defer stopLogLevel()
	if opts.graceTooShort {
		slog.WarnContext(ctx, "grace time does not exceed the dead threshold",
			slog.Float64("vote_interval", opts.voteInterval.Seconds()),
			slog.Float64("grace_time", opts.graceTime.Seconds()),
			slog.Float64("dead_threshold",
				2*opts.voteInterval.Seconds()),
		)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Capacity 2: the first signal starts the ordered drain of CM5, the
	// second aborts it. signal.NotifyContext is not used because it stops
	// consuming after the first signal, which would swallow the second one.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go watchSignals(ctx, cancel, sigCh, os.Exit)

	cli, err := etcdutil.New(ctx, opts.endpoints, opts.dialTimeout)
	if err != nil {
		return err
	}
	// worker.Run closes the client as the last step of its CM5 drain, so it
	// is deliberately not closed here.
	return runWorker(ctx, cli, worker.Config{
		Roles:          opts.roles,
		VoteInterval:   opts.voteInterval,
		GraceTime:      opts.graceTime,
		LeaveGraceTime: opts.leaveGraceTime,
		Endpoints:      opts.endpoints,
	})
}

// runWorker is worker.Run, the hand-off; a seam, so a test of the startup
// path can see what run set before the hand-off and while it runs.
var runWorker = worker.Run

// watchSignals implements CM5: the first SIGINT/SIGTERM cancels ctx, which
// starts worker.Run's ordered drain (stop the heartbeat, tell every shard
// worker to stop, delete this worker's registrations, join the shard workers,
// close the cache and the client); a second signal during that window gives up
// on the drain and exits immediately with status 1, because an in-flight
// Syncup*/Push* may hold the process for up to
// common.DefaultWorkerSyncupTimeout.
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
	slog.InfoContext(ctx, "second signal, exiting without a clean drain",
		slog.String("signal", second.String()),
	)
	exit(1)
}
