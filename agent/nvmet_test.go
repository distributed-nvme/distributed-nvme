package agent

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"slices"
	"sort"
	"strconv"
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
	// takenButFails makes every write of one path store its data and then
	// fail: a write the kernel took before the soft timeout cut it off.
	takenButFails string
	// readFailsAfterWrite makes every read of one path fail EIO once a
	// write of it was tried.
	readFailsAfterWrite string
	// listErr answers a listing of one path with an error that is not
	// fs.ErrNotExist: context.DeadlineExceeded for one the soft timeout cut
	// off, an I/O error for one that failed. Only a directory the tree does
	// not hold lists as missing.
	listErr map[string]error
	// listed is every path listed, in order, so a test whose listing fails
	// can check that the call under test reached it.
	listed []string
	// connStays holds the nvmet connections, by their debug directory, that
	// a write of "fatal" to their "state" leaves in place: a connection
	// whose end the kernel has not finished. Every other one goes at the
	// write, its directory with it.
	connStays map[string]bool
	// stateRefusal refuses a write of an nvmet connection's "state" with
	// EPERM, as a kernel under lockdown refuses to open the file.
	stateRefusal bool
	// removing holds files of nvmet connections whose next read or write
	// answers EIO, as debugfs answers through a file the kernel removed
	// after its open; the connection's directory goes with that answer, and
	// reuse, by the same directory, names the host whose new connection
	// takes the freed name at once.
	removing map[string]bool
	reuse    map[string]string
}

func newFakeConfigfs() *fakeConfigfs {
	return &fakeConfigfs{
		dirs: map[string]bool{
			NvmetRoot + "/subsystems": true,
			NvmetRoot + "/hosts":      true,
		},
		files:     map[string]string{},
		links:     map[string]string{},
		readErr:   map[string]error{},
		listErr:   map[string]error{},
		connStays: map[string]bool{},
		removing:  map[string]bool{},
		reuse:     map[string]string{},
	}
}

// removed answers EIO for a file of a connection whose removal began after
// its open (removing), and drops the connection, or gives its name to the
// connection reuse names.
func (c *fakeConfigfs) removed(path string) error {
	if !c.removing[path] {
		return nil
	}
	delete(c.removing, path)
	dir := path[:strings.LastIndex(path, "/")]
	c.rmdir(dir)
	if host, ok := c.reuse[dir]; ok {
		delete(c.reuse, dir)
		c.dirs[dir] = true
		c.files[dir+"/hostnqn"] = host
		c.files[dir+"/state"] = "ready\n"
	}
	return &iofs.PathError{Op: "read", Path: path, Err: syscall.EIO}
}

// nvmetConn plants one nvmet connection of the subsystem nqn in the debug
// tree, as the kernel shows it: the directory ctrl<cntlid>, its "hostnqn"
// with no newline at its end, and its "state".
func (c *fakeConfigfs) nvmetConn(nqn string, cntlid int, hostNqn string) {
	dir := fmt.Sprintf("%s/%s/ctrl%d", NvmetDebugRoot, nqn, cntlid)
	c.dirs[NvmetDebugRoot+"/"+nqn] = true
	c.dirs[dir] = true
	c.files[dir+"/hostnqn"] = hostNqn
	c.files[dir+"/state"] = "ready\n"
}

// writeState models a write to an nvmet connection's "state", as nvmet's
// debug handler and debugfs take it: a file that is not there fails with
// ENOENT, a refusal with EPERM, anything that does not start with "fatal" in
// fewer than 16 bytes with EINVAL; "fatal" ends the connection, whose
// directory goes at once unless connStays holds it.
func (c *fakeConfigfs) writeState(path, data string) error {
	if err := c.removed(path); err != nil {
		return err
	}
	if _, ok := c.files[path]; !ok {
		return &iofs.PathError{Op: "open", Path: path, Err: syscall.ENOENT}
	}
	if c.stateRefusal {
		return &iofs.PathError{Op: "open", Path: path, Err: syscall.EPERM}
	}
	if len(data) >= 16 || !strings.HasPrefix(data, "fatal") {
		return &iofs.PathError{Op: "write", Path: path, Err: syscall.EINVAL}
	}
	dir := path[:strings.LastIndex(path, "/")]
	if c.connStays[dir] {
		c.files[path] = "fatal\n"
		return nil
	}
	c.rmdir(dir)
	return nil
}

func (c *fakeConfigfs) mkdir(path string) {
	c.dirs[path] = true
	if path[:strings.LastIndex(path, "/")] == NvmetRoot+"/subsystems" {
		c.dirs[path+"/allowed_hosts"] = true
		c.files[path+"/attr_allow_any_host"] = "0"
	}
}

// rmdir drops a directory and everything under it, as configfs drops a
// group's attributes and default groups with it.
func (c *fakeConfigfs) rmdir(path string) {
	under := func(p string) bool {
		return p == path || strings.HasPrefix(p, path+"/")
	}
	for p := range c.dirs {
		if under(p) {
			delete(c.dirs, p)
		}
	}
	for p := range c.files {
		if under(p) {
			delete(c.files, p)
		}
	}
	for p := range c.links {
		if under(p) {
			delete(c.links, p)
		}
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
			case "rmdir":
				c.changes = append(c.changes, "rmdir "+path)
				c.rmdir(path)
				return "", "", 0, nil
			}
			return "", "", 127, fmt.Errorf("unexpected command %q", cmd)
		},
		ReadDirFn: func(ctx context.Context, path string) ([]string, error) {
			c.listed = append(c.listed, path)
			if err, ok := c.listErr[path]; ok {
				return nil, err
			}
			if !c.dirs[path] {
				return nil, &iofs.PathError{Op: "open", Path: path,
					Err: syscall.ENOENT}
			}
			return c.children(path), nil
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
			if err := c.removed(path); err != nil {
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
			if strings.HasPrefix(path, NvmetDebugRoot+"/") {
				return c.writeState(path, data)
			}
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
			if path == c.readFailsAfterWrite {
				c.readErr[path] = syscall.EIO
			}
			if path == c.takenButFails {
				c.files[path] = data
				return &iofs.PathError{Op: "write", Path: path,
					Err: context.DeadlineExceeded}
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

// TestEnsureSubsystemNamesAFrozenSerial: a serial write nvmet refuses, as it
// does once a host has identified the subsystem, over a live serial other
// than the wanted one is a *SerialFrozenError naming both serials
// (cnagent.md CN16, A subsystem made again); a live serial that matches is
// not written at all, a refused model write over a matching serial is a
// plain error, and so is a serial write the kernel took but that reported a
// failure, the live serial then the wanted one, and a refusal whose strict
// read of the live serial fails.
func TestEnsureSubsystemNamesAFrozenSerial(t *testing.T) {
	for _, tc := range []struct {
		name      string
		serial    string
		model     string
		taken     bool
		readFails bool
		frozen    bool
		ok        bool
	}{
		{"another serial", "000000000000000b", "dnv", false, false, true,
			false},
		{"the same serial", "000000000000000a", "dnv", false, false, false,
			true},
		{"the same serial, another model", "000000000000000a", "other",
			false, false, false, false},
		{"a serial write taken but failed", "000000000000000b", "dnv",
			true, false, false, false},
		{"the serial's read after the refusal fails", "000000000000000b",
			"dnv", false, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conf := admissionConfs(admissionHostA)["identity"]
			cfs := newFakeConfigfs()
			cfs.openSubsys(conf.Nqn)
			nvmet := NewNvmet(cfs.osClient())
			subsysPath := nvmet.SubsysPath(conf.Nqn)
			cfs.files[subsysPath+"/attr_serial"] = tc.serial + "\n"
			cfs.files[subsysPath+"/attr_model"] = tc.model
			cfs.discovered = !tc.taken
			if tc.taken {
				cfs.takenButFails = subsysPath + "/attr_serial"
			}
			if tc.readFails {
				cfs.readFailsAfterWrite = subsysPath + "/attr_serial"
			}
			err := nvmet.EnsureSubsystem(context.Background(), conf)
			var frozen *SerialFrozenError
			if got := errors.As(err, &frozen); got != tc.frozen {
				t.Fatalf("EnsureSubsystem = %v, a frozen serial %v, want %v",
					err, got, tc.frozen)
			}
			if (err == nil) != tc.ok {
				t.Fatalf("EnsureSubsystem = %v, want success %v", err, tc.ok)
			}
			if tc.frozen && (frozen.Live != tc.serial ||
				frozen.Want != conf.Serial || frozen.Nqn != conf.Nqn ||
				!errors.Is(err, syscall.EINVAL)) {
				t.Fatalf("the refusal is %+v, want live %q, wanted %q, "+
					"wrapping EINVAL", frozen, tc.serial, conf.Serial)
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
		listFail   string
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
			cur: "128", listFail: "here", want: nil, wantErr: true},
		{name: "the ports listing does not answer", qidMax: 8,
			cur: "128", listFail: "ports", want: nil, wantErr: true},
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
			switch tc.listFail {
			case "here":
				cfs.listErr[nvmet.PortPath(portId)+"/subsystems"] =
					context.DeadlineExceeded
			case "ports":
				cfs.listErr[NvmetRoot+"/ports"] = context.DeadlineExceeded
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

// ---------------------------------------------------------------------------
// A configfs listing that fails is an error, never "absent" (dnagent.md SH15,
// SH18)
// ---------------------------------------------------------------------------

const (
	listingPort = 3
	// listingUnlinkedNqn is a subsystem linked to no port, whose queue cap
	// differs, so its port link first asks whether any port links it.
	listingUnlinkedNqn = "nqn.2024-01.io.dnv-it:s:vol2"
)

var listingPortConf = PortConf{PortId: listingPort, TrType: "tcp",
	AdrFam: "ipv4", TrAddr: "192.0.2.1", TrSvcId: "4420"}

// listingTree is a configfs tree that every Nvmet method of the listing
// table reads as it is: the port with its attributes and its three groups,
// one subsystem linked to it with one host and namespace 1, and one subsystem
// linked to no port.
func listingTree() (*fakeConfigfs, *Nvmet) {
	cfs := newFakeConfigfs()
	n := NewNvmet(cfs.osClient())
	port := n.PortPath(listingPort)
	for _, dir := range []string{NvmetRoot + "/ports", port,
		port + "/subsystems", port + "/ana_groups"} {
		cfs.dirs[dir] = true
	}
	for _, attr := range n.portAttrs(listingPortConf) {
		cfs.files[port+"/"+attr[0]] = attr[1]
	}
	for _, grpId := range fixedAnaGrpIds {
		cfs.dirs[n.AnaGroupPath(listingPort, grpId)] = true
		cfs.files[n.AnaGroupPath(listingPort, grpId)+"/ana_state"] =
			AnaStateOf(grpId)
	}
	subsys := n.SubsysPath(admissionNqn)
	cfs.mkdir(subsys)
	cfs.dirs[subsys+"/namespaces"] = true
	ns := n.NsPath(admissionNqn, 1)
	cfs.dirs[ns] = true
	for attr, value := range map[string]string{
		"enable": "1", "device_path": "/dev/mapper/dnv-x",
		"device_uuid": "", "device_nguid": "",
		"ana_grpid": strconv.Itoa(common.AnaGrpIdOptimized),
	} {
		cfs.files[ns+"/"+attr] = value
	}
	cfs.dirs[n.HostPath(admissionHostA)] = true
	cfs.links[subsys+"/allowed_hosts/"+admissionHostA] =
		n.HostPath(admissionHostA)
	cfs.links[n.portLinkPath(listingPort, admissionNqn)] = subsys
	cfs.mkdir(n.SubsysPath(listingUnlinkedNqn))
	cfs.files[n.SubsysPath(listingUnlinkedNqn)+"/attr_qid_max"] = "128"
	return cfs, n
}

// listingCalls is every Nvmet method that lists the nvmet configfs tree,
// each with the one listing it is to meet failed, and the call made on
// listingTree. Every call answers with no error when nothing fails.
func listingCalls() []struct {
	name string
	// path is the listing that fails, built from the same paths the
	// production code builds.
	path func(n *Nvmet) string
	// setup changes listingTree for a call that reaches the listing only
	// on another shape.
	setup func(cfs *fakeConfigfs, n *Nvmet)
	call  func(n *Nvmet) error
} {
	ctx := context.Background()
	port := func(n *Nvmet) string { return n.PortPath(listingPort) }
	portLinks := func(n *Nvmet) string {
		return n.PortPath(listingPort) + "/subsystems"
	}
	ports := func(*Nvmet) string { return NvmetRoot + "/ports" }
	subsystems := func(*Nvmet) string { return NvmetRoot + "/subsystems" }
	subsys := func(n *Nvmet) string { return n.SubsysPath(admissionNqn) }
	hosts := func(n *Nvmet) string { return subsys(n) + "/allowed_hosts" }
	namespaces := func(n *Nvmet) string { return subsys(n) + "/namespaces" }
	ns := func(n *Nvmet) string { return n.NsPath(admissionNqn, 1) }
	withHosts := func(hosts ...string) SubsysConf {
		return SubsysConf{Nqn: admissionNqn, AllowedHosts: hosts}
	}
	nsConf := NsConf{Nqn: admissionNqn, Nsid: 1,
		DevicePath: "/dev/mapper/dnv-x", AnaGrpId: common.AnaGrpIdOptimized}
	return []struct {
		name  string
		path  func(n *Nvmet) string
		setup func(cfs *fakeConfigfs, n *Nvmet)
		call  func(n *Nvmet) error
	}{
		{name: "EnsurePort, the port", path: port,
			call: func(n *Nvmet) error {
				return n.EnsurePort(ctx, listingPort, listingPortConf)
			}},
		{name: "EnsurePort, an ANA group",
			path: func(n *Nvmet) string {
				return n.AnaGroupPath(listingPort, common.AnaGrpIdInaccessible)
			},
			call: func(n *Nvmet) error {
				return n.EnsurePort(ctx, listingPort, listingPortConf)
			}},
		{name: "ProbePortState, the port's links", path: portLinks,
			// The links are listed only for a transport attribute that
			// differs.
			setup: func(cfs *fakeConfigfs, n *Nvmet) {
				cfs.files[n.PortPath(listingPort)+"/addr_traddr"] =
					"192.0.2.2"
			},
			call: func(n *Nvmet) error {
				state, _, err := n.ProbePortState(
					ctx, listingPort, listingPortConf)
				if err == nil && state != PortAttrMismatch {
					return fmt.Errorf("state %d, want PortAttrMismatch",
						state)
				}
				return err
			}},
		{name: "ProbePort, the port's links", path: portLinks,
			setup: func(cfs *fakeConfigfs, n *Nvmet) {
				cfs.files[n.PortPath(listingPort)+"/addr_traddr"] =
					"192.0.2.2"
			},
			call: func(n *Nvmet) error {
				_, _, err := n.ProbePort(ctx, listingPort, listingPortConf)
				return err
			}},
		{name: "SubsysExists", path: subsys,
			call: func(n *Nvmet) error {
				_, err := n.SubsysExists(ctx, admissionNqn)
				return err
			}},
		{name: "EnsureSubsystem, the subsystem", path: subsys,
			call: func(n *Nvmet) error {
				return n.EnsureSubsystem(ctx, withHosts(admissionHostA))
			}},
		{name: "EnsureSubsystem, its allowed hosts", path: hosts,
			// The access hole: with no host wanted, a listing read as an
			// empty one unlinks no host and answers converged.
			call: func(n *Nvmet) error {
				return n.EnsureSubsystem(ctx, withHosts())
			}},
		{name: "EnsureSubsystem, a host to link",
			path: func(n *Nvmet) string { return n.HostPath(admissionHostB) },
			call: func(n *Nvmet) error {
				return n.EnsureSubsystem(
					ctx, withHosts(admissionHostA, admissionHostB))
			}},
		{name: "ProbeSubsystem, the subsystem", path: subsys,
			call: func(n *Nvmet) error {
				ok, details, err := n.ProbeSubsystem(
					ctx, withHosts(admissionHostA))
				if err == nil && !ok {
					return fmt.Errorf("not converged: %s", details)
				}
				return err
			}},
		{name: "ProbeSubsystem, its allowed hosts", path: hosts,
			// The other half of the access hole: the probe of a subsystem
			// that must admit no host while one is linked.
			setup: func(cfs *fakeConfigfs, n *Nvmet) {
				delete(cfs.links, n.SubsysPath(admissionNqn)+
					"/allowed_hosts/"+admissionHostA)
			},
			call: func(n *Nvmet) error {
				ok, details, err := n.ProbeSubsystem(ctx, withHosts())
				if err == nil && !ok {
					return fmt.Errorf("not converged: %s", details)
				}
				return err
			}},
		{name: "ProbeNamespace", path: ns,
			call: func(n *Nvmet) error {
				_, err := n.ProbeNamespace(ctx, admissionNqn, 1)
				return err
			}},
		{name: "EnsureNamespace", path: ns,
			call: func(n *Nvmet) error {
				return n.EnsureNamespace(ctx, nsConf)
			}},
		{name: "ListNamespaces", path: namespaces,
			call: func(n *Nvmet) error {
				_, _, err := n.ListNamespaces(ctx, admissionNqn)
				return err
			}},
		{name: "ListSubsystems", path: subsystems,
			call: func(n *Nvmet) error {
				_, err := n.ListSubsystems(ctx)
				return err
			}},
		{name: "ListPortSubsystems", path: portLinks,
			call: func(n *Nvmet) error {
				_, err := n.ListPortSubsystems(ctx, listingPort)
				return err
			}},
		{name: "ListPorts", path: ports,
			call: func(n *Nvmet) error {
				_, err := n.ListPorts(ctx)
				return err
			}},
		{name: "RemoveNamespace", path: ns,
			call: func(n *Nvmet) error {
				return n.RemoveNamespace(ctx, admissionNqn, 1)
			}},
		{name: "NamespaceGone", path: ns,
			call: func(n *Nvmet) error {
				_, err := n.NamespaceGone(ctx, admissionNqn, 1)
				return err
			}},
		{name: "PortLinked", path: portLinks,
			call: func(n *Nvmet) error {
				_, err := n.PortLinked(ctx, listingPort, admissionNqn)
				return err
			}},
		{name: "PortLinkedState", path: portLinks,
			call: func(n *Nvmet) error {
				_, _, err := n.PortLinkedState(ctx, listingPort, admissionNqn)
				return err
			}},
		{name: "EnsurePortLink, the port's links", path: portLinks,
			call: func(n *Nvmet) error {
				return n.EnsurePortLink(ctx, listingPort, admissionNqn, 0)
			}},
		{name: "EnsurePortLink, the ports for the queue cap", path: ports,
			call: func(n *Nvmet) error {
				return n.EnsurePortLink(
					ctx, listingPort, listingUnlinkedNqn, 8)
			}},
		{name: "RemovePortLink", path: portLinks,
			call: func(n *Nvmet) error {
				return n.RemovePortLink(ctx, listingPort, admissionNqn)
			}},
		{name: "RemoveSubsystem, the subsystem", path: subsys,
			call: func(n *Nvmet) error {
				return n.RemoveSubsystem(ctx, listingPort, admissionNqn)
			}},
		{name: "RemoveSubsystem, its namespaces", path: namespaces,
			call: func(n *Nvmet) error {
				return n.RemoveSubsystem(ctx, listingPort, admissionNqn)
			}},
		{name: "RemoveSubsystem, its allowed hosts", path: hosts,
			call: func(n *Nvmet) error {
				return n.RemoveSubsystem(ctx, listingPort, admissionNqn)
			}},
	}
}

// TestAFailedConfigfsListingIsAnError pins that every Nvmet method that lists
// the nvmet configfs tree answers a listing that failed with an error, never
// with an absence (dnagent.md SH15, SH18): a listing read as an empty or
// missing directory makes RemoveSubsystem succeed having done nothing, a
// sweep see no export, and, with a failed allowed_hosts listing,
// EnsureSubsystem unlink no retired host while ProbeSubsystem reads it
// converged. Each call first runs on a tree whose listings all answer, and
// must answer with no error; then with its listing failing with EIO, and with
// it cut off at the soft timeout: both are errors, and the EIO one names the
// listing.
func TestAFailedConfigfsListingIsAnError(t *testing.T) {
	for _, tc := range listingCalls() {
		t.Run(tc.name, func(t *testing.T) {
			for _, failure := range []struct {
				name string
				err  func(path string) error
			}{
				{"none", nil},
				{"EIO", func(path string) error {
					return &iofs.PathError{Op: "readdirent", Path: path,
						Err: syscall.EIO}
				}},
				{"killed", func(string) error {
					return context.DeadlineExceeded
				}},
			} {
				cfs, n := listingTree()
				if tc.setup != nil {
					tc.setup(cfs, n)
				}
				path := tc.path(n)
				if failure.err != nil {
					cfs.listErr[path] = failure.err(path)
				}
				err := tc.call(n)
				if !slices.Contains(cfs.listed, path) {
					t.Fatalf("%s: %s was never listed (listings %v)",
						failure.name, path, cfs.listed)
				}
				switch {
				case failure.err == nil:
					if err != nil {
						t.Fatalf("fixture is wrong: with every listing "+
							"answering: %v", err)
					}
				case err == nil:
					t.Fatalf("%s: a listing of %s that failed gave no "+
						"error (changes %v)", failure.name, path,
						cfs.changes)
				case failure.name == "EIO" &&
					(!errors.Is(err, syscall.EIO) ||
						!strings.Contains(err.Error(), "list "+path)):
					t.Errorf("%s: the error %q does not name the failed "+
						"listing of %s", failure.name, err, path)
				}
			}
		})
	}
}

// TestAMissingConfigfsDirectoryIsAbsent is the other half: a directory the
// tree does not hold is an answer, not an error, as on a node whose nvmet
// module was never loaded or whose object is already gone. A method that
// turned it into an error would name a leftover on every pass over a clean
// node. Only linkedToAnyPort's missing ports directory is an error, since its
// caller has made its own port, and its words are no failed listing's.
func TestAMissingConfigfsDirectoryIsAbsent(t *testing.T) {
	ctx := context.Background()
	cfs := newFakeConfigfs()
	delete(cfs.dirs, NvmetRoot+"/subsystems")
	n := NewNvmet(cfs.osClient())
	for _, tc := range []struct {
		name string
		// absent reports whether the method read the object as not there.
		absent func() (bool, error)
	}{
		{"ListSubsystems", func() (bool, error) {
			got, err := n.ListSubsystems(ctx)
			return len(got) == 0, err
		}},
		{"ListPorts", func() (bool, error) {
			got, err := n.ListPorts(ctx)
			return len(got) == 0, err
		}},
		{"ListPortSubsystems", func() (bool, error) {
			got, err := n.ListPortSubsystems(ctx, listingPort)
			return len(got) == 0, err
		}},
		{"SubsysExists", func() (bool, error) {
			exists, err := n.SubsysExists(ctx, admissionNqn)
			return !exists, err
		}},
		{"ListNamespaces", func() (bool, error) {
			_, ok, err := n.ListNamespaces(ctx, admissionNqn)
			return !ok, err
		}},
		{"NamespaceGone", func() (bool, error) {
			return n.NamespaceGone(ctx, admissionNqn, 1)
		}},
		{"ProbeNamespace", func() (bool, error) {
			state, err := n.ProbeNamespace(ctx, admissionNqn, 1)
			return state != nil && !state.Exists, err
		}},
		{"ProbeSubsystem", func() (bool, error) {
			ok, details, err := n.ProbeSubsystem(ctx,
				SubsysConf{Nqn: admissionNqn})
			return !ok && details == "subsystem missing", err
		}},
		{"PortLinkedState", func() (bool, error) {
			linked, portThere, err := n.PortLinkedState(
				ctx, listingPort, admissionNqn)
			return !linked && !portThere, err
		}},
		{"RemoveNamespace", func() (bool, error) {
			err := n.RemoveNamespace(ctx, admissionNqn, 1)
			return len(cfs.changes) == 0, err
		}},
		{"RemoveSubsystem", func() (bool, error) {
			err := n.RemoveSubsystem(ctx, listingPort, admissionNqn)
			return len(cfs.changes) == 0, err
		}},
	} {
		absent, err := tc.absent()
		if err != nil {
			t.Errorf("%s: a missing directory gave the error %v", tc.name,
				err)
		} else if !absent {
			t.Errorf("%s: a missing directory did not read as absent",
				tc.name)
		}
	}

	cfs.dirs[NvmetRoot+"/subsystems"] = true
	cfs.mkdir(n.SubsysPath(admissionNqn))
	err := n.EnsurePortLink(ctx, listingPort, admissionNqn, 8)
	if err == nil {
		t.Fatal("a queue cap with no ports directory gave no error")
	}
	if strings.HasPrefix(err.Error(), "list ") {
		t.Errorf("the missing ports directory reads as a failed "+
			"listing: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Nvmet connections (cnagent.md CN33)
// ---------------------------------------------------------------------------

// TestListNvmetConnsReadsEachConnectionsHost: a subsystem's nvmet connections
// are its debug directory's "ctrl" entries, each with the host NQN its
// "hostnqn" names; another entry is no connection, a connection whose
// "hostnqn" went before its read has gone, a subsystem with no debug
// directory reads as absent, and a listing or a read that fails is an
// error, never an absence.
func TestListNvmetConnsReadsEachConnectionsHost(t *testing.T) {
	ctx := context.Background()
	cfs := newFakeConfigfs()
	n := NewNvmet(cfs.osClient())
	conns, ok, err := n.ListNvmetConns(ctx, admissionNqn)
	if err != nil || ok || conns != nil {
		t.Fatalf("no debug directory: ListNvmetConns = %v, %v, %v; want "+
			"absent", conns, ok, err)
	}
	cfs.nvmetConn(admissionNqn, 1, admissionHostA)
	cfs.nvmetConn(admissionNqn, 2, admissionHostB)
	debugDir := n.SubsysDebugPath(admissionNqn)
	// A connection whose directory still shows while its hostnqn is gone,
	// as between the listing and the read of a connection that ends.
	cfs.dirs[debugDir+"/ctrl3"] = true
	cfs.files[debugDir+"/notes"] = "x"
	conns, ok, err = n.ListNvmetConns(ctx, admissionNqn)
	want := []NvmetConn{
		{Name: "ctrl1", HostNqn: admissionHostA},
		{Name: "ctrl2", HostNqn: admissionHostB},
	}
	if err != nil || !ok || !slices.Equal(conns, want) {
		t.Fatalf("ListNvmetConns = %v, %v, %v; want %v", conns, ok, err,
			want)
	}

	cfs.readErr[debugDir+"/ctrl2/hostnqn"] = errors.New("input/output error")
	if conns, _, err := n.ListNvmetConns(ctx, admissionNqn); err == nil {
		t.Errorf("a hostnqn read that failed gave %v and no error", conns)
	}
	delete(cfs.readErr, debugDir+"/ctrl2/hostnqn")
	cfs.listErr[debugDir] = context.DeadlineExceeded
	if conns, ok, err := n.ListNvmetConns(ctx, admissionNqn); err == nil ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a listing that failed gave %v, %v, %v; want its error",
			conns, ok, err)
	}
}

// TestEndNvmetConnWritesFatal: ending an nvmet connection reads its host
// again and writes "fatal" to its "state", which the kernel takes as the end
// of the connection, and says it wrote. A directory that is gone, at the
// read or at the write (EIO from a file removed after its open), and one
// that now names another host, a new connection under the freed name, are
// left alone with no error and no write to the new one; a refused write, as
// under lockdown, is an error.
func TestEndNvmetConnWritesFatal(t *testing.T) {
	ctx := context.Background()
	cfs := newFakeConfigfs()
	n := NewNvmet(cfs.osClient())
	debugDir := n.SubsysDebugPath(admissionNqn)
	connA := NvmetConn{Name: "ctrl1", HostNqn: admissionHostA}
	cfs.nvmetConn(admissionNqn, 1, admissionHostA)
	wrote, err := n.EndNvmetConn(ctx, admissionNqn, connA)
	if err != nil || !wrote {
		t.Fatalf("EndNvmetConn = %v, %v; want a write", wrote, err)
	}
	if want := []string{"write " + debugDir + "/ctrl1/state=fatal"}; !slices.Equal(
		cfs.changes, want) {
		t.Fatalf("changes %v, want %v", cfs.changes, want)
	}
	if conns, _, err := n.ListNvmetConns(ctx, admissionNqn); err != nil ||
		len(conns) != 0 {
		t.Fatalf("after the end: %v, %v; want no connection", conns, err)
	}
	if wrote, err := n.EndNvmetConn(ctx, admissionNqn, connA); err != nil ||
		wrote {
		t.Errorf("a connection that has gone: EndNvmetConn = %v, %v; want "+
			"no write and no error", wrote, err)
	}

	cfs.changes = nil
	cfs.nvmetConn(admissionNqn, 1, admissionHostB)
	if wrote, err := n.EndNvmetConn(ctx, admissionNqn, connA); err != nil ||
		wrote || len(cfs.changes) != 0 {
		t.Fatalf("a name another host took: EndNvmetConn = %v, %v, changes "+
			"%v; want the new connection left alone", wrote, err, cfs.changes)
	}

	connB := NvmetConn{Name: "ctrl2", HostNqn: admissionHostA}
	cfs.nvmetConn(admissionNqn, 2, admissionHostA)
	cfs.removing[debugDir+"/ctrl2/state"] = true
	if wrote, err := n.EndNvmetConn(ctx, admissionNqn, connB); err != nil ||
		wrote {
		t.Errorf("a state removed after its open: EndNvmetConn = %v, %v; "+
			"want a connection that has gone", wrote, err)
	}

	for _, reuse := range []bool{false, true} {
		// A host file removed after its open at the write's own read: gone,
		// or a name another host's new connection took at once.
		cfs.changes = nil
		cfs.nvmetConn(admissionNqn, 4, admissionHostA)
		cfs.removing[debugDir+"/ctrl4/hostnqn"] = true
		if reuse {
			cfs.reuse[debugDir+"/ctrl4"] = admissionHostB
		}
		wrote, err := n.EndNvmetConn(ctx, admissionNqn,
			NvmetConn{Name: "ctrl4", HostNqn: admissionHostA})
		if err != nil || wrote || len(cfs.changes) != 0 {
			t.Errorf("reuse %v: a host file removed after its open: "+
				"EndNvmetConn = %v, %v, changes %v; want no write", reuse,
				wrote, err, cfs.changes)
		}
	}
	cfs.nvmetConn(admissionNqn, 5, admissionHostA)
	hostFile := debugDir + "/ctrl5/hostnqn"
	cfs.readErr[hostFile] = &iofs.PathError{Op: "open", Path: hostFile,
		Err: syscall.EACCES}
	_, err = n.EndNvmetConn(ctx, admissionNqn,
		NvmetConn{Name: "ctrl5", HostNqn: admissionHostA})
	var readErr *NvmetConnReadError
	if !errors.As(err, &readErr) {
		t.Errorf("a host read that failed: EndNvmetConn = %v, want an "+
			"NvmetConnReadError", err)
	}
	delete(cfs.readErr, hostFile)

	cfs.nvmetConn(admissionNqn, 3, admissionHostA)
	cfs.stateRefusal = true
	_, err = n.EndNvmetConn(ctx, admissionNqn,
		NvmetConn{Name: "ctrl3", HostNqn: admissionHostA})
	if err == nil || !errors.Is(err, syscall.EPERM) {
		t.Errorf("a refused write: EndNvmetConn = %v, want EPERM", err)
	}
}

// TestListNvmetConnsReadsARemovedHostFileAgain: a "hostnqn" that answers EIO
// was removed after its open: read once more, a file that is not there is a
// connection that has gone, and one that answers is a new connection that
// took the freed name, listed by its own host.
func TestListNvmetConnsReadsARemovedHostFileAgain(t *testing.T) {
	ctx := context.Background()
	cfs := newFakeConfigfs()
	n := NewNvmet(cfs.osClient())
	debugDir := n.SubsysDebugPath(admissionNqn)
	cfs.nvmetConn(admissionNqn, 1, admissionHostA)
	cfs.nvmetConn(admissionNqn, 2, admissionHostA)
	cfs.removing[debugDir+"/ctrl1/hostnqn"] = true
	cfs.removing[debugDir+"/ctrl2/hostnqn"] = true
	cfs.reuse[debugDir+"/ctrl2"] = admissionHostB
	conns, ok, err := n.ListNvmetConns(ctx, admissionNqn)
	want := []NvmetConn{{Name: "ctrl2", HostNqn: admissionHostB}}
	if err != nil || !ok || !slices.Equal(conns, want) {
		t.Fatalf("ListNvmetConns = %v, %v, %v; want %v", conns, ok, err, want)
	}
}

// TestNvmetConnState: a connection's "state" reads the set bits of its
// controller status, nothing when no bit is set, and a connection that has
// gone reads as not there; a read that fails otherwise is an error.
func TestNvmetConnState(t *testing.T) {
	ctx := context.Background()
	cfs := newFakeConfigfs()
	n := NewNvmet(cfs.osClient())
	state := n.SubsysDebugPath(admissionNqn) + "/ctrl1/state"
	cfs.nvmetConn(admissionNqn, 1, admissionHostA)
	for _, want := range []string{"ready", "ready|fatal", ""} {
		cfs.files[state] = want + "\n"
		if got, ok, err := n.NvmetConnState(ctx, admissionNqn, "ctrl1"); err !=
			nil || !ok || got != want {
			t.Errorf("NvmetConnState = %q, %v, %v; want %q", got, ok, err,
				want)
		}
	}
	cfs.readErr[state] = &iofs.PathError{Op: "open", Path: state,
		Err: syscall.EPERM}
	if _, _, err := n.NvmetConnState(ctx, admissionNqn, "ctrl1"); err == nil {
		t.Errorf("a refused read gave no error")
	}
	delete(cfs.readErr, state)
	cfs.removing[state] = true
	if _, ok, err := n.NvmetConnState(ctx, admissionNqn, "ctrl1"); err !=
		nil || ok {
		t.Errorf("a state removed after its open: ok %v, %v; want gone",
			ok, err)
	}
}

// TestAdmitsOnly: the strict read of what a subsystem admits (cnagent.md
// CN33) answers yes for a subsystem that links no host off the desired
// list, a desired host not linked yet included, and for one that is not
// there; no for one that links a host off the list or reads open; and an
// error, never an answer, for a read that does not answer.
func TestAdmitsOnly(t *testing.T) {
	ctx := context.Background()
	conf := SubsysConf{Nqn: admissionNqn,
		AllowedHosts: []string{admissionHostA, admissionHostB}}
	built := func(t *testing.T) (*fakeConfigfs, *Nvmet) {
		cfs := newFakeConfigfs()
		nvmet := NewNvmet(cfs.osClient())
		if err := nvmet.EnsureSubsystem(ctx, conf); err != nil {
			t.Fatalf("EnsureSubsystem: %v", err)
		}
		return cfs, nvmet
	}
	path := NewNvmet(newFakeConfigfs().osClient()).SubsysPath(admissionNqn)
	for _, tc := range []struct {
		name    string
		fault   func(cfs *fakeConfigfs)
		allowed []string
		ok      bool
		why     string
		err     bool
	}{
		{"the desired list", nil, conf.AllowedHosts, true, "", false},
		{"a desired host not linked yet", nil,
			append(slices.Clone(conf.AllowedHosts),
				"nqn.2024-01.io.dnv-it:host:c"),
			true, "", false},
		{"a host off the list", nil, []string{admissionHostA}, false,
			"allowed host " + admissionHostB + " is not desired", false},
		{"an open subsystem", func(cfs *fakeConfigfs) {
			cfs.files[path+"/attr_allow_any_host"] = "1"
		}, conf.AllowedHosts, false, `attr_allow_any_host is "1"`, false},
		{"no subsystem", func(cfs *fakeConfigfs) {
			for p := range cfs.dirs {
				if strings.HasPrefix(p, path) {
					delete(cfs.dirs, p)
				}
			}
		}, nil, true, "", false},
		{"a subsystem listing that does not answer", func(cfs *fakeConfigfs) {
			cfs.listErr[path] = context.DeadlineExceeded
		}, conf.AllowedHosts, false, "", true},
		{"a listing that does not answer", func(cfs *fakeConfigfs) {
			cfs.listErr[path+"/allowed_hosts"] = context.DeadlineExceeded
		}, conf.AllowedHosts, false, "", true},
		{"a read that does not answer", func(cfs *fakeConfigfs) {
			cfs.readErr[path+"/attr_allow_any_host"] = &iofs.PathError{
				Op: "read", Path: path, Err: syscall.EIO}
		}, conf.AllowedHosts, false, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfs, nvmet := built(t)
			if tc.fault != nil {
				tc.fault(cfs)
			}
			ok, why, err := nvmet.AdmitsOnly(ctx, admissionNqn, tc.allowed)
			if ok != tc.ok || why != tc.why || (err != nil) != tc.err {
				t.Fatalf("AdmitsOnly = %v, %q, %v; want %v, %q, error %v",
					ok, why, err, tc.ok, tc.why, tc.err)
			}
		})
	}
}
