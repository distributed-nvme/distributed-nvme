// Command dnv-csi is the Container Storage Interface driver of dnv (csi.md,
// The driver): one cobra root with the controller and node subcommands,
// each parsing its flags through viper and handing off to the library's
// Run. main itself is thin (layout.md, `cmd/` wiring): it opens no socket
// and dials nothing; the lifecycle records of csi.md, Log records, are the
// library's.
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

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/csi"
)

// envPrefix makes every flag settable as DNV_CSI_<FLAG_WITH_UNDERSCORES>.
const envPrefix = "DNV_CSI"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// newRootCmd builds the command: one root, the controller subcommand.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "dnv-csi",
		Short: "dnv Container Storage Interface driver",
		Long: "dnv-csi serves the Container Storage Interface to Kubernetes: " +
			"the controller plugin creates, deletes, attaches and detaches " +
			"volumes through the dnv gateway (csi.md).",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newControllerCmd(), newNodeCmd())
	return root
}

func newNodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "serve the identity service and the node plugin",
		Args:  cobra.NoArgs,
		RunE:  runNode,
	}
	flags := cmd.Flags()
	flags.String("csi-address", "",
		"path of the unix socket to serve the CSI services on (required)")
	flags.String("hostnqn-file", csi.DefaultHostNqnPath,
		"file that holds the node's host NQN, its node id")
	flags.String("hostid-file", csi.DefaultHostIdPath,
		"file that holds the node's host id")
	flags.Int("device-wait", int(csi.DefaultDeviceWait/time.Second),
		"seconds a stage waits for the volume's device")
	flags.Int("sync-period", int(csi.DefaultSyncPeriod/time.Second),
		"seconds between two follows of the cdc, the safety net under the "+
			"kernel's change notification")
	flags.String("config", "", "optional viper config file")
	return cmd
}

func newControllerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "controller",
		Short: "serve the identity service and the controller plugin",
		// No positional arguments: every input is a flag, a config key or an
		// environment variable.
		Args: cobra.NoArgs,
		RunE: runController,
	}
	flags := cmd.Flags()
	flags.String("csi-address", "",
		"path of the unix socket to serve the CSI services on (required)")
	flags.String("gateway-address", "",
		"host:port of the dnv gateway (required)")
	flags.String("cdc-addresses", "",
		"comma-separated ip:port list of the dnv cdc endpoints, handed to "+
			"the node plugin at attach (required)")
	flags.Int("gateway-timeout", int(csi.DefaultGatewayTimeout/time.Second),
		"seconds one gateway call may take")
	flags.String("config", "", "optional viper config file")
	return cmd
}

// bindViper wires flags, config file and environment together as every dnv
// daemon does; every value is read through viper afterwards, so a file or
// the environment satisfies a required value just as a flag does.
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

// controllerOptionsFromViper reads and validates the controller's values
// after the binding. It performs no I/O and logs nothing.
func controllerOptionsFromViper() (csi.Config, error) {
	endpoint := strings.TrimSpace(viper.GetString("csi-address"))
	if endpoint == "" {
		return csi.Config{}, errors.New("missing required flag: --csi-address")
	}
	gateway := strings.TrimSpace(viper.GetString("gateway-address"))
	if gateway == "" {
		return csi.Config{}, errors.New("missing required flag: --gateway-address")
	}
	if _, _, err := net.SplitHostPort(gateway); err != nil {
		return csi.Config{}, fmt.Errorf("--gateway-address: %q is not host:port", gateway)
	}
	cdc := splitList(viper.GetString("cdc-addresses"))
	if len(cdc) == 0 {
		return csi.Config{}, errors.New("missing required flag: --cdc-addresses")
	}
	for _, addr := range cdc {
		if err := checkCdcAddress(addr); err != nil {
			return csi.Config{}, err
		}
	}
	timeout := viper.GetInt("gateway-timeout")
	if timeout <= 0 {
		return csi.Config{}, fmt.Errorf(
			"--gateway-timeout: %d is not a positive number of seconds", timeout)
	}
	return csi.Config{
		Endpoint:       endpoint,
		GatewayAddress: gateway,
		CdcAddresses:   cdc,
		GatewayTimeout: time.Duration(timeout) * time.Second,
	}, nil
}

// nodeOptionsFromViper reads and validates the node's values after the
// binding. It performs no I/O and logs nothing; the identity files are read
// by the library at start.
func nodeOptionsFromViper() (csi.Config, error) {
	endpoint := strings.TrimSpace(viper.GetString("csi-address"))
	if endpoint == "" {
		return csi.Config{}, errors.New("missing required flag: --csi-address")
	}
	nqnPath := strings.TrimSpace(viper.GetString("hostnqn-file"))
	idPath := strings.TrimSpace(viper.GetString("hostid-file"))
	if nqnPath == "" || idPath == "" {
		return csi.Config{}, errors.New(
			"--hostnqn-file and --hostid-file must name the identity files")
	}
	deviceWait := viper.GetInt("device-wait")
	if deviceWait <= 0 {
		return csi.Config{}, fmt.Errorf(
			"--device-wait: %d is not a positive number of seconds", deviceWait)
	}
	syncPeriod := viper.GetInt("sync-period")
	if syncPeriod <= 0 {
		return csi.Config{}, fmt.Errorf(
			"--sync-period: %d is not a positive number of seconds", syncPeriod)
	}
	return csi.Config{
		Endpoint:    endpoint,
		HostNqnPath: nqnPath,
		HostIdPath:  idPath,
		DeviceWait:  time.Duration(deviceWait) * time.Second,
		SyncPeriod:  time.Duration(syncPeriod) * time.Second,
	}, nil
}

// splitList parses one comma-separated flag value: entries are trimmed and
// empty ones dropped. The flag is a plain string rather than a pflag string
// slice on purpose: viper's GetStringSlice does not split a single
// environment or config string on commas.
func splitList(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// checkCdcAddress refuses a cdc endpoint that is not an IP literal and a
// port: the node plugin connects to it as the kernel's discovery address,
// which takes no name.
func checkCdcAddress(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--cdc-addresses: %q is not ip:port", addr)
	}
	if net.ParseIP(host) == nil {
		return fmt.Errorf("--cdc-addresses: %q is not an IP address", host)
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return fmt.Errorf("--cdc-addresses: %q is not a tcp port in [1, 65535]",
			port)
	}
	return nil
}

// runController binds, validates and runs the controller plugin.
func runController(cmd *cobra.Command, args []string) error {
	return runPlugin(cmd, controllerOptionsFromViper, csi.RunController)
}

// runNode binds, validates and runs the node plugin.
func runNode(cmd *cobra.Command, args []string) error {
	return runPlugin(cmd, nodeOptionsFromViper, csi.RunNode)
}

// runPlugin is the startup path both subcommands share: bind, validate,
// mint the startup trace id, arm the signal handling and hand off.
func runPlugin(
	cmd *cobra.Command,
	options func() (csi.Config, error),
	run func(context.Context, csi.Config) error,
) error {
	if err := bindViper(cmd); err != nil {
		return err
	}
	cfg, err := options()
	if err != nil {
		return err
	}
	ctx := common.WithTraceId(context.Background(), common.NewTraceId())
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Capacity 2: the first signal starts the drain, the second aborts it.
	// signal.NotifyContext is not used because it stops consuming after the
	// first signal, which would swallow the second one.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go watchSignals(ctx, cancel, sigCh, os.Exit)
	return run(ctx, cfg)
}

// watchSignals is the daemons' shared shape: the first SIGINT/SIGTERM cancels
// ctx, which stops the server gracefully; a second signal during that window
// gives up on the drain and exits immediately with status 1. exit is a
// parameter so the unit tests can observe the second-signal path.
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
