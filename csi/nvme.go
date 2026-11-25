package csi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"path"
	"regexp"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The node's side of NVMe-oF (csi.md, The node plugin): the node's identity,
// the connects and disconnects, the discovery log, the few sysfs attributes
// the follower reads and the one it writes, a path's ctrl_loss_tmo. Everything goes through the OS client, as
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
	// connectRetry is the pause between two tries of a connect that a cntlr
	// refused because it has not built the subsystem.
	connectRetry = time.Second
	// nvmeDeadline bounds a connect and a discovery read, the agents' bound
	// on a wrapper command (architecture.md, Common validation): the
	// follower runs them one at a time, so one that hangs holds up every
	// stage of the node. It ends a hang of nvme-cli and a kernel wait a
	// signal ends, the TCP connect and the first message of a connection;
	// a Connect command or a log read that the target takes and does not
	// answer waits in the kernel, whatever the signal, until the admin
	// timeout or the connection's error recovery ends it, about a minute or
	// a little more (csi.md CSI14). A disconnect has none: the kernel's delete
	// runs to its end whatever signal comes, and a kill would only make a
	// delete that was done look failed.
	nvmeDeadline = common.CmdSoftTimeout * time.Second
)

// identityFix is the fix every refusal of readIdentity names.
const identityFix = "on a node whose NVMe connections carry an identity, " +
	"write back the one in /sys/class/nvme/nvme*/hostnqn and hostid; on a " +
	"node with no connection, generate it with `nvme gen-hostnqn` and `uuidgen`"

// hostIdentity is the node's NVMe host identity: the node id Kubernetes learns
// and the pair every connect carries.
type hostIdentity struct {
	hostNqn string
	hostId  string
}

// readIdentity reads the two identity files; a file that is missing or
// empty is an error naming the file and the fix, since the plugin must not
// start without the node's identity (csi.md CSI13). The fix it names keeps
// a node's volumes: write back the identity its connections carry, and
// generate a new one only on a node that holds none, since a plugin under
// a new host NQN takes its volumes for detached and drops their paths.
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
				"(%v); %s", p, err, identityFix)
		}
		value := strings.TrimSpace(data)
		if value == "" {
			return "", fmt.Errorf("the node's identity file %s is empty; %s",
				p, identityFix)
		}
		return value, nil
	}
	nqn, err := read(nqnPath)
	if err != nil {
		return hostIdentity{}, err
	}
	if !isNodeId(nqn) {
		return hostIdentity{}, fmt.Errorf("the node's identity file %s holds %q, "+
			"which is not a host NQN; %s", nqnPath, nqn, identityFix)
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
// the subsystem it belongs to, the address of the cntlr it reaches, its
// state and the host NQN it was made under, empty for a controller that is
// not over a fabric.
type ctrl struct {
	name    string
	subNqn  string
	trAddr  string
	trSvcId string
	state   string
	hostNqn string
}

// key is a cntlr's address as the discovery log and sysfs name it, in the
// form an endpoint is written in.
func (c ctrl) key() string {
	return net.JoinHostPort(c.trAddr, c.trSvcId)
}

// controllers lists the node's NVMe controllers from sysfs: every
// nvme<N> directory with the three attributes the follower reads, and its
// host NQN where it has one. A controller that vanishes between the listing
// and the read is skipped; a controller that cannot be read otherwise is an
// error.
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
			file     string
			dst      *string
			optional bool
		}{
			{"subsysnqn", &c.subNqn, false},
			{"address", nil, false},
			{"state", &c.state, false},
			{"hostnqn", &c.hostNqn, true},
		} {
			data, err := h.oc.ReadFile(ctx, path.Join(dir, attr.file))
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					if attr.optional {
						continue
					}
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

// runBounded is run under nvmeDeadline: a command it ends is a failed
// call, as a command any other error ends.
func (h *nvmeHost) runBounded(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, nvmeDeadline)
	defer cancel()
	return h.run(ctx, args...)
}

// nsDevicePattern is a namespace device the kernel shows under a
// controller's sysfs directory: nvme<S>c<C>n<N> for a path of a multipath
// namespace, nvme<C>n<N> without multipath.
var nsDevicePattern = regexp.MustCompile(`^nvme[0-9]+(c[0-9]+)?n[0-9]+$`)

// showsNamespace reports whether the kernel shows a namespace under one
// controller, which a connect's scan can add after the connect returns;
// a listing that fails reads as no.
func (h *nvmeHost) showsNamespace(ctx context.Context, name string) bool {
	entries, err := h.oc.ReadDir(ctx, path.Join(sysClassNvme, name))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if nsDevicePattern.MatchString(e) {
			return true
		}
	}
	return false
}

// ctrlLossTmoNever is nvme-cli's --ctrl-loss-tmo for a reconnect that never
// gives up.
const ctrlLossTmoNever = "-1"

// connect makes one path: the node's own identity, a reconnect that never
// gives up, as the agents' paths have (dnagent.md SH20), and the kernel's
// defaults for the rest (csi.md CSI14). A connect that fails, or that the
// deadline ends, counts as made when sysfs then shows the path, whatever
// nvme-cli printed (pathMade).
func (h *nvmeHost) connect(ctx context.Context, nqn string, ep endpoint) error {
	_, err := h.runBounded(ctx, "connect", "-t", trTypeTcp,
		"-a", ep.trAddr, "-s", ep.trSvcId, "-n", nqn,
		"--hostnqn", h.id.hostNqn, "--hostid", h.id.hostId,
		"--ctrl-loss-tmo", ctrlLossTmoNever)
	if err == nil {
		return nil
	}
	if made, rerr := h.pathMade(ctx, nqn, ep); rerr == nil && made {
		return nil
	}
	return err
}

// pathMade reads sysfs for a controller the kernel would refuse a second
// connect over, as already there: one of the subsystem, at the cntlr's
// address, made under the node's host NQN, in a state other than being
// deleted or dead. A connect that another tool or an earlier pass made
// between the follower's read of sysfs and its connect, or that the kernel
// finished after the deadline ended nvme-cli, is such a controller.
func (h *nvmeHost) pathMade(ctx context.Context, nqn string, ep endpoint) (bool, error) {
	ctrls, err := h.controllers(ctx)
	if err != nil {
		return false, err
	}
	for _, c := range ctrls {
		if c.subNqn != nqn || c.key() != ep.String() || c.hostNqn != h.id.hostNqn {
			continue
		}
		switch c.state {
		case ctrlStateDeleting, ctrlStateDeletingNoIo, ctrlStateDead:
			continue
		}
		return true, nil
	}
	return false, nil
}

// The states of a controller as sysfs names them in which the kernel no
// longer counts it as a path (its match of a duplicate connect skips them).
const (
	ctrlStateDeleting     = "deleting"
	ctrlStateDeletingNoIo = "deleting (no IO)"
	ctrlStateDead         = "dead"
)

// connectDiscovery opens a persistent discovery connection to one cdc
// address: the connection the cdc's change notification arrives on.
func (h *nvmeHost) connectDiscovery(ctx context.Context, ep endpoint) error {
	_, err := h.runBounded(ctx, "discover", "-t", trTypeTcp,
		"-a", ep.trAddr, "-s", ep.trSvcId,
		"--hostnqn", h.id.hostNqn, "--hostid", h.id.hostId, "-p")
	return err
}

// ctrlStillIs reports whether a controller's device name still names the
// path a drop or a lift was decided for, the same subsystem at the same
// cntlr address, read again from sysfs: the kernel gives a later connection
// the lowest free number, a deleted controller's name among them, which can
// be another volume's path by the time a step decided on an earlier read
// runs (csi.md CSI14, CSI15). A name that names no controller any more is
// not the path.
func (h *nvmeHost) ctrlStillIs(ctx context.Context, name, nqn, key string) (bool, error) {
	dir := path.Join(sysClassNvme, name)
	sub, err := h.oc.ReadFile(ctx, path.Join(dir, "subsysnqn"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading subsysnqn of %s: %w", name, err)
	}
	address, err := h.oc.ReadFile(ctx, path.Join(dir, "address"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading address of %s: %w", name, err)
	}
	c := ctrl{subNqn: strings.TrimSpace(sub)}
	c.trAddr, c.trSvcId = parseAddress(strings.TrimSpace(address))
	return c.subNqn == nqn && c.key() == key, nil
}

// lossTmoOff is what sysfs shows as a controller's ctrl_loss_tmo when its
// reconnects never give up.
const lossTmoOff = "off"

// lossTmo reads a controller's ctrl_loss_tmo from sysfs: "off" for a
// reconnect that never gives up, else its limit in seconds; empty, with no
// error, for a kernel or a controller that shows none.
func (h *nvmeHost) lossTmo(ctx context.Context, name string) (string, error) {
	data, err := h.oc.ReadFile(ctx, path.Join(sysClassNvme, name, "ctrl_loss_tmo"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading ctrl_loss_tmo of %s: %w", name, err)
	}
	return strings.TrimSpace(data), nil
}

// lossTmoNever sets a controller's reconnects to never give up, as the
// plugin's own connect makes them (ctrlLossTmoNever): the kernel takes a
// negative ctrl_loss_tmo written to sysfs as no limit. The write goes
// straight into the attribute, since sysfs lets no file be created
// (osclient.md, ReadFile / WriteFile / WriteFileDirect).
func (h *nvmeHost) lossTmoNever(ctx context.Context, name string) error {
	return h.oc.WriteFileDirect(ctx,
		path.Join(sysClassNvme, name, "ctrl_loss_tmo"), ctrlLossTmoNever)
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

// String writes an endpoint as ip:port, an IPv6 address in brackets.
func (e endpoint) String() string { return net.JoinHostPort(e.trAddr, e.trSvcId) }

// parseEndpoint reads an ip:port, an IPv6 address in brackets, which it
// strips: nvme-cli and sysfs carry the bare address.
func parseEndpoint(s string) (endpoint, error) {
	host, port, err := net.SplitHostPort(s)
	if err != nil || host == "" || port == "" {
		return endpoint{}, fmt.Errorf("%q is not ip:port", s)
	}
	return endpoint{trAddr: host, trSvcId: port}, nil
}

// subTypeCurrentDiscovery is how nvme-cli names, in lower case, the subtype
// of a discovery log record that describes the discovery service itself.
const subTypeCurrentDiscovery = "current discovery subsystem"

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
	out, err := h.runBounded(ctx, "discover", "-d", ctrlName, "-o", "json")
	if err != nil {
		return nil, err
	}
	return parseDiscoveryLog(out)
}

// parseDiscoveryLog reads nvme-cli's JSON rendering of a discovery log. An
// output that is empty, or a document without its list of records, is not
// a log that lists nothing: it cannot be read, and so holds the follower's
// drops (csi.md CSI15). Nor is a log without the record of the cdc itself,
// the current discovery subsystem, which every log page of a dnv cdc holds
// first (cdc.md DS12): an empty list, or one that only another service
// could have sent, would read as a cdc that lists no cntlr for any staged
// volume, and drop every path of every volume in use.
func parseDiscoveryLog(out string) ([]logEntry, error) {
	var doc struct {
		Records *[]struct {
			TrType  string `json:"trtype"`
			TrAddr  string `json:"traddr"`
			TrSvcId string `json:"trsvcid"`
			SubNqn  string `json:"subnqn"`
			SubType string `json:"subtype"`
		} `json:"records"`
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, errors.New("the discovery log read printed nothing")
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return nil, fmt.Errorf("the discovery log is not the JSON nvme-cli "+
			"renders: %w", err)
	}
	if doc.Records == nil {
		return nil, errors.New("the discovery log is not the JSON nvme-cli " +
			"renders: it has no records")
	}
	self := false
	for _, r := range *doc.Records {
		if strings.Contains(strings.ToLower(r.SubType), subTypeCurrentDiscovery) {
			self = true
			break
		}
	}
	if !self {
		return nil, fmt.Errorf("the discovery log holds no record of the cdc "+
			"itself (%d records), which a dnv cdc's log always holds first, "+
			"so it is not taken as a list", len(*doc.Records))
	}
	var entries []logEntry
	for _, r := range *doc.Records {
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
