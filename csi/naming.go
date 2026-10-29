package csi

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The names of a volume's objects (csi.md, The volume model). Every object
// of a volume is found by the one name Kubernetes gives the volume: the pool
// and the thin device carry it, the subsystem NQN appends it to a prefix of
// the driver's own, which keeps the dnv-owned NQN prefix untouched, and the
// namespace uuid derives from it, so a node predicts the device path with no
// lookup and a re-created volume gets the same identity.
const (
	// nqnPrefix starts every subsystem NQN of the driver. It is not the
	// dnv-owned prefix the gateway refuses for a host-facing subsystem,
	// which ends in a colon right after the domain.
	nqnPrefix = "nqn.2024-01.io.dnv.csi:"
	// volumeIdSep joins the cluster name and the volume name into the
	// volume id Kubernetes stores. The split is at the LAST separator, so a
	// cluster name may carry the character and a volume name may not.
	volumeIdSep = "/"
	// uuidNamespace is the driver's own namespace uuid: the namespace uuid
	// of a volume is the version-5 uuid of the volume name under it.
	uuidNamespace = "9e2c4b7a-1f53-4d8e-a6c0-5b3d7e8f1a2c"
	// nsIdx is the one namespace of a volume's subsystem.
	nsIdx = 1
	// nameHashLen is the hex digits of the hash a shortened dnv name ends
	// in: 64 bits, enough that two volume names never meet.
	nameHashLen = 16
)

var (
	validName = regexp.MustCompile(common.ValidStrPattern)
	validNqn  = regexp.MustCompile(common.ValidNqnPattern)
)

// checkVolumeName holds a volume name to the gateway's rule for a name
// (architecture.md, Common validation) and keeps the id separator out of
// it, so the volume id splits.
func checkVolumeName(name string) error {
	if name == "" {
		return status.Error(codes.InvalidArgument, "the volume name is required")
	}
	if len(name) > common.MaxStrSize || !validName.MatchString(name) ||
		strings.Contains(name, volumeIdSep) {
		return status.Errorf(codes.InvalidArgument,
			"the volume name %q is not a name dnv accepts", name)
	}
	return nil
}

// dnvNameOf is the dnv name of a volume (csi.md, The volume model): the
// name Kubernetes gives it when dnv takes that name as it is, else its
// leading bytes, each made acceptable, and a hash of the whole name. The
// spec lets a caller send names of 128 bytes, twice dnv's limit. The same
// name always gives the same dnv name, so a repeated create finds its
// objects, and the result is one dnv takes, so the volume id splits.
func dnvNameOf(name string) string {
	if checkVolumeName(name) == nil {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	hash := hex.EncodeToString(sum[:])[:nameHashLen]
	keep := common.MaxStrSize - nameHashLen - 1
	if len(name) < keep {
		keep = len(name)
	}
	prefix := []byte(name[:keep])
	for i, b := range prefix {
		if !acceptableByte(b) {
			prefix[i] = '-'
		}
	}
	return string(prefix) + "-" + hash
}

// acceptableByte reports whether a byte may stand in a dnv name of this
// driver's: the gateway's set less the volume id's separator.
func acceptableByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' ||
		b == '-' || b == '_' || b == '.' || b == ':'
}

// checkClusterName holds a cluster parameter to the gateway's rule for a
// name; the empty name is the gateway's default cluster.
func checkClusterName(name string) error {
	if name == "" {
		return nil
	}
	if len(name) > common.MaxStrSize || !validName.MatchString(name) {
		return status.Errorf(codes.InvalidArgument,
			"the cluster name %q is not a name dnv accepts", name)
	}
	return nil
}

// clusterNameOf resolves the default the gateway applies, so that a volume
// id always names its cluster.
func clusterNameOf(name string) string {
	if name == "" {
		return common.DefaultClusterName
	}
	return name
}

// volumeIdOf is the volume id Kubernetes stores: the cluster name and the
// volume name, which is also the pool's and the thin device's.
func volumeIdOf(cluster string, name string) string {
	return clusterNameOf(cluster) + volumeIdSep + name
}

// parseVolumeId is the inverse of volumeIdOf. An id that is not of this
// driver's form is an error of the caller's choosing: a required field that
// is empty is INVALID_ARGUMENT, a well-formed field that names no volume of
// this driver is NOT_FOUND, and a delete of it is done.
func parseVolumeId(id string) (string, string, error) {
	idx := strings.LastIndex(id, volumeIdSep)
	if idx <= 0 || idx == len(id)-1 {
		return "", "", fmt.Errorf("volume id %q is not <cluster>/<volume>", id)
	}
	cluster, name := id[:idx], id[idx+1:]
	if len(cluster) > common.MaxStrSize || !validName.MatchString(cluster) {
		return "", "", fmt.Errorf("volume id %q names no cluster dnv accepts", id)
	}
	if err := checkVolumeName(name); err != nil {
		return "", "", fmt.Errorf("volume id %q names no volume dnv accepts", id)
	}
	return cluster, name, nil
}

// subsystemNqnOf is the NQN of a volume's subsystem.
func subsystemNqnOf(name string) string {
	return nqnPrefix + name
}

// namespaceUuidOf is the uuid of a volume's namespace: the version-5 uuid
// of the volume name under the driver's namespace uuid, in the dashed
// lower-case form the gateway stores and the kernel reads back.
func namespaceUuidOf(name string) string {
	ns, err := hex.DecodeString(strings.ReplaceAll(uuidNamespace, "-", ""))
	if err != nil || len(ns) != 16 {
		panic("csi: the driver's namespace uuid is malformed")
	}
	h := sha1.New()
	h.Write(ns)
	h.Write([]byte(name))
	sum := h.Sum(nil)
	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x50
	u[8] = (u[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// isNodeId reports whether a node id is one this driver issues: a host NQN
// (csi.md, Attach and detach). A node id of another form names no node of
// this driver.
func isNodeId(id string) bool {
	return len(id) <= common.MaxNqnLength && validNqn.MatchString(id)
}
