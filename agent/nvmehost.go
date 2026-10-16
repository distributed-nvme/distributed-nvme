package agent

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// NvmeHost wraps the nvme-cli host patterns (SH17, SH20). Every dnv-internal
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
// (cnagent.md, `NvmeHost.DisconnectDevice`; CN10).
func (h *NvmeHost) DisconnectDevice(ctx context.Context, dev string) error {
	return h.runOk(ctx, "nvme", "disconnect", "--device", dev)
}

// SubsysState is what sysfs says about one subsystem the host holds.
type SubsysState struct {
	// Nqn is the subsystem's own NQN, the one ListSubsys was asked for.
	Nqn        string
	Found      bool
	Live       bool
	States     []string
	DevicePath string
	// Paths carries one entry per controller of the subsystem, so a caller
	// can pick a single path out of a multipath subsystem (DisconnectDevice)
	// or judge per-side liveness and ANA state (cnagent.md CN11/CN12). A
	// controller whose device the kernel has deleted stays listed until the
	// last reference to it drops, with every field but Name empty (HasCtrl).
	Paths []PathState
}

// HasCtrl reports whether the walk found a controller in the subsystem. A
// controller counts only when its "state" reads present: the subsystem keeps
// listing a deleted controller until the last reference to it drops, with
// nothing of it left to read (cnagent.md CN10), and such a controller is no
// connection. A nil state has no controller.
func (s *SubsysState) HasCtrl() bool {
	if s == nil {
		return false
	}
	for _, path := range s.Paths {
		if path.State != "" {
			return true
		}
	}
	return false
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
//     and the destination of architecture.md, Migration, could never find its
//     migration source.
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
	// Inside a subsystem directory, `nvme0` is a controller, `nvme0n1` the
	// multipath namespace and `ng0n1` that namespace's ng device
	// (ngDevOf); inside a controller directory, `nvme0c1n1` is that
	// controller's own (hidden) path device, the only place ana_state lives.
	nvmeCtrlEntryPattern = regexp.MustCompile(`^nvme\d+$`)
	nvmeNsEntryPattern   = regexp.MustCompile(`^nvme\d+n\d+$`)
	nvmePathEntryPattern = regexp.MustCompile(`^nvme\d+c\d+n\d+$`)
)

// listSysfs lists a directory of the nvme sysfs tree in the agent's own
// process (readDir). An absent directory is "no entries" —
// /sys/class/nvme-subsystem is missing until nvme-core loads, and a host
// holds no fabrics controller before it has — but a listing that did not
// answer is an error, so a caller enumerating connections cannot read a
// failed listing as "this host holds nothing".
func (h *NvmeHost) listSysfs(
	ctx context.Context,
	path string,
) ([]string, error) {
	names, _, err := h.readDir(ctx, path)
	return names, err
}

// readTrimmed reads one sysfs attribute under the soft timeout
// (architecture.md, Common validation; SH15).
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

// ListSubsys probes one subsystem NQN: whether the host holds its subsystem
// entry (Found), the controllers in it (Paths), whether any path is live, and
// the block device of its namespace. An absent subsystem is "not found",
// never an error. Found is not "connected": while something holds the
// subsystem's multipath head open, the kernel keeps its entry after the last
// controller, with no controller and no namespace node in it, so whether the
// NQN is connected is read from HasCtrl, never from Found.
//
// Each call walks afresh and stops at the first entry named nqn, which it
// reads under ReadStrict with the transport (SH20). A subsysnqn read that did
// not answer before the match fails the call, as does any read inside the
// match that did not answer; no read past the match is made. A read that
// finds nothing fails nothing: a matched directory gone since the listing is
// found and empty.
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
		got, ok, err := h.ReadSubsysNqn(ctx, dir)
		if err != nil {
			return nil, err
		}
		if !ok || got != nqn {
			continue
		}
		read, err := h.ReadSubsys(ctx, dir, SubsysReadOpts{
			Transport: true,
			Rule:      ReadStrict,
		})
		if err != nil {
			return nil, err
		}
		return read.State(nqn), nil
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
	// storage pool's worth of legs that is one listing per leg in every
	// sweep, and a sweep wants the node for a handful of them at most.
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
//
// It reads each controller's hostnqn alone, in listing order, up to the
// first that matches (CtrlHostNqn, under ReadStrict): a hostnqn read that did
// not answer before it is the error, an absent one is passed over, and a
// directory gone since the walk holds no controller.
func (h *NvmeHost) HeldWithHostNqn(
	ctx context.Context,
	brief SubsysBrief,
	hostNqn string,
) (bool, error) {
	read, err := h.ReadSubsys(ctx, brief.Dir, SubsysReadOpts{
		Ctrls:   CtrlHostNqn,
		HostNqn: hostNqn,
		Rule:    ReadStrict,
	})
	if err != nil {
		return false, err
	}
	return read.HostNqnHeld, nil
}

// SubsysDevicePath is the multipath namespace node of one enumerated
// subsystem, or "" when it has none. It reads the directory's listing alone
// (CtrlNone, under ReadStrict) and takes the first namespace node in it,
// whatever its nsid; a directory gone since the walk has none.
func (h *NvmeHost) SubsysDevicePath(
	ctx context.Context,
	brief SubsysBrief,
) (string, error) {
	read, err := h.ReadSubsys(ctx, brief.Dir, SubsysReadOpts{
		Ctrls: CtrlNone,
		Rule:  ReadStrict,
	})
	if err != nil {
		return "", err
	}
	return read.NsDev, nil
}

// ListAllSubsys enumerates every subsystem entry this host holds, an entry
// the kernel keeps with no controller included (ListSubsys). It is what lets
// a sweep find connections no desired state names — a clone source whose
// cntlr is gone, a leg of an sp that left the pointer list — which a per-NQN
// lookup by definition cannot. It walks afresh and fails on the first
// subsysnqn read that did not answer, reading no NQN past it (Briefs).
func (h *NvmeHost) ListAllSubsys(ctx context.Context) ([]SubsysBrief, error) {
	walk, err := h.walk(ctx, true)
	if err != nil {
		return nil, err
	}
	return walk.Briefs()
}

// ---------------------------------------------------------------------------
// One walk for both agents (SH20)
//
// A walk lists /sys/class/nvme-subsystem and reads each entry's subsysnqn: it
// says where each subsystem is, not what is in it. A read of one entry
// (ReadSubsys) lists the entry's directory and reads, of its namespace nodes
// and its controllers, what its caller reads. Both agents read the nvme host
// tree through this one code, each caller under its own rule
// (SubsysReadRule) and reading no more than that rule needs
// (SubsysCtrlReads): two codes reading one entry's layout drift apart, and a
// change to one is easily missed in the other.
// ---------------------------------------------------------------------------

// SubsysEntry is one entry of /sys/class/nvme-subsystem as a walk read it.
type SubsysEntry struct {
	// Dir is the entry's directory, /sys/class/nvme-subsystem/nvme-subsysN.
	Dir string
	// Nqn is the trimmed subsysnqn; "" when Gone or Err is set.
	Nqn string
	// Gone is a subsysnqn read that found nothing: the subsystem went away
	// between the listing and the read.
	Gone bool
	// Err is the subsysnqn read that did not answer.
	Err error
}

// SubsysWalk is one listing of /sys/class/nvme-subsystem with each entry's
// NQN, in listing (byte) order.
type SubsysWalk struct {
	Entries []SubsysEntry
}

// WalkSubsys lists /sys/class/nvme-subsystem and reads every entry's
// subsysnqn, recording each read that did not answer apart from one that
// found nothing. A missing class directory is a walk with no entry. The
// error is the listing's, when it did not answer.
func (h *NvmeHost) WalkSubsys(ctx context.Context) (*SubsysWalk, error) {
	return h.walk(ctx, false)
}

// walk is WalkSubsys; failFast ends it after the first subsysnqn read that
// did not answer, so a caller that fails on that read reads no NQN past it.
func (h *NvmeHost) walk(
	ctx context.Context,
	failFast bool,
) (*SubsysWalk, error) {
	names, err := h.listSysfs(ctx, sysfsNvmeSubsysDir)
	if err != nil {
		return nil, err
	}
	walk := &SubsysWalk{}
	for _, name := range names {
		entry := SubsysEntry{Dir: sysfsNvmeSubsysDir + "/" + name}
		nqn, ok, err := h.ReadSubsysNqn(ctx, entry.Dir)
		switch {
		case err != nil:
			entry.Err = err
		case !ok:
			entry.Gone = true
		default:
			entry.Nqn = nqn
		}
		walk.Entries = append(walk.Entries, entry)
		if err != nil && failFast {
			break
		}
	}
	return walk, nil
}

// Find is the first entry whose NQN is nqn, and the first entry whose
// subsysnqn read did not answer, each nil when there is none. Which of the
// two decides is the caller's rule: the cn's leg lookup takes a match beside
// an entry that did not answer, because the host keeps one subsystem for
// every controller of one NQN ([D1]; cnagent.md CN10).
func (w *SubsysWalk) Find(
	nqn string,
) (match *SubsysEntry, unread *SubsysEntry) {
	for i := range w.Entries {
		entry := &w.Entries[i]
		switch {
		case entry.Err != nil:
			if unread == nil {
				unread = entry
			}
		case !entry.Gone && entry.Nqn == nqn && match == nil:
			match = entry
		}
	}
	return match, unread
}

// Briefs is the walk as ListAllSubsys's list of connections: the first entry
// whose subsysnqn read did not answer, in listing order, is the error, since
// a list that passed over it could miss a connection a sweep must remove; an
// entry gone since the listing is no connection.
func (w *SubsysWalk) Briefs() ([]SubsysBrief, error) {
	var out []SubsysBrief
	for _, entry := range w.Entries {
		if entry.Err != nil {
			return nil, entry.Err
		}
		if entry.Gone {
			continue
		}
		out = append(out, SubsysBrief{Nqn: entry.Nqn, Dir: entry.Dir})
	}
	return out, nil
}

// ReadSubsysNqn reads the subsysnqn of one entry's directory, trimmed, under
// readTrimmed's rule: ok is false only when it is not there. A walk reads it
// for every entry, and a caller holding an older walk reads it again before
// it trusts the entry, because the kernel gives a freed entry's number to the
// next new subsystem.
func (h *NvmeHost) ReadSubsysNqn(
	ctx context.Context,
	dir string,
) (string, bool, error) {
	return h.readTrimmed(ctx, dir+"/subsysnqn")
}

// SubsysReadRule is what a read inside ReadSubsys that did not answer does to
// the rest of it. A read that found nothing fails nothing under either rule.
type SubsysReadRule int

const (
	// ReadStrict is ListSubsys's rule: the first read that did not answer
	// ends the read and is its error. A controller whose address is not
	// there is read in full all the same.
	ReadStrict SubsysReadRule = iota
	// ReadTolerant is the cn lookup's rule (cnagent.md CN10): a read that
	// did not answer is recorded in the read's Err field of its kind and the
	// read goes on, so the caller judges each one. A controller whose
	// address is not there is gone, and nothing more of it is read. A
	// hostnqn and a transport read have no Err field: one that did not
	// answer ends the read under this rule too (CtrlHostNqn, Transport).
	ReadTolerant
)

// apply is the rule's answer to one read's error, nil when the read
// answered: under ReadStrict the error comes back, to end the whole read;
// under ReadTolerant it is kept in rec, unless rec holds an earlier one, and
// nil comes back, so the read goes on.
func (r SubsysReadRule) apply(rec *error, err error) error {
	if err == nil {
		return nil
	}
	if r != ReadTolerant {
		return err
	}
	if *rec == nil {
		*rec = err
	}
	return nil
}

// SubsysCtrlReads is what ReadSubsys reads of each controller of the entry:
// each caller reads what its rule needs and no more (SH20).
type SubsysCtrlReads int

const (
	// CtrlPaths reads, into Ctrls, each controller's address, its transport
	// when Transport is set, its state, its directory and its path devices'
	// ana_state (ListSubsys, the cn lookup). The zero value.
	CtrlPaths SubsysCtrlReads = iota
	// CtrlHostNqn reads each controller's hostnqn alone, in listing order,
	// and ends the read at the first one equal to HostNqn: nothing after
	// that controller in the listing is read (HeldWithHostNqn). A hostnqn
	// read that did not answer ends the read with its error, under either
	// rule.
	CtrlHostNqn
	// CtrlNone reads nothing of a controller (SubsysDevicePath).
	CtrlNone
)

// SubsysReadOpts is what one ReadSubsys reads, and under which rule.
type SubsysReadOpts struct {
	// NsIdx 0 takes the first namespace node and reads no nsid. Any other
	// value takes the first node whose nsid reads that number: a node whose
	// nsid is not there is passed over, and under ReadTolerant so is one
	// whose nsid read did not answer.
	NsIdx uint32
	// Ctrls is what is read of each controller.
	Ctrls SubsysCtrlReads
	// HostNqn is the host NQN CtrlHostNqn looks for.
	HostNqn string
	// Transport reads each controller's transport too, under CtrlPaths
	// (ListSubsys). A transport read that did not answer ends the read with
	// its error, under either rule.
	Transport bool
	// Rule is what a read that did not answer does to the rest of the read.
	Rule SubsysReadRule
}

// SubsysRead is one read of one entry's directory. Each Err field holds the
// first read of its kind that did not answer under ReadTolerant; under
// ReadStrict such a read ends ReadSubsys with its error instead, so none is
// set.
type SubsysRead struct {
	Dir string
	// DirGone is a listing of Dir that found nothing, and DirErr one that
	// did not answer; with either, nothing else was read.
	DirGone bool
	DirErr  error
	// NsDev is "/dev/<node>" of the namespace node NsIdx picked, "" when
	// none; NsErr is the first nsid read that did not answer.
	NsDev string
	NsErr error
	// NgListed says the listing of Dir that picked NsDev holds the node's
	// ng device too (ngDevOf). The kernel adds the ng device last, after
	// the node can be opened by its number, so it is the sign that the
	// node is complete; it costs no read. False when NsDev is "".
	NgListed bool
	// Ctrls is one CtrlRead per controller, in listing order (CtrlPaths).
	Ctrls []CtrlRead
	// HostNqnHeld says some controller's hostnqn equals HostNqn
	// (CtrlHostNqn).
	HostNqnHeld bool
}

// CtrlRead is the read of one controller, /sys/class/nvme/<Name>, under
// CtrlPaths.
type CtrlRead struct {
	// Name is the controller device, e.g. "nvme0".
	Name string
	// TrAddr and TrSvcId are parsed out of the controller's address.
	TrAddr  string
	TrSvcId string
	// AddrGone is an address that is not there: the kernel has deleted the
	// controller's device, and the subsystem lists it until the last
	// reference to it drops (cnagent.md CN10). AddrErr is an address read
	// that did not answer; its text is "read <dir>/address: <cause>".
	AddrGone bool
	AddrErr  error
	// Transport is read only with SubsysReadOpts.Transport.
	Transport string
	// State is "" when the state is not there or its read did not answer.
	State    string
	StateErr error
	// DirErr is a listing of the controller's directory that did not
	// answer; a directory that is not there holds no path device.
	DirErr error
	// AnaState is the ana_state of the first path device whose ana_state
	// is there, and AnaErr the first ana_state read that did not answer.
	AnaState string
	AnaErr   error
}

// ReadSubsys reads one entry's directory under opts, in listing order: the
// namespace node NsIdx picks, whether its ng device is in the same listing,
// and of each controller what opts.Ctrls says. A directory that is not there
// reads with DirGone set and nothing else. Under ReadStrict the first read
// that did not answer ends the read, and ReadSubsys returns nil and that
// read's error; under ReadTolerant only a hostnqn or a transport read does
// (CtrlHostNqn, Transport).
func (h *NvmeHost) ReadSubsys(
	ctx context.Context,
	dir string,
	opts SubsysReadOpts,
) (*SubsysRead, error) {
	read := &SubsysRead{Dir: dir}
	names, present, err := h.readDir(ctx, dir)
	if err != nil {
		if err = opts.Rule.apply(&read.DirErr, err); err != nil {
			return nil, err
		}
		return read, nil
	}
	if !present {
		read.DirGone = true
		return read, nil
	}
	for _, name := range names {
		switch {
		case nvmeNsEntryPattern.MatchString(name):
			if read.NsDev != "" {
				continue
			}
			picked, err := h.nsidIs(ctx, dir+"/"+name, opts.NsIdx)
			if err = opts.Rule.apply(&read.NsErr, err); err != nil {
				return nil, err
			}
			if picked {
				read.NsDev = "/dev/" + name
				// The whole listing, whichever order it is in: the ng
				// device sorts before its node.
				read.NgListed = slices.Contains(names, ngDevOf(name))
			}
		case nvmeCtrlEntryPattern.MatchString(name):
			switch opts.Ctrls {
			case CtrlPaths:
				ctrl, err := h.readCtrl(ctx, name, opts)
				if err != nil {
					return nil, err
				}
				read.Ctrls = append(read.Ctrls, ctrl)
			case CtrlHostNqn:
				got, ok, err := h.readTrimmed(
					ctx, sysfsNvmeCtrlDir+"/"+name+"/hostnqn")
				if err != nil {
					return nil, err
				}
				if ok && got == opts.HostNqn {
					read.HostNqnHeld = true
					return read, nil
				}
			}
		}
	}
	return read, nil
}

// ngDevOf is the name of the ng device the kernel adds beside the multipath
// namespace node nsName, inside the same subsystem directory: ng<X>n<Y> for
// nvme<X>n<Y>, named after the node and never after the nsid.
func ngDevOf(nsName string) string {
	return "ng" + strings.TrimPrefix(nsName, "nvme")
}

// nsidIs says whether the namespace node at nsDir is the one nsIdx names: any
// node when nsIdx is 0, which reads nothing, else the node whose nsid reads
// that number. A node whose nsid is not there is not it.
func (h *NvmeHost) nsidIs(
	ctx context.Context,
	nsDir string,
	nsIdx uint32,
) (bool, error) {
	if nsIdx == 0 {
		return true, nil
	}
	nsid, _, err := h.readTrimmed(ctx, nsDir+"/nsid")
	if err != nil {
		return false, err
	}
	return nsid == strconv.FormatUint(uint64(nsIdx), 10), nil
}

// readCtrl reads one controller under CtrlPaths: its address, its transport
// when opts.Transport is set, its liveness and — from its own hidden path
// device, the only place it exists — the ANA state. Under ReadStrict the
// error is the first read that did not answer; under ReadTolerant it is a
// transport read that did not answer, and there is none without one.
func (h *NvmeHost) readCtrl(
	ctx context.Context,
	name string,
	opts SubsysReadOpts,
) (CtrlRead, error) {
	ctrlDir := sysfsNvmeCtrlDir + "/" + name
	ctrl := CtrlRead{Name: name}
	address, ok, err := h.readTrimmed(ctx, ctrlDir+"/address")
	switch {
	case err != nil:
		if err = opts.Rule.apply(&ctrl.AddrErr, err); err != nil {
			return ctrl, err
		}
	case !ok:
		ctrl.AddrGone = true
		if opts.Rule == ReadTolerant {
			// The device is deleted and nothing else of it is left to
			// read; only the subsystem's link to it is.
			return ctrl, nil
		}
	default:
		ctrl.TrAddr, ctrl.TrSvcId = ParseNvmeAddress(address)
	}
	if opts.Transport {
		ctrl.Transport, _, err = h.readTrimmed(ctx, ctrlDir+"/transport")
		if err != nil {
			return ctrl, err
		}
	}
	ctrl.State, _, err = h.readTrimmed(ctx, ctrlDir+"/state")
	if err = opts.Rule.apply(&ctrl.StateErr, err); err != nil {
		return ctrl, err
	}
	entries, err := h.listSysfs(ctx, ctrlDir)
	if err = opts.Rule.apply(&ctrl.DirErr, err); err != nil {
		return ctrl, err
	}
	for _, entry := range entries {
		if !nvmePathEntryPattern.MatchString(entry) {
			continue
		}
		ana, ok, err := h.readTrimmed(ctx, ctrlDir+"/"+entry+"/ana_state")
		if err = opts.Rule.apply(&ctrl.AnaErr, err); err != nil {
			return ctrl, err
		}
		if ok {
			ctrl.AnaState = ana
			break
		}
	}
	return ctrl, nil
}

// State is the read as ListSubsys answers it, for the entry named nqn: the
// subsystem found, its namespace node, one PathState per controller, and
// live when any controller's state is.
func (r *SubsysRead) State(nqn string) *SubsysState {
	state := &SubsysState{Found: true, Nqn: nqn, DevicePath: r.NsDev}
	for _, ctrl := range r.Ctrls {
		state.Paths = append(state.Paths, PathState{
			Name:      ctrl.Name,
			Transport: ctrl.Transport,
			TrAddr:    ctrl.TrAddr,
			TrSvcId:   ctrl.TrSvcId,
			State:     ctrl.State,
			AnaState:  ctrl.AnaState,
		})
		state.States = append(state.States, ctrl.State)
		if ctrl.State == "live" {
			state.Live = true
		}
	}
	return state
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
