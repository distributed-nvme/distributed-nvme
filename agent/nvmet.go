package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"

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

// Nvmet wraps the nvmet configfs patterns of Appendix A. Attribute writes go
// through OsClient.WriteFileDirect, directory and link operations through
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
// Port — the one port per node, with the three fixed ANA groups (SH19, [D4])
// ---------------------------------------------------------------------------

// PortConf is the node's single nvmet port, built from the --tr-* flags.
type PortConf struct {
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
	portPath := n.PortPath(portId)
	exists, err := n.dirExists(ctx, portPath)
	if err != nil {
		return false, "", err
	}
	if !exists {
		return false, "port directory missing", nil
	}
	for _, attr := range n.portAttrs(conf) {
		if attr[1] == "" {
			continue
		}
		cur, ok, err := n.readAttr(ctx, portPath+"/"+attr[0])
		if err != nil {
			return false, "", err
		}
		if !ok || cur != strings.TrimSpace(attr[1]) {
			return false, fmt.Sprintf(
				"%s is %q, want %q", attr[0], cur, attr[1]), nil
		}
	}
	for _, grpId := range fixedAnaGrpIds {
		cur, ok, err := n.readAttr(
			ctx, n.AnaGroupPath(portId, grpId)+"/ana_state")
		if err != nil {
			return false, "", err
		}
		if !ok {
			return false, fmt.Sprintf("ana group %d missing", grpId), nil
		}
		if cur != AnaStateOf(grpId) {
			return false, fmt.Sprintf(
				"ana group %d is %q, want %q",
				grpId, cur, AnaStateOf(grpId)), nil
		}
	}
	return true, "", nil
}

// ---------------------------------------------------------------------------
// Subsystem
// ---------------------------------------------------------------------------

// SubsysConf is one nvmet subsystem. Empty/zero fields are left alone: the
// DN↔DN migration-source subsystem carries no host-facing identity.
type SubsysConf struct {
	Nqn          string
	Serial       string
	Model        string
	CntlidMin    uint32
	CntlidMax    uint32
	AllowedHosts []string
}

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

// ProbeSubsystem reports whether the subsystem exists with the desired
// attributes and allowed hosts.
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
	return true, "", nil
}

// ---------------------------------------------------------------------------
// Namespace
// ---------------------------------------------------------------------------

// NsConf is one namespace of a subsystem. Uuid/Nguid are empty for the
// DN↔DN migration-source export and set for every side export, where both
// sides of a migrating leg MUST present the same identity (§3.1).
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
		(conf.Uuid == "" || strings.EqualFold(state.Uuid, conf.Uuid)) &&
		(conf.Nguid == "" || strings.EqualFold(state.Nguid, conf.Nguid))
	if state.Enabled && !identityOk {
		if err := n.writeAttr(ctx, nsPath+"/enable", "0"); err != nil {
			return err
		}
		state.Enabled = false
	}
	if !state.Enabled {
		for _, attr := range [][2]string{
			{"device_path", conf.DevicePath},
			{"device_uuid", conf.Uuid},
			{"device_nguid", conf.Nguid},
		} {
			if attr[1] == "" {
				continue
			}
			if _, err := n.ensureAttr(
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
		enable, ok, err := n.readAttr(ctx, nsPath+"/enable")
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
