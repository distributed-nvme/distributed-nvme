package cnagent

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The md half of "did not answer is not absent" (plan §1.2, §3.3.1)
// ---------------------------------------------------------------------------

// TestMdDetailKilledIsNotAbsent pins the primitive the whole teardown defect
// started at. `mdadm --detail` opens the array's member devices and loads a
// superblock from one of them, so on a leg whose DN side is gone it sits in
// the nvme multipath head's requeue list until fast_io_fail_tmo expires —
// past the SH15 soft timeout — and is killed. Reading that kill as "the array
// is not there" is what let a teardown skip `mdadm --stop`, leave the array
// pinning its two leg wrappers, fail their `dmsetup remove` with EBUSY,
// forget the cntlr anyway and reply OK. Nothing ever looked again.
//
// The other half is just as load-bearing: a non-zero exit really does mean
// the array is not running, and assembly is exactly what fixes that. A
// Detail that turned every refusal into an error would fail every first
// converge of every group.
func TestMdDetailKilledIsNotAbsent(t *testing.T) {
	ctx := context.Background()
	const dev = "/dev/md/dnv-grp"
	const member = "/dev/mapper/dnv-leg-a"
	node := newFakeNode()
	md := NewMd(node.osClient())

	// The tool ran and answered: no such array.
	detail, err := md.Detail(ctx, dev)
	if err != nil {
		t.Fatalf("a reported \"no such array\" came back as an error: %v", err)
	}
	if detail.Exists {
		t.Fatal("an array that is not running read as present")
	}

	// A running array, so the absence above is not vacuous.
	node.seedArray(dev, "dnv-grp", member)
	detail, err = md.Detail(ctx, dev)
	if err != nil {
		t.Fatalf("Detail of a running array: %v", err)
	}
	if !detail.Exists || detail.State != "clean" ||
		len(detail.Devices) != 1 || detail.Devices[0] != member {
		t.Fatalf("running array read as %+v", detail)
	}

	// Killed. The array is still there — the fake's state is untouched —
	// which is exactly the situation the caller must not be told anything
	// about.
	node.killCmdAlways["mdadm --detail "+dev] = true
	if detail, err = md.Detail(ctx, dev); err == nil {
		t.Fatalf("a killed `mdadm --detail` read as %+v, and its array "+
			"would have been left un-stopped, pinning its leg wrappers",
			detail)
	}
	delete(node.killCmdAlways, "mdadm --detail "+dev)

	// The member list comes from a second run, and it is under the same
	// rule: a caller that read an empty Devices list off an array that in
	// fact holds its members would re-add members the array already has.
	node.killCmdAlways["mdadm --detail --export"] = true
	if detail, err = md.Detail(ctx, dev); err == nil {
		t.Fatalf("a killed `mdadm --detail --export` read as %+v", detail)
	}
}

// TestMdHasSuperblockKilledIsAnError pins the most destructive reading of a
// killed probe in the tree. `mdadm --examine` opens the member device exactly
// as `--detail` does and blocks on a dead leg in exactly the same way, and
// its answer selects between the two §11.1.1 assembly cases: create, or
// assemble. "No superblock" on every available member means case 1, and case
// 1 is `mdadm --create --assume-clean` — over whatever those members already
// hold. So a kill read as "no superblock" does not leak anything; it
// destroys the group's data.
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
	// the §11.1.1 case 1 answer, not a failure — a freshly zeroed side.
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
