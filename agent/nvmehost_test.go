package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// ---------------------------------------------------------------------------
// One walk for both agents, each caller under its own rule (SH20)
// ---------------------------------------------------------------------------

// nvmeSysfs is a fakeSysfs holding nvme subsystems and controllers. It traces
// every listing ("list <path>") and every file read ("read <path>") in the
// order they reach the tree, because each caller's rule is as much which
// reads it makes, and in which order, as what it returns: a read past a
// match, or past a failure, is a read the dn agent never made.
type nvmeSysfs struct {
	fakeSysfs
	trace []string
}

func newNvmeSysfs() *nvmeSysfs {
	return &nvmeSysfs{fakeSysfs: fakeSysfs{
		dirs:    map[string][]string{sysfsNvmeSubsysDir: nil},
		files:   map[string]string{},
		killLs:  map[string]bool{},
		readErr: map[string]error{},
	}}
}

// host is an NvmeHost over the tree, with the trace started afresh.
func (s *nvmeSysfs) host() *NvmeHost {
	s.trace = nil
	oc := s.osClient()
	readDir, readFile := oc.ReadDirFn, oc.ReadFileFn
	oc.ReadDirFn = func(ctx context.Context, path string) ([]string, error) {
		s.trace = append(s.trace, "list "+path)
		return readDir(ctx, path)
	}
	oc.ReadFileFn = func(ctx context.Context, path string) (string, error) {
		s.trace = append(s.trace, "read "+path)
		return readFile(ctx, path)
	}
	return NewNvmeHost(oc)
}

// subsys adds the class directory entry for nqn, holding names (namespace
// nodes and controllers) beside its subsysnqn, and returns its directory.
func (s *nvmeSysfs) subsys(entry, nqn string, names ...string) string {
	dir := sysfsNvmeSubsysDir + "/" + entry
	s.dirs[sysfsNvmeSubsysDir] = append(s.dirs[sysfsNvmeSubsysDir], entry)
	s.dirs[dir] = append([]string{"subsysnqn"}, names...)
	s.files[dir+"/subsysnqn"] = nqn + "\n"
	return dir
}

// ctrl adds the controller name with every attribute any caller reads of
// one, and a path device "<name>c1n<i>" for each ANA state given, in order.
func (s *nvmeSysfs) ctrl(name, trAddr, state string, anaStates ...string) {
	dir := sysfsNvmeCtrlDir + "/" + name
	s.dirs[dir] = []string{"address", "hostnqn", "state", "transport"}
	s.files[dir+"/address"] = "traddr=" + trAddr + ",trsvcid=4420\n"
	s.files[dir+"/transport"] = "tcp\n"
	s.files[dir+"/state"] = state + "\n"
	s.files[dir+"/hostnqn"] = "nqn.2014-08.org.nvmexpress:uuid:other\n"
	for i, ana := range anaStates {
		path := fmt.Sprintf("%sc1n%d", name, i+1)
		s.dirs[dir] = append(s.dirs[dir], path)
		s.files[dir+"/"+path+"/ana_state"] = ana + "\n"
	}
}

// fail makes one call of the trace not answer: a listing the way the soft
// timeout cuts one off, a file read with EIO, as a controller's attribute
// can answer while the controller resets.
func (s *nvmeSysfs) fail(call string) {
	if path, ok := strings.CutPrefix(call, "list "); ok {
		s.killLs[path] = true
		return
	}
	path := strings.TrimPrefix(call, "read ")
	s.readErr[path] = &fs.PathError{Op: "read", Path: path, Err: syscall.EIO}
}

// failedOn says whether err is the failure fail armed for call, named.
func failedOn(err error, call string) bool {
	path := call[strings.Index(call, " ")+1:]
	return err != nil && strings.Contains(err.Error(), path) &&
		(errors.Is(err, syscall.EIO) ||
			errors.Is(err, context.DeadlineExceeded))
}

// bounded fails the test when a read or a listing of the tree came without
// the SH15 deadline: the nvme class directories stall while a controller
// resets, and no caller may be held on one past the soft timeout.
func (s *nvmeSysfs) bounded(t *testing.T) {
	t.Helper()
	if len(s.noDeadline) != 0 {
		t.Errorf("sysfs reads or listings without an SH15 deadline: %v",
			s.noDeadline)
	}
}

// TestListSubsysRules pins the per-NQN lookup of the dn agent and of the cn
// sweep's removal check (SH20). Each call walks afresh and stops at the first
// entry named nqn: no read past the match is made. A subsysnqn read that did
// not answer before the match fails the call, one past it is never made.
// Inside the match the reads run in one order — the listing, then per
// controller its address, transport, state, listing and path devices'
// ana_state — and the first that did not answer ends the call with its
// error. The first path device whose ana_state reads gives the controller's
// ANA state, and no later one is read. A read that finds nothing fails
// nothing: a controller with no address is read in full, an absent
// ana_state passes to the next path device, and a matched directory gone
// since the listing is found and empty.
func TestListSubsysRules(t *testing.T) {
	ctx := context.Background()
	const nqn = "nqn.2024-01.io.dnv:3:a:b:c"
	const (
		before = sysfsNvmeSubsysDir + "/nvme-subsys0"
		match  = sysfsNvmeSubsysDir + "/nvme-subsys1"
		after  = sysfsNvmeSubsysDir + "/nvme-subsys2"
		nvme3  = sysfsNvmeCtrlDir + "/nvme3"
		nvme4  = sysfsNvmeCtrlDir + "/nvme4"
	)
	tree := func() *nvmeSysfs {
		s := newNvmeSysfs()
		s.subsys("nvme-subsys0", "nqn.2024-01.io.dnv:3:x:y:z", "nvme2")
		s.subsys("nvme-subsys1", nqn, "nvme1n1", "nvme3", "nvme4")
		s.subsys("nvme-subsys2", "nqn.2024-01.io.dnv:3:p:q:r", "nvme5")
		// Both of nvme3's path devices have an ana_state: the first one's
		// is the controller's, and the second is never read.
		s.ctrl("nvme3", "10.0.0.1", "live", "optimized", "inaccessible")
		// nvme4's first path device has no ana_state: the walk passes to
		// the next one.
		s.ctrl("nvme4", "10.0.0.2", "connecting", "", "non-optimized")
		delete(s.files, nvme4+"/nvme4c1n1/ana_state")
		return s
	}
	full := []string{
		"list " + sysfsNvmeSubsysDir,
		"read " + before + "/subsysnqn",
		"read " + match + "/subsysnqn",
		"list " + match,
		"read " + nvme3 + "/address",
		"read " + nvme3 + "/transport",
		"read " + nvme3 + "/state",
		"list " + nvme3,
		"read " + nvme3 + "/nvme3c1n1/ana_state",
		"read " + nvme4 + "/address",
		"read " + nvme4 + "/transport",
		"read " + nvme4 + "/state",
		"list " + nvme4,
		"read " + nvme4 + "/nvme4c1n1/ana_state",
		"read " + nvme4 + "/nvme4c1n2/ana_state",
	}
	want := &SubsysState{
		Nqn:        nqn,
		Found:      true,
		Live:       true,
		States:     []string{"live", "connecting"},
		DevicePath: "/dev/nvme1n1",
		Paths: []PathState{
			{Name: "nvme3", Transport: "tcp", TrAddr: "10.0.0.1",
				TrSvcId: "4420", State: "live", AnaState: "optimized"},
			{Name: "nvme4", Transport: "tcp", TrAddr: "10.0.0.2",
				TrSvcId: "4420", State: "connecting",
				AnaState: "non-optimized"},
		},
	}
	sameState := func(t *testing.T, got, want *SubsysState) {
		t.Helper()
		if got == nil || got.Nqn != want.Nqn || got.Found != want.Found ||
			got.Live != want.Live || got.DevicePath != want.DevicePath ||
			!slices.Equal(got.States, want.States) ||
			!slices.Equal(got.Paths, want.Paths) {
			t.Errorf("state = %+v\nwant    %+v", got, want)
		}
	}

	t.Run("the match, and nothing past it", func(t *testing.T) {
		s := tree()
		// The entry past the match does not answer: the call never reads
		// it, so it cannot fail on it.
		s.fail("read " + after + "/subsysnqn")
		state, err := s.host().ListSubsys(ctx, nqn)
		if err != nil {
			t.Fatalf("ListSubsys: %v", err)
		}
		sameState(t, state, want)
		if !slices.Equal(s.trace, full) {
			t.Errorf("reads:\n%s\nwant:\n%s", strings.Join(s.trace, "\n"),
				strings.Join(full, "\n"))
		}
		s.bounded(t)
	})

	// Each read that does not answer ends the call with its error, and
	// nothing is read after it: the reads before it are the full call's.
	for _, failing := range []string{
		"read " + before + "/subsysnqn",
		"list " + match,
		"read " + nvme3 + "/address",
		"read " + nvme3 + "/transport",
		"read " + nvme3 + "/state",
		"list " + nvme3,
		"read " + nvme3 + "/nvme3c1n1/ana_state",
		"read " + nvme4 + "/state",
		"read " + nvme4 + "/nvme4c1n2/ana_state",
	} {
		t.Run("fails on "+failing, func(t *testing.T) {
			s := tree()
			s.fail(failing)
			state, err := s.host().ListSubsys(ctx, nqn)
			if !failedOn(err, failing) || state != nil {
				t.Fatalf("got %+v, %v; want the error of %s", state, err,
					failing)
			}
			at := slices.Index(full, failing)
			if !slices.Equal(s.trace, full[:at+1]) {
				t.Errorf("reads:\n%s\nwant:\n%s",
					strings.Join(s.trace, "\n"),
					strings.Join(full[:at+1], "\n"))
			}
		})
	}

	t.Run("an entry gone before the match", func(t *testing.T) {
		s := tree()
		delete(s.files, before+"/subsysnqn")
		state, err := s.host().ListSubsys(ctx, nqn)
		if err != nil {
			t.Fatalf("ListSubsys: %v", err)
		}
		sameState(t, state, want)
		if !slices.Equal(s.trace, full) {
			t.Errorf("reads:\n%s", strings.Join(s.trace, "\n"))
		}
	})

	t.Run("absent reads inside the match", func(t *testing.T) {
		s := tree()
		// No address: the controller is read in full all the same. No
		// transport and no state read as "", and a controller directory
		// that is not there holds no path device.
		delete(s.files, nvme3+"/address")
		delete(s.files, nvme3+"/transport")
		delete(s.files, nvme4+"/state")
		delete(s.dirs, nvme4)
		state, err := s.host().ListSubsys(ctx, nqn)
		if err != nil {
			t.Fatalf("ListSubsys: %v", err)
		}
		sameState(t, state, &SubsysState{
			Nqn:        nqn,
			Found:      true,
			Live:       true,
			States:     []string{"live", ""},
			DevicePath: "/dev/nvme1n1",
			Paths: []PathState{
				{Name: "nvme3", State: "live", AnaState: "optimized"},
				{Name: "nvme4", Transport: "tcp", TrAddr: "10.0.0.2",
					TrSvcId: "4420"},
			},
		})
		at := slices.Index(full, "list "+nvme4)
		if !slices.Equal(s.trace, full[:at+1]) {
			t.Errorf("reads:\n%s", strings.Join(s.trace, "\n"))
		}
	})

	t.Run("a matched directory gone since the listing", func(t *testing.T) {
		s := tree()
		delete(s.dirs, match)
		state, err := s.host().ListSubsys(ctx, nqn)
		if err != nil {
			t.Fatalf("ListSubsys: %v", err)
		}
		sameState(t, state, &SubsysState{Nqn: nqn, Found: true})
		at := slices.Index(full, "list "+match)
		if !slices.Equal(s.trace, full[:at+1]) {
			t.Errorf("reads:\n%s", strings.Join(s.trace, "\n"))
		}
	})

	t.Run("no entry named nqn", func(t *testing.T) {
		s := tree()
		state, err := s.host().ListSubsys(ctx, "nqn.2024-01.io.dnv:3:none")
		if err != nil || state == nil || state.Found {
			t.Fatalf("got %+v, %v; want not found", state, err)
		}
		if !slices.Equal(s.trace, []string{
			"list " + sysfsNvmeSubsysDir,
			"read " + before + "/subsysnqn",
			"read " + match + "/subsysnqn",
			"read " + after + "/subsysnqn",
		}) {
			t.Errorf("reads:\n%s", strings.Join(s.trace, "\n"))
		}

		// No class directory is no subsystem; a listing of it that did
		// not answer is an error.
		s = newNvmeSysfs()
		delete(s.dirs, sysfsNvmeSubsysDir)
		state, err = s.host().ListSubsys(ctx, nqn)
		if err != nil || state == nil || state.Found {
			t.Fatalf("missing class directory: got %+v, %v", state, err)
		}
		s = tree()
		s.fail("list " + sysfsNvmeSubsysDir)
		if state, err = s.host().ListSubsys(ctx, nqn); state != nil ||
			!failedOn(err, "list "+sysfsNvmeSubsysDir) {
			t.Fatalf("class listing that did not answer: got %+v, %v",
				state, err)
		}
	})
}

// TestWalkSubsysAndBriefs pins the walk (SH20): one listing of the class
// directory and every entry's subsysnqn read, in listing order, each read
// that did not answer recorded apart from one that found nothing. Find gives
// the first match and the first entry that did not answer, for the caller's
// rule to weigh; of two entries that name one NQN, the match is the first,
// as ListSubsys's is. Briefs, ListAllSubsys's list of connections, fails on
// the first entry that did not answer and passes over a gone one;
// ListAllSubsys itself reads no NQN past that entry.
func TestWalkSubsysAndBriefs(t *testing.T) {
	ctx := context.Background()
	const (
		nqnA = "nqn.2024-01.io.dnv:3:a:a:a"
		nqnB = "nqn.2024-01.io.dnv:3:b:b:b"
	)
	s := newNvmeSysfs()
	a := s.subsys("nvme-subsys0", nqnA)
	gone := s.subsys("nvme-subsys1", "nqn.2024-01.io.dnv:3:g:g:g")
	unread := s.subsys("nvme-subsys2", "nqn.2024-01.io.dnv:3:u:u:u")
	b := s.subsys("nvme-subsys3", nqnB)
	unread2 := s.subsys("nvme-subsys4", "nqn.2024-01.io.dnv:3:v:v:v")
	// A second entry names nqnA, as for a moment two do while the kernel
	// deletes a subsystem whose last reference dropped and a connect of its
	// NQN makes a new one.
	a2 := s.subsys("nvme-subsys5", nqnA)
	delete(s.files, gone+"/subsysnqn")
	s.fail("read " + unread + "/subsysnqn")
	s.fail("read " + unread2 + "/subsysnqn")

	walk, err := s.host().WalkSubsys(ctx)
	if err != nil {
		t.Fatalf("WalkSubsys: %v", err)
	}
	reads := []string{"list " + sysfsNvmeSubsysDir}
	for _, dir := range []string{a, gone, unread, b, unread2, a2} {
		reads = append(reads, "read "+dir+"/subsysnqn")
	}
	if !slices.Equal(s.trace, reads) {
		t.Errorf("reads:\n%s", strings.Join(s.trace, "\n"))
	}
	s.bounded(t)
	if len(walk.Entries) != 6 {
		t.Fatalf("entries = %+v", walk.Entries)
	}
	for i, want := range []SubsysEntry{
		{Dir: a, Nqn: nqnA},
		{Dir: gone, Gone: true},
		{Dir: unread},
		{Dir: b, Nqn: nqnB},
		{Dir: unread2},
		{Dir: a2, Nqn: nqnA},
	} {
		got := walk.Entries[i]
		if got.Dir != want.Dir || got.Nqn != want.Nqn ||
			got.Gone != want.Gone ||
			(got.Err != nil) != (want.Dir == unread || want.Dir == unread2) {
			t.Errorf("entry %d = %+v, want %+v", i, got, want)
		}
	}
	if !failedOn(walk.Entries[2].Err, "read "+unread+"/subsysnqn") {
		t.Errorf("unread entry's error = %v", walk.Entries[2].Err)
	}

	for _, tc := range []struct {
		nqn   string
		match *SubsysEntry
	}{
		// The first of the two entries that name nqnA.
		{nqnA, &walk.Entries[0]},
		{nqnB, &walk.Entries[3]},
		{"nqn.2024-01.io.dnv:3:none", nil},
		// A gone or unread entry carries no NQN, and names none.
		{"", nil},
	} {
		match, firstUnread := walk.Find(tc.nqn)
		if match != tc.match || firstUnread != &walk.Entries[2] {
			t.Errorf("Find(%q) = %+v, %+v", tc.nqn, match, firstUnread)
		}
	}

	if briefs, err := walk.Briefs(); briefs != nil ||
		!failedOn(err, "read "+unread+"/subsysnqn") {
		t.Errorf("Briefs over two unread entries = %+v, %v; want the "+
			"first one's error", briefs, err)
	}
	host := s.host()
	if briefs, err := host.ListAllSubsys(ctx); briefs != nil ||
		!failedOn(err, "read "+unread+"/subsysnqn") {
		t.Errorf("ListAllSubsys = %+v, %v", briefs, err)
	}
	if !slices.Equal(s.trace, reads[:4]) {
		t.Errorf("ListAllSubsys read past the first unread entry:\n%s",
			strings.Join(s.trace, "\n"))
	}

	// Every entry answers: the gone one is no connection.
	delete(s.readErr, unread+"/subsysnqn")
	delete(s.readErr, unread2+"/subsysnqn")
	walk, err = s.host().WalkSubsys(ctx)
	if err != nil {
		t.Fatalf("WalkSubsys: %v", err)
	}
	wantBriefs := []SubsysBrief{
		{Nqn: nqnA, Dir: a},
		{Nqn: "nqn.2024-01.io.dnv:3:u:u:u", Dir: unread},
		{Nqn: nqnB, Dir: b},
		{Nqn: "nqn.2024-01.io.dnv:3:v:v:v", Dir: unread2},
		{Nqn: nqnA, Dir: a2},
	}
	briefs, err := walk.Briefs()
	if err != nil || !slices.Equal(briefs, wantBriefs) {
		t.Errorf("Briefs = %+v, %v; want %+v", briefs, err, wantBriefs)
	}
	if _, firstUnread := walk.Find(nqnA); firstUnread != nil {
		t.Errorf("Find names an unread entry in a walk with none: %+v",
			firstUnread)
	}
	briefs, err = host.ListAllSubsys(ctx)
	if err != nil || !slices.Equal(briefs, wantBriefs) {
		t.Errorf("ListAllSubsys = %+v, %v; want %+v", briefs, err,
			wantBriefs)
	}

	// No class directory is a walk with no entry; a listing of it that did
	// not answer fails the walk and ListAllSubsys alike.
	empty := newNvmeSysfs()
	delete(empty.dirs, sysfsNvmeSubsysDir)
	walk, err = empty.host().WalkSubsys(ctx)
	if err != nil || len(walk.Entries) != 0 {
		t.Errorf("missing class directory: %+v, %v", walk, err)
	}
	if briefs, err := empty.host().ListAllSubsys(ctx); err != nil ||
		len(briefs) != 0 {
		t.Errorf("missing class directory: %+v, %v", briefs, err)
	}
	s.fail("list " + sysfsNvmeSubsysDir)
	if walk, err := s.host().WalkSubsys(ctx); walk != nil ||
		!failedOn(err, "list "+sysfsNvmeSubsysDir) {
		t.Errorf("class listing that did not answer: %+v, %v", walk, err)
	}
	if briefs, err := s.host().ListAllSubsys(ctx); briefs != nil ||
		!failedOn(err, "list "+sysfsNvmeSubsysDir) {
		t.Errorf("class listing that did not answer: %+v, %v", briefs, err)
	}
}

// TestReadSubsysTolerant pins the read of one entry under the cn lookup's
// rule (cnagent.md CN10): each read that did not answer is recorded, the
// first of its kind, and the read goes on. An absent address is a gone
// controller, and nothing more of it is read; an address that did not answer
// is recorded with its read's text, and the controller's other reads still
// run; a state that did not answer reads ""; a controller listing that did
// not answer leaves its ANA state unread; the ANA state comes from the first
// path device whose ana_state reads, past those that did not answer, and no
// later one is read; and the namespace node is the first whose nsid reads
// the number asked for, past those whose nsid read did not answer. A node
// whose nsid is not there is passed over under either rule, and is no failed
// read. A transport read has no record: one that did not answer ends the
// read under this rule too. The same entry under ReadStrict ends at its
// first failure.
func TestReadSubsysTolerant(t *testing.T) {
	ctx := context.Background()
	const (
		nvme3 = sysfsNvmeCtrlDir + "/nvme3"
		nvme4 = sysfsNvmeCtrlDir + "/nvme4"
		nvme5 = sysfsNvmeCtrlDir + "/nvme5"
		nvme6 = sysfsNvmeCtrlDir + "/nvme6"
	)
	s := newNvmeSysfs()
	names := []string{"nvme0n1", "nvme0n2", "nvme0n3", "nvme0n4", "nvme0n5",
		"nvme3", "nvme4", "nvme5", "nvme6"}
	dir := s.subsys("nvme-subsys0", "nqn.2024-01.io.dnv:3:a:b:c", names...)
	s.files[dir+"/nvme0n1/nsid"] = "1\n"
	s.files[dir+"/nvme0n4/nsid"] = "2\n"
	s.files[dir+"/nvme0n5/nsid"] = "2\n"
	s.fail("read " + dir + "/nvme0n2/nsid")
	s.fail("read " + dir + "/nvme0n3/nsid")
	// nvme3 is gone: its device is deleted, yet its other attributes are
	// planted, so a read past the absent address would show.
	s.ctrl("nvme3", "10.0.0.3", "live", "optimized")
	delete(s.files, nvme3+"/address")
	// nvme4's address does not answer, nor do its first two path devices.
	// The third one's ana_state is the controller's, and the fourth, in
	// another state, is never read.
	s.ctrl("nvme4", "10.0.0.4", "live", "inaccessible", "inaccessible",
		"optimized", "non-optimized")
	s.fail("read " + nvme4 + "/address")
	s.fail("read " + nvme4 + "/nvme4c1n1/ana_state")
	s.fail("read " + nvme4 + "/nvme4c1n2/ana_state")
	// nvme5's state and its listing do not answer.
	s.ctrl("nvme5", "10.0.0.5", "live", "optimized")
	s.fail("read " + nvme5 + "/state")
	s.fail("list " + nvme5)
	// nvme6 has no directory to list.
	s.ctrl("nvme6", "10.0.0.6", "connecting", "optimized")
	delete(s.dirs, nvme6)

	read, err := s.host().ReadSubsys(ctx, dir, SubsysReadOpts{
		NsIdx: 2,
		Rule:  ReadTolerant,
	})
	if err != nil || read == nil {
		t.Fatalf("ReadSubsys: %+v, %v", read, err)
	}
	if !slices.Equal(s.trace, []string{
		"list " + dir,
		"read " + dir + "/nvme0n1/nsid",
		"read " + dir + "/nvme0n2/nsid",
		"read " + dir + "/nvme0n3/nsid",
		"read " + dir + "/nvme0n4/nsid",
		"read " + nvme3 + "/address",
		"read " + nvme4 + "/address",
		"read " + nvme4 + "/state",
		"list " + nvme4,
		"read " + nvme4 + "/nvme4c1n1/ana_state",
		"read " + nvme4 + "/nvme4c1n2/ana_state",
		"read " + nvme4 + "/nvme4c1n3/ana_state",
		"read " + nvme5 + "/address",
		"read " + nvme5 + "/state",
		"list " + nvme5,
		"read " + nvme6 + "/address",
		"read " + nvme6 + "/state",
		"list " + nvme6,
	}) {
		t.Errorf("reads:\n%s", strings.Join(s.trace, "\n"))
	}
	s.bounded(t)
	if read.Dir != dir || read.DirGone || read.DirErr != nil ||
		read.NsDev != "/dev/nvme0n4" ||
		!failedOn(read.NsErr, "read "+dir+"/nvme0n2/nsid") {
		t.Errorf("entry = %+v", read)
	}
	if len(read.Ctrls) != 4 {
		t.Fatalf("controllers = %+v", read.Ctrls)
	}
	if read.Ctrls[0] != (CtrlRead{Name: "nvme3", AddrGone: true}) {
		t.Errorf("gone controller = %+v", read.Ctrls[0])
	}
	ctrl := read.Ctrls[1]
	if ctrl.AddrErr == nil || !strings.HasPrefix(ctrl.AddrErr.Error(),
		"read "+nvme4+"/address: ") ||
		!errors.Is(ctrl.AddrErr, syscall.EIO) ||
		ctrl.TrAddr != "" || ctrl.AddrGone || ctrl.State != "live" ||
		ctrl.AnaState != "optimized" ||
		!failedOn(ctrl.AnaErr, "read "+nvme4+"/nvme4c1n1/ana_state") {
		t.Errorf("controller with an unanswered address = %+v", ctrl)
	}
	ctrl = read.Ctrls[2]
	if ctrl.TrAddr != "10.0.0.5" || ctrl.TrSvcId != "4420" ||
		ctrl.State != "" ||
		!failedOn(ctrl.StateErr, "read "+nvme5+"/state") ||
		!failedOn(ctrl.DirErr, "list "+nvme5) || ctrl.AnaState != "" ||
		ctrl.AnaErr != nil {
		t.Errorf("controller with an unanswered state and listing = %+v",
			ctrl)
	}
	if read.Ctrls[3] != (CtrlRead{Name: "nvme6", TrAddr: "10.0.0.6",
		TrSvcId: "4420", State: "connecting"}) {
		t.Errorf("controller with no directory = %+v", read.Ctrls[3])
	}

	// NsIdx 0 takes the first namespace node, reading no nsid.
	read, err = s.host().ReadSubsys(ctx, dir, SubsysReadOpts{
		Ctrls: CtrlNone,
		Rule:  ReadTolerant,
	})
	if err != nil || read.NsDev != "/dev/nvme0n1" || read.NsErr != nil ||
		!slices.Equal(s.trace, []string{"list " + dir}) {
		t.Errorf("NsIdx 0: %+v, %v, reads %v", read, err, s.trace)
	}

	// A node whose nsid is not there is passed over, under either rule, and
	// its read is no failure.
	other := s.subsys("nvme-subsys1", "nqn.2024-01.io.dnv:3:d:e:f",
		"nvme1n1", "nvme1n2")
	s.files[other+"/nvme1n2/nsid"] = "2\n"
	for _, rule := range []SubsysReadRule{ReadTolerant, ReadStrict} {
		for _, tc := range []struct {
			nsIdx uint32
			want  string
		}{{2, "/dev/nvme1n2"}, {1, ""}} {
			read, err = s.host().ReadSubsys(ctx, other,
				SubsysReadOpts{NsIdx: tc.nsIdx, Rule: rule})
			if err != nil || read.NsDev != tc.want || read.NsErr != nil ||
				!slices.Equal(s.trace, []string{"list " + other,
					"read " + other + "/nvme1n1/nsid",
					"read " + other + "/nvme1n2/nsid"}) {
				t.Errorf("rule %d, NsIdx %d past an absent nsid: %+v, %v, "+
					"reads %v", rule, tc.nsIdx, read, err, s.trace)
			}
		}
	}

	// The entry's own listing: one that did not answer is recorded, one
	// that found nothing is gone; either way nothing else is read.
	s.fail("list " + dir)
	read, err = s.host().ReadSubsys(ctx, dir,
		SubsysReadOpts{NsIdx: 2, Rule: ReadTolerant})
	if err != nil || !failedOn(read.DirErr, "list "+dir) || read.DirGone ||
		read.NsDev != "" || read.Ctrls != nil ||
		!slices.Equal(s.trace, []string{"list " + dir}) {
		t.Errorf("unanswered listing: %+v, %v, reads %v", read, err,
			s.trace)
	}
	delete(s.killLs, dir)
	delete(s.dirs, dir)
	read, err = s.host().ReadSubsys(ctx, dir,
		SubsysReadOpts{NsIdx: 2, Rule: ReadTolerant})
	if err != nil || !read.DirGone || read.DirErr != nil ||
		!slices.Equal(s.trace, []string{"list " + dir}) {
		t.Errorf("missing directory: %+v, %v, reads %v", read, err, s.trace)
	}
	s.dirs[dir] = append([]string{"subsysnqn"}, names...)

	// The same entry under ReadStrict ends at its first failure.
	read, err = s.host().ReadSubsys(ctx, dir,
		SubsysReadOpts{NsIdx: 2, Rule: ReadStrict})
	if read != nil || !failedOn(err, "read "+dir+"/nvme0n2/nsid") {
		t.Errorf("ReadStrict: %+v, %v", read, err)
	}

	// A transport read has no record to hold its failure, so one that did
	// not answer ends the read under ReadTolerant too.
	s.fail("read " + nvme4 + "/transport")
	read, err = s.host().ReadSubsys(ctx, dir, SubsysReadOpts{
		NsIdx:     2,
		Transport: true,
		Rule:      ReadTolerant,
	})
	if read != nil || !failedOn(err, "read "+nvme4+"/transport") {
		t.Errorf("ReadTolerant, unanswered transport: %+v, %v", read, err)
	}
}

// TestReadSubsysNgListed pins the sign of a complete head (SH20): the read
// records whether the listing that picked the namespace node holds the
// node's ng device too, ng<X>n<Y> for nvme<X>n<Y>, named after the node and
// not after the nsid, and in the listing before the node. The kernel adds the
// ng device last, so the cn agent's head wait after a connect waits for it
// (cnagent.md CN10). It is read from the names the listing already holds:
// no read of the ng device is made. A read that picks no node records no ng
// device, whatever the listing holds.
func TestReadSubsysNgListed(t *testing.T) {
	ctx := context.Background()
	s := newNvmeSysfs()
	dir := s.subsys("nvme-subsys0", "nqn.2024-01.io.dnv:3:a:b:c",
		"ng0n1", "nvme0n1", "nvme0n2", "nvme3")
	s.files[dir+"/nvme0n1/nsid"] = "2\n"
	s.files[dir+"/nvme0n2/nsid"] = "1\n"
	s.ctrl("nvme3", "10.0.0.3", "live", "optimized")
	for _, tc := range []struct {
		name     string
		ng       string // an ng device added to the listing
		nsIdx    uint32
		nsDev    string
		ngListed bool
		reads    []string
	}{
		{"the node of nsid 2, its ng device listed before it", "", 2,
			"/dev/nvme0n1", true,
			[]string{"read " + dir + "/nvme0n1/nsid"}},
		{"the node of nsid 1, another node's ng device listed", "", 1,
			"/dev/nvme0n2", false,
			[]string{"read " + dir + "/nvme0n1/nsid",
				"read " + dir + "/nvme0n2/nsid"}},
		{"the node of nsid 1, its ng device listed", "ng0n2", 1,
			"/dev/nvme0n2", true,
			[]string{"read " + dir + "/nvme0n1/nsid",
				"read " + dir + "/nvme0n2/nsid"}},
		{"NsIdx 0, the first node", "", 0, "/dev/nvme0n1", true, nil},
		{"no node of nsid 3", "ng0n2", 3, "", false,
			[]string{"read " + dir + "/nvme0n1/nsid",
				"read " + dir + "/nvme0n2/nsid"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			names := []string{"subsysnqn", "ng0n1", "nvme0n1", "nvme0n2",
				"nvme3"}
			if tc.ng != "" {
				names = append(names, tc.ng)
			}
			s.dirs[dir] = names
			read, err := s.host().ReadSubsys(ctx, dir, SubsysReadOpts{
				NsIdx: tc.nsIdx,
				Ctrls: CtrlNone,
				Rule:  ReadTolerant,
			})
			if err != nil || read.NsDev != tc.nsDev ||
				read.NgListed != tc.ngListed || read.NsErr != nil {
				t.Fatalf("ReadSubsys = %+v, %v; want NsDev %q, NgListed %v",
					read, err, tc.nsDev, tc.ngListed)
			}
			want := append([]string{"list " + dir}, tc.reads...)
			if !slices.Equal(s.trace, want) {
				t.Errorf("reads:\n%s\nwant:\n%s",
					strings.Join(s.trace, "\n"), strings.Join(want, "\n"))
			}
		})
	}
	s.bounded(t)
}

// TestReadSubsysCtrlReads pins the two sweep reads folded into ReadSubsys
// (SH20), each reading exactly what it reads alone. HeldWithHostNqn reads
// each controller's hostnqn and nothing else of it, in listing order, up to
// the first one equal to the host NQN asked for: a hostnqn read that did not
// answer before it fails the call, an absent one is passed over, and no
// controller past the match is read. A hostnqn read has no record, so one
// that did not answer ends a read under ReadTolerant too. SubsysDevicePath
// reads the listing alone and takes the first namespace node. A directory
// gone since the walk holds neither; a listing that did not answer fails
// both.
func TestReadSubsysCtrlReads(t *testing.T) {
	ctx := context.Background()
	const ours = "nqn.2014-08.org.nvmexpress:uuid:ours"
	s := newNvmeSysfs()
	dir := s.subsys("nvme-subsys0", "nqn.2024-01.io.dnv:2:a:b:c:d",
		"nvme0n2", "nvme0n1", "nvme3", "nvme4", "nvme5", "nvme6")
	for _, name := range []string{"nvme3", "nvme4", "nvme5", "nvme6"} {
		s.ctrl(name, "10.0.0.1", "live", "optimized")
	}
	delete(s.files, sysfsNvmeCtrlDir+"/nvme3/hostnqn")
	s.files[sysfsNvmeCtrlDir+"/nvme5/hostnqn"] = ours + "\n"
	s.files[sysfsNvmeCtrlDir+"/nvme6/hostnqn"] = ours + "\n"
	brief := SubsysBrief{Nqn: "nqn.2024-01.io.dnv:2:a:b:c:d", Dir: dir}
	hostNqnRead := func(name string) string {
		return "read " + sysfsNvmeCtrlDir + "/" + name + "/hostnqn"
	}
	upToMatch := []string{"list " + dir, hostNqnRead("nvme3"),
		hostNqnRead("nvme4"), hostNqnRead("nvme5")}

	held, err := s.host().HeldWithHostNqn(ctx, brief, ours)
	if err != nil || !held {
		t.Fatalf("HeldWithHostNqn = %v, %v; want held", held, err)
	}
	if !slices.Equal(s.trace, upToMatch) {
		t.Errorf("reads:\n%s\nwant:\n%s", strings.Join(s.trace, "\n"),
			strings.Join(upToMatch, "\n"))
	}
	s.bounded(t)

	// A hostnqn past the match that does not answer is never read.
	s.fail(hostNqnRead("nvme6"))
	if held, err = s.host().HeldWithHostNqn(ctx, brief, ours); err != nil ||
		!held || !slices.Equal(s.trace, upToMatch) {
		t.Errorf("unanswered hostnqn past the match: %v, %v, reads %v",
			held, err, s.trace)
	}

	// One before the match ends the call with its error.
	s.fail(hostNqnRead("nvme4"))
	if held, err = s.host().HeldWithHostNqn(ctx, brief, ours); held ||
		!failedOn(err, hostNqnRead("nvme4")) ||
		!slices.Equal(s.trace, upToMatch[:3]) {
		t.Errorf("unanswered hostnqn before the match: %v, %v, reads %v",
			held, err, s.trace)
	}
	if read, err := s.host().ReadSubsys(ctx, dir, SubsysReadOpts{
		Ctrls:   CtrlHostNqn,
		HostNqn: ours,
		Rule:    ReadTolerant,
	}); read != nil || !failedOn(err, hostNqnRead("nvme4")) {
		t.Errorf("ReadTolerant, unanswered hostnqn: %+v, %v", read, err)
	}
	delete(s.readErr, sysfsNvmeCtrlDir+"/nvme4/hostnqn")
	delete(s.readErr, sysfsNvmeCtrlDir+"/nvme6/hostnqn")

	// No controller holds it: every hostnqn is read, and nothing else.
	held, err = s.host().HeldWithHostNqn(ctx, brief,
		"nqn.2014-08.org.nvmexpress:uuid:nobody")
	if err != nil || held || !slices.Equal(s.trace, append(upToMatch,
		hostNqnRead("nvme6"))) {
		t.Errorf("no match: %v, %v, reads %v", held, err, s.trace)
	}

	path, err := s.host().SubsysDevicePath(ctx, brief)
	if err != nil || path != "/dev/nvme0n1" ||
		!slices.Equal(s.trace, []string{"list " + dir}) {
		t.Errorf("SubsysDevicePath = %q, %v, reads %v", path, err, s.trace)
	}
	s.dirs[dir] = []string{"nvme3", "subsysnqn"}
	if path, err = s.host().SubsysDevicePath(ctx, brief); err != nil ||
		path != "" {
		t.Errorf("no namespace node: %q, %v", path, err)
	}

	s.fail("list " + dir)
	if held, err = s.host().HeldWithHostNqn(ctx, brief, ours); held ||
		!failedOn(err, "list "+dir) {
		t.Errorf("unanswered listing: HeldWithHostNqn = %v, %v", held, err)
	}
	if path, err = s.host().SubsysDevicePath(ctx, brief); path != "" ||
		!failedOn(err, "list "+dir) {
		t.Errorf("unanswered listing: SubsysDevicePath = %q, %v", path, err)
	}
	delete(s.killLs, dir)
	delete(s.dirs, dir)
	if held, err = s.host().HeldWithHostNqn(ctx, brief, ours); held ||
		err != nil {
		t.Errorf("missing directory: HeldWithHostNqn = %v, %v", held, err)
	}
	if path, err = s.host().SubsysDevicePath(ctx, brief); path != "" ||
		err != nil {
		t.Errorf("missing directory: SubsysDevicePath = %q, %v", path, err)
	}
}
