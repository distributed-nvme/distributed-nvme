package agent

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// ---------------------------------------------------------------------------
// Subsystem admission (architecture.md, Primary cntlr, step 6)
//
// The host links are the only admission gate of every subsystem dnv builds,
// host-facing or dnv-internal: an empty AllowedHosts admits no host, and
// "attr_allow_any_host" is only ever written 0, ahead of the host links.
// ---------------------------------------------------------------------------

const (
	admissionNqn   = "nqn.2024-01.io.dnv-it:s:vol1"
	admissionHostA = "nqn.2024-01.io.dnv-it:host:a"
	admissionHostB = "nqn.2024-01.io.dnv-it:host:b"
)

// fakeConfigfs is the part of the nvmet configfs tree EnsureSubsystem and
// ProbeSubsystem touch, with nvmet's two allow-any-host refusals: a host link
// is refused while the subsystem's "attr_allow_any_host" is 1, and a 1 is
// refused while a host is linked, both EINVAL. A new subsystem directory comes
// with its allowed_hosts directory and the attribute at 0, as nvmet makes it.
// changes is every write, mkdir, link and unlink, in order. readErr answers a
// read of one path with an error that is not fs.ErrNotExist: a read that did
// not answer, or an attribute that could not be read, as opposed to one that
// is not there.
type fakeConfigfs struct {
	dirs    map[string]bool
	files   map[string]string
	links   map[string]string
	readErr map[string]error
	changes []string
	// discovered makes every "attr_serial" and "attr_model" write fail
	// EINVAL, as nvmet does once a host has identified the subsystem.
	discovered bool
	// unwritable makes every write of one path fail EIO: a write that did
	// not go through, as when the soft timeout cuts it off.
	unwritable string
	// lsFail makes the `ls` of one path not answer, as a killed one does.
	lsFail map[string]bool
}

func newFakeConfigfs() *fakeConfigfs {
	return &fakeConfigfs{
		dirs: map[string]bool{
			NvmetRoot + "/subsystems": true,
			NvmetRoot + "/hosts":      true,
		},
		files:   map[string]string{},
		links:   map[string]string{},
		readErr: map[string]error{},
		lsFail:  map[string]bool{},
	}
}

func (c *fakeConfigfs) mkdir(path string) {
	c.dirs[path] = true
	if path[:strings.LastIndex(path, "/")] == NvmetRoot+"/subsystems" {
		c.dirs[path+"/allowed_hosts"] = true
		c.files[path+"/attr_allow_any_host"] = "0"
	}
}

// openSubsys plants a subsystem that admits every host: the attribute at 1,
// and so no host link, which nvmet refuses beside it.
func (c *fakeConfigfs) openSubsys(nqn string) {
	path := NvmetRoot + "/subsystems/" + nqn
	c.mkdir(path)
	c.files[path+"/attr_allow_any_host"] = "1"
}

func (c *fakeConfigfs) hostLinks(subsysPath string) []string {
	var out []string
	for link := range c.links {
		if strings.HasPrefix(link, subsysPath+"/allowed_hosts/") {
			out = append(out, link[strings.LastIndex(link, "/")+1:])
		}
	}
	sort.Strings(out)
	return out
}

func (c *fakeConfigfs) children(dir string) []string {
	seen := map[string]bool{}
	for _, set := range []map[string]bool{c.dirs, keysOf(c.files),
		keysOf(c.links)} {
		for path := range set {
			if strings.HasPrefix(path, dir+"/") &&
				!strings.Contains(path[len(dir)+1:], "/") {
				seen[path[len(dir)+1:]] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func keysOf(m map[string]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for key := range m {
		out[key] = true
	}
	return out
}

func (c *fakeConfigfs) osClient() *common.FakeOsClient {
	return &common.FakeOsClient{
		RunCommandFn: func(
			ctx context.Context, cmd string, args []string, stdin string,
		) (string, string, int, error) {
			path := args[len(args)-1]
			switch cmd {
			case "ls":
				if c.lsFail[path] {
					return "", "", -1, errors.New("signal: killed")
				}
				if !c.dirs[path] {
					return "", "", 2, errors.New("exit status 2")
				}
				return strings.Join(c.children(path), "\n") + "\n", "", 0,
					nil
			case "mkdir":
				c.changes = append(c.changes, "mkdir "+path)
				c.mkdir(path)
				return "", "", 0, nil
			case "ln":
				c.changes = append(c.changes, "link "+path)
				if strings.HasPrefix(path, NvmetRoot+"/ports/") {
					c.links[path] = args[len(args)-2]
					return "", "", 0, nil
				}
				subsysPath := path[:strings.LastIndex(path, "/allowed_hosts/")]
				if c.files[subsysPath+"/attr_allow_any_host"] == "1" {
					return "", "ln: Invalid argument", 1,
						errors.New("exit status 1")
				}
				c.links[path] = args[len(args)-2]
				return "", "", 0, nil
			case "rm":
				c.changes = append(c.changes, "unlink "+path)
				delete(c.links, path)
				return "", "", 0, nil
			}
			return "", "", 127, fmt.Errorf("unexpected command %q", cmd)
		},
		ReadMtimeFn: func(ctx context.Context, path string) (time.Time, error) {
			if _, link := c.links[path]; c.dirs[path] || link {
				return time.Unix(1, 0), nil
			}
			return time.Time{}, &iofs.PathError{Op: "lstat", Path: path,
				Err: iofs.ErrNotExist}
		},
		ReadFileFn: func(ctx context.Context, path string) (string, error) {
			if err, ok := c.readErr[path]; ok {
				return "", err
			}
			data, ok := c.files[path]
			if !ok {
				return "", fmt.Errorf("no such file: %s: %w", path,
					iofs.ErrNotExist)
			}
			return data, nil
		},
		WriteFileDirectFn: func(
			ctx context.Context, path string, data string,
		) error {
			c.changes = append(c.changes, "write "+path+"="+data)
			subsysPath := path[:strings.LastIndex(path, "/")]
			if !c.dirs[subsysPath] {
				return &iofs.PathError{Op: "write", Path: path,
					Err: iofs.ErrNotExist}
			}
			if strings.HasSuffix(path, "/attr_allow_any_host") &&
				strings.TrimSpace(data) == "1" &&
				len(c.hostLinks(subsysPath)) > 0 {
				return &iofs.PathError{Op: "write", Path: path,
					Err: syscall.EINVAL}
			}
			if path == c.unwritable {
				return &iofs.PathError{Op: "write", Path: path,
					Err: syscall.EIO}
			}
			if c.discovered && (strings.HasSuffix(path, "/attr_serial") ||
				strings.HasSuffix(path, "/attr_model")) {
				return &iofs.PathError{Op: "write", Path: path,
					Err: syscall.EINVAL}
			}
			c.files[path] = data
			return nil
		},
	}
}

// admissionConfs is a subsystem in the two shapes dnv builds: with a
// host-facing identity (a cn subsystem, a transfer, a side export) and with
// none (a migration-source export). Admission is the same rule for both.
func admissionConfs(hosts ...string) map[string]SubsysConf {
	return map[string]SubsysConf{
		"identity": {Nqn: admissionNqn, Serial: "000000000000000a",
			Model: "dnv", CntlidMin: 1, CntlidMax: 8, AllowedHosts: hosts},
		"no identity": {Nqn: admissionNqn, AllowedHosts: hosts},
	}
}

// assertAdmits checks the subsystem is closed and links exactly want, never
// had a 1 written, and probes converged.
func assertAdmits(
	t *testing.T,
	cfs *fakeConfigfs,
	nvmet *Nvmet,
	conf SubsysConf,
	want ...string,
) {
	t.Helper()
	subsysPath := nvmet.SubsysPath(conf.Nqn)
	if got := cfs.files[subsysPath+"/attr_allow_any_host"]; got != "0" {
		t.Fatalf("attr_allow_any_host is %q, want 0", got)
	}
	if got := cfs.hostLinks(subsysPath); strings.Join(got, ",") !=
		strings.Join(want, ",") {
		t.Fatalf("host links %v, want %v", got, want)
	}
	for _, change := range cfs.changes {
		if strings.HasSuffix(change, "/attr_allow_any_host=1") {
			t.Fatalf("%s was written: dnv never opens a subsystem", change)
		}
	}
	ok, details, err := nvmet.ProbeSubsystem(context.Background(), conf)
	if err != nil || !ok {
		t.Fatalf("ProbeSubsystem = %v, %q, %v; want converged",
			ok, details, err)
	}
}

// TestEnsureSubsystemEmptyListAdmitsNoHost: an empty list builds a subsystem
// no host can reach — the attribute stays at nvmet's 0 and nothing is linked —
// and that subsystem probes converged.
func TestEnsureSubsystemEmptyListAdmitsNoHost(t *testing.T) {
	for name, conf := range admissionConfs() {
		t.Run(name, func(t *testing.T) {
			cfs := newFakeConfigfs()
			nvmet := NewNvmet(cfs.osClient())
			if err := nvmet.EnsureSubsystem(
				context.Background(), conf); err != nil {
				t.Fatalf("EnsureSubsystem: %v", err)
			}
			assertAdmits(t, cfs, nvmet, conf)
		})
	}
}

// TestEnsureSubsystemClosesAnOpenSubsystemFirst: a subsystem found admitting
// every host probes as not converged, and the converge closes it whatever the
// list. With a host to admit, the fake refuses that host's link while the
// attribute is still 1, so a converge that linked before writing the 0 fails
// here as it fails on nvmet.
func TestEnsureSubsystemClosesAnOpenSubsystemFirst(t *testing.T) {
	for _, hosts := range [][]string{nil, {admissionHostA}} {
		for name, conf := range admissionConfs(hosts...) {
			t.Run(fmt.Sprintf("%s, %d hosts", name, len(hosts)),
				func(t *testing.T) {
					cfs := newFakeConfigfs()
					cfs.openSubsys(conf.Nqn)
					nvmet := NewNvmet(cfs.osClient())
					ok, details, err := nvmet.ProbeSubsystem(
						context.Background(), conf)
					if err != nil || ok ||
						!strings.Contains(details, "attr_allow_any_host") {
						t.Fatalf("the open subsystem probes %v, %q, %v; "+
							"want not converged on attr_allow_any_host",
							ok, details, err)
					}
					if err := nvmet.EnsureSubsystem(
						context.Background(), conf); err != nil {
						t.Fatalf("EnsureSubsystem: %v", err)
					}
					assertAdmits(t, cfs, nvmet, conf, hosts...)
				})
		}
	}
}

// TestEnsureSubsystemClosesBeforeAnythingThatCanStopIt: the close of a
// subsystem found open is the first thing EnsureSubsystem writes, so a
// converge stopped by a later step still leaves that subsystem closed. Two
// such stops are planted here: a failed write of the cntlid max raised for a
// subsystem adopted from a lower slot, and an identity attribute nvmet
// refuses once a host has identified the subsystem.
func TestEnsureSubsystemClosesBeforeAnythingThatCanStopIt(t *testing.T) {
	for name, stop := range map[string]func(
		cfs *fakeConfigfs, subsysPath string, conf *SubsysConf,
	){
		"the cntlid max write fails": func(
			cfs *fakeConfigfs, subsysPath string, conf *SubsysConf,
		) {
			// A range wholly below the wanted one, so the max is raised
			// ahead of the attribute loop (raiseCntlidMax).
			conf.CntlidMin, conf.CntlidMax = 100, 108
			cfs.files[subsysPath+"/attr_cntlid_min"] = "1"
			cfs.files[subsysPath+"/attr_cntlid_max"] = "8"
			cfs.unwritable = subsysPath + "/attr_cntlid_max"
		},
		"nvmet refuses the serial": func(
			cfs *fakeConfigfs, subsysPath string, conf *SubsysConf,
		) {
			cfs.files[subsysPath+"/attr_serial"] = "another serial"
			cfs.discovered = true
		},
	} {
		t.Run(name, func(t *testing.T) {
			conf := admissionConfs(admissionHostA)["identity"]
			cfs := newFakeConfigfs()
			cfs.openSubsys(conf.Nqn)
			nvmet := NewNvmet(cfs.osClient())
			subsysPath := nvmet.SubsysPath(conf.Nqn)
			stop(cfs, subsysPath, &conf)
			if err := nvmet.EnsureSubsystem(
				context.Background(), conf); err == nil {
				t.Fatal("fixture is wrong: the converge was not stopped")
			}
			closed := "write " + subsysPath + "/attr_allow_any_host=0"
			if len(cfs.changes) == 0 || cfs.changes[0] != closed {
				t.Errorf("changes = %v, want %q first", cfs.changes, closed)
			}
			if got := cfs.files[subsysPath+"/attr_allow_any_host"]; got !=
				"0" {
				t.Errorf("the stopped converge left attr_allow_any_host "+
					"at %q, want 0", got)
			}
		})
	}
}

// TestEnsureSubsystemEmptyingTheListRevokesEveryHost: emptying the list
// unlinks every host and leaves the attribute untouched at 0, so revoking
// every host is a list like any other. Until that converge runs, the probe
// reports a host still linked, since the empty list admits none.
func TestEnsureSubsystemEmptyingTheListRevokesEveryHost(t *testing.T) {
	granted := admissionConfs(admissionHostA, admissionHostB)
	for name, conf := range admissionConfs() {
		t.Run(name, func(t *testing.T) {
			cfs := newFakeConfigfs()
			nvmet := NewNvmet(cfs.osClient())
			if err := nvmet.EnsureSubsystem(
				context.Background(), granted[name]); err != nil {
				t.Fatalf("grant: %v", err)
			}
			assertAdmits(t, cfs, nvmet, granted[name],
				admissionHostA, admissionHostB)

			ok, details, err := nvmet.ProbeSubsystem(
				context.Background(), conf)
			if err != nil || ok ||
				!strings.Contains(details, "unexpected allowed host") {
				t.Fatalf("the unrevoked subsystem probes %v, %q, %v; "+
					"want not converged on a linked host", ok, details, err)
			}

			cfs.changes = nil
			if err := nvmet.EnsureSubsystem(
				context.Background(), conf); err != nil {
				t.Fatalf("revoke: %v", err)
			}
			assertAdmits(t, cfs, nvmet, conf)
			for _, change := range cfs.changes {
				if strings.HasSuffix(change, "/attr_allow_any_host=0") {
					t.Fatalf("%s: the revoke rewrote a closed attribute",
						change)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Namespace reads
// ---------------------------------------------------------------------------

// TestNsAnaGrpIdIsStrict pins the read by which a destination pass whose step
// stopped decides whether a per-CN stack serves through its dm-clone
// (dnagent.md DN13). A namespace with no ana_grpid is an answer: it is in no
// group. A read that did not answer, or could not be read, and a value that is
// not a number must be errors: taken as a group that is not optimized, they
// would reload the leg's only serving path onto its dm-error.
func TestNsAnaGrpIdIsStrict(t *testing.T) {
	ctx := context.Background()
	cfs := newFakeConfigfs()
	nvmet := NewNvmet(cfs.osClient())
	// The fixture is filled through the same NsPath the production code
	// builds, so a change to the configfs layout cannot leave this test
	// passing against paths nobody reads.
	grp := func(nsid int) string {
		return nvmet.NsPath(admissionNqn, nsid) + "/ana_grpid"
	}
	cfs.files[grp(1)] = fmt.Sprintf("%d\n", common.AnaGrpIdNonOptimized)
	// nsid 2 stalled, nsid 3 could not be read at all, and nsid 4 holds no
	// number.
	cfs.readErr[grp(2)] = context.DeadlineExceeded
	cfs.readErr[grp(3)] = errors.New("input/output error")
	cfs.files[grp(4)] = "optimized\n"

	grpId, ok, err := nvmet.NsAnaGrpId(ctx, admissionNqn, 1)
	if err != nil || !ok || grpId != common.AnaGrpIdNonOptimized {
		t.Fatalf("a readable ana_grpid gave (%d, %v, %v), want (%d, true, "+
			"nil)", grpId, ok, err, common.AnaGrpIdNonOptimized)
	}

	// Absent: the namespace is not there, so it serves through nothing.
	if grpId, ok, err = nvmet.NsAnaGrpId(ctx, admissionNqn, 5); err != nil ||
		ok {
		t.Fatalf("an absent ana_grpid gave (%d, %v, %v), want no group and "+
			"no error", grpId, ok, err)
	}

	for _, nsid := range []int{2, 3, 4} {
		if grpId, ok, err = nvmet.NsAnaGrpId(
			ctx, admissionNqn, nsid); err == nil {
			t.Fatalf("nsid %d: gave (%d, %v, nil), want an error: no group "+
				"was read", nsid, grpId, ok)
		}
	}
}

// ---------------------------------------------------------------------------
// The IO queue cap (dnagent.md SH32)
// ---------------------------------------------------------------------------

// TestEnsurePortLinkCapsQueuesBeforeTheLink: the cap is written,
// probe-first, before a subsystem's port link, and only while the subsystem
// is linked to no port at all, since nvmet deletes every controller of a
// subsystem whose cap is written; a zero cap writes none; a read that did not
// answer writes nothing and makes no link, and so does a cap that could not
// be written.
func TestEnsurePortLinkCapsQueuesBeforeTheLink(t *testing.T) {
	const portId, otherPort = 3, 5
	for _, tc := range []struct {
		name   string
		qidMax int
		// cur is the cap as it reads; "" is a cap that is not there.
		cur string
		// linkedHere and linkedThere link the subsystem to this agent's
		// port and to another agent's.
		linkedHere, linkedThere bool
		// hideHere makes this port's listing read absent while its link
		// is there.
		hideHere   bool
		unwritable bool
		capReadErr bool
		lsFail     string
		want       []string
		wantErr    bool
	}{
		{name: "unlinked, nvmet's default", qidMax: 8, cur: "128",
			want: []string{"write {s}/attr_qid_max=8", "link {l}"}},
		{name: "unlinked, the cap already", qidMax: 8, cur: "8",
			want: []string{"link {l}"}},
		{name: "unlinked, no cap attribute", qidMax: 8, cur: "",
			want: []string{"write {s}/attr_qid_max=8", "link {l}"}},
		{name: "unlinked, a zero cap", qidMax: 0, cur: "128",
			want: []string{"link {l}"}},
		{name: "linked here, another cap", qidMax: 8, cur: "128",
			linkedHere: true, want: nil},
		{name: "linked to another agent's port", qidMax: 8, cur: "128",
			linkedThere: true, want: []string{"link {l}"}},
		{name: "this port's listing misses a link that is there",
			qidMax: 8, cur: "128", linkedHere: true, hideHere: true,
			want: []string{"link {l}"}},
		{name: "the write fails", qidMax: 8, cur: "128", unwritable: true,
			want: []string{"write {s}/attr_qid_max=8"}, wantErr: true},
		{name: "the cap read does not answer", qidMax: 8, cur: "128",
			capReadErr: true, want: nil, wantErr: true},
		{name: "this port's listing does not answer", qidMax: 8,
			cur: "128", lsFail: "here", want: nil, wantErr: true},
		{name: "the ports listing does not answer", qidMax: 8,
			cur: "128", lsFail: "ports", want: nil, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfs := newFakeConfigfs()
			nvmet := NewNvmet(cfs.osClient())
			subsysPath := nvmet.SubsysPath(admissionNqn)
			linkPath := nvmet.PortPath(portId) + "/subsystems/" + admissionNqn
			cfs.dirs[NvmetRoot+"/ports"] = true
			for _, id := range []int{portId, otherPort} {
				cfs.dirs[nvmet.PortPath(id)] = true
				cfs.dirs[nvmet.PortPath(id)+"/subsystems"] = true
			}
			cfs.mkdir(subsysPath)
			if tc.cur != "" {
				cfs.files[subsysPath+"/attr_qid_max"] = tc.cur
			}
			if tc.linkedHere {
				cfs.links[linkPath] = subsysPath
			}
			if tc.hideHere {
				cfs.dirs[nvmet.PortPath(portId)+"/subsystems"] = false
			}
			if tc.linkedThere {
				cfs.links[nvmet.PortPath(otherPort)+"/subsystems/"+
					admissionNqn] = subsysPath
			}
			if tc.unwritable {
				cfs.unwritable = subsysPath + "/attr_qid_max"
			}
			if tc.capReadErr {
				cfs.readErr[subsysPath+"/attr_qid_max"] = syscall.EIO
			}
			switch tc.lsFail {
			case "here":
				cfs.lsFail[nvmet.PortPath(portId)+"/subsystems"] = true
			case "ports":
				cfs.lsFail[NvmetRoot+"/ports"] = true
			}
			err := nvmet.EnsurePortLink(context.Background(), portId,
				admissionNqn, tc.qidMax)
			if (err != nil) != tc.wantErr {
				t.Fatalf("EnsurePortLink = %v, want an error: %v", err,
					tc.wantErr)
			}
			var want []string
			for _, w := range tc.want {
				w = strings.ReplaceAll(w, "{s}", subsysPath)
				want = append(want, strings.ReplaceAll(w, "{l}", linkPath))
			}
			if strings.Join(cfs.changes, "\n") != strings.Join(want, "\n") {
				t.Fatalf("changes:\n%s\nwant:\n%s",
					strings.Join(cfs.changes, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}
