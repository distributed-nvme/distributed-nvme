package agent

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// NvmeHost wraps the nvme-cli host patterns of Appendix A. Every dnv-internal
// connection is made with --fast_io_fail_tmo DefaultNvmeFastIoFailTmo,
// --ctrl-loss-tmo -1 (SH20) and the common.NvmeHostId derived from its own
// hostnqn — never the node-wide /etc/nvme/hostid, which the kernel's 1:1
// hostnqn<->hostid rule would turn into an EINVAL the moment anything else
// on the node holds it (see common.NvmeHostId).
//
// The underscores in --fast_io_fail_tmo are not a typo and must not be
// "fixed" to dashes: nvme-cli spells that one option with underscores
// (fabrics.c OPT_INT("fast_io_fail_tmo", 'F', ...)), and its argconfig
// getopt_long table carries the name verbatim, so --fast-io-fail-tmo is
// rejected outright with `unrecognized option`. Every neighbouring option
// (--ctrl-loss-tmo, --reconnect-delay, --keep-alive-tmo) really is dashed.
type NvmeHost struct {
	osBase
}

func NewNvmeHost(oc common.OsClient) *NvmeHost {
	return &NvmeHost{osBase{oc: oc}}
}

// TrConf is one NVMe-oF transport endpoint.
type TrConf struct {
	TrType  string
	AdrFam  string
	TrAddr  string
	TrSvcId string
}

func (h *NvmeHost) Connect(
	ctx context.Context,
	tr TrConf,
	nqn string,
	hostNqn string,
) error {
	return h.runOk(ctx, "nvme", "connect",
		"--transport", tr.TrType,
		"--traddr", tr.TrAddr,
		"--trsvcid", tr.TrSvcId,
		"--nqn", nqn,
		"--hostnqn", hostNqn,
		"--hostid", common.NvmeHostId(hostNqn),
		"--fast_io_fail_tmo",
		strconv.Itoa(common.DefaultNvmeFastIoFailTmo),
		"--ctrl-loss-tmo", "-1")
}

func (h *NvmeHost) Disconnect(ctx context.Context, nqn string) error {
	return h.runOk(ctx, "nvme", "disconnect", "--nqn", nqn)
}

// DisconnectDevice retires exactly one controller of a subsystem (SH20). The
// two sides of a migrating leg share one subsystem NQN ([D1]), so
// `nvme disconnect --nqn` would kill the live path together with the dead one;
// the cn agent drops the dead side by its controller device instead
// (cnagent.md §2.3, CN10).
func (h *NvmeHost) DisconnectDevice(ctx context.Context, dev string) error {
	return h.runOk(ctx, "nvme", "disconnect", "--device", dev)
}

// SubsysState is what sysfs says about one subsystem the host holds.
type SubsysState struct {
	// Nqn is the subsystem's own NQN. ListSubsys already knows it (it is
	// what the caller asked for); ListAllSubsys is why it is carried.
	Nqn        string
	Found      bool
	Live       bool
	States     []string
	DevicePath string
	// Paths carries one entry per controller of the subsystem, so a caller
	// can pick a single path out of a multipath subsystem (DisconnectDevice)
	// or judge per-side liveness and ANA state (cnagent.md CN11/CN12).
	Paths []PathState
}

// PathState is one controller (one path) of a subsystem.
type PathState struct {
	// Name is the controller device, e.g. "nvme0" — what
	// `nvme disconnect --device` takes.
	Name      string
	Transport string
	// TrAddr / TrSvcId are parsed out of the "traddr=…,trsvcid=…" bag the
	// controller's sysfs `address` attribute holds.
	TrAddr   string
	TrSvcId  string
	State    string
	AnaState string
}

// ---------------------------------------------------------------------------
// The sysfs view of one subsystem
//
// State comes from sysfs, not from `nvme list-subsys -o json`, for the same
// measured reasons the cn agent reads sysfs for legs (cnagent.md CN10/CN12)
// plus one the dn hit first:
//
//   - `nvme list-subsys -o json` lists **no namespaces at all** (nvme-cli
//     2.16, with or without --verbose): a subsystem entry is just Name, NQN
//     and Paths. There is therefore no block device to hand dm-clone in it,
//     and the §11.2 destination could never find its migration source.
//   - It emits no `ANAState` either unless given a namespace block device,
//     and with one it answers an all-inaccessible namespace with an *empty*
//     list, indistinguishable from "not connected".
//
// sysfs has all of it: /sys/class/nvme-subsystem/<subsys>/subsysnqn keys the
// lookup, the same directory holds the multipath namespace nodes (nvme0n1)
// and the controllers (nvme0), and each controller carries its transport
// address, its state and — on its own hidden path device (nvme0c1n1) — the
// ana_state.
// ---------------------------------------------------------------------------

const (
	sysfsNvmeSubsysDir = "/sys/class/nvme-subsystem"
	sysfsNvmeCtrlDir   = "/sys/class/nvme"
)

var (
	// Inside a subsystem directory, `nvme0` is a controller and `nvme0n1`
	// the multipath namespace; inside a controller directory, `nvme0c1n1` is
	// that controller's own (hidden) path device, the only place ana_state
	// lives.
	nvmeCtrlEntryPattern = regexp.MustCompile(`^nvme\d+$`)
	nvmeNsEntryPattern   = regexp.MustCompile(`^nvme\d+n\d+$`)
	nvmePathEntryPattern = regexp.MustCompile(`^nvme\d+c\d+n\d+$`)
)

// listSysfs lists a directory of the nvme sysfs tree. An absent directory is
// "no entries" — the whole /sys/class/nvme-subsystem tree is missing until
// the host holds its first fabrics controller — but a listing that did not
// answer is an error, so a caller enumerating connections cannot read a
// killed `ls` as "this host holds nothing".
func (h *NvmeHost) listSysfs(
	ctx context.Context,
	path string,
) ([]string, error) {
	entries, _, err := h.listDir(ctx, path)
	return entries, err
}

// readTrimmed reads one sysfs attribute under the §7 soft timeout (SH15).
// Unlike most of sysfs, the /sys/class/nvme* tree can stall while a controller
// is mid-reset or being torn down, which is exactly when this walk runs
// (SH15 applies to every read of it). Only a genuine ENOENT is "absent": a
// stalled read is an error, because ListSubsys reading it as absence would
// report a live subsystem as not connected — and the caller of that answer
// disconnects nothing and forgets it.
func (h *NvmeHost) readTrimmed(
	ctx context.Context,
	path string,
) (string, bool, error) {
	return h.readAttrStrict(ctx, path)
}

// ListSubsys probes one subsystem NQN: whether the host holds a controller
// for it, whether any path is live, and the block device of its namespace.
// An absent subsystem is "not found", never an error.
func (h *NvmeHost) ListSubsys(
	ctx context.Context,
	nqn string,
) (*SubsysState, error) {
	entries, err := h.listSysfs(ctx, sysfsNvmeSubsysDir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		dir := sysfsNvmeSubsysDir + "/" + entry
		got, ok, err := h.readTrimmed(ctx, dir+"/subsysnqn")
		if err != nil {
			return nil, err
		}
		if !ok || got != nqn {
			continue
		}
		state, err := h.readSubsysDir(ctx, dir, nqn)
		if err != nil {
			return nil, err
		}
		return state, nil
	}
	return &SubsysState{}, nil
}

// SubsysBrief is the cheap view of one subsystem this host holds: which
// subsystem it is, and where to look if more is wanted.
//
// It deliberately carries no per-controller detail. Reading a controller's
// transport, state and ANA costs four file reads EACH, and a node carrying a
// storage pool's worth of legs holds hundreds of them — while a sweep needs
// none of it: the question a sweep asks is "which subsystems are here", and
// for a handful of them "what is the namespace node called".
type SubsysBrief struct {
	Nqn string
	// Dir is the subsystem's sysfs directory, so a caller that needs the
	// namespace node can ask for it with SubsysDevicePath instead of paying
	// for a directory listing on every subsystem. On a node carrying a
	// storage pool's worth of legs that is one forked `ls` per leg per pass,
	// and a sweep wants the node for a handful of them at most.
	Dir string
}

// HeldWithHostNqn reports whether any controller of one enumerated subsystem
// was opened with the given host NQN.
//
// It is what tells a sweep its own connection from a sibling agent's on a
// node that runs several: the nvme HOST namespace is per kernel, not per
// agent, and a MigrSrcNqn carries the SOURCE dn's id, not the connecting
// one's. The host NQN is the only field that names who opened the
// connection.
func (h *NvmeHost) HeldWithHostNqn(
	ctx context.Context,
	brief SubsysBrief,
	hostNqn string,
) (bool, error) {
	names, err := h.listSysfs(ctx, brief.Dir)
	if err != nil {
		return false, err
	}
	for _, name := range names {
		if !nvmeCtrlEntryPattern.MatchString(name) {
			continue
		}
		got, ok, err := h.readTrimmed(
			ctx, sysfsNvmeCtrlDir+"/"+name+"/hostnqn")
		if err != nil {
			return false, err
		}
		if ok && got == hostNqn {
			return true, nil
		}
	}
	return false, nil
}

// SubsysDevicePath is the multipath namespace node of one enumerated
// subsystem, or "" when it has none.
func (h *NvmeHost) SubsysDevicePath(
	ctx context.Context,
	brief SubsysBrief,
) (string, error) {
	names, err := h.listSysfs(ctx, brief.Dir)
	if err != nil {
		return "", err
	}
	for _, name := range names {
		if nvmeNsEntryPattern.MatchString(name) {
			return "/dev/" + name, nil
		}
	}
	return "", nil
}

// ListAllSubsys enumerates every subsystem this host holds a controller for.
// It is what lets a sweep find connections no desired state names — a clone
// source whose cntlr is gone, a leg of an sp that left the pointer list —
// which a per-NQN lookup by definition cannot.
func (h *NvmeHost) ListAllSubsys(ctx context.Context) ([]SubsysBrief, error) {
	entries, err := h.listSysfs(ctx, sysfsNvmeSubsysDir)
	if err != nil {
		return nil, err
	}
	var out []SubsysBrief
	for _, entry := range entries {
		dir := sysfsNvmeSubsysDir + "/" + entry
		nqn, ok, err := h.readTrimmed(ctx, dir+"/subsysnqn")
		if err != nil {
			return nil, err
		}
		if !ok {
			// The subsystem went away between the listing and the read.
			continue
		}
		out = append(out, SubsysBrief{Nqn: nqn, Dir: dir})
	}
	return out, nil
}

func (h *NvmeHost) readSubsysDir(
	ctx context.Context,
	dir string,
	nqn string,
) (*SubsysState, error) {
	state := &SubsysState{Found: true, Nqn: nqn}
	names, err := h.listSysfs(ctx, dir)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		switch {
		case nvmeNsEntryPattern.MatchString(name):
			if state.DevicePath == "" {
				state.DevicePath = "/dev/" + name
			}
		case nvmeCtrlEntryPattern.MatchString(name):
			path, err := h.readCtrl(ctx, name)
			if err != nil {
				return nil, err
			}
			state.Paths = append(state.Paths, path)
			state.States = append(state.States, path.State)
			if path.State == "live" {
				state.Live = true
			}
		}
	}
	return state, nil
}

// readCtrl reads one controller's transport, liveness and — from its own
// hidden path device, the only place it exists — the ANA state.
func (h *NvmeHost) readCtrl(
	ctx context.Context,
	name string,
) (PathState, error) {
	ctrlDir := sysfsNvmeCtrlDir + "/" + name
	path := PathState{Name: name}
	address, ok, err := h.readTrimmed(ctx, ctrlDir+"/address")
	if err != nil {
		return path, err
	}
	if ok {
		path.TrAddr, path.TrSvcId = ParseNvmeAddress(address)
	}
	transport, ok, err := h.readTrimmed(ctx, ctrlDir+"/transport")
	if err != nil {
		return path, err
	}
	if ok {
		path.Transport = transport
	}
	ctrlState, ok, err := h.readTrimmed(ctx, ctrlDir+"/state")
	if err != nil {
		return path, err
	}
	if ok {
		path.State = ctrlState
	}
	entries, err := h.listSysfs(ctx, ctrlDir)
	if err != nil {
		return path, err
	}
	for _, entry := range entries {
		if !nvmePathEntryPattern.MatchString(entry) {
			continue
		}
		ana, ok, err := h.readTrimmed(ctx, ctrlDir+"/"+entry+"/ana_state")
		if err != nil {
			return path, err
		}
		if ok {
			path.AnaState = ana
			break
		}
	}
	return path, nil
}

// ParseNvmeAddress splits the "traddr=1.2.3.4,trsvcid=4420,src_addr=…" bag
// that both nvme-cli and sysfs (/sys/class/nvme/{ctrl}/address) print for a
// controller. It is parsed as comma-separated key=value pairs and never by
// position: `src_addr` drops out while a controller is reconnecting. Unknown
// keys are ignored; a missing key yields an empty string, which never matches
// a desired side.
func ParseNvmeAddress(address string) (string, string) {
	var trAddr, trSvcId string
	for _, field := range strings.Split(address, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "traddr":
			trAddr = strings.TrimSpace(value)
		case "trsvcid":
			trSvcId = strings.TrimSpace(value)
		}
	}
	return trAddr, trSvcId
}
