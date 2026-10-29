package csi

import (
	"regexp"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// TestVolumeIdRoundTrips pins csi.md, The volume model: the id is the
// cluster and the volume name joined by a slash, split at the last one, so
// a cluster name may carry a slash and a volume name may not.
func TestVolumeIdRoundTrips(t *testing.T) {
	for _, tc := range []struct{ cluster, name, id string }{
		{"", testVolume, common.DefaultClusterName + "/" + testVolume},
		{"c0", testVolume, "c0/" + testVolume},
		{"rack/a", testVolume, "rack/a/" + testVolume},
	} {
		id := volumeIdOf(tc.cluster, tc.name)
		if id != tc.id {
			t.Errorf("volumeIdOf(%q, %q) = %q, want %q", tc.cluster, tc.name, id, tc.id)
		}
		cluster, name, err := parseVolumeId(id)
		if err != nil || cluster != clusterNameOf(tc.cluster) || name != tc.name {
			t.Errorf("parseVolumeId(%q) = %q, %q, %v", id, cluster, name, err)
		}
	}
	for _, bad := range []string{"", "/", "c0/", "/pvc-1", "pvc-1", "c0/a b", "c0//"} {
		if _, _, err := parseVolumeId(bad); err == nil {
			t.Errorf("parseVolumeId(%q) accepted", bad)
		}
	}
	if err := checkVolumeName(strings.Repeat("a", common.MaxStrSize+1)); err == nil {
		t.Error("a name above the size limit was accepted")
	}
}

// TestDnvNameOf pins csi.md, The volume model: a name dnv takes is the dnv
// name itself; any other, too long or with a character outside dnv's set or
// the id's separator, becomes its acceptable leading bytes and a hash, one
// dnv takes, the same for the same name and distinct for distinct names.
func TestDnvNameOf(t *testing.T) {
	if got := dnvNameOf(testVolume); got != testVolume {
		t.Errorf("dnvNameOf(%q) = %q, want the name itself", testVolume, got)
	}
	long := strings.Repeat("v", 128)
	longer := long + "w"
	for _, name := range []string{long, longer, "a/b", "a b", "héllo", "x" + strings.Repeat("/", 70)} {
		got := dnvNameOf(name)
		if got == name {
			t.Errorf("dnvNameOf(%q) kept a name dnv does not take", name)
		}
		if err := checkVolumeName(got); err != nil {
			t.Errorf("dnvNameOf(%q) = %q, not a name dnv takes: %v", name, got, err)
		}
		if got != dnvNameOf(name) {
			t.Errorf("dnvNameOf(%q) differs between two calls", name)
		}
		if nqn := subsystemNqnOf(got); len(nqn) > common.MaxNqnLength || !validNqn.MatchString(nqn) {
			t.Errorf("the NQN of %q, %q, is not one the gateway accepts", got, nqn)
		}
	}
	if dnvNameOf(long) == dnvNameOf(longer) {
		t.Error("two long names share a dnv name")
	}
	if got := dnvNameOf(long); len(got) != common.MaxStrSize || !strings.HasPrefix(got, "vvv") {
		t.Errorf("dnvNameOf(long) = %q, want the leading bytes and a hash at the limit", got)
	}
}

// TestNamesAreTheGatewaysForms pins the subsystem NQN and the namespace
// uuid: an NQN the gateway accepts, outside the dnv-owned prefix, and a
// version-5 uuid in the dashed lower-case form, the same for the same name.
func TestNamesAreTheGatewaysForms(t *testing.T) {
	nqn := subsystemNqnOf(testVolume)
	if !validNqn.MatchString(nqn) || len(nqn) > common.MaxNqnLength {
		t.Errorf("subsystem NQN %q is not one the gateway accepts", nqn)
	}
	if common.IsDnvNqn(nqn) {
		t.Errorf("subsystem NQN %q is in the dnv-owned namespace", nqn)
	}
	long := strings.Repeat("x", common.MaxStrSize)
	if len(subsystemNqnOf(long)) > common.MaxNqnLength {
		t.Error("the longest volume name overflows the NQN length")
	}
	uuid := namespaceUuidOf(testVolume)
	form := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !form.MatchString(uuid) {
		t.Errorf("namespace uuid %q is not a dashed version-5 uuid", uuid)
	}
	if uuid != namespaceUuidOf(testVolume) {
		t.Error("the namespace uuid differs between two calls")
	}
	if uuid == namespaceUuidOf(testVolume+"x") {
		t.Error("two names share a namespace uuid")
	}
}

// TestNodeIdIsAHostNqn pins what a node id of this driver is.
func TestNodeIdIsAHostNqn(t *testing.T) {
	if !isNodeId(testNodeA) {
		t.Errorf("%q is not taken as a node id", testNodeA)
	}
	for _, bad := range []string{"", "some-fake-node", "nqn.2014-08.org.nvmexpress"} {
		if isNodeId(bad) {
			t.Errorf("%q is taken as a node id", bad)
		}
	}
}
