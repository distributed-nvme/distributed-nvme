package csi

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
)

// TestRunControllerServesOverTheSocket runs the whole controller plugin
// against a fake gateway on loopback: the identity service answers the
// driver's name, version and the controller capability, a create goes
// through the socket and the interceptor, a stale socket file is replaced,
// and the end of the ctx stops the server and removes the socket.
func TestRunControllerServesOverTheSocket(t *testing.T) {
	g := newFakeGateway()
	gateway := g.serveTcp(t)
	// t.TempDir can be long; a unix socket path is bounded, so the socket
	// lives under a short directory of its own.
	dir, err := os.MkdirTemp("", "dnv-csi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	endpoint := filepath.Join(dir, "csi.sock")
	if err := os.WriteFile(endpoint, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- RunController(ctx, Config{
			Endpoint:       endpoint,
			GatewayAddress: gateway,
			CdcAddresses:   testCdc,
		})
	}()
	// RunController removes the stale file and listens in its own time;
	// wait until something answers on the socket.
	deadline := time.Now().Add(10 * time.Second)
	for {
		probe, err := net.DialTimeout("unix", endpoint, 200*time.Millisecond)
		if err == nil {
			_ = probe.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing listens on %s: %v", endpoint, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	conn, err := grpc.NewClient("unix://"+endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	identity := csi.NewIdentityClient(conn)
	ctl := csi.NewControllerClient(conn)

	callCtx, callCancel := context.WithTimeout(ctx, 10*time.Second)
	defer callCancel()
	info, err := identity.GetPluginInfo(callCtx, &csi.GetPluginInfoRequest{})
	wantCode(t, err, codes.OK, "GetPluginInfo")
	if info.GetName() != DriverName || info.GetVendorVersion() != Version {
		t.Errorf("plugin info %v", info)
	}
	caps, err := identity.GetPluginCapabilities(callCtx, &csi.GetPluginCapabilitiesRequest{})
	wantCode(t, err, codes.OK, "GetPluginCapabilities")
	if len(caps.GetCapabilities()) != 1 ||
		caps.GetCapabilities()[0].GetService().GetType() !=
			csi.PluginCapability_Service_CONTROLLER_SERVICE {
		t.Errorf("plugin capabilities %v", caps)
	}
	probe, err := identity.Probe(callCtx, &csi.ProbeRequest{})
	wantCode(t, err, codes.OK, "Probe")
	if !probe.GetReady().GetValue() {
		t.Error("the probe is not ready")
	}
	reply, err := ctl.CreateVolume(callCtx, createReq(testVolume, gib))
	wantCode(t, err, codes.OK, "CreateVolume over the socket")
	if reply.GetVolume().GetVolumeId() != fakeClusterOne+"/"+testVolume {
		t.Errorf("volume id %q", reply.GetVolume().GetVolumeId())
	}
	_, err = ctl.CreateVolume(callCtx, &csi.CreateVolumeRequest{})
	wantCode(t, err, codes.InvalidArgument, "an empty create over the socket")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunController returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunController did not stop")
	}
	if _, err := os.Stat(endpoint); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the socket file is still there: %v", err)
	}
}

// TestRunControllerRefusesABadSocketDir pins the one start failure: a
// socket directory that does not exist.
func TestRunControllerRefusesABadSocketDir(t *testing.T) {
	err := RunController(t.Context(), Config{
		Endpoint:       filepath.Join(t.TempDir(), "missing", "csi.sock"),
		GatewayAddress: "127.0.0.1:1",
		CdcAddresses:   testCdc,
	})
	if err == nil {
		t.Fatal("RunController accepted a socket in a missing directory")
	}
}
