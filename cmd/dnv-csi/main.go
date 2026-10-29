// Command dnv-csi is the Container Storage Interface driver of dnv (csi.md,
// The driver): one cobra root with the controller subcommand, which
// parses its flags through viper and hands off to csi.RunController. main
// itself is thin (layout.md, `cmd/` wiring): it opens no socket and dials
// nothing; the lifecycle records of csi.md, Log records, are the library's.
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
	root.AddCommand(newControllerCmd())
	return root
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

// runController binds, validates, mints the startup trace id, arms the
// signal handling and hands off to csi.RunController.
func runController(cmd *cobra.Command, args []string) error {
	if err := bindViper(cmd); err != nil {
		return err
	}
	cfg, err := controllerOptionsFromViper()
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
	return csi.RunController(ctx, cfg)
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
