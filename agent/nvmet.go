package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// NvmetRoot is the configfs mount point of the kernel NVMe target.
const NvmetRoot = "/sys/kernel/config/nvmet"

// ANA state strings written into ana_groups/{id}/ana_state, exactly once at
// port setup ([D4]).
const (
	AnaStateOptimized    = "optimized"
	AnaStateNonOptimized = "non-optimized"
	AnaStateInaccessible = "inaccessible"
)

// fixedAnaGrpIds is the [D4] group set, in the order the port setup walks it.
var fixedAnaGrpIds = []int{
	common.AnaGrpIdOptimized,
	common.AnaGrpIdNonOptimized,
	common.AnaGrpIdInaccessible,
}

// AnaStateOf maps one of the three fixed group ids to its fixed state.
func AnaStateOf(grpId int) string {
	switch grpId {
	case common.AnaGrpIdOptimized:
		return AnaStateOptimized
	case common.AnaGrpIdNonOptimized:
		return AnaStateNonOptimized
	case common.AnaGrpIdInaccessible:
		return AnaStateInaccessible
	}
	return ""
}

// Nvmet wraps the nvmet configfs patterns (SH18, SH19; cnagent.md CN16).
// Attribute writes go through OsClient.WriteFileDirect, directory and link
// operations through
// RunCommand, probing through ReadFile (SH18); WriteFile is never used on a
// /sys/kernel/config path.
type Nvmet struct {
	osBase
}

func NewNvmet(oc common.OsClient) *Nvmet {
	return &Nvmet{osBase{oc: oc}}
}

func (n *Nvmet) PortPath(portId int) string {
	return fmt.Sprintf("%s/ports/%d", NvmetRoot, portId)
}

func (n *Nvmet) AnaGroupPath(portId int, grpId int) string {
	return fmt.Sprintf("%s/ana_groups/%d", n.PortPath(portId), grpId)
}

func (n *Nvmet) SubsysPath(nqn string) string {
	return fmt.Sprintf("%s/subsystems/%s", NvmetRoot, nqn)
}

func (n *Nvmet) NsPath(nqn string, nsid int) string {
	return fmt.Sprintf("%s/namespaces/%d", n.SubsysPath(nqn), nsid)
}

func (n *Nvmet) HostPath(hostNqn string) string {
	return fmt.Sprintf("%s/hosts/%s", NvmetRoot, hostNqn)
}

func (n *Nvmet) portLinkPath(portId int, nqn string) string {
	return fmt.Sprintf("%s/subsystems/%s", n.PortPath(portId), nqn)
}

// ---------------------------------------------------------------------------
// Port — the one port per agent, with the three fixed ANA groups (SH19, [D4])
// ---------------------------------------------------------------------------

// PortConf is the agent's single nvmet port: the four --tr-* flags plus the
// configfs id the port lives under.
type PortConf struct {
	// PortId is the configfs id of the port this agent converges
	// (/sys/kernel/config/nvmet/ports/<PortId>). common.NvmetPortId is the
	// default; `dnv-agent --nvmet-port-id` overrides it so several agents
	// can share one node's kernel. Every Ensure/Probe/PortLink* method
	// still takes the id as an explicit argument — this field is only where
	// the two role servers keep the one they were built with.
	PortId int

	TrType   string
	AdrFam   string
	TrAddr   string
	TrSvcId  string
	AnaGrpId []int
}

func (n *Nvmet) portAttrs(conf PortConf) [][2]string {
	return [][2]string{
		{"addr_trtype", conf.TrType},
		{"addr_adrfam", conf.AdrFam},
		{"addr_traddr", conf.TrAddr},
		{"addr_trsvcid", conf.TrSvcId},
	}
}

// EnsurePort creates ports/{portId} with the node's transport attributes and
// the three fixed ANA groups, writing each ana_state exactly once. No other
// code path ever writes an ana_state; every later transition rewrites a
// namespace's ana_grpid instead.
func (n *Nvmet) EnsurePort(
	ctx context.Context,
	portId int,
	conf PortConf,
) error {
	portPath := n.PortPath(portId)
	exists, err := n.dirExists(ctx, portPath)
	if err != nil {
		return err
	}
	if !exists {
		if err := n.mkdir(ctx, portPath); err != nil {
			return err
		}
	}
	for _, attr := range n.portAttrs(conf) {
		if attr[1] == "" {
			continue
		}
		if _, err := n.ensureAttr(
			ctx, portPath+"/"+attr[0], attr[1]); err != nil {
			return err
		}
	}
	// Group 1 always exists in nvmet; 2 and 3 are created here, and only
	// then are the three fixed states written — once, and never again.
	for _, grpId := range fixedAnaGrpIds {
		groupPath := n.AnaGroupPath(portId, grpId)
		groupExists, err := n.dirExists(ctx, groupPath)
		if err != nil {
			return err
		}
		if groupExists {
			continue
		}
		if err := n.mkdir(ctx, groupPath); err != nil {
			return err
		}
	}
	for _, grpId := range fixedAnaGrpIds {
		if _, err := n.ensureAttr(ctx,
			n.AnaGroupPath(portId, grpId)+"/ana_state",
			AnaStateOf(grpId)); err != nil {
			return err
		}
	}
	return nil
}

// ProbePort reports whether the port matches the desired transport and
// carries the three fixed groups with their fixed states.
func (n *Nvmet) ProbePort(
	ctx context.Context,
	portId int,
	conf PortConf,
) (bool, string, error) {
	state, details, err := n.probePort(ctx, portId, conf, n.readAttr)
	return state == PortOk, details, err
}

// PortState is what ProbePortState found of a port.
type PortState int

const (
	// PortOk is a port that matches: its directory, the desired transport
	// attributes, and the three fixed ANA groups in their fixed states.
	PortOk PortState = iota + 1
	// PortAbsent is a port whose directory, or the directory of one of its
	// fixed ANA groups, is not there: what EnsurePort creates.
	PortAbsent
	// PortAttrMismatch is a port that is there with a transport attribute
	// other than the desired one; the probe does not read its groups. nvmet
	// refuses every addr_* write while a subsystem is linked to the port
	// (EACCES), and EnsurePort stops at the write that fails, before the
	// groups.
	PortAttrMismatch
	// PortAnaStateMismatch is a port whose transport attributes all match
	// and one of whose fixed ANA groups is not in its fixed state. nvmet
	// takes an ana_state write whatever is linked to the port, and
	// EnsurePort rewrites a differing state once the attributes match.
	PortAnaStateMismatch
)

// ProbePortState is ProbePort for a caller that must tell a port that is
// absent from one that does not match — and a transport attribute that
// differs from a group state that does — and all of them from a probe that
// did not answer (cnagent.md CN28): every read goes through the strict probe
// (SH15), so err is set whenever a listing or a read did not answer, and
// PortAbsent is reported only on an answer that the port's directory or a
// group's is not there. ProbePort reads the attributes loosely instead, so
// there a group whose state read failed for any reason reads missing. The
// state is meaningless when err is set.
func (n *Nvmet) ProbePortState(
	ctx context.Context,
	portId int,
	conf PortConf,
) (PortState, string, error) {
	return n.probePort(ctx, portId, conf, n.readAttrStrict)
}

func (n *Nvmet) probePort(
	ctx context.Context,
	portId int,
	conf PortConf,
	read func(context.Context, string) (string, bool, error),
) (PortState, string, error) {
	portPath := n.PortPath(portId)
	exists, err := n.dirExists(ctx, portPath)
	if err != nil {
		return 0, "", err
	}
	if !exists {
		return PortAbsent, "port directory missing", nil
	}
	for _, attr := range n.portAttrs(conf) {
		if attr[1] == "" {
			continue
		}
		cur, ok, err := read(ctx, portPath+"/"+attr[0])
		if err != nil {
			return 0, "", err
		}
		// An attribute is never absent from a port directory that exists;
		// one that reads absent went with its port between the listing
		// and the read, and the next probe finds the port missing.
		if !ok || cur != strings.TrimSpace(attr[1]) {
			return PortAttrMismatch, fmt.Sprintf(
				"%s is %q, want %q", attr[0], cur, attr[1]), nil
		}
	}
	for _, grpId := range fixedAnaGrpIds {
		cur, ok, err := read(
			ctx, n.AnaGroupPath(portId, grpId)+"/ana_state")
		if err != nil {
			return 0, "", err
		}
		if !ok {
			return PortAbsent, fmt.Sprintf("ana group %d missing", grpId),
				nil
		}
		if cur != AnaStateOf(grpId) {
			return PortAnaStateMismatch, fmt.Sprintf(
				"ana group %d is %q, want %q",
				grpId, cur, AnaStateOf(grpId)), nil
		}
	}
	return PortOk, "", nil
}

// ---------------------------------------------------------------------------
// Subsystem
// ---------------------------------------------------------------------------

// SubsysConf is one nvmet subsystem. Serial, Model and the cntlid range are
// left alone while empty or zero: the DN↔DN migration-source subsystem carries
// no host-facing identity. AllowedHosts is the exact set of hosts the
// subsystem admits, host-facing or dnv-internal alike, and an empty list
// admits no host: allowed_hosts is the only admission gate, and dnv never
// sets "attr_allow_any_host" (architecture.md, Primary cntlr, step 6).
type SubsysConf struct {
	Nqn          string
	Serial       string
	Model        string
	CntlidMin    uint32
	CntlidMax    uint32
	AllowedHosts []string
}

// attrs lists the attributes ProbeSubsystem compares and EnsureSubsystem's
// attribute loop writes, in the loop's order. "attr_allow_any_host" is always
// "0" and comes first, so a later attribute that nvmet refuses, which stops
// the loop, cannot keep a subsystem found open from being closed; the probe's
// comparison of it is what reports such a subsystem as not converged.
func (c SubsysConf) attrs() [][2]string {
	attrs := [][2]string{{"attr_allow_any_host", "0"}}
	if c.Serial != "" {
		attrs = append(attrs, [2]string{"attr_serial", c.Serial})
	}
	if c.Model != "" {
		attrs = append(attrs, [2]string{"attr_model", c.Model})
	}
	if c.CntlidMin != 0 || c.CntlidMax != 0 {
		attrs = append(attrs,
			[2]string{"attr_cntlid_min",
				strconv.FormatUint(uint64(c.CntlidMin), 10)},
			[2]string{"attr_cntlid_max",
				strconv.FormatUint(uint64(c.CntlidMax), 10)})
	}
	return attrs
}

func (n *Nvmet) SubsysExists(
	ctx context.Context,
	nqn string,
) (bool, error) {
	return n.dirExists(ctx, n.SubsysPath(nqn))
}

// EnsureSubsystem converges one subsystem and its allowed-hosts links.
func (n *Nvmet) EnsureSubsystem(
	ctx context.Context,
	conf SubsysConf,
) error {
	subsysPath := n.SubsysPath(conf.Nqn)
	exists, err := n.dirExists(ctx, subsysPath)
	if err != nil {
		return err
	}
	if !exists {
		if err := n.mkdir(ctx, subsysPath); err != nil {
			return err
		}
	}
	// nvmet refuses an `attr_cntlid_min` above the subsystem's current
	// `attr_cntlid_max`, and an `attr_cntlid_max` below its current
	// `attr_cntlid_min` (-EINVAL either way). attrs() lists min before max,
	// which moves a range down, or up while the new min still falls inside
	// the old range; a range that lies wholly above the live one, as when a
	// subsystem is adopted from a lower cntlid slot (architecture.md,
	// cntlid slots), has its max written here first, or its min would be
	// refused on every converge.
	if err := n.raiseCntlidMax(ctx, subsysPath, conf); err != nil {
		return err
	}
	// The attribute loop writes "attr_allow_any_host" = 0 before the host
	// links: nvmet refuses a new host link while the attribute is 1 (-EINVAL),
	// and the same write closes a subsystem found open, whoever opened it.
	// Nothing here writes 1 (SubsysConf).
	for _, attr := range conf.attrs() {
		if _, err := n.ensureAttr(
			ctx, subsysPath+"/"+attr[0], attr[1]); err != nil {
			return err
		}
	}
	linked, _, err := n.listDir(ctx, subsysPath+"/allowed_hosts")
	if err != nil {
		return err
	}
	for _, hostNqn := range conf.AllowedHosts {
		if contains(linked, hostNqn) {
			continue
		}
		hostPath := n.HostPath(hostNqn)
		hostExists, err := n.dirExists(ctx, hostPath)
		if err != nil {
			return err
		}
		if !hostExists {
			if err := n.mkdir(ctx, hostPath); err != nil {
				return err
			}
		}
		if err := n.symlink(ctx, hostPath,
			subsysPath+"/allowed_hosts/"+hostNqn); err != nil {
			return err
		}
	}
	for _, hostNqn := range linked {
		if !contains(conf.AllowedHosts, hostNqn) {
			if err := n.unlink(
				ctx, subsysPath+"/allowed_hosts/"+hostNqn); err != nil {
				return err
			}
		}
	}
	return nil
}

// raiseCntlidMax writes conf's attr_cntlid_max ahead of EnsureSubsystem's
// attribute loop when conf's whole range lies above the one the live
// subsystem holds, the one move nvmet refuses min first. The loop then writes
// the min and finds the max already equal. A max that cannot be read, or does
// not parse, leaves the order to the loop: its min write is then refused or
// not on its own, and a refusal is the converge's error.
func (n *Nvmet) raiseCntlidMax(
	ctx context.Context,
	subsysPath string,
	conf SubsysConf,
) error {
	if conf.CntlidMin == 0 && conf.CntlidMax == 0 {
		return nil
	}
	maxPath := subsysPath + "/attr_cntlid_max"
	cur, ok, err := n.readAttr(ctx, maxPath)
	if err != nil || !ok {
		return err
	}
	curMax, err := strconv.ParseUint(cur, 10, 32)
	if err != nil || uint64(conf.CntlidMin) <= curMax {
		return nil
	}
	return n.writeAttr(ctx, maxPath,
		strconv.FormatUint(uint64(conf.CntlidMax), 10))
}

// ProbeSubsystem reports whether the subsystem exists with the desired
// attributes and allowed hosts. The attributes are attrs(), so an
// "attr_allow_any_host" that reads 1 is a mismatch: a subsystem found open is
// not converged, whatever its host links.
func (n *Nvmet) ProbeSubsystem(
	ctx context.Context,
	conf SubsysConf,
) (bool, string, error) {
	subsysPath := n.SubsysPath(conf.Nqn)
	exists, err := n.dirExists(ctx, subsysPath)
	if err != nil {
		return false, "", err
	}
	if !exists {
		return false, "subsystem missing", nil
	}
	for _, attr := range conf.attrs() {
		cur, ok, err := n.readAttr(ctx, subsysPath+"/"+attr[0])
		if err != nil {
			return false, "", err
		}
		if !ok || cur != strings.TrimSpace(attr[1]) {
			return false, fmt.Sprintf(
				"%s is %q, want %q", attr[0], cur, attr[1]), nil
		}
	}
	linked, _, err := n.listDir(ctx, subsysPath+"/allowed_hosts")
	if err != nil {
		return false, "", err
	}
	for _, hostNqn := range conf.AllowedHosts {
		if !contains(linked, hostNqn) {
			return false, "allowed host " + hostNqn + " missing", nil
		}
	}
	// The set must match exactly: a link the converge failed to remove, or
	// one added out of band, still grants a retired host access, and the
	// probe is all the worker sees between converges.
	for _, hostNqn := range linked {
		if !contains(conf.AllowedHosts, hostNqn) {
			return false, "unexpected allowed host " + hostNqn, nil
		}
	}
	return true, "", nil
}

// ---------------------------------------------------------------------------
// Namespace
// ---------------------------------------------------------------------------

// NsConf is one namespace of a subsystem. Uuid/Nguid are empty for the
// DN↔DN migration-source export and set for every side export, where both
// sides of a migrating leg MUST present the same identity (architecture.md,
// Disk node).
type NsConf struct {
	Nqn        string
	Nsid       int
	DevicePath string
	Uuid       string
	Nguid      string
	AnaGrpId   int
}

// NsState is the probed live state of a namespace.
type NsState struct {
	Exists     bool
	Enabled    bool
	DevicePath string
	Uuid       string
	Nguid      string
	AnaGrpId   int
}

func (n *Nvmet) ProbeNamespace(
	ctx context.Context,
	nqn string,
	nsid int,
) (*NsState, error) {
	nsPath := n.NsPath(nqn, nsid)
	exists, err := n.dirExists(ctx, nsPath)
	if err != nil {
		return nil, err
	}
	state := &NsState{Exists: exists}
	if !exists {
		return state, nil
	}
	for _, field := range []struct {
		name string
		dst  *string
	}{
		{"device_path", &state.DevicePath},
		{"device_uuid", &state.Uuid},
		{"device_nguid", &state.Nguid},
	} {
		value, _, err := n.readAttr(ctx, nsPath+"/"+field.name)
		if err != nil {
			return nil, err
		}
		*field.dst = value
	}
	enable, _, err := n.readAttr(ctx, nsPath+"/enable")
	if err != nil {
		return nil, err
	}
	state.Enabled = enable == "1"
	grpId, _, err := n.readAttr(ctx, nsPath+"/ana_grpid")
	if err != nil {
		return nil, err
	}
	state.AnaGrpId, _ = strconv.Atoi(grpId)
	return state, nil
}

// SameNsId compares an nvmet namespace identity attribute as the kernel
// stores it, not as dnv wrote it. Both device_uuid and device_nguid accept a
// bare 32-hex-digit string or a dash-separated one on write, and both always
// read back dash-separated *and* lower-cased (the kernel prints them with
// %pUb). DnNsIdentity supplies the uuid dashed and the nguid bare, so a
// byte comparison reports a permanent difference on the nguid: every converge
// then disabled the namespace, rewrote the attribute and re-enabled it — an
// SH16 idempotency break that momentarily drops a live export's namespace on
// every Check round — and every probe reported the healthy namespace as
// RES_STATUS_ERROR.
//
// Both roles compare identities through this one function: the dn's side
// exports and the cn's host-facing namespaces hit the same kernel behaviour.
func SameNsId(a, b string) bool {
	return strings.EqualFold(
		strings.ReplaceAll(a, "-", ""), strings.ReplaceAll(b, "-", ""))
}

// EnsureNamespace converges one namespace. On a live namespace only the
// ana_grpid is rewritten (the cheap, non-disruptive transition of [D4]);
// identity or backing-device changes — which never happen with deterministic
// names — require the disable/rewrite/enable cycle.
func (n *Nvmet) EnsureNamespace(
	ctx context.Context,
	conf NsConf,
) error {
	nsPath := n.NsPath(conf.Nqn, conf.Nsid)
	state, err := n.ProbeNamespace(ctx, conf.Nqn, conf.Nsid)
	if err != nil {
		return err
	}
	if !state.Exists {
		if err := n.mkdir(ctx, nsPath); err != nil {
			return err
		}
		state = &NsState{Exists: true}
	}
	identityOk := state.DevicePath == conf.DevicePath &&
		(conf.Uuid == "" || SameNsId(state.Uuid, conf.Uuid)) &&
		(conf.Nguid == "" || SameNsId(state.Nguid, conf.Nguid))
	if state.Enabled && !identityOk {
		if err := n.writeAttr(ctx, nsPath+"/enable", "0"); err != nil {
			return err
		}
		state.Enabled = false
	}
	if !state.Enabled {
		if conf.DevicePath != "" {
			if _, err := n.ensureAttr(
				ctx, nsPath+"/device_path", conf.DevicePath); err != nil {
				return err
			}
		}
		// The identity attributes are compared canonically here too, for the
		// same reason identityOk is: ensureAttr's byte comparison would write
		// the nguid on every pass (see SameNsId).
		for _, attr := range [][3]string{
			{"device_uuid", conf.Uuid, state.Uuid},
			{"device_nguid", conf.Nguid, state.Nguid},
		} {
			if attr[1] == "" || SameNsId(attr[2], attr[1]) {
				continue
			}
			if err := n.writeAttr(
				ctx, nsPath+"/"+attr[0], attr[1]); err != nil {
				return err
			}
		}
	}
	if state.AnaGrpId != conf.AnaGrpId {
		if err := n.SetNsAnaGrpId(
			ctx, conf.Nqn, conf.Nsid, conf.AnaGrpId); err != nil {
			return err
		}
	}
	if !state.Enabled {
		if err := n.writeAttr(ctx, nsPath+"/enable", "1"); err != nil {
			return err
		}
	}
	return nil
}

// ListNamespaces returns the nsids currently present under a subsystem; ok is
// false when the subsystem does not exist. It completes the SH19 lifecycle
// family: a subsystem that outlives one of its namespaces (a CN namespace
// leaving `ns_list`) needs the stale one removed without tearing the
// subsystem down.
func (n *Nvmet) ListNamespaces(
	ctx context.Context,
	nqn string,
) ([]int, bool, error) {
	entries, ok, err := n.listDir(ctx, n.SubsysPath(nqn)+"/namespaces")
	if err != nil || !ok {
		return nil, ok, err
	}
	var nsids []int
	for _, entry := range entries {
		nsid, convErr := strconv.Atoi(entry)
		if convErr != nil {
			continue
		}
		nsids = append(nsids, nsid)
	}
	return nsids, true, nil
}

// ListSubsystems enumerates every subsystem in the target's configfs tree,
// linked to a port or not: a partially removed subsystem has already lost its
// port link and is exactly what a sweep must still find. An absent
// /sys/kernel/config/nvmet/subsystems is "none" (the module may not be
// loaded); a listing that did not answer is an error.
func (n *Nvmet) ListSubsystems(ctx context.Context) ([]string, error) {
	entries, _, err := n.listDir(ctx, NvmetRoot+"/subsystems")
	return entries, err
}

// ListPortSubsystems is the subsystems currently linked to one port.
func (n *Nvmet) ListPortSubsystems(
	ctx context.Context,
	portId int,
) ([]string, error) {
	linked, _, err := n.listDir(ctx, n.PortPath(portId)+"/subsystems")
	return linked, err
}

// ListPorts enumerates the port ids present in the target's configfs tree.
// A sweep on a node running several dn agents needs it to tell a subsystem
// that is linked to ITS port from one linked to a sibling agent's.
func (n *Nvmet) ListPorts(ctx context.Context) ([]int, error) {
	entries, _, err := n.listDir(ctx, NvmetRoot+"/ports")
	if err != nil {
		return nil, err
	}
	var out []int
	for _, entry := range entries {
		portId, convErr := strconv.Atoi(entry)
		if convErr != nil {
			continue
		}
		out = append(out, portId)
	}
	return out, nil
}

// SubsysMtime is the mtime of one subsystem's configfs directory, the age a
// sweep judges an unattributable export by (dnagent.md DN6); ok is false when
// the subsystem is gone. On the lab's 7.0 kernel the mtime is set at `mkdir`
// and moved to "now" by every lookup of one of the subsystem's OWN attribute
// files — a read, a write, even a stat of `attr_*`: configfs instantiates an
// attribute's inode on each lookup and, in that kernel, stamps the parent
// directory when it does. Adding an allowed-host link or a namespace directory
// under it does not move it, and neither does listing it. So it reads "time
// since the subsystem was created or an attribute of it was last touched":
// a build in flight writes the attributes and reads young, and an export
// nobody touches ages. A kernel that stamps the directory only when a
// directory or a link is created under it (7.3 moved configfs's stamp there)
// reads the mkdir time instead, the plain age of the subsystem.
func (n *Nvmet) SubsysMtime(
	ctx context.Context,
	nqn string,
) (time.Time, bool, error) {
	return n.dirMtime(ctx, n.SubsysPath(nqn))
}

// NsDevicePath reads one namespace's backing device path. It is how a sweep
// attributes a host-facing subsystem, whose NQN is chosen by the user and
// carries no ids of ours ([D15]) — so it reads through the strict probe: an
// unreadable device_path must be an error, never an absence that would make
// the subsystem look unowned and get it removed.
func (n *Nvmet) NsDevicePath(
	ctx context.Context,
	nqn string,
	nsid int,
) (string, bool, error) {
	return n.readAttrStrict(ctx, n.NsPath(nqn, nsid)+"/device_path")
}

// NsAnaGrpId reads one namespace's ANA group. It is how a destination pass
// whose step stopped reads whether a per-CN stack serves through the dm-clone
// (dnagent.md DN13), so it reads through the strict probe: ok is false only
// when the attribute is not there, and a read that did not answer, or a value
// that is not a number, is an error, never a group. Taken as a group that is
// not optimized, such a read would reload the leg's only serving path onto
// its dm-error.
func (n *Nvmet) NsAnaGrpId(
	ctx context.Context,
	nqn string,
	nsid int,
) (int, bool, error) {
	path := n.NsPath(nqn, nsid) + "/ana_grpid"
	value, ok, err := n.readAttrStrict(ctx, path)
	if err != nil || !ok {
		return 0, false, err
	}
	grpId, err := strconv.Atoi(value)
	if err != nil {
		return 0, false, fmt.Errorf("read %s: %q is not a group id",
			path, value)
	}
	return grpId, true, nil
}

// RemoveNamespace disables and removes one namespace, leaving its subsystem in
// place (reverse build order, SH19). Absent objects are skipped.
func (n *Nvmet) RemoveNamespace(
	ctx context.Context,
	nqn string,
	nsid int,
) error {
	nsPath := n.NsPath(nqn, nsid)
	exists, err := n.dirExists(ctx, nsPath)
	if err != nil || !exists {
		return err
	}
	// Strict: a read that did not answer must not make this skip the
	// `enable = 0` write and rmdir a namespace the kernel still has enabled.
	enable, ok, err := n.readAttrStrict(ctx, nsPath+"/enable")
	if err != nil {
		return err
	}
	if ok && enable != "0" {
		if err := n.writeAttr(ctx, nsPath+"/enable", "0"); err != nil {
			return err
		}
	}
	return n.rmdir(ctx, nsPath)
}

// NamespaceGone verifies one namespace's removal. `rmdir` may have been
// killed after configfs had already dropped the directory, so the removal's
// exit status is not evidence and this read is.
func (n *Nvmet) NamespaceGone(
	ctx context.Context,
	nqn string,
	nsid int,
) (bool, error) {
	exists, err := n.dirExists(ctx, n.NsPath(nqn, nsid))
	return !exists, err
}

// SetNsAnaGrpId performs one ANA transition: a single attribute write, valid
// on a live namespace because the target group always exists ([D4]).
func (n *Nvmet) SetNsAnaGrpId(
	ctx context.Context,
	nqn string,
	nsid int,
	grpId int,
) error {
	return n.writeAttr(ctx,
		n.NsPath(nqn, nsid)+"/ana_grpid", strconv.Itoa(grpId))
}

// ---------------------------------------------------------------------------
// Port links and teardown
// ---------------------------------------------------------------------------

// PortLinked reports whether a subsystem is attached to the port. The probe
// matters: re-linking a live port↔subsystem link stalls host IO (SH16).
func (n *Nvmet) PortLinked(
	ctx context.Context,
	portId int,
	nqn string,
) (bool, error) {
	linked, _, err := n.listDir(ctx, n.PortPath(portId)+"/subsystems")
	if err != nil {
		return false, err
	}
	return contains(linked, nqn), nil
}

func (n *Nvmet) EnsurePortLink(
	ctx context.Context,
	portId int,
	nqn string,
) error {
	linked, err := n.PortLinked(ctx, portId, nqn)
	if err != nil {
		return err
	}
	if linked {
		return nil
	}
	return n.symlink(ctx, n.SubsysPath(nqn), n.portLinkPath(portId, nqn))
}

func (n *Nvmet) RemovePortLink(
	ctx context.Context,
	portId int,
	nqn string,
) error {
	linked, err := n.PortLinked(ctx, portId, nqn)
	if err != nil {
		return err
	}
	if !linked {
		return nil
	}
	return n.unlink(ctx, n.portLinkPath(portId, nqn))
}

// RemoveSubsystem tears one subsystem down in reverse build order (SH19):
// port link, namespace disable + rmdir, allowed-hosts unlink, subsystem
// rmdir. Absent objects are skipped, so a re-run after a crash is a no-op.
func (n *Nvmet) RemoveSubsystem(
	ctx context.Context,
	portId int,
	nqn string,
) error {
	if err := n.RemovePortLink(ctx, portId, nqn); err != nil {
		return err
	}
	subsysPath := n.SubsysPath(nqn)
	exists, err := n.dirExists(ctx, subsysPath)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	namespaces, _, err := n.listDir(ctx, subsysPath+"/namespaces")
	if err != nil {
		return err
	}
	for _, ns := range namespaces {
		nsPath := subsysPath + "/namespaces/" + ns
		enable, ok, err := n.readAttrStrict(ctx, nsPath+"/enable")
		if err != nil {
			return err
		}
		if ok && enable != "0" {
			if err := n.writeAttr(ctx, nsPath+"/enable", "0"); err != nil {
				return err
			}
		}
		if err := n.rmdir(ctx, nsPath); err != nil {
			return err
		}
	}
	hosts, _, err := n.listDir(ctx, subsysPath+"/allowed_hosts")
	if err != nil {
		return err
	}
	for _, hostNqn := range hosts {
		if err := n.unlink(
			ctx, subsysPath+"/allowed_hosts/"+hostNqn); err != nil {
			return err
		}
	}
	return n.rmdir(ctx, subsysPath)
}
