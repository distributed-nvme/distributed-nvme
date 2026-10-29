package csi

import (
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The storage class parameters (csi.md, The volume model): the dnv cluster
// the class names, and the shape of the pool every volume of the class
// gets. Any other key is refused, so a typo never passes silently; the keys
// Kubernetes reserves for itself are ignored, since the provisioner strips
// them from a create but hands them over whole with a capacity query.
const (
	paramCluster    = "cluster"
	paramRedundancy = "redundancy"
	paramCntlrs     = "cntlrs"

	redundancyRaid1 = "raid1"
	redundancyNone  = "none"

	kubernetesParamPrefix = "csi.storage.k8s.io/"
)

// params is a storage class's settings, parsed and defaulted.
type params struct {
	cluster    string
	redundancy string
	cntlrs     uint32
}

// parseParams reads the parameters of a storage class. The cluster defaults
// to the gateway's default cluster, the redundancy to RAID1 and the cntlr
// count to the gateway's default.
func parseParams(m map[string]string) (params, error) {
	p := params{redundancy: redundancyRaid1}
	for key, value := range m {
		if strings.HasPrefix(key, kubernetesParamPrefix) {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case paramCluster:
			if err := checkClusterName(value); err != nil {
				return params{}, err
			}
			p.cluster = value
		case paramRedundancy:
			switch value {
			case redundancyRaid1, redundancyNone:
				p.redundancy = value
			default:
				return params{}, status.Errorf(codes.InvalidArgument,
					"parameter %s must be %q or %q, got %q",
					paramRedundancy, redundancyRaid1, redundancyNone, value)
			}
		case paramCntlrs:
			n, err := strconv.ParseUint(value, 10, 32)
			if err != nil || n < common.MinCntlrCntPerSp ||
				n > common.MaxCntlrCntPerSp {
				return params{}, status.Errorf(codes.InvalidArgument,
					"parameter %s must be a cntlr count from %d to %d, got %q",
					paramCntlrs, common.MinCntlrCntPerSp,
					common.MaxCntlrCntPerSp, value)
			}
			p.cntlrs = uint32(n)
		default:
			return params{}, status.Errorf(codes.InvalidArgument,
				"unknown parameter %q; the parameters are %s, %s and %s",
				key, paramCluster, paramRedundancy, paramCntlrs)
		}
	}
	return p, nil
}

// legs is how many legs a group has under the class's redundancy.
func (p params) legs() int {
	if p.redundancy == redundancyNone {
		return 1
	}
	return common.MaxAllocLegPerGrp
}

// cntlrCnt is the cntlr count a pool of the class gets, the gateway's
// default resolved for the capacity arithmetic.
func (p params) cntlrCnt() uint32 {
	if p.cntlrs == 0 {
		return common.DefaultCntlrCntPerSp
	}
	return p.cntlrs
}

// redundConf is the redundancy a pool of the class is created with.
func (p params) redundConf() *pb.RedundConf {
	if p.redundancy == redundancyNone {
		return &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundNone{RedundNone: &pb.RedundNone{}},
		}
	}
	return &pb.RedundConf{
		RedunKind: &pb.RedundConf_RedundMdRaid1{
			RedundMdRaid1: &pb.RedundMdRaid1{},
		},
	}
}
