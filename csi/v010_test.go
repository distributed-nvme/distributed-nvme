package csi

import (
	"maps"
	"strings"
	"testing"
)

// v010Volumes are volume ids and contexts as v0.1.0's controller plugin
// handed them to Kubernetes, which keeps them across an upgrade
// (architecture.md, Upgrade in lockstep): made once by a throwaway test in a
// git archive of the tag, from v0.1.0's dnvNameOf, volumeIdOf,
// subsystemNqnOf and namespaceUuidOf, with two cdc addresses; never
// refreshed. The cluster wh1 is the one of the v0.1.0 etcd sample
// (pb/samples_test.go); the empty cluster is the gateway's default.
var v010Volumes = []struct {
	coName, cluster, name, id     string
	volumeContext, publishContext map[string]string
}{
	{coName: "pvc-6f1a2c3e-0b4d-4e5f-8a9b-0c1d2e3f4a5b", cluster: "wh1", name: "pvc-6f1a2c3e-0b4d-4e5f-8a9b-0c1d2e3f4a5b", id: "wh1/pvc-6f1a2c3e-0b4d-4e5f-8a9b-0c1d2e3f4a5b",
		volumeContext:  map[string]string{"nqn": "nqn.2024-01.io.dnv.csi:pvc-6f1a2c3e-0b4d-4e5f-8a9b-0c1d2e3f4a5b", "uuid": "cee49417-dca5-5c49-921d-b0d524cff5b9"},
		publishContext: map[string]string{"nqn": "nqn.2024-01.io.dnv.csi:pvc-6f1a2c3e-0b4d-4e5f-8a9b-0c1d2e3f4a5b", "uuid": "cee49417-dca5-5c49-921d-b0d524cff5b9", "cdc": "10.1.0.1:8009,10.1.0.2:8009"}},
	{coName: "pvc-0d9e8f7a-6b5c-4d3e-9f2a-1b0c9d8e7f6a", cluster: "", name: "pvc-0d9e8f7a-6b5c-4d3e-9f2a-1b0c9d8e7f6a", id: "default/pvc-0d9e8f7a-6b5c-4d3e-9f2a-1b0c9d8e7f6a",
		volumeContext:  map[string]string{"nqn": "nqn.2024-01.io.dnv.csi:pvc-0d9e8f7a-6b5c-4d3e-9f2a-1b0c9d8e7f6a", "uuid": "01365c09-45b1-562d-8d8c-1ae72e0cad59"},
		publishContext: map[string]string{"nqn": "nqn.2024-01.io.dnv.csi:pvc-0d9e8f7a-6b5c-4d3e-9f2a-1b0c9d8e7f6a", "uuid": "01365c09-45b1-562d-8d8c-1ae72e0cad59", "cdc": "10.1.0.1:8009,10.1.0.2:8009"}},
	{coName: "csi-sanity-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/x", cluster: "wh1", name: "csi-sanity-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-3922b80206f08955", id: "wh1/csi-sanity-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-3922b80206f08955",
		volumeContext:  map[string]string{"nqn": "nqn.2024-01.io.dnv.csi:csi-sanity-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-3922b80206f08955", "uuid": "26b7f3bc-78e7-522b-b1ec-02a355adb72c"},
		publishContext: map[string]string{"nqn": "nqn.2024-01.io.dnv.csi:csi-sanity-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-3922b80206f08955", "uuid": "26b7f3bc-78e7-522b-b1ec-02a355adb72c", "cdc": "10.1.0.1:8009,10.1.0.2:8009"}},
}

// TestV010VolumeIdsAndContexts holds the driver to the ids and contexts
// v0.1.0 handed out (csi.md, The volume model, CSI10): each id parses to its
// cluster and volume and is rebuilt as it is, a repeated create of the same
// Kubernetes name finds the same volume, the volume's subsystem NQN and
// namespace uuid are the ones the contexts carry, and the node plugin reads
// the publish context.
func TestV010VolumeIdsAndContexts(t *testing.T) {
	for _, v := range v010Volumes {
		if got := dnvNameOf(v.coName); got != v.name {
			t.Errorf("dnvNameOf(%q) = %q, want v0.1.0's %q", v.coName, got, v.name)
		}
		cluster, name, err := parseVolumeId(v.id)
		if err != nil {
			t.Errorf("parseVolumeId(%q): %v", v.id, err)
			continue
		}
		if name != v.name || cluster != clusterNameOf(v.cluster) {
			t.Errorf("parseVolumeId(%q) = %q, %q, want %q, %q",
				v.id, cluster, name, clusterNameOf(v.cluster), v.name)
		}
		if got := volumeIdOf(v.cluster, v.name); got != v.id {
			t.Errorf("volumeIdOf(%q, %q) = %q, want v0.1.0's %q", v.cluster, v.name, got, v.id)
		}
		want := map[string]string{ctxNqn: subsystemNqnOf(v.name), ctxUuid: namespaceUuidOf(v.name)}
		if !maps.Equal(want, v.volumeContext) {
			t.Errorf("volume %s: today's volume context %v, v0.1.0's %v", v.name, want, v.volumeContext)
		}
		pc, err := readPublishContext(v.publishContext)
		if err != nil {
			t.Errorf("volume %s: readPublishContext: %v", v.name, err)
			continue
		}
		var cdcs []string
		for _, ep := range pc.cdcs {
			cdcs = append(cdcs, ep.String())
		}
		if pc.nqn != want[ctxNqn] || pc.uuid != want[ctxUuid] ||
			strings.Join(cdcs, cdcSep) != v.publishContext[ctxCdc] {
			t.Errorf("volume %s: the publish context reads as %q, %q, %v", v.name, pc.nqn, pc.uuid, cdcs)
		}
	}
}
