package gateway

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The machine as the default location (architecture.md, Disk nodes and
// Controller nodes; Per-operation allocation, CreateStoragePool): a node
// created with no location takes the host name its agent reports, and a pool
// create keeps its legs out of its first primary's location while another
// location has room.

// TestNodeCreateTakesTheHostNameAsLocation pins the location default of
// architecture.md, Disk nodes and Controller nodes, on both node creates: a
// node created with no location takes the host name its agent reports, in
// its record and in its capacity key; a location sent wins over the host
// name; and a host name that is no valid location — none, the kernel's
// "(none)" of a machine never named, one over the name size limit — refuses
// the create FAILED_PRECONDITION with the advice to pass a location, and
// writes no record and mints no id: the node global's revision stands.
func TestNodeCreateTakesTheHostNameAsLocation(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	cluster := hnodeName("hostloc")
	cid := mustCluster(t, s, cluster)
	type create func(addr string, location string) error
	kinds := []struct {
		name   string
		create create
		// stored reads the node's stored location and the one its capacity
		// key carries, found reporting whether the record exists.
		stored    func(addr string) (conf string, index string, found bool)
		globalKey string
	}{
		{
			name: "disk node",
			create: func(addr string, location string) error {
				_, err := s.CreateDiskNode(ctx, &pb.CreateDiskNodeRequest{
					ClusterName: cluster,
					AddrPort:    addr,
					NvmeTrConf:  testTrConf(addr),
					Location:    location,
				})
				return err
			},
			stored: func(addr string) (string, string, bool) {
				if !hnodeExists(t, s, model.DnConfKey(cid, addr)) {
					return "", "", false
				}
				dn := &pb.DnConf{}
				hnodeGet(t, s, model.DnConfKey(cid, addr), dn)
				index := ""
				for _, key := range hnodeDnCapacityKeys(t, s, cid) {
					if strings.HasSuffix(key, addr) {
						capacity := &pb.DnCapacity{}
						hnodeGet(t, s, key, capacity)
						index = capacity.GetLocation()
					}
				}
				return dn.GetLocation(), index, true
			},
			globalKey: model.DnGlobalKey(cid),
		},
		{
			name: "controller node",
			create: func(addr string, location string) error {
				_, err := s.CreateControllerNode(ctx,
					&pb.CreateControllerNodeRequest{
						ClusterName: cluster,
						AddrPort:    addr,
						NvmeTrConf:  testTrConf(addr),
						Location:    location,
					})
				return err
			},
			stored: func(addr string) (string, string, bool) {
				if !hnodeExists(t, s, model.CnConfKey(cid, addr)) {
					return "", "", false
				}
				cn := &pb.CnConf{}
				hnodeGet(t, s, model.CnConfKey(cid, addr), cn)
				index := ""
				for _, key := range hnodeCnCapacityKeys(t, s, cid) {
					if strings.HasSuffix(key, addr) {
						capacity := &pb.CnCapacity{}
						hnodeGet(t, s, key, capacity)
						index = capacity.GetLocation()
					}
				}
				return cn.GetLocation(), index, true
			},
			globalKey: model.CnGlobalKey(cid),
		},
	}
	for _, kind := range kinds {
		t.Run(kind.name, func(t *testing.T) {
			for _, tc := range []struct {
				name     string
				hostName string
				location string
				// want is the stored location, "" for a refusal.
				want string
			}{
				{"no location: the host name", "host-a", "", "host-a"},
				{"a location sent wins", "host-a", "rack-1", "rack-1"},
				{"a location sent, no host name", "", "rack-1", "rack-1"},
				{"no location and no host name", "", "", ""},
				{"no location and the unset name", "(none)", "", ""},
				{"no location and a name over the limit",
					strings.Repeat("h", common.MaxStrSize+1), "", ""},
			} {
				t.Run(tc.name, func(t *testing.T) {
					addr := fakeAddrPort(t, "loc")
					startFakeAgent(t, addr, 64<<30).setHostName(tc.hostName)
					globalRev := hnodeModRev(t, s, kind.globalKey)
					err := kind.create(addr, tc.location)
					conf, index, found := kind.stored(addr)
					if tc.want != "" {
						wantCode(t, err, codes.OK, tc.name)
						if conf != tc.want || index != tc.want {
							t.Errorf("location %q, capacity key's %q, "+
								"want %q in both", conf, index, tc.want)
						}
						return
					}
					wantCode(t, err, codes.FailedPrecondition, tc.name)
					hnodeWantMsg(t, err, addr, tc.name)
					hnodeWantMsg(t, err, "pass a location", tc.name)
					if found {
						t.Errorf("the refused create wrote a record")
					}
					if index != "" {
						t.Errorf("the refused create wrote a capacity key")
					}
					if got := hnodeModRev(
						t, s, kind.globalKey); got != globalRev {
						t.Errorf("the global moved: %d -> %d", globalRev, got)
					}
				})
			}
		})
	}
}

// locRaid1 is the md-raid1 conf the pool creates below ask for.
func locRaid1() *pb.BdevConf {
	return &pb.BdevConf{RedundConf: &pb.RedundConf{
		RedunKind: &pb.RedundConf_RedundMdRaid1{
			RedundMdRaid1: &pb.RedundMdRaid1{}}}}
}

// locCreate asks for an md-raid1 pool of sliceCnt slices of one-extent data
// groups with cntlrCnt cntlrs.
func locCreate(
	s *Server,
	cluster string,
	name string,
	cntlrCnt uint32,
	sliceCnt uint32,
) error {
	_, err := s.CreateStoragePool(context.Background(),
		&pb.CreateStoragePoolRequest{
			ClusterName: cluster,
			SpName:      name,
			CntlrCnt:    cntlrCnt,
			SliceCnt:    sliceCnt,
			InitExtCnt:  1,
			BdevConf:    locRaid1(),
		})
	return err
}

// locGroup is one group of a stored pool: whether it is a meta group and the
// locations of its legs' disk nodes.
type locGroup struct {
	meta bool
	locs []string
}

// locPool reads a stored pool's groups, every slice's meta group first, and
// the location of its primary cntlr's controller node.
func locPool(
	t *testing.T,
	s *Server,
	cid uint64,
	name string,
) ([]locGroup, string) {
	t.Helper()
	conf := &pb.SpConf{}
	hnodeGet(t, s, model.SpConfKey(cid, name), conf)
	dnLoc := func(addr string) string {
		dn := &pb.DnConf{}
		hnodeGet(t, s, model.DnConfKey(cid, addr), dn)
		return dn.GetLocation()
	}
	var grps []locGroup
	for _, sliceId := range conf.GetSliceIdList() {
		slice := &pb.Slice{}
		hnodeGet(t, s, model.SliceKey(cid, conf.GetSpId(), sliceId), slice)
		for idx, grp := range allGroups(slice) {
			out := locGroup{meta: idx < len(slice.GetMetaGrpList())}
			for _, leg := range grp.GetLegList() {
				for _, side := range leg.GetSideList() {
					out.locs = append(out.locs, dnLoc(side.GetAddrPort()))
				}
			}
			grps = append(grps, out)
		}
	}
	primary := ""
	for _, cntlrId := range conf.GetCntlrIdList() {
		cntlr := &pb.Cntlr{}
		hnodeGet(t, s, model.CntlrKey(cid, conf.GetSpId(), cntlrId), cntlr)
		if !cntlr.GetPrimary() {
			continue
		}
		cn := &pb.CnConf{}
		hnodeGet(t, s, model.CnConfKey(cid, cntlr.GetAddrPort()), cn)
		primary = cn.GetLocation()
	}
	if primary == "" {
		t.Fatalf("pool %q has no primary cntlr", name)
	}
	return grps, primary
}

// TestCreateStoragePoolRefusesOneHostForRaid1 pins the hard rule of
// architecture.md, Per-operation allocation, under the location default:
// four disk nodes whose agents all report one host name, created with no
// location, are one location, so an md-raid1 pool, whose groups each need
// two locations, is refused RESOURCE_EXHAUSTED, the message saying the disk
// nodes it found are in distinct locations. Four more disk nodes of the same
// machine, created with four locations, build the pool, each group's legs in
// two distinct locations: the first four, still one location, may hold one
// leg of a group.
func TestCreateStoragePoolRefusesOneHostForRaid1(t *testing.T) {
	s := newTestServer(t)
	cluster := hnodeName("onehost")
	cid := mustCluster(t, s, cluster)
	mustCn(t, s, cluster, fakeAddrPort(t, "cn"), "rack-cn", 1<<40)
	for idx := 0; idx < 4; idx++ {
		addr := fakeAddrPort(t, "dn")
		startFakeAgent(t, addr, 1<<40).setHostName("host-a")
		if _, err := s.CreateDiskNode(context.Background(),
			&pb.CreateDiskNodeRequest{
				ClusterName: cluster,
				AddrPort:    addr,
				NvmeTrConf:  testTrConf(addr),
			}); err != nil {
			t.Fatalf("CreateDiskNode %q: %v", addr, err)
		}
	}
	err := locCreate(s, cluster, "onehost", 1, 1)
	wantCode(t, err, codes.ResourceExhausted, "a pool on one host")
	hnodeWantMsg(t, err,
		"1 disk nodes with 1 free extents in distinct locations, need 2",
		"a pool on one host")
	if hnodeExists(t, s, model.SpConfKey(cid, "onehost")) {
		t.Fatalf("the refused create wrote the pool")
	}

	for idx := 0; idx < 4; idx++ {
		addr := fakeAddrPort(t, "dn")
		startFakeAgent(t, addr, 1<<40).setHostName("host-a")
		if _, err := s.CreateDiskNode(context.Background(),
			&pb.CreateDiskNodeRequest{
				ClusterName: cluster,
				AddrPort:    addr,
				NvmeTrConf:  testTrConf(addr),
				Location:    fmt.Sprintf("disk-%d", idx),
			}); err != nil {
			t.Fatalf("CreateDiskNode %q: %v", addr, err)
		}
	}
	if err := locCreate(s, cluster, "fourlocs", 1, 1); err != nil {
		t.Fatalf("a pool over four locations: %v", err)
	}
	grps, _ := locPool(t, s, cid, "fourlocs")
	for idx, grp := range grps {
		if len(grp.locs) != 2 || grp.locs[0] == grp.locs[1] {
			t.Errorf("group %d: legs in %q, want two locations",
				idx, grp.locs)
		}
	}
}

// locRacks names cnt locations, rack-0 onwards.
func locRacks(cnt int) []string {
	racks := make([]string, 0, cnt)
	for idx := 0; idx < cnt; idx++ {
		racks = append(racks, fmt.Sprintf("rack-%d", idx))
	}
	return racks
}

// TestCreateStoragePoolKeepsLegsOffThePrimaryLocation pins the first
// preference of architecture.md, Per-operation allocation, CreateStoragePool:
// the cntlrs are picked first, and every group's disk node scan keeps the
// first primary's location out at its first tier.
//
//   - Five disk nodes in five locations, one of them the controller node's:
//     ten pools of two groups, four legs each, never put a leg there. Without
//     the preference a pool leaves one location of five out with odds of one
//     in five, so ten pools that all leave the primary's out come up once in
//     ten million.
//   - Four disk nodes in four locations, one of them the controller node's:
//     every pool needs all four, so the preference falls to its second tier
//     for the data group, the second scanned, and the primary's location
//     holds exactly one leg, a data group's. Without the preference the meta
//     group takes it half the time.
//   - Two cntlrs, their controller nodes in two locations each shared with a
//     disk node, and six disk nodes in six locations: no leg is in the
//     primary's location; the standby's may hold one. Without the
//     preference a pool leaves one location of six out with odds of one in
//     three.
func TestCreateStoragePoolKeepsLegsOffThePrimaryLocation(t *testing.T) {
	const pools = 10
	for _, tc := range []struct {
		name     string
		dnLocs   []string
		cnLocs   []string
		cntlrCnt uint32
		// oneDataLeg asks that the primary's location hold exactly one leg,
		// a data group's; otherwise it must hold none.
		oneDataLeg bool
	}{
		{"five locations", locRacks(5), []string{"rack-0"}, 1, false},
		{"four locations", locRacks(4), []string{"rack-0"}, 1, true},
		{"two cntlrs", locRacks(6), []string{"rack-0", "rack-5"}, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			cluster := hnodeName("primloc")
			cid := mustCluster(t, s, cluster)
			for _, loc := range tc.cnLocs {
				mustCn(t, s, cluster, fakeAddrPort(t, "cn"), loc, 1<<40)
			}
			for _, loc := range tc.dnLocs {
				mustDn(t, s, cluster, fakeAddrPort(t, "dn"), loc, 1<<40)
			}
			for idx := 0; idx < pools; idx++ {
				name := fmt.Sprintf("p%d", idx)
				if err := locCreate(
					s, cluster, name, tc.cntlrCnt, 1); err != nil {
					t.Fatalf("pool %d: %v", idx, err)
				}
				grps, primary := locPool(t, s, cid, name)
				metaLegs, dataLegs := 0, 0
				for _, grp := range grps {
					for _, loc := range grp.locs {
						if loc != primary {
							continue
						}
						if grp.meta {
							metaLegs++
						} else {
							dataLegs++
						}
					}
				}
				wantData := 0
				if tc.oneDataLeg {
					wantData = 1
				}
				if metaLegs != 0 || dataLegs != wantData {
					t.Fatalf("pool %d: the primary's location %s holds %d "+
						"meta and %d data legs, want 0 and %d (groups %v)",
						idx, primary, metaLegs, dataLegs, wantData, grps)
				}
			}
		})
	}
}

// TestCreateStoragePoolFallsBackToThePlainPick pins the second half of the
// first preference (architecture.md, Per-operation allocation,
// CreateStoragePool): a pool the preferred scans refuse is picked once more
// by the plain scans, so the preference never refuses a create the plain
// pick places. Three machines, each with three disk nodes and one
// controller node, all registered under the machine's name: a pool of two
// slices needs four groups, eight legs on eight disk nodes in pairs of
// distinct locations. The preferred scans give the first groups the two
// other machines until each of their disk nodes holds a leg of the pool, and
// the black list keeps those disk nodes from the last group, which then
// finds only the primary's machine, every time; the plain pick places such a
// pool about eight times in nine. Of twenty creates, at least one must build
// its pool.
func TestCreateStoragePoolFallsBackToThePlainPick(t *testing.T) {
	s := newTestServer(t)
	cluster := hnodeName("fallback")
	mustCluster(t, s, cluster)
	for _, machine := range []string{"A", "B", "C"} {
		for idx := 0; idx < 3; idx++ {
			mustDn(t, s, cluster, fakeAddrPort(t, "dn"), machine, 1<<40)
		}
		mustCn(t, s, cluster, fakeAddrPort(t, "cn"), machine, 1<<40)
	}
	built := 0
	var refusal string
	for idx := 0; idx < 20; idx++ {
		err := locCreate(s, cluster, fmt.Sprintf("p%d", idx), 1, 2)
		switch status.Code(err) {
		case codes.OK:
			built++
		case codes.ResourceExhausted:
			refusal = status.Convert(err).Message()
		default:
			t.Fatalf("create %d: %v", idx, err)
		}
	}
	if built == 0 {
		t.Fatalf("none of 20 creates built its pool; the last refusal: %q",
			refusal)
	}
}
