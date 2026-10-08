package cnagent

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The md half of "did not answer is not absent" (CN12; architecture.md,
// Teardown by sweep)
// ---------------------------------------------------------------------------

// mdLookup is one Detail the way a lone group takes it: a fresh walk of
// /sys/block, then the match.
func mdLookup(ctx context.Context, md *Md, names ...string) (*MdDetail, error) {
	walk, err := md.Walk(ctx)
	if err != nil {
		return nil, err
	}
	return md.Detail(ctx, walk, names)
}

// TestMdDetailFromSysfsOnly pins where CN12 and CN28 read an array from:
// /sys/block, which touches no member, and no mdadm runs at all. Not from
// `mdadm --detail`, which loads a superblock from a member device: when that
// member's DN side has gone, the read sits in the multipath head's requeue
// list until the path's failfast expires, and a run the soft timeout killed
// would read the group's md row ERROR for a member fault the leg row already
// reports. md rows count toward cntlr health, so that ERROR would fail the
// primary over; and the same kill, read as "absent", would let a teardown
// skip `mdadm --stop`.
//
// The array is found by member, never by name: it is the one holding any of
// the group's leg wrappers, so either leg finds it and a set naming none of
// its members does not. Two arrays holding legs of one group are an error, not
// a coin flip between them, and a member that is not a dm device makes the
// array Foreign. ListArrays' rule carries over with one exception: a
// /sys/block listing or a read of the matched array that did not answer is an
// error, never "absent" — because absent is the answer that sends the caller
// into an assembly — and so is an unanswered array when no answering array
// matched, or the match stopped since the walk; another array that did not
// answer beside a match is left alone (TestMdWalkUnansweredArray). The md/
// and dm-name kills below hit every array at once, so no array answers and
// matches.
func TestMdDetailFromSysfsOnly(t *testing.T) {
	ctx := context.Background()
	const dev = "/dev/md/dnv-grp"
	const legA = "/dev/mapper/dnv-leg-a"
	const legB = "/dev/mapper/dnv-leg-b"
	node := newFakeNode()
	md := NewMd(node.osClient())

	// Nothing on the node: absent, and not an error.
	detail, err := mdLookup(ctx, md, "dnv-leg-a")
	if err != nil {
		t.Fatalf("an empty node errored: %v", err)
	}
	if detail.Exists {
		t.Fatalf("an empty node read as holding an array: %+v", detail)
	}

	// A rebuilding array: one member in sync, the other being recovered onto.
	node.seedArray(dev, "dnv-grp", legA, legB)
	node.setMdSync(dev, 1, "recover", "100 / 200")
	node.setMemberState(dev, legB, "spare,failfast")
	for _, names := range [][]string{
		{"dnv-leg-a"}, {"dnv-leg-b"}, {"dnv-leg-x", "dnv-leg-b"},
	} {
		detail, err = mdLookup(ctx, md, names...)
		if err != nil {
			t.Fatalf("Detail(%v): %v", names, err)
		}
		if !detail.Exists || detail.Dev != node.mdNode(dev) ||
			detail.State != "clean" || detail.Degraded != 1 ||
			detail.SyncAction != "recover" ||
			detail.SyncCompleted != "100 / 200" || detail.Foreign {
			t.Fatalf("Detail(%v) read %+v, want the array at %s, clean, "+
				"degraded 1, recover at 100 / 200", names, detail,
				node.mdNode(dev))
		}
	}
	byName := make(map[string]MdMember, len(detail.Members))
	for _, member := range detail.Members {
		byName[member.DmName] = member
	}
	for _, want := range []MdMember{
		{DevNo: node.devNo[legA], DmName: "dnv-leg-a", State: "in_sync,failfast"},
		{DevNo: node.devNo[legB], DmName: "dnv-leg-b", State: "spare,failfast"},
	} {
		if got := byName[want.DmName]; got != want {
			t.Errorf("member %s read as %+v, want %+v", want.DmName, got, want)
		}
	}
	if len(detail.Members) != 2 {
		t.Errorf("members = %+v, want exactly the two wrappers", detail.Members)
	}
	for _, call := range node.Calls() {
		if strings.Contains(call, "cmd mdadm") {
			t.Fatalf("Detail ran mdadm: %s", call)
		}
	}

	// Not this group's array.
	if detail, err = mdLookup(ctx, md, "dnv-leg-x"); err != nil ||
		detail.Exists {
		t.Fatalf("a set naming none of the members read as (%+v, %v), "+
			"want absent", detail, err)
	}

	// A member that is not a dm device.
	node.seedArray("/dev/md/mixed", "mixed", "/dev/mapper/dnv-leg-c",
		"/dev/sdb1")
	detail, err = mdLookup(ctx, md, "dnv-leg-c")
	if err != nil {
		t.Fatalf("Detail of the mixed array: %v", err)
	}
	if !detail.Exists || !detail.Foreign {
		t.Fatalf("an array with a non-dm member read as %+v, want Foreign",
			detail)
	}
	if got := detail.foreignErr().Error(); !strings.Contains(
		got, node.devNo["/dev/sdb1"]) {
		t.Errorf("the foreign-member error %q does not name its devno %s",
			got, node.devNo["/dev/sdb1"])
	}

	// Two arrays holding legs of one group.
	node.seedArray("/dev/md/stale", "stale", "/dev/mapper/dnv-leg-d")
	if detail, err = mdLookup(ctx, md,
		"dnv-leg-a", "dnv-leg-d"); err == nil {
		t.Fatalf("two arrays holding legs of one group read as %+v and "+
			"no error", detail)
	}

	// Did not answer: the /sys/block listing, every array's md/ listing,
	// every member's dm name, the array_state that decides present, each
	// running-only attribute, a member's state and a member's block/dev.
	// The md/ and dm-name kills match every array, so no array answers
	// and matches; the rest are reads of the matched array itself.
	for _, kill := range []func(){
		func() { node.killCmdAlways["ls -1 "+sysfsBlockDir] = true },
		func() { node.killCmdAlways["ls -1 "+sysfsBlockDir+"/md"] = true },
		func() { node.killReadAlways["/block/dm/name"] = true },
		func() { node.killReadAlways["/md/array_state"] = true },
		func() { node.killReadAlways["/md/degraded"] = true },
		func() { node.killReadAlways["/md/sync_action"] = true },
		func() { node.killReadAlways["/md/sync_completed"] = true },
		func() { node.failReadAlways["/state"] = true },
		func() { node.killReadAlways["/block/dev"] = true },
	} {
		kill()
		if detail, err = mdLookup(ctx, md, "dnv-leg-a"); err == nil {
			t.Errorf("an unanswered read reported %+v and no error", detail)
		}
		clear(node.killCmdAlways)
		clear(node.killReadAlways)
		clear(node.failReadAlways)
	}

	// A member's block/dev gone between the walk and the read: the member
	// went mid-read, which is an error and never a member with no devno.
	walk, err := md.Walk(ctx)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	devAttr := "/dev-" + node.kernelName(legA) + "/block/dev"
	node.mu.Lock()
	for path := range node.files {
		if strings.HasSuffix(path, devAttr) {
			delete(node.files, path)
		}
	}
	node.mu.Unlock()
	if detail, err = md.Detail(ctx, walk, []string{"dnv-leg-a"}); err == nil {
		t.Errorf("a member whose block/dev vanished read as %+v and no "+
			"error", detail)
	}
}

// TestMdDetailInactiveArray pins the lab kernel's shape of an array that is
// assembled but not running (kernel 7.0, mdadm 4.5): md publishes no
// md/degraded, md/sync_action or md/sync_completed for it at all, and its
// members read a bare "spare". Detail reads array_state first and leaves the
// three unread, so the array comes back present with State "inactive" — the
// state CN28 reports as ERROR — rather than as an ENOENT read error that
// would hide which state it was in.
func TestMdDetailInactiveArray(t *testing.T) {
	ctx := context.Background()
	const dev = "/dev/md/dnv-grp"
	const legA = "/dev/mapper/dnv-leg-a"
	node := newFakeNode()
	md := NewMd(node.osClient())
	node.seedArray(dev, "dnv-grp", legA)
	node.setArrayState(dev, "inactive")
	mdDir := sysfsBlockDir + "/" + strings.TrimPrefix(node.mdNode(dev), "/dev/") +
		"/md"
	for _, attr := range []string{"degraded", "sync_action", "sync_completed"} {
		if _, ok := node.files[mdDir+"/"+attr]; ok {
			t.Fatalf("the fake publishes md/%s for an inactive array, "+
				"which the kernel does not", attr)
		}
	}

	detail, err := mdLookup(ctx, md, "dnv-leg-a")
	if err != nil {
		t.Fatalf("an inactive array errored: %v", err)
	}
	if !detail.Exists || detail.State != "inactive" || detail.Degraded != 0 ||
		detail.SyncAction != "" || detail.SyncCompleted != "" {
		t.Fatalf("an inactive array read as %+v", detail)
	}
	if len(detail.Members) != 1 || detail.Members[0].State != "spare" {
		t.Fatalf("an inactive array's members read as %+v, want one "+
			"\"spare\"", detail.Members)
	}

	// Running again: the three are back, and read.
	node.setArrayState(dev, "clean")
	if detail, err = mdLookup(ctx, md, "dnv-leg-a"); err != nil ||
		detail.SyncAction != "idle" || detail.SyncCompleted != "none" {
		t.Fatalf("a running array read as (%+v, %v)", detail, err)
	}
}

// TestMdWalkRefresh pins Md.Refresh, which keeps the walk a pass shares
// between its groups up to date after the one change the pass itself makes —
// an assembly. It re-walks the nodes that may hold something the walk has
// not seen: a node taken over (another cntlr's converge stops its array and
// this pass's assembly gets the freed mdN, so the walk holds the name with
// another array's member directories), a node the walk recorded with no
// member, a member directory that now carries another dm name (a dm-N name is
// the dm minor, which a new leg wrapper can reuse), and a new node; a missed
// re-walk reads the assembled group "did not start". It drops a node that
// went, which kept would be a second array holding the group's legs. And it
// lists no unchanged node again.
func TestMdWalkRefresh(t *testing.T) {
	ctx := context.Background()
	const legA = "/dev/mapper/dnv-leg-a"
	const legB = "/dev/mapper/dnv-leg-b"
	// stopAndAssemble stops the array at oldDev and assembles one holding
	// member under the node it freed.
	stopAndAssemble := func(node *fakeNode, oldDev, newDev, member string) {
		node.mu.Lock()
		defer node.mu.Unlock()
		old := node.arrays[oldDev]
		node.unpublishArray(old)
		delete(node.arrays, oldDev)
		if _, ok := node.devNo[member]; !ok {
			node.devNo[member] = node.newDevNo()
		}
		array := &fakeArray{name: "ours", node: old.node,
			members: []string{member}}
		node.installArray(newDev, array)
		node.publishArray(array)
	}
	// missThenFind asserts that the stale walk misses the array holding
	// dmName — the case is vacuous otherwise — and that after Refresh it is
	// found at want, and alone.
	missThenFind := func(
		label string, node *fakeNode, md *Md, walk *MdWalk,
		dmName, want string,
	) {
		t.Helper()
		if detail, err := md.Detail(ctx, walk, []string{dmName}); err != nil ||
			detail.Exists {
			t.Fatalf("%s: the stale walk read (%+v, %v); the case is "+
				"vacuous unless it misses the array", label, detail, err)
		}
		if err := md.Refresh(ctx, walk); err != nil {
			t.Fatalf("%s: Refresh: %v", label, err)
		}
		detail, err := md.Detail(ctx, walk, []string{dmName})
		if err != nil || !detail.Exists || detail.Dev != want {
			t.Fatalf("%s: after Refresh the array at %s read (%+v, %v)",
				label, want, detail, err)
		}
	}

	// A node taken over: its recorded member directory is gone.
	node := newFakeNode()
	md := NewMd(node.osClient())
	node.seedArray("/dev/md/other", "other", legA)
	walk, err := md.Walk(ctx)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	taken := node.mdNode("/dev/md/other")
	stopAndAssemble(node, "/dev/md/other", "/dev/md/ours", legB)
	missThenFind("taken over", node, md, walk, "dnv-leg-b", taken)

	// A new node is picked up; an unchanged one is not listed again.
	node.seedArray("/dev/md/new", "new", "/dev/mapper/dnv-leg-c")
	node.Reset()
	if err := md.Refresh(ctx, walk); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	detail, err := md.Detail(ctx, walk, []string{"dnv-leg-c"})
	if err != nil || !detail.Exists {
		t.Fatalf("a new array read (%+v, %v) after Refresh", detail, err)
	}
	unchanged := "cmd ls -1 " + sysfsBlockDir + strings.TrimPrefix(taken, "/dev")
	if got := node.callsMatching(unchanged); len(got) != 0 {
		t.Errorf("Refresh listed an unchanged array again: %v", got)
	}

	// A node recorded with no member: an md/ directory and no dev-* entry,
	// which a check of its members would pass vacuously.
	node = newFakeNode()
	md = NewMd(node.osClient())
	node.seedArray("/dev/md/empty", "empty")
	if walk, err = md.Walk(ctx); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	node.mu.Lock()
	node.devNo[legB] = node.newDevNo()
	empty := node.arrays["/dev/md/empty"]
	empty.members = []string{legB}
	node.publishArray(empty)
	node.mu.Unlock()
	missThenFind("recorded with no member", node, md, walk, "dnv-leg-b",
		node.mdNode("/dev/md/empty"))

	// The same dev-dm-N directory under another dm name: leg-a's wrapper
	// went with its array, and leg-b's took its minor.
	node = newFakeNode()
	md = NewMd(node.osClient())
	node.seedArray("/dev/md/other", "other", legA)
	if walk, err = md.Walk(ctx); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	kname := node.kernelName(legA)
	node.mu.Lock()
	node.devNo[legB] = node.devNo[legA]
	delete(node.devNo, legA)
	node.mu.Unlock()
	if got := node.kernelName(legB); got != kname {
		t.Fatalf("leg-b's member directory is dev-%s, want leg-a's dev-%s",
			got, kname)
	}
	taken = node.mdNode("/dev/md/other")
	stopAndAssemble(node, "/dev/md/other", "/dev/md/ours", legB)
	missThenFind("another dm name", node, md, walk, "dnv-leg-b", taken)

	// A node that went: the array stops, and its leg is assembled again
	// under a new node. The stale entry must not be a second match.
	node = newFakeNode()
	md = NewMd(node.osClient())
	node.seedArray("/dev/md/old", "old", legA)
	if walk, err = md.Walk(ctx); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	node.mu.Lock()
	node.unpublishArray(node.arrays["/dev/md/old"])
	delete(node.arrays, "/dev/md/old")
	node.mu.Unlock()
	node.seedArray("/dev/md/again", "again", legA)
	missThenFind("a node that went", node, md, walk, "dnv-leg-a",
		node.mdNode("/dev/md/again"))
}

// TestMdWalkUnansweredArray pins that the walk, which reads every array on
// the node, never fails over ANOTHER array that did not answer — its md/
// listing killed (the soft timeout), or a member's dm-name read failing with
// an error that is not ENOENT — and that an unanswered array can still hide
// the group's own: with no answering array holding the names, Detail is an
// error, never absent. That includes a match whose md/ has gone by the read
// (stopped since the walk; a stop removes md/ whole, fakeNode.dropMdDir):
// beside an unanswered array it is the same error, and only with every array
// answering does it read absent. Once the array answers again, Refresh walks
// it and drops the stopped one.
func TestMdWalkUnansweredArray(t *testing.T) {
	ctx := context.Background()
	const ours = "/dev/mapper/dnv-leg-a"
	const theirs = "/dev/mapper/dnv-leg-x"
	for _, tc := range []struct {
		name string
		kill func(node *fakeNode)
	}{
		{"member dm name", func(node *fakeNode) {
			node.failReadAlways["/dev-"+node.kernelName(theirs)+
				"/block/dm/name"] = true
		}},
		{"md listing", func(node *fakeNode) {
			node.killCmdAlways["ls -1 "+sysfsBlockDir+strings.TrimPrefix(
				node.mdNode("/dev/md/theirs"), "/dev")+"/md"] = true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := newFakeNode()
			md := NewMd(node.osClient())
			node.seedArray("/dev/md/ours", "ours", ours)
			node.seedArray("/dev/md/theirs", "theirs", theirs)
			tc.kill(node)

			walk, err := md.Walk(ctx)
			if err != nil {
				t.Fatalf("another array that did not answer failed the "+
					"walk: %v", err)
			}
			detail, err := md.Detail(ctx, walk, []string{"dnv-leg-a"})
			if err != nil || !detail.Exists ||
				detail.Dev != node.mdNode("/dev/md/ours") {
				t.Fatalf("beside an unanswered array the group's own read "+
					"(%+v, %v), want the array at %s", detail, err,
					node.mdNode("/dev/md/ours"))
			}
			// No answering array holds these names; the unanswered one
			// may, so absent would be a guess.
			for _, names := range [][]string{{"dnv-leg-x"}, {"dnv-leg-y"}} {
				detail, err = md.Detail(ctx, walk, names)
				if err == nil ||
					!strings.Contains(err.Error(), "did not answer") {
					t.Fatalf("Detail(%v) beside an unanswered array read "+
						"(%+v, %v), want a did-not-answer error", names,
						detail, err)
				}
			}
			// The group's own array stops between the walk and the read:
			// its md/ goes whole — the members unbound, then the md
			// kobject — while /sys/block still lists the node, and absent
			// would again be a guess.
			ourNode := strings.TrimPrefix(node.mdNode("/dev/md/ours"), "/dev/")
			node.dropMdDir("/dev/md/ours")
			detail, err = md.Detail(ctx, walk, []string{"dnv-leg-a"})
			if err == nil || !strings.Contains(err.Error(), "did not answer") {
				t.Fatalf("a match stopped since the walk, beside an "+
					"unanswered array, read (%+v, %v), want a "+
					"did-not-answer error", detail, err)
			}

			clear(node.failReadAlways)
			clear(node.killCmdAlways)
			if err := md.Refresh(ctx, walk); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			if _, held := walk.arrays[ourNode]; held {
				t.Fatalf("Refresh kept a record of %s, whose md/ went",
					ourNode)
			}
			if detail, err = md.Detail(ctx, walk,
				[]string{"dnv-leg-a"}); err != nil || detail.Exists {
				t.Fatalf("with every array answering, names no array "+
					"holds any more read (%+v, %v), want absent", detail,
					err)
			}
			detail, err = md.Detail(ctx, walk, []string{"dnv-leg-x"})
			if err != nil || !detail.Exists ||
				detail.Dev != node.mdNode("/dev/md/theirs") {
				t.Fatalf("answering again, the array read (%+v, %v), want "+
					"it at %s", detail, err, node.mdNode("/dev/md/theirs"))
			}
			if detail, err = md.Detail(ctx, walk,
				[]string{"dnv-leg-y"}); err != nil || detail.Exists {
				t.Fatalf("with every array answering, a name none holds "+
					"read (%+v, %v), want absent", detail, err)
			}

			// And with every array answering, a match whose md/ goes
			// after the walk reads absent with no Refresh in between:
			// the walk still holds it, and the read finds it stopped.
			fresh, err := md.Walk(ctx)
			if err != nil || len(fresh.unanswered) != 0 {
				t.Fatalf("a walk with every array answering read "+
					"(%v unanswered, %v)", fresh.unanswered, err)
			}
			if detail, err = md.Detail(ctx, fresh,
				[]string{"dnv-leg-x"}); err != nil || !detail.Exists {
				t.Fatalf("before its stop the array read (%+v, %v); the "+
					"case is vacuous unless the walk matches it", detail,
					err)
			}
			node.dropMdDir("/dev/md/theirs")
			if detail, err = md.Detail(ctx, fresh,
				[]string{"dnv-leg-x"}); err != nil || detail.Exists {
				t.Fatalf("with every array answering, a match stopped "+
					"since the walk read (%+v, %v), want absent", detail,
					err)
			}
		})
	}
}

// TestMdWalkRefreshUnanswered pins Refresh's bookkeeping of unanswered nodes
// (CN12). A node recorded as unanswered that has since gone is dropped with
// it: kept, it would make every later lookup that matches nothing an error.
// A stopping array's md/ goes before its /sys/block node does
// (fakeNode.dropMdDir), and a node still listed with no md/ answers "no":
// the walk drops it and records nothing unanswered, whether it had not seen
// the node, had recorded it — kept, the stale record would be a second array
// holding a leg assembled again elsewhere — or had recorded it as
// unanswered. And a node the walk recorded whose re-walk does not answer
// loses its record, never keeps it: its member directory may now carry
// another dm name — a dm-N name is the dm minor, which a new leg wrapper can
// reuse — and the stale record would hand a lookup that array as the group's
// own, with the stale dm name on a member that is another device.
func TestMdWalkRefreshUnanswered(t *testing.T) {
	ctx := context.Background()
	const legA = "/dev/mapper/dnv-leg-a"
	const legB = "/dev/mapper/dnv-leg-b"
	mdListing := func(node *fakeNode, dev string) string {
		return "ls -1 " + sysfsBlockDir +
			strings.TrimPrefix(node.mdNode(dev), "/dev") + "/md"
	}

	nodeOf := func(node *fakeNode, dev string) string {
		return strings.TrimPrefix(node.mdNode(dev), "/dev/")
	}
	absent := func(t *testing.T, md *Md, walk *MdWalk, label string) {
		t.Helper()
		if detail, err := md.Detail(ctx, walk,
			[]string{"dnv-leg-y"}); err != nil || detail.Exists {
			t.Fatalf("%s: a name no array holds read (%+v, %v), want "+
				"absent", label, detail, err)
		}
	}

	t.Run("listed node with no md/", func(t *testing.T) {
		node := newFakeNode()
		md := NewMd(node.osClient())
		node.seedArray("/dev/md/ours", "ours", legA)
		node.dirs[sysfsBlockDir+"/md99"] = true
		walk, err := md.Walk(ctx)
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if _, ok := walk.arrays["md99"]; ok {
			t.Fatalf("a node with no md/ was recorded as an array")
		}
		if err := walk.unanswered["md99"]; err != nil {
			t.Fatalf("a node with no md/ was recorded as unanswered: %v",
				err)
		}
		absent(t, md, walk, "a node with no md/ listed")
	})

	t.Run("recorded node whose md/ went", func(t *testing.T) {
		node := newFakeNode()
		md := NewMd(node.osClient())
		node.seedArray("/dev/md/theirs", "theirs", legA)
		walk, err := md.Walk(ctx)
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		gone := nodeOf(node, "/dev/md/theirs")
		if _, ok := walk.arrays[gone]; !ok {
			t.Fatalf("the walk did not record %s; the case is vacuous", gone)
		}
		node.dropMdDir("/dev/md/theirs")
		// Its leg assembled again under a new node before the Refresh.
		node.seedArray("/dev/md/again", "again", legA)
		if err := md.Refresh(ctx, walk); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if _, ok := walk.arrays[gone]; ok {
			t.Fatalf("Refresh kept the record of %s, whose md/ went", gone)
		}
		if err := walk.unanswered[gone]; err != nil {
			t.Fatalf("Refresh recorded %s, whose md/ went, as unanswered: "+
				"%v", gone, err)
		}
		detail, err := md.Detail(ctx, walk, []string{"dnv-leg-a"})
		if err != nil || !detail.Exists ||
			detail.Dev != node.mdNode("/dev/md/again") {
			t.Fatalf("the leg assembled again read (%+v, %v), want the "+
				"array at %s alone", detail, err,
				node.mdNode("/dev/md/again"))
		}
		absent(t, md, walk, "a recorded node whose md/ went")
	})

	t.Run("unanswered node whose md/ went", func(t *testing.T) {
		node := newFakeNode()
		md := NewMd(node.osClient())
		node.seedArray("/dev/md/theirs", "theirs", legA)
		gone := nodeOf(node, "/dev/md/theirs")
		node.killCmdAlways[mdListing(node, "/dev/md/theirs")] = true
		walk, err := md.Walk(ctx)
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if walk.unanswered[gone] == nil {
			t.Fatalf("the walk did not record %s as unanswered; the case "+
				"is vacuous", gone)
		}
		clear(node.killCmdAlways)
		node.dropMdDir("/dev/md/theirs")
		if err := md.Refresh(ctx, walk); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if err := walk.unanswered[gone]; err != nil {
			t.Fatalf("Refresh kept %s, whose md/ went, unanswered: %v",
				gone, err)
		}
		if _, ok := walk.arrays[gone]; ok {
			t.Fatalf("Refresh recorded %s, whose md/ went, as an array",
				gone)
		}
		absent(t, md, walk, "an unanswered node whose md/ went")
	})

	t.Run("unanswered node that went", func(t *testing.T) {
		node := newFakeNode()
		md := NewMd(node.osClient())
		node.seedArray("/dev/md/theirs", "theirs", legA)
		node.killCmdAlways[mdListing(node, "/dev/md/theirs")] = true
		walk, err := md.Walk(ctx)
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if _, err := md.Detail(ctx, walk, []string{"dnv-leg-y"}); err == nil {
			t.Fatalf("the unanswered array was not recorded; the case " +
				"is vacuous")
		}
		node.mu.Lock()
		node.unpublishArray(node.arrays["/dev/md/theirs"])
		delete(node.arrays, "/dev/md/theirs")
		node.mu.Unlock()
		if err := md.Refresh(ctx, walk); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if detail, err := md.Detail(ctx, walk,
			[]string{"dnv-leg-y"}); err != nil || detail.Exists {
			t.Fatalf("with the unanswered array gone, a name none holds "+
				"read (%+v, %v), want absent", detail, err)
		}
	})

	for _, tc := range []struct {
		name string
		// fault makes the recorded node's check fail and its re-walk not
		// answer; lookup is the dm name the stale record would match, and
		// holds the one the array's member carries once it answers.
		fault  func(node *fakeNode)
		lookup string
		holds  string
	}{
		{"member dm name unreadable", func(node *fakeNode) {
			node.failReadAlways["/dev-"+node.kernelName(legA)+
				"/block/dm/name"] = true
		}, "dnv-leg-a", "dnv-leg-a"},
		{"minor reused, md listing killed", func(node *fakeNode) {
			node.mu.Lock()
			node.devNo[legB] = node.devNo[legA]
			delete(node.devNo, legA)
			array := node.arrays["/dev/md/theirs"]
			array.members = []string{legB}
			node.publishArray(array)
			node.mu.Unlock()
			node.killCmdAlways[mdListing(node, "/dev/md/theirs")] = true
		}, "dnv-leg-a", "dnv-leg-b"},
	} {
		t.Run("recorded node, "+tc.name, func(t *testing.T) {
			node := newFakeNode()
			md := NewMd(node.osClient())
			node.seedArray("/dev/md/theirs", "theirs", legA)
			walk, err := md.Walk(ctx)
			if err != nil {
				t.Fatalf("Walk: %v", err)
			}
			if detail, err := md.Detail(ctx, walk,
				[]string{tc.lookup}); err != nil || !detail.Exists {
				t.Fatalf("the walk did not record the array (%+v, %v); "+
					"the case is vacuous", detail, err)
			}
			tc.fault(node)
			if err := md.Refresh(ctx, walk); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			detail, err := md.Detail(ctx, walk, []string{tc.lookup})
			if err == nil || !strings.Contains(err.Error(), "did not answer") {
				t.Fatalf("a recorded array whose re-walk did not answer "+
					"read (%+v, %v), want a did-not-answer error", detail,
					err)
			}

			clear(node.failReadAlways)
			clear(node.killCmdAlways)
			if err := md.Refresh(ctx, walk); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			detail, err = md.Detail(ctx, walk, []string{tc.holds})
			if err != nil || !detail.Exists ||
				detail.Dev != node.mdNode("/dev/md/theirs") ||
				len(detail.Members) != 1 ||
				detail.Members[0].DmName != tc.holds {
				t.Fatalf("answering again, the array read (%+v, %v), want "+
					"it at %s holding %s", detail, err,
					node.mdNode("/dev/md/theirs"), tc.holds)
			}
		})
	}
}

// TestMdStateLine pins the CN28 details of a running array. The suites read
// the words — the e2e grp_md_clean wait fails on degraded, recovering and
// resyncing — so they are mdadm's State-line words; the state itself and the
// sectors suffix are sysfs's own. The word needs a sync that is actually
// running: sync_completed reads "none" exactly when none is, and the lab saw
// sync_action "recover" with "none" for seconds on a degraded array that had
// nothing to rebuild onto.
func TestMdStateLine(t *testing.T) {
	for _, tc := range []struct {
		state, action, completed string
		degraded                 int
		want                     string
	}{
		{"clean", "idle", "none", 0, "clean"},
		{"clean", "idle", "none", 1, "clean, degraded"},
		{"active", "recover", "100 / 200", 1,
			"active, degraded, recovering (100 / 200)"},
		{"clean", "resync", "130944 / 2093056", 0,
			"clean, resyncing (130944 / 2093056)"},
		{"clean", "check", "0 / 2093056", 0, "clean, checking (0 / 2093056)"},
		{"clean", "repair", "5 / 10", 0, "clean, repairing (5 / 10)"},
		{"clean", "reshape", "5 / 10", 0, "clean, reshaping (5 / 10)"},
		// Nothing to rebuild onto: no word, but still degraded.
		{"write-pending", "recover", "none", 1, "write-pending, degraded"},
		// The first sample after a bitmap re-add: done, not yet idle.
		{"clean", "recover", "2093056 / 2093056", 1,
			"clean, degraded, recovering (2093056 / 2093056)"},
		{"clean", "frozen", "none", 0, "clean"},
		{"clean", "idle", "5 / 10", 0, "clean"},
		{"readonly", "idle", "none", 0, "readonly"},
	} {
		got := mdStateLine(&MdDetail{
			Exists: true, State: tc.state, Degraded: tc.degraded,
			SyncAction: tc.action, SyncCompleted: tc.completed,
		})
		if got != tc.want {
			t.Errorf("%s/%d/%s/%q: got %q, want %q", tc.state, tc.degraded,
				tc.action, tc.completed, got, tc.want)
		}
	}
}

// TestMdHasSuperblockKilledIsAnError pins the most destructive reading of a
// killed probe in the tree. `mdadm --examine` opens and reads the member
// device, so it blocks on a dead leg until failfast, and its answer selects
// between the two assembly cases of cnagent.md CN12: create, or assemble. "No
// superblock" on every available member means case 1, and case 1 is `mdadm
// --create --assume-clean` — over whatever those members already hold. So a
// kill read as "no superblock" does not leak anything; it destroys the
// group's data.
//
// The second half drives a whole converge, because the property that matters
// is not that the primitive returns an error but that the decision above it
// refuses: with no answer this pass cannot tell case 1 from case 2, and
// guessing is not allowed.
func TestMdHasSuperblockKilledIsAnError(t *testing.T) {
	ctx := context.Background()
	const member = "/dev/mapper/dnv-leg-a"
	node := newFakeNode()
	md := NewMd(node.osClient())

	// The tool ran and answered: this device carries no md metadata. That is
	// the case 1 answer of cnagent.md CN12, not a failure — a freshly zeroed
	// side.
	has, err := md.HasSuperblock(ctx, member)
	if err != nil {
		t.Fatalf("a device without a superblock errored: %v", err)
	}
	if has {
		t.Fatal("a device without a superblock read as carrying one")
	}
	node.superblocks[member] = true
	if has, err = md.HasSuperblock(ctx, member); err != nil || !has {
		t.Fatalf("a device carrying a superblock read as (%v, %v)", has, err)
	}
	node.killCmdAlways["mdadm --examine"] = true
	if has, err = md.HasSuperblock(ctx, member); err == nil {
		t.Fatalf("a killed `mdadm --examine` read as has=%v, which is the "+
			"answer that creates over live data", has)
	}

	// The refusal, through the converge that would otherwise create. Every
	// `--examine` of the pass is killed, so no member can be told apart from
	// a freshly zeroed one.
	srv, srvNode := newTestServer(t)
	srvNode.killCmdAlways["mdadm --examine"] = true
	reply := syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, raid1: true, twoLegs: true})
	created := srvNode.callsMatching("cmd mdadm --create")
	if len(created) != 0 {
		t.Fatalf("an array was created although no member could be "+
			"examined: %v", created)
	}
	info := reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp]
	if info.GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
		t.Fatalf("the group reported %v, want ERROR", info.GetStatus())
	}
}

// TestMdNameInUseKilledIsAnError pins the answer of CN12's other case-1
// guard. NameInUse asks whether an array already runs under /dev/md/<name>,
// and its "no" is what lets assembleGroup go on to `mdadm --create
// --assume-clean`: a create beside a running array puts a second array under
// the name the pool's concat resolves. So an lsblk that did not answer is an
// error, never a "no" — here it hides a "yes". The converge half is in
// TestGroupNeverCreatesBesideARunningArray.
func TestMdNameInUseKilledIsAnError(t *testing.T) {
	ctx := context.Background()
	const dev = "/dev/md/dnv-grp"
	node := newFakeNode()
	md := NewMd(node.osClient())

	if inUse, err := md.NameInUse(ctx, dev); err != nil || inUse {
		t.Fatalf("a name no node answers to read as (%v, %v), want "+
			"(false, nil)", inUse, err)
	}
	node.devNo[dev] = node.newDevNo()
	if inUse, err := md.NameInUse(ctx, dev); err != nil || !inUse {
		t.Fatalf("a name an array runs under read as (%v, %v), want "+
			"(true, nil)", inUse, err)
	}
	node.killCmdAlways["lsblk --nodeps --noheadings --output MAJ:MIN "+dev] =
		true
	if inUse, err := md.NameInUse(ctx, dev); err == nil {
		t.Fatalf("a killed lsblk read as inUse=%v, and a \"no\" there "+
			"creates beside the running array", inUse)
	}
}

// ---------------------------------------------------------------------------
// The sysfs enumerators the sweep uses instead of mdadm
// ---------------------------------------------------------------------------

// TestMdListArraysFromSysfsOnly pins that the sweep's array enumerator never
// runs mdadm. Every mdadm probe opens a member device, and a sweep runs
// exactly when a member's DN side is gone — so `mdadm --detail --scan`, the
// obvious enumerator, is the one thing that must not be used. sysfs answers
// the three questions the sweep has without touching a member: which arrays
// exist, whether each still holds its members, and which dm devices those
// members are.
//
// Attribution is the reason the member names are read at all: an md name
// carries no sp id, so an array belongs to the sp of the leg wrappers among
// its members. An array with a member that is not a dm device is Foreign and
// is never attributed and never stopped — that is what keeps a co-hosted
// dn agent's, or the operator's own, udev-assembled array untouched.
//
// And an array that vanished between the listing and its reads is dropped
// rather than reported as an error: a concurrent teardown finishing its work
// is the normal case, not a failure of this pass.
func TestMdListArraysFromSysfsOnly(t *testing.T) {
	ctx := context.Background()
	const ours = "/dev/md/dnv-grp"
	node := newFakeNode()
	node.seedArray(ours, "dnv-grp",
		"/dev/mapper/dnv-leg-a", "/dev/mapper/dnv-leg-b")
	node.seedArray("/dev/md/foreign", "foreign", "/dev/sdb1")
	// An array node whose md/ directory is already gone: the listing named
	// it, the reads find nothing.
	node.dirs[sysfsBlockDir+"/md99"] = true

	arrays, err := NewMd(node.osClient()).ListArrays(ctx)
	if err != nil {
		t.Fatalf("ListArrays: %v", err)
	}
	for _, call := range node.Calls() {
		if strings.Contains(call, "cmd mdadm") {
			t.Fatalf("the sysfs enumerator ran mdadm: %s", call)
		}
	}
	if len(arrays) != 2 {
		t.Fatalf("arrays = %+v, want ours and the foreign one only "+
			"(the vanished node is dropped)", arrays)
	}

	byDev := make(map[string]MdArray, len(arrays))
	for _, array := range arrays {
		byDev[array.Dev] = array
	}
	// The sweep only ever has the kernel's own spelling: it enumerates
	// /sys/block, and /dev/md/{name} is a udev symlink it never sees.
	array, ok := byDev[node.mdNode(ours)]
	if !ok {
		t.Fatalf("our array is missing from %+v, want it at %s",
			arrays, node.mdNode(ours))
	}
	if array.Foreign {
		t.Error("an array of dm members only was reported foreign")
	}
	if array.State != "clean" {
		t.Errorf("array_state = %q, want it verbatim", array.State)
	}
	members := append([]string(nil), array.Members...)
	sort.Strings(members)
	if len(members) != 2 || members[0] != "dnv-leg-a" ||
		members[1] != "dnv-leg-b" {
		t.Errorf("members = %v, want the two wrappers by dm name", members)
	}

	foreign, ok := byDev[node.mdNode("/dev/md/foreign")]
	if !ok {
		t.Fatalf("the foreign array is missing from %+v", arrays)
	}
	if !foreign.Foreign {
		t.Error("an array with a non-dm member was not reported foreign")
	}
	if len(foreign.Members) != 0 {
		t.Errorf("a non-dm member was named as a dm device: %v",
			foreign.Members)
	}

	// A listing that did not answer is an error, never "this node runs no
	// arrays" — the answer that would let a sweep walk past every array it
	// should have stopped.
	node.killCmdAlways["ls -1 "+sysfsBlockDir] = true
	if arrays, err = NewMd(node.osClient()).ListArrays(ctx); err == nil {
		t.Fatalf("a killed listing reported %+v and no error", arrays)
	}
	clear(node.killCmdAlways)

	// So is one array that did not answer — its md/ listing, a member's dm
	// name or its array_state (CN21: strict, never Md.Walk's unanswered
	// rule): a removal decision needs the whole node, and an array left out
	// would read as nothing to stop. The walk over the same node records
	// the array as unanswered instead of failing, for the two faults it
	// reads at all.
	oursNode := strings.TrimPrefix(node.mdNode(ours), "/dev/")
	for _, tc := range []struct {
		name string
		kill func()
		walk bool // Md.Walk reads it too, and records the array unanswered
	}{
		{"md listing", func() {
			node.killCmdAlways["ls -1 "+sysfsBlockDir+"/"+oursNode+"/md"] =
				true
		}, true},
		{"member dm name", func() {
			node.failReadAlways["/dev-"+
				node.kernelName("/dev/mapper/dnv-leg-a")+
				"/block/dm/name"] = true
		}, true},
		{"array_state", func() {
			node.killReadAlways[sysfsBlockDir+"/"+oursNode+
				"/md/array_state"] = true
		}, false},
	} {
		tc.kill()
		if arrays, err = NewMd(node.osClient()).ListArrays(ctx); err == nil {
			t.Errorf("%s did not answer: ListArrays reported %+v and no "+
				"error", tc.name, arrays)
		}
		if tc.walk {
			walk, err := NewMd(node.osClient()).Walk(ctx)
			if err != nil {
				t.Errorf("%s did not answer: the walk failed: %v",
					tc.name, err)
			} else if walk.unanswered[oursNode] == nil {
				t.Errorf("%s did not answer: the walk did not record %s "+
					"as unanswered", tc.name, oursNode)
			}
		}
		clear(node.killCmdAlways)
		clear(node.failReadAlways)
		clear(node.killReadAlways)
	}
}

// TestMdNamedKernelNode pins that an array whose kernel node is named, not
// numbered, is found by every reader of /sys/block (CN12). With mdadm.conf
// `CREATE names=yes`, mdadm creates /dev/md/<name> on the kernel node
// md_<name> (architecture.md, md names, sizes CnMdDevName for it), and
// /sys/block lists md_<name>. The md rows are read from the walk: a walk
// listing md[0-9]+ alone would read the group's running array as absent on
// every Check round and send every converge into an assembly of an array
// already active — ERROR on every md row of the cn — and the sweep would
// never stop such an array either (the stop itself is
// TestSweepStopsANamedArrayNode's).
func TestMdNamedKernelNode(t *testing.T) {
	ctx := context.Background()
	const named = "md_0123456789abcdef0123456789ab"
	const legA = "/dev/mapper/dnv-leg-a"
	const legB = "/dev/mapper/dnv-leg-b"
	node := newFakeNode()
	md := NewMd(node.osClient())
	node.mu.Lock()
	for _, member := range []string{legA, legB} {
		node.devNo[member] = node.newDevNo()
	}
	array := &fakeArray{name: "dnv-grp", node: named,
		members: []string{legA, legB}}
	node.installArray("/dev/md/dnv-grp", array)
	node.publishArray(array)
	node.mu.Unlock()
	node.seedArray("/dev/md/numbered", "numbered", "/dev/mapper/dnv-leg-c")
	if got := node.mdNode("/dev/md/dnv-grp"); got != "/dev/"+named {
		t.Fatalf("the fake put the array at %s, want /dev/%s", got, named)
	}

	detail, err := mdLookup(ctx, md, "dnv-leg-b")
	if err != nil || !detail.Exists || detail.Dev != "/dev/"+named ||
		detail.State != "clean" || len(detail.Members) != 2 {
		t.Fatalf("the named array read (%+v, %v), want it at /dev/%s, "+
			"clean, with both members", detail, err, named)
	}
	arrays, err := md.ListArrays(ctx)
	if err != nil {
		t.Fatalf("ListArrays: %v", err)
	}
	byDev := make(map[string]MdArray, len(arrays))
	for _, array := range arrays {
		byDev[array.Dev] = array
	}
	if len(arrays) != 2 || len(byDev["/dev/"+named].Members) != 2 ||
		len(byDev[node.mdNode("/dev/md/numbered")].Members) != 1 {
		t.Fatalf("ListArrays = %+v, want the named array and the "+
			"numbered one", arrays)
	}
	if gone, err := md.Gone(ctx, "/dev/"+named); err != nil || gone {
		t.Fatalf("Gone(/dev/%s) of the running array read (%v, %v), "+
			"want (false, nil)", named, gone, err)
	}
}

// TestMdBlockEntryPattern pins which /sys/block entries are array nodes
// (CN12). md names its disks mdN, md_dN (a partitionable array) or md_<name>
// (mdadm.conf `CREATE names=yes`), and no other in-tree driver names a disk
// md*. A <name> need not be hex: under `CREATE names=yes`, an array mdadm
// names from its superblock name — what `--assemble --scan` or incremental
// assembly does — runs on md_<that name>, for a dnv array
// md_dnv-<sp>-<slice>-<grp>, with a `_N` suffix after a name conflict; an
// array on our legs the sweep never lists is never stopped. Everything else
// there — dm-N, nvmeXnY and its hidden nvmeXcYnZ paths, sdX, loopN — must
// stay out: the walk lists every matching entry's md/ with an `ls` each, a
// process under the agent's shared OsClient semaphore, and a listing that
// does not answer can refuse an assembly (TestGroupProbeWalksOnce and
// TestGroupConvergeWalksOnce pin that neither reader touches one). A
// partition of a partitionable array, md_d0p1, is no top-level entry:
// /sys/block links whole disks only, and the partition lives under its disk.
func TestMdBlockEntryPattern(t *testing.T) {
	for _, entry := range []string{
		"md0", "md127", "md_d0", "md_0123456789abcdef0123456789ab",
		"md_dnv-0000000000000002-00-00", "md_dnv-0000000000000002-00-00_0",
	} {
		if !mdBlockEntryPattern.MatchString(entry) {
			t.Errorf("%s is an array node and did not match", entry)
		}
	}
	for _, entry := range []string{
		"md", "mdp", "md0p1", "dm-0", "nvme0n1", "nvme0c0n1", "sda",
		"loop0", "xmd0",
	} {
		if mdBlockEntryPattern.MatchString(entry) {
			t.Errorf("%s is no array node and matched", entry)
		}
	}
}

// TestMdWalkUnboundMember pins why an array another cntlr is stopping is not
// unanswered (CN12): md has unbound its member — the block link is gone, so
// dev-*/block/dev and dev-*/block/dm/name read ENOENT, and the member's own
// dev-*/state reads ENODEV (fakeNode.unbindMember). The walk answers and
// records that member with no dm name, which no group's names match, so a
// lookup of the member's old dm name, or of a name no array holds, reads
// absent, and the group beside it reads its own array. In this shape the
// sweep's ListArrays answers too and reads the array as foreign, which it
// never stops — not for the whole stop: once md marks the array deleted, its
// array_state reads EBUSY until md/ goes, and ListArrays fails that pass
// (CN12). A walk that read the missing block/dev as "did not answer" would
// refuse every assembly on the node while the stop runs. The kernel never
// binds an unbound member again, and the fake holds that through a
// republish.
func TestMdWalkUnboundMember(t *testing.T) {
	ctx := context.Background()
	const ours = "/dev/mapper/dnv-leg-a"
	const theirs = "/dev/mapper/dnv-leg-x"
	node := newFakeNode()
	md := NewMd(node.osClient())
	node.seedArray("/dev/md/ours", "ours", ours)
	node.seedArray("/dev/md/theirs", "theirs", theirs)
	node.unbindMember("/dev/md/theirs", theirs)

	walk, err := md.Walk(ctx)
	if err != nil {
		t.Fatalf("the walk beside an unbound member failed: %v", err)
	}
	if len(walk.unanswered) != 0 {
		t.Fatalf("the walk recorded %v as unanswered, want none",
			walk.unanswered)
	}
	for _, name := range []string{"dnv-leg-x", "dnv-leg-y"} {
		detail, err := md.Detail(ctx, walk, []string{name})
		if err != nil || detail.Exists {
			t.Fatalf("Detail(%s) beside an unbound member read (%+v, %v), "+
				"want absent", name, detail, err)
		}
	}
	detail, err := md.Detail(ctx, walk, []string{"dnv-leg-a"})
	if err != nil || !detail.Exists ||
		detail.Dev != node.mdNode("/dev/md/ours") {
		t.Fatalf("the group's own array read (%+v, %v), want it at %s",
			detail, err, node.mdNode("/dev/md/ours"))
	}

	arrays, err := md.ListArrays(ctx)
	if err != nil {
		t.Fatalf("ListArrays beside an unbound member failed: %v", err)
	}
	var found bool
	for _, array := range arrays {
		if array.Dev != node.mdNode("/dev/md/theirs") {
			continue
		}
		found = true
		if !array.Foreign || len(array.Members) != 0 {
			t.Fatalf("the array mid-stop read %+v, want it foreign with no "+
				"dm member", array)
		}
	}
	if !found {
		t.Fatalf("ListArrays = %+v left out the array mid-stop", arrays)
	}

	node.setMdSync("/dev/md/theirs", 1, "idle", "none")
	if detail, err := mdLookup(ctx, md, "dnv-leg-x"); err != nil ||
		detail.Exists {
		t.Fatalf("after a republish the unbound member's old name read "+
			"(%+v, %v), want absent: the member stays unbound", detail, err)
	}
}

// TestMdGoneOnlyForClearOrAbsent pins the verification of a stop. `mdadm
// --stop` may be killed after the kernel has already stopped the array, and
// it may be killed having done nothing, so its exit status is not evidence
// and this read is the only thing that is.
//
// Which makes the vocabulary the whole content of the test: array_state
// "inactive" is an array that is assembled but not running, and it holds its
// members just as hard as a running one. Reading it as gone would make the
// sweep descend to the layer below (D8), find `dmsetup remove` fail EBUSY on
// the leg wrappers it still pins, and — because the array is "gone" — never
// stop it on any later pass either. The wrappers would be un-removable for
// ever.
func TestMdGoneOnlyForClearOrAbsent(t *testing.T) {
	ctx := context.Background()
	const dev = "/dev/md/dnv-grp"
	node := newFakeNode()
	md := NewMd(node.osClient())

	// No /sys/block entry at all: the array is gone.
	gone, err := md.Gone(ctx, "/dev/md42")
	if err != nil || !gone {
		t.Fatalf("an absent array read as (%v, %v), want (true, nil)",
			gone, err)
	}

	node.seedArray(dev, "dnv-grp", "/dev/mapper/dnv-leg-a")
	mdDev := node.mdNode(dev)
	for _, tc := range []struct {
		state string
		gone  bool
	}{
		{"clean", false},
		{"active", false},
		// The one that matters.
		{"inactive", false},
		{"clear", true},
	} {
		node.setArrayState(dev, tc.state)
		gone, err = md.Gone(ctx, mdDev)
		if err != nil {
			t.Fatalf("array_state %q: Gone: %v", tc.state, err)
		}
		if gone != tc.gone {
			t.Errorf("array_state %q read as gone=%v, want %v",
				tc.state, gone, tc.gone)
		}
	}

	// A read that did not answer is an error: "gone" is the verdict that
	// ends the sweep's interest in this array.
	node.killReadAlways["/md/array_state"] = true
	if gone, err = md.Gone(ctx, mdDev); err == nil {
		t.Fatalf("a stalled array_state read as gone=%v and no error", gone)
	}
}
