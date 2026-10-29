package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/distributed-nvme/distributed-nvme/csi"
)

// controllerFlags is the controller subcommand's flag set, verbatim.
var controllerFlags = []string{
	"cdc-addresses",
	"config",
	"csi-address",
	"gateway-address",
	"gateway-timeout",
}

var exampleArgs = []string{
	"--csi-address", "/csi/csi.sock",
	"--gateway-address", "192.168.122.78:29870",
	"--cdc-addresses", "192.168.122.78:18030, 192.168.122.79:18030",
	"--gateway-timeout", "7",
}

// parse binds one invocation's flags into a fresh viper. viper is
// process-global, so every case resets it.
func parse(t *testing.T, args []string) *cobra.Command {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
	cmd := newControllerCmd()
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("parsing %v: %v", args, err)
	}
	if err := bindViper(cmd); err != nil {
		t.Fatalf("binding %v: %v", args, err)
	}
	return cmd
}

func mustConfig(t *testing.T, args []string) csi.Config {
	t.Helper()
	parse(t, args)
	cfg, err := controllerOptionsFromViper()
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return cfg
}

func wantError(t *testing.T, args []string, want string) {
	t.Helper()
	parse(t, args)
	_, err := controllerOptionsFromViper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("%v: error %v, want one containing %q", args, err, want)
	}
}

func TestRootHasTheControllerSubcommand(t *testing.T) {
	root := newRootCmd()
	var names []string
	for _, c := range root.Commands() {
		names = append(names, c.Name())
	}
	if !reflect.DeepEqual(names, []string{"controller", "node"}) {
		t.Errorf("subcommands %v, want [controller node]", names)
	}
}

// nodeFlags is the node subcommand's flag set, verbatim.
var nodeFlags = []string{
	"cdc-addresses",
	"config",
	"csi-address",
	"device-wait",
	"hostid-file",
	"hostnqn-file",
	"sync-period",
}

func TestNodeFlagSetIsExactlyTheNodes(t *testing.T) {
	cmd := newNodeCmd()
	var got []string
	cmd.Flags().VisitAll(func(f *pflag.Flag) { got = append(got, f.Name) })
	sort.Strings(got)
	if !reflect.DeepEqual(got, nodeFlags) {
		t.Errorf("flags %v, want %v", got, nodeFlags)
	}
}

func TestNodeOptions(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	cmd := newNodeCmd()
	if err := cmd.ParseFlags([]string{"--csi-address", "/csi/csi.sock",
		"--cdc-addresses", "192.168.122.78:18030"}); err != nil {
		t.Fatal(err)
	}
	if err := bindViper(cmd); err != nil {
		t.Fatal(err)
	}
	cfg, err := nodeOptionsFromViper()
	if err != nil {
		t.Fatal(err)
	}
	want := csi.Config{
		Endpoint:     "/csi/csi.sock",
		CdcAddresses: []string{"192.168.122.78:18030"},
		HostNqnPath:  csi.DefaultHostNqnPath,
		HostIdPath:   csi.DefaultHostIdPath,
		DeviceWait:   csi.DefaultDeviceWait,
		SyncPeriod:   csi.DefaultSyncPeriod,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("config %+v, want %+v", cfg, want)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{}, "--csi-address"},
		{[]string{"--csi-address", "/csi/csi.sock", "--hostnqn-file", ""}, "--hostnqn-file"},
		{[]string{"--csi-address", "/csi/csi.sock", "--device-wait", "0"}, "--device-wait"},
		{[]string{"--csi-address", "/csi/csi.sock", "--sync-period", "-1"}, "--sync-period"},
		{[]string{"--csi-address", "/csi/csi.sock"}, "--cdc-addresses"},
		{[]string{"--csi-address", "/csi/csi.sock", "--cdc-addresses", "cdc.example:8009"},
			"not an IP address"},
	} {
		viper.Reset()
		cmd := newNodeCmd()
		if err := cmd.ParseFlags(tc.args); err != nil {
			t.Fatal(err)
		}
		if err := bindViper(cmd); err != nil {
			t.Fatal(err)
		}
		if _, err := nodeOptionsFromViper(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: error %v, want one containing %q", tc.args, err, tc.want)
		}
	}
}

func TestFlagSetIsExactlyTheControllers(t *testing.T) {
	cmd := newControllerCmd()
	var got []string
	cmd.Flags().VisitAll(func(f *pflag.Flag) { got = append(got, f.Name) })
	sort.Strings(got)
	if !reflect.DeepEqual(got, controllerFlags) {
		t.Errorf("flags %v, want %v", got, controllerFlags)
	}
}

func TestExampleInvocationBuildsTheConfig(t *testing.T) {
	cfg := mustConfig(t, exampleArgs)
	want := csi.Config{
		Endpoint:       "/csi/csi.sock",
		GatewayAddress: "192.168.122.78:29870",
		CdcAddresses:   []string{"192.168.122.78:18030", "192.168.122.79:18030"},
		GatewayTimeout: 7 * time.Second,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("config %+v, want %+v", cfg, want)
	}
}

func TestDefaults(t *testing.T) {
	cfg := mustConfig(t, exampleArgs[:6])
	if cfg.GatewayTimeout != csi.DefaultGatewayTimeout {
		t.Errorf("timeout %v, want %v", cfg.GatewayTimeout, csi.DefaultGatewayTimeout)
	}
}

func TestValidationRejections(t *testing.T) {
	wantError(t, []string{"--gateway-address", "1.2.3.4:1", "--cdc-addresses", "1.2.3.4:8009"},
		"--csi-address")
	wantError(t, []string{"--csi-address", "/csi/csi.sock", "--cdc-addresses", "1.2.3.4:8009"},
		"--gateway-address")
	wantError(t, []string{"--csi-address", "/csi/csi.sock", "--gateway-address", "1.2.3.4:1"},
		"--cdc-addresses")
	wantError(t, []string{"--csi-address", "/csi/csi.sock", "--gateway-address", "nohostport",
		"--cdc-addresses", "1.2.3.4:8009"}, "not host:port")
	wantError(t, []string{"--csi-address", "/csi/csi.sock", "--gateway-address", "1.2.3.4:1",
		"--cdc-addresses", "cdc.example:8009"}, "not an IP address")
	wantError(t, []string{"--csi-address", "/csi/csi.sock", "--gateway-address", "1.2.3.4:1",
		"--cdc-addresses", "1.2.3.4:0"}, "tcp port")
	wantError(t, []string{"--csi-address", "/csi/csi.sock", "--gateway-address", "1.2.3.4:1",
		"--cdc-addresses", "1.2.3.4:8009", "--gateway-timeout", "0"}, "--gateway-timeout")
}

func TestEnvironmentSuppliesEveryValue(t *testing.T) {
	t.Setenv("DNV_CSI_CSI_ADDRESS", "/run/csi.sock")
	t.Setenv("DNV_CSI_GATEWAY_ADDRESS", "10.0.0.1:29527")
	t.Setenv("DNV_CSI_CDC_ADDRESSES", "10.0.0.1:8009")
	t.Setenv("DNV_CSI_GATEWAY_TIMEOUT", "3")
	cfg := mustConfig(t, nil)
	want := csi.Config{
		Endpoint:       "/run/csi.sock",
		GatewayAddress: "10.0.0.1:29527",
		CdcAddresses:   []string{"10.0.0.1:8009"},
		GatewayTimeout: 3 * time.Second,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("config %+v, want %+v", cfg, want)
	}
}

func TestFlagBeatsEnvironment(t *testing.T) {
	t.Setenv("DNV_CSI_GATEWAY_ADDRESS", "10.0.0.1:29527")
	cfg := mustConfig(t, exampleArgs)
	if cfg.GatewayAddress != "192.168.122.78:29870" {
		t.Errorf("gateway address %q, want the flag's", cfg.GatewayAddress)
	}
}

func TestConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "csi.yaml")
	body := "csi-address: /csi/csi.sock\ngateway-address: 10.0.0.2:29527\n" +
		"cdc-addresses: 10.0.0.2:8009,10.0.0.3:8009\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, []string{"--config", path})
	if cfg.GatewayAddress != "10.0.0.2:29527" || len(cfg.CdcAddresses) != 2 {
		t.Errorf("config %+v", cfg)
	}
}

func TestMissingConfigFileIsAnError(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	cmd := newControllerCmd()
	if err := cmd.ParseFlags([]string{"--config", filepath.Join(t.TempDir(), "none.yaml")}); err != nil {
		t.Fatal(err)
	}
	if err := bindViper(cmd); err == nil {
		t.Error("a missing config file was accepted")
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" a, b,, c ,")
	if !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("splitList = %v", got)
	}
	if got := splitList(""); got != nil {
		t.Errorf("splitList of nothing = %v", got)
	}
}

func TestWatchSignalsSecondSignalExits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 2)
	exited := make(chan int, 1)
	done := make(chan struct{})
	go func() {
		watchSignals(ctx, cancel, sigCh, func(code int) { exited <- code })
		close(done)
	}()
	sigCh <- syscall.SIGTERM
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the first signal did not cancel the ctx")
	}
	sigCh <- syscall.SIGINT
	select {
	case code := <-exited:
		if code != 1 {
			t.Errorf("exit code %d, want 1", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second signal did not exit")
	}
	<-done
}

func TestWatchSignalsReturnsWhenCtxEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		watchSignals(ctx, cancel, make(chan os.Signal), func(int) {
			t.Error("exit was called")
		})
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watchSignals did not return")
	}
}
