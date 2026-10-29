package csi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The node's side of NVMe-oF (csi.md, The node plugin): the node's identity,
// the connects and disconnects, the discovery log and the few sysfs
// attributes the follower reads. Everything goes through the OS client, as
// for the agents (osclient.md): nvme-cli for what the kernel takes as a
// command, plain reads for sysfs.
const (
	// DefaultHostNqnPath and DefaultHostIdPath are the node's identity
	// files, which nvme-cli and nvme-stas read too; the operator owns them.
	DefaultHostNqnPath = "/etc/nvme/hostnqn"
	DefaultHostIdPath  = "/etc/nvme/hostid"
	// DefaultDeviceWait bounds a stage's wait for the volume's device: the
	// kubelet calls again when it runs out, and a pool whose cntlrs are
	// still being built needs a few seconds after its create.
	DefaultDeviceWait = 90 * time.Second

	sysClassNvme = "/sys/class/nvme"
	byIdDir      = "/dev/disk/by-id"
	nvmeBin      = "nvme"
	trTypeTcp    = "tcp"
	// connectRetry is the pause between two tries of a connect whose cntlr
	// does not listen yet.
	connectRetry = time.Second
)

// hostIdentity is the node's NVMe host identity: the node id Kubernetes learns
// and the pair every connect carries.
type hostIdentity struct {
	hostNqn string
	hostId  string
}

// readIdentity reads the two identity files; a file that is missing or
// empty is an error naming the file and the fix, since the plugin must not
// start without the node's identity (csi.md CSI13).
func readIdentity(
	ctx context.Context,
	oc common.OsClient,
	nqnPath string,
	idPath string,
) (hostIdentity, error) {
	read := func(p string) (string, error) {
		data, err := oc.ReadFile(ctx, p)
		if err != nil {
			return "", fmt.Errorf("the node's identity file %s cannot be read "+
				"(%v); generate it with `nvme gen-hostnqn` and `uuidgen`", p, err)
		}
		value := strings.TrimSpace(data)
		if value == "" {
			return "", fmt.Errorf("the node's identity file %s is empty; "+
				"generate it with `nvme gen-hostnqn` and `uuidgen`", p)
		}
		return value, nil
	}
	nqn, err := read(nqnPath)
	if err != nil {
		return hostIdentity{}, err
	}
	if !isNodeId(nqn) {
		return hostIdentity{}, fmt.Errorf("the node's identity file %s holds %q, "+
			"which is not a host NQN", nqnPath, nqn)
	}
	id, err := read(idPath)
	if err != nil {
		return hostIdentity{}, err
	}
	return hostIdentity{hostNqn: nqn, hostId: id}, nil
}

// nvmeHost runs the node's NVMe-oF operations.
type nvmeHost struct {
	oc common.OsClient
	id hostIdentity
}

// ctrl is one controller of the node as sysfs shows it: its device name,
// the subsystem it belongs to, the address of the cntlr it reaches and its
// state.
type ctrl struct {
	name    string
	subNqn  string
	trAddr  string
	trSvcId string
	state   string
}

// key is a cntlr's address as the discovery log and sysfs name it.
func (c ctrl) key() string {
	return c.trAddr + ":" + c.trSvcId
}

// controllers lists the node's NVMe controllers from sysfs: every
// nvme<N> directory with the three attributes the follower reads. A
// controller that vanishes between the listing and the read is skipped; a
// controller that cannot be read otherwise is an error.
func (h *nvmeHost) controllers(ctx context.Context) ([]ctrl, error) {
	names, err := h.oc.ReadDir(ctx, sysClassNvme)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing %s: %w", sysClassNvme, err)
	}
	var out []ctrl
	for _, name := range names {
		if !strings.HasPrefix(name, "nvme") {
			continue
		}
		dir := path.Join(sysClassNvme, name)
		c := ctrl{name: name}
		var missing bool
		for _, attr := range []struct {
			file string
			dst  *string
		}{
			{"subsysnqn", &c.subNqn},
			{"address", nil},
			{"state", &c.state},
		} {
			data, err := h.oc.ReadFile(ctx, path.Join(dir, attr.file))
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					missing = true
					break
				}
				return nil, fmt.Errorf("reading %s of %s: %w", attr.file, name, err)
			}
			data = strings.TrimSpace(data)
			if attr.dst != nil {
				*attr.dst = data
			} else {
				c.trAddr, c.trSvcId = parseAddress(data)
			}
		}
		if missing {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// parseAddress reads traddr and trsvcid out of a controller's sysfs address
// attribute, a comma-separated list of key=value pairs.
func parseAddress(address string) (string, string) {
	var trAddr, trSvcId string
	for _, field := range strings.Split(address, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok {
			continue
		}
		switch key {
		case "traddr":
			trAddr = value
		case "trsvcid":
			trSvcId = value
		}
	}
	return trAddr, trSvcId
}

// run runs one nvme-cli command and returns its stdout; a failure carries
// the command's stderr.
func (h *nvmeHost) run(ctx context.Context, args ...string) (string, error) {
	stdout, stderr, _, err := h.oc.RunCommand(ctx, nvmeBin, args, "")
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", nvmeBin, strings.Join(args, " "),
			err, strings.TrimSpace(stderr))
	}
	return stdout, nil
}

// connect makes one path: the node's own identity and the kernel's default
// settings (csi.md CSI14). A path that is already there is not an error.
func (h *nvmeHost) connect(ctx context.Context, nqn string, ep endpoint) error {
	_, err := h.run(ctx, "connect", "-t", trTypeTcp,
		"-a", ep.trAddr, "-s", ep.trSvcId, "-n", nqn,
		"--hostnqn", h.id.hostNqn, "--hostid", h.id.hostId)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "already") {
		return nil
	}
	return err
}

// connectDiscovery opens a persistent discovery connection to one cdc
// address: the connection the cdc's change notification arrives on.
func (h *nvmeHost) connectDiscovery(ctx context.Context, ep endpoint) error {
	_, err := h.run(ctx, "discover", "-t", trTypeTcp,
		"-a", ep.trAddr, "-s", ep.trSvcId,
		"--hostnqn", h.id.hostNqn, "--hostid", h.id.hostId, "-p")
	return err
}

// disconnectCtrl drops one controller by its device name.
func (h *nvmeHost) disconnectCtrl(ctx context.Context, name string) error {
	_, err := h.run(ctx, "disconnect", "-d", name)
	return err
}

// disconnectSubsystem drops every path of a subsystem.
func (h *nvmeHost) disconnectSubsystem(ctx context.Context, nqn string) error {
	_, err := h.run(ctx, "disconnect", "-n", nqn)
	return err
}

// endpoint is a cntlr's or a cdc's address.
type endpoint struct {
	trAddr  string
	trSvcId string
}

func (e endpoint) String() string { return e.trAddr + ":" + e.trSvcId }

// parseEndpoint reads an ip:port.
func parseEndpoint(s string) (endpoint, error) {
	idx := strings.LastIndex(s, ":")
	if idx <= 0 || idx == len(s)-1 {
		return endpoint{}, fmt.Errorf("%q is not ip:port", s)
	}
	return endpoint{trAddr: s[:idx], trSvcId: s[idx+1:]}, nil
}

// logEntry is one entry of a discovery log: a subsystem a cntlr exports to
// this node.
type logEntry struct {
	subNqn string
	ep     endpoint
}

// discoveryLog reads the discovery log through a persistent discovery
// controller and keeps the subsystem entries over tcp: the discovery
// subsystem's own entries and referrals are not cntlrs.
func (h *nvmeHost) discoveryLog(ctx context.Context, ctrlName string) ([]logEntry, error) {
	out, err := h.run(ctx, "discover", "-d", ctrlName, "-o", "json")
	if err != nil {
		return nil, err
	}
	return parseDiscoveryLog(out)
}

// parseDiscoveryLog reads nvme-cli's JSON rendering of a discovery log.
func parseDiscoveryLog(out string) ([]logEntry, error) {
	var doc struct {
		Records []struct {
			TrType  string `json:"trtype"`
			TrAddr  string `json:"traddr"`
			TrSvcId string `json:"trsvcid"`
			SubNqn  string `json:"subnqn"`
			SubType string `json:"subtype"`
		} `json:"records"`
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return nil, fmt.Errorf("the discovery log is not the JSON nvme-cli "+
			"renders: %w", err)
	}
	var entries []logEntry
	for _, r := range doc.Records {
		if !strings.EqualFold(r.TrType, trTypeTcp) ||
			r.SubNqn == common.NvmeDiscoveryNqn ||
			strings.Contains(strings.ToLower(r.SubType), "discovery") ||
			strings.Contains(strings.ToLower(r.SubType), "referral") {
			continue
		}
		entries = append(entries, logEntry{
			subNqn: r.SubNqn,
			ep:     endpoint{trAddr: r.TrAddr, trSvcId: r.TrSvcId},
		})
	}
	return entries, nil
}

// devicePath is the one name of a volume's device that survives a
// reconnect: the by-id link of its namespace uuid.
func devicePath(uuid string) string {
	return path.Join(byIdDir, "nvme-uuid."+strings.ToLower(uuid))
}

// waitDevice waits until the volume's device link leads to a block device,
// which happens once a path to the volume is live and optimized, or until
// the bound runs out.
func (h *nvmeHost) waitDevice(
	ctx context.Context,
	uuid string,
	bound time.Duration,
) (string, error) {
	dev := devicePath(uuid)
	deadline := time.Now().Add(bound)
	for {
		if _, err := h.oc.ReadDevNo(ctx, dev); err == nil {
			return dev, nil
		}
		if time.Now().After(deadline) {
			return "", status.Errorf(codes.Unavailable,
				"the device %s did not appear within %s", dev, bound)
		}
		select {
		case <-ctx.Done():
			return "", status.Errorf(codes.Unavailable,
				"waiting for the device %s: %v", dev, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}
