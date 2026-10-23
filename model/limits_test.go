package model

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The widest messages and the widest etcd request dnv builds, at the limits of
// architecture.md, Cardinality limits and Common validation. One builder,
// widestBuilder, fills a message from its descriptor: every list and map at
// the count its table gives, every string at its bound, every number at its
// widest varint, so a message is as large as the limits let it be. A list or
// a map the table does not name fails the test that meets it, so a field a
// later schema adds is sized here before it ships.

const (
	// rowTextAllowance is the text this file allows on every row of a cntlr
	// report. A row whose command failed carries that command's output, and
	// nothing cuts it (architecture.md, Known limits); a row that reads OK
	// can carry short text too, such as a pool's or a dm-clone's status
	// line, an array's state or a standby leg's path states (cnagent.md
	// CN28).
	rowTextAllowance = 256
	// dmNameMax is the longest dm device name, the kernel's DM_NAME_LEN less
	// its terminating NUL: the bound of every cntlr report row a dm device
	// names.
	dmNameMax = 127
	// uuidLen and nguidLen are a namespace's dev_uuid and dev_nguid in the
	// only forms validateDevIdentity accepts: the dashed uuid and 32 hex
	// digits.
	uuidLen  = 36
	nguidLen = 32
	// nsRowSuffix bounds what a namespace row's name adds to its NQN: a slash
	// and the namespace index, a uint32 in decimal.
	nsRowSuffix = 1 + 10
	// clientv3DefaultSendBytes is the etcd client's own send limit, which
	// etcdutil.New raises to EtcdMaxRequestBytes (dnv-worker.md EU1).
	clientv3DefaultSendBytes = 2 << 20
)

// idKeyLen is the length of an id rendered as a key field (common.IdKeyFmt),
// the keys of a cntlr syncup's slice map and a subsystem's serial.
var idKeyLen = len(fmt.Sprintf(common.IdKeyFmt, uint64(math.MaxUint64)))

// widestCounts is the entry count of every list and map the three widest
// messages reach: the cntlr syncup, the pool get reply and the cntlr report.
// A group holds one repair or MaxSpareLegPerGrp spare legs, never both: a
// spare leg and a repair's new leg would race for one place in the array, so
// each refuses a group that has the other. withRepairs gives every group of
// the pool its repair and no spare; without it every group has its spares
// and no repair. Both pools are built, since neither is the wider in every
// message.
func widestCounts(withRepairs bool) map[protoreflect.FullName]int {
	// Every group of the pool: the slices' and one clone meta group per
	// clone (Clone.meta_grp, which the builder fills as any group).
	grpsPerSp := common.MaxSliceCntPerSp*2*common.MaxGrpCntPerSlice +
		common.MaxCloneCntPerSp
	// A leg row per leg and per spare or new leg of every group, the wider
	// of the two pools' counts.
	legsPerGrp := common.MaxAllocLegPerGrp +
		max(common.MaxSpareLegPerGrp, 1)
	sparesPerGrp, repsPerSp := common.MaxSpareLegPerGrp, 0
	if withRepairs {
		sparesPerGrp, repsPerSp = 0, grpsPerSp
	}
	return map[protoreflect.FullName]int{
		// The pool's state, as a cntlr syncup and a pool get reply carry it.
		"SyncupCntlrRequest.id_to_slice":      common.MaxSliceCntPerSp,
		"SyncupCntlrRequest.td_list":          common.MaxTdCntPerSp,
		"SyncupCntlrRequest.nqn_to_subsystem": common.MaxSsCntPerSp,
		"SyncupCntlrRequest.clone_list":       common.MaxCloneCntPerSp,
		"SyncupCntlrRequest.xfer_list":        common.MaxXferCntPerSp,
		"SyncupCntlrRequest.name_to_repair":   repsPerSp,
		"GetStoragePoolReply.cntlr_list":      common.MaxCntlrCntPerSp,
		"GetStoragePoolReply.slice_list":      common.MaxSliceCntPerSp,
		"Slice.meta_grp_list":                 common.MaxGrpCntPerSlice,
		"Slice.data_grp_list":                 common.MaxGrpCntPerSlice,
		"Group.leg_list":                      common.MaxAllocLegPerGrp,
		"Group.spare_leg_list":                sparesPerGrp,
		"Group.retired_leg_id_list":           common.MaxRetiredLegPerGrp,
		// One bad leg and one new leg per repair, and the retired time of
		// a new leg taken from a retired leg.
		"Repair.new_leg_list":                1,
		"Repair.bad_leg_id_list":             1,
		"Repair.new_leg_id_to_retired_epoch": 1,
		// One side per leg; withMigrationSides gives MaxMigrCntPerSp legs of
		// the pool their second.
		"Leg.side_list": 1,
		// No feature but the empty one exists.
		"BdevConf.bdev_feature_list": 0,
		"Subsystem.ns_list":          common.MaxNsCntPerSs,
		"Subsystem.allowed_hosts":    common.MaxHostCntPerSs,
		"Transfer.allowed_hosts":     common.MaxHostCntPerSs,
		// A clone's source transport list: the gateway refuses one longer
		// than MaxCntlrCntPerSp, the most cntlrs a source pool has, in
		// CreateClone and UpdateCloneTrConf (architecture.md, Common
		// validation).
		"Clone.src_tr_conf_list":  common.MaxCntlrCntPerSp,
		"SpConf.cntlid_slot_list": common.CnCntlidSlotCnt,
		"SpConf.cntlr_id_list":    common.MaxCntlrCntPerSp,
		"SpConf.slice_id_list":    common.MaxSliceCntPerSp,
		"SpConf.td_name_list":     common.MaxTdCntPerSp,
		"SpConf.nqn_list":         common.MaxSsCntPerSp,
		"SpConf.clone_name_list":  common.MaxCloneCntPerSp,
		"SpConf.xfer_name_list":   common.MaxXferCntPerSp,
		"SpConf.migr_name_list":   common.MaxMigrCntPerSp,
		"SpConf.repair_name_list": repsPerSp,
		"SpConf.retired_leg_id_list": grpsPerSp *
			common.MaxRetiredLegPerGrp,

		// A cntlr report: a row per object of the pool the cntlr builds.
		"CntlrInfo.ss_id_to_subsystem":           common.MaxSsCntPerSp,
		"CntlrInfo.ns_id_to_namespace":           common.MaxSsCntPerSp * common.MaxNsCntPerSs,
		"CntlrInfo.ns_id_to_dm_linear":           common.MaxSsCntPerSp * common.MaxNsCntPerSs,
		"CntlrInfo.td_id_to_raid0":               common.MaxTdCntPerSp,
		"CntlrInfo.td_id_to_dm_error":            common.MaxTdCntPerSp,
		"CntlrInfo.td_id_to_thin_info":           common.MaxTdCntPerSp,
		"CntlrInfo.ThinInfo.slice_id_to_dm_thin": common.MaxSliceCntPerSp,
		"CntlrInfo.slice_id_to_dm_pool":          common.MaxSliceCntPerSp,
		"CntlrInfo.slice_id_to_meta":             common.MaxSliceCntPerSp,
		"CntlrInfo.slice_id_to_data":             common.MaxSliceCntPerSp,
		"CntlrInfo.grp_id_to_md_raid":            grpsPerSp,
		"CntlrInfo.leg_id_to_leg":                grpsPerSp * legsPerGrp,
		"CntlrInfo.xfer_id_to_dm_linear":         common.MaxXferCntPerSp,
		"CntlrInfo.xfer_id_to_subsystem":         common.MaxXferCntPerSp,
		"CntlrInfo.xfer_id_to_namespace":         common.MaxXferCntPerSp,
		"CntlrInfo.clone_id_to_target":           common.MaxCloneCntPerSp,
		"CntlrInfo.clone_id_to_dm_clone":         common.MaxCloneCntPerSp,
		// A path row per side of each leg with two sides: at most
		// MaxMigrCntPerSp migrations, two sides each.
		"CntlrInfo.side_id_to_path":     2 * common.MaxMigrCntPerSp,
		"SyncupCntlrReply.bm_info_list": common.MaxCloneCntPerSp,
		// A verdict per repair and a reading per group, which names the
		// group's legs and its repair's new leg that md holds in sync.
		"CntlrInfo.rep_id_to_repair_info":       repsPerSp,
		"CntlrInfo.grp_id_to_grp_info":          grpsPerSp,
		"CntlrInfo.GrpInfo.in_sync_leg_id_list": common.MaxAllocLegPerGrp + 1,
		// A migration's list; a cntlr reports the bitmaps of clones only.
		"BitmapInfo.bm_idx_list":   0,
		"BitmapInfo.chunk_id_list": common.MaxSliceCntPerSp * common.MaxCloneBmCnt,
	}
}

// widestStrLens is the length of every string field whose bound is not
// MaxStrSize; a list of strings is named by its field. A group's rep_name is
// empty in the pool without repairs (widestCounts).
func widestStrLens(withRepairs bool) map[protoreflect.FullName]int {
	repName := common.MaxStrSize
	if !withRepairs {
		repName = 0
	}
	return map[protoreflect.FullName]int{
		"Group.rep_name":          repName,
		"Subsystem.serial":        idKeyLen,
		"Subsystem.model":         len(common.NvmeModel),
		"Subsystem.allowed_hosts": common.MaxNqnLength,
		"Namespace.dev_uuid":      uuidLen,
		"Namespace.dev_nguid":     nguidLen,
		"Clone.src_nqn":           common.MaxNqnLength,
		"Transfer.ori_nqn":        common.MaxNqnLength,
		"Transfer.allowed_hosts":  common.MaxNqnLength,
		"SpConf.nqn_list":         common.MaxNqnLength,
	}
}

// widestKeyLens is the length of the keys of every map with string keys.
func widestKeyLens() map[protoreflect.FullName]int {
	return map[protoreflect.FullName]int{
		"SyncupCntlrRequest.id_to_slice":      idKeyLen,
		"SyncupCntlrRequest.nqn_to_subsystem": common.MaxNqnLength,
		"SyncupCntlrRequest.name_to_repair":   common.MaxStrSize,
	}
}

// widestRowNames is the length of the name of a row of each map of a cntlr
// report: an NQN, an NQN and a namespace index, a dm device name, or a
// side's address.
func widestRowNames() map[protoreflect.FullName]int {
	nqnRow := common.MaxNqnLength
	nsRow := common.MaxNqnLength + nsRowSuffix
	pathRow := 2*common.MaxStrSize + len(":")
	return map[protoreflect.FullName]int{
		"CntlrInfo.ss_id_to_subsystem":           nqnRow,
		"CntlrInfo.ns_id_to_namespace":           nsRow,
		"CntlrInfo.ns_id_to_dm_linear":           dmNameMax,
		"CntlrInfo.td_id_to_raid0":               dmNameMax,
		"CntlrInfo.td_id_to_dm_error":            dmNameMax,
		"CntlrInfo.ThinInfo.slice_id_to_dm_thin": dmNameMax,
		"CntlrInfo.slice_id_to_dm_pool":          dmNameMax,
		"CntlrInfo.slice_id_to_meta":             dmNameMax,
		"CntlrInfo.slice_id_to_data":             dmNameMax,
		"CntlrInfo.grp_id_to_md_raid":            dmNameMax,
		"CntlrInfo.leg_id_to_leg":                dmNameMax,
		"CntlrInfo.xfer_id_to_dm_linear":         dmNameMax,
		"CntlrInfo.xfer_id_to_subsystem":         nqnRow,
		"CntlrInfo.xfer_id_to_namespace":         nsRow,
		"CntlrInfo.clone_id_to_target":           nqnRow,
		"CntlrInfo.clone_id_to_dm_clone":         dmNameMax,
		// A path row is named by its side's address, tr_addr:tr_svc_id
		// (legPathRows), each part a name of at most MaxStrSize.
		"CntlrInfo.side_id_to_path": pathRow,
	}
}

// widestBuilder fills messages as wide as the limits let them be, from the
// four tables above. rowText is the text of every row of a cntlr report and
// of its agent reply.
type widestBuilder struct {
	t        *testing.T
	counts   map[protoreflect.FullName]int
	strLens  map[protoreflect.FullName]int
	keyLens  map[protoreflect.FullName]int
	rowNames map[protoreflect.FullName]int
	rowText  int
	// used is every table entry a build has read, so a test can find an
	// entry no field of the schema reaches any more.
	used map[protoreflect.FullName]bool
	strs map[int]string
	// key is the last map key handed out: keys count down from the widest
	// uint64, so each is distinct and a ten-byte varint.
	key uint64
}

func newWidestBuilder(
	t *testing.T,
	rowText int,
	withRepairs bool,
) *widestBuilder {
	return &widestBuilder{
		t:        t,
		counts:   widestCounts(withRepairs),
		strLens:  widestStrLens(withRepairs),
		keyLens:  widestKeyLens(),
		rowNames: widestRowNames(),
		rowText:  rowText,
		used:     map[protoreflect.FullName]bool{},
		strs:     map[int]string{},
		key:      math.MaxUint64,
	}
}

func (b *widestBuilder) build(m proto.Message) {
	b.t.Helper()
	b.fillMessage(m.ProtoReflect(), "")
}

// unusedByAll lists the table entries no build of any of the builders has
// read.
func unusedByAll(builders ...*widestBuilder) []string {
	used := map[protoreflect.FullName]bool{}
	names := map[protoreflect.FullName]bool{}
	for _, b := range builders {
		for _, table := range []map[protoreflect.FullName]int{
			b.counts, b.strLens, b.keyLens, b.rowNames,
		} {
			for name := range table {
				names[name] = true
				used[name] = used[name] || b.used[name]
			}
		}
	}
	var out []string
	for name := range names {
		if !used[name] {
			out = append(out, string(name))
		}
	}
	sort.Strings(out)
	return out
}

// fillMessage fills every field of m; in is the list or map field m is an
// entry of, which names the rows of a cntlr report.
func (b *widestBuilder) fillMessage(m protoreflect.Message, in protoreflect.FullName) {
	fields := m.Descriptor().Fields()
	oneofDone := map[protoreflect.Name]bool{}
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if od := fd.ContainingOneof(); od != nil && !od.IsSynthetic() {
			if !oneofDone[od.Name()] {
				oneofDone[od.Name()] = true
				b.fillOneof(m, od, in)
			}
			continue
		}
		b.fillField(m, fd, in)
	}
}

// fillOneof sets the member of od whose filled value is the widest.
func (b *widestBuilder) fillOneof(
	m protoreflect.Message, od protoreflect.OneofDescriptor,
	in protoreflect.FullName,
) {
	var widest protoreflect.FieldDescriptor
	var value protoreflect.Value
	size := -1
	for i := 0; i < od.Fields().Len(); i++ {
		fd := od.Fields().Get(i)
		trial := m.Type().New()
		b.fillField(trial, fd, in)
		if n := proto.Size(trial.Interface()); n > size {
			size, widest, value = n, fd, trial.Get(fd)
		}
	}
	m.Set(widest, value)
}

func (b *widestBuilder) fillField(
	m protoreflect.Message, fd protoreflect.FieldDescriptor,
	in protoreflect.FullName,
) {
	switch {
	case fd.IsMap():
		entries := m.Mutable(fd).Map()
		for i := b.count(fd); i > 0; i-- {
			key := b.mapKey(fd)
			if fd.MapValue().Kind() == protoreflect.MessageKind {
				value := entries.NewValue()
				b.fillMessage(value.Message(), fd.FullName())
				entries.Set(key, value)
			} else {
				entries.Set(key, b.scalar(fd.MapValue(), fd.FullName()))
			}
		}
	case fd.IsList():
		list := m.Mutable(fd).List()
		for i := b.count(fd); i > 0; i-- {
			if fd.Kind() == protoreflect.MessageKind {
				elem := list.NewElement()
				b.fillMessage(elem.Message(), fd.FullName())
				list.Append(elem)
			} else {
				list.Append(b.scalar(fd, fd.FullName()))
			}
		}
	case fd.Kind() == protoreflect.MessageKind:
		b.fillMessage(m.Mutable(fd).Message(), in)
	default:
		m.Set(fd, b.scalar(fd, in))
	}
}

func (b *widestBuilder) count(fd protoreflect.FieldDescriptor) int {
	n, ok := b.counts[fd.FullName()]
	if !ok {
		b.t.Fatalf("%s is a list or map with no count in widestCounts: size "+
			"it at its cardinality limit (architecture.md, Cardinality "+
			"limits) before a message carries it", fd.FullName())
	}
	b.used[fd.FullName()] = true
	return n
}

func (b *widestBuilder) mapKey(fd protoreflect.FieldDescriptor) protoreflect.MapKey {
	b.key--
	switch fd.MapKey().Kind() {
	case protoreflect.Uint64Kind:
		return protoreflect.ValueOfUint64(b.key).MapKey()
	case protoreflect.StringKind:
		n, ok := b.keyLens[fd.FullName()]
		if !ok {
			b.t.Fatalf("%s has string keys and no key length in "+
				"widestKeyLens", fd.FullName())
		}
		b.used[fd.FullName()] = true
		return protoreflect.ValueOfString(fmt.Sprintf("%0*x", n, b.key)).MapKey()
	default:
		b.t.Fatalf("%s: no widest key for a %v key", fd.FullName(),
			fd.MapKey().Kind())
		return protoreflect.MapKey{}
	}
}

// scalar is the widest value of one non-message field; in is the list or map
// field its message is an entry of.
func (b *widestBuilder) scalar(
	fd protoreflect.FieldDescriptor, in protoreflect.FullName,
) protoreflect.Value {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return protoreflect.ValueOfBool(true)
	case protoreflect.EnumKind:
		values := fd.Enum().Values()
		widest := values.Get(0).Number()
		for i := 1; i < values.Len(); i++ {
			widest = max(widest, values.Get(i).Number())
		}
		return protoreflect.ValueOfEnum(widest)
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return protoreflect.ValueOfUint32(math.MaxUint32)
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return protoreflect.ValueOfUint64(math.MaxUint64)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind,
		protoreflect.Sfixed32Kind:
		return protoreflect.ValueOfInt32(math.MinInt32)
	case protoreflect.Int64Kind, protoreflect.Sint64Kind,
		protoreflect.Sfixed64Kind:
		return protoreflect.ValueOfInt64(math.MinInt64)
	case protoreflect.FloatKind:
		return protoreflect.ValueOfFloat32(math.MaxFloat32)
	case protoreflect.DoubleKind:
		return protoreflect.ValueOfFloat64(math.MaxFloat64)
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(b.str(b.strLen(fd, in)))
	default:
		b.t.Fatalf("%s: no widest value for a %v field", fd.FullName(),
			fd.Kind())
		return protoreflect.Value{}
	}
}

func (b *widestBuilder) strLen(
	fd protoreflect.FieldDescriptor, in protoreflect.FullName,
) int {
	switch fd.FullName() {
	case "ResInfo.res_name":
		n, ok := b.rowNames[in]
		if !ok {
			b.t.Fatalf("a row of %s has no name length in widestRowNames", in)
		}
		b.used[in] = true
		return n
	case "ResInfo.details", "AgentReply.details":
		return b.rowText
	}
	if n, ok := b.strLens[fd.FullName()]; ok {
		b.used[fd.FullName()] = true
		return n
	}
	return common.MaxStrSize
}

func (b *widestBuilder) str(n int) string {
	s, ok := b.strs[n]
	if !ok {
		s = strings.Repeat("a", n)
		b.strs[n] = s
	}
	return s
}

// withMigrationSides gives the first MaxMigrCntPerSp legs of the slices a
// second side, the destination a migration adds to its leg: a pool runs at
// most that many migrations at once.
func withMigrationSides(slices []*pb.Slice) {
	left := common.MaxMigrCntPerSp
	for _, slice := range slices {
		for _, grp := range append(slice.GetMetaGrpList(), slice.GetDataGrpList()...) {
			for _, leg := range append(grp.GetLegList(), grp.GetSpareLegList()...) {
				if left == 0 {
					return
				}
				dst := proto.Clone(leg.GetSideList()[0]).(*pb.Side)
				dst.SideId--
				leg.SideList = append(leg.SideList, dst)
				left--
			}
		}
	}
}

// slicesOf is the slices of a cntlr syncup in key order.
func slicesOf(req *pb.SyncupCntlrRequest) []*pb.Slice {
	keys := make([]string, 0, len(req.GetIdToSlice()))
	for key := range req.GetIdToSlice() {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	slices := make([]*pb.Slice, 0, len(keys))
	for _, key := range keys {
		slices = append(slices, req.GetIdToSlice()[key])
	}
	return slices
}

// sliceShape is what a pool's slices hold.
type sliceShape struct {
	slices, groups, legs, spares, sides int
}

func shapeOf(slices []*pb.Slice) sliceShape {
	shape := sliceShape{slices: len(slices)}
	for _, slice := range slices {
		for _, grp := range append(slice.GetMetaGrpList(), slice.GetDataGrpList()...) {
			shape.groups++
			shape.legs += len(grp.GetLegList())
			shape.spares += len(grp.GetSpareLegList())
			for _, leg := range append(grp.GetLegList(), grp.GetSpareLegList()...) {
				shape.sides += len(leg.GetSideList())
			}
		}
	}
	return shape
}

// widestPoolShape is the shape of a pool's slices at the limits: every slice
// of MaxSliceCntPerSp holds MaxGrpCntPerSlice groups in each of its two
// lists, every group MaxAllocLegPerGrp legs of one side each and, in the pool
// without repairs, MaxSpareLegPerGrp spares of one side each, and
// MaxMigrCntPerSp migrations add a side each. The clones' groups are not in
// it; cloneGrpShape is theirs.
func widestPoolShape(withRepairs bool) sliceShape {
	groups := common.MaxSliceCntPerSp * 2 * common.MaxGrpCntPerSlice
	legs := groups * common.MaxAllocLegPerGrp
	spares := groups * common.MaxSpareLegPerGrp
	if withRepairs {
		spares = 0
	}
	return sliceShape{
		slices: common.MaxSliceCntPerSp,
		groups: groups,
		legs:   legs,
		spares: spares,
		sides:  legs + spares + common.MaxMigrCntPerSp,
	}
}

// cloneGrpShape is the shape of the clone meta groups of a pool at the
// limits: one group per clone of MaxCloneCntPerSp, of the legs and spares of
// a slice's group. No migration moves one of its legs, so each side is alone
// on its leg.
func cloneGrpShape(withRepairs bool) sliceShape {
	groups := common.MaxCloneCntPerSp
	legs := groups * common.MaxAllocLegPerGrp
	spares := groups * common.MaxSpareLegPerGrp
	if withRepairs {
		spares = 0
	}
	return sliceShape{
		groups: groups,
		legs:   legs,
		spares: spares,
		sides:  legs + spares,
	}
}

func mib(n int) string {
	return fmt.Sprintf("%d bytes (%.2f MiB)", n, float64(n)/(1<<20))
}

// TestWidestMessagesFitGrpcMaxMsgSize holds the widest messages dnv sends to
// GrpcMaxMsgSize, the message limit of every dnv gRPC server and client
// (grpc.md, Wiring): the cntlr syncup and the pool get reply of a pool at the
// limits, and the report of a cntlr of that pool, with no row text and with
// rowTextAllowance of text on every row, each for the pool whose every group
// has its spares and for the one whose every group has its repair
// (widestCounts). The first three fit with half the limit to spare; the
// report with text fits. A row's text is the one part of the report no limit
// bounds (architecture.md, Known limits).
func TestWidestMessagesFitGrpcMaxMsgSize(t *testing.T) {
	if common.EtcdMaxRequestBytes > common.GrpcMaxMsgSize {
		t.Errorf("EtcdMaxRequestBytes %d is above GrpcMaxMsgSize %d: a value "+
			"etcd took could fail the message that carries it to an agent",
			common.EtcdMaxRequestBytes, common.GrpcMaxMsgSize)
	}
	var builders []*widestBuilder
	for _, withRepairs := range []bool{false, true} {
		pool := "the pool with spares"
		if withRepairs {
			pool = "the pool with repairs"
		}
		t.Run(pool, func(t *testing.T) {
			b := newWidestBuilder(t, 0, withRepairs)
			builders = append(builders, b)
			checkWidestMessages(t, b, withRepairs)
		})
	}
	if unused := unusedByAll(builders...); len(unused) > 0 {
		t.Errorf("table entries no field of the three messages reaches: %v",
			unused)
	}
}

// checkWidestMessages builds the three messages of one pool with b and holds
// them to their shapes and to GrpcMaxMsgSize.
func checkWidestMessages(t *testing.T, b *widestBuilder, withRepairs bool) {
	syncup := &pb.SyncupCntlrRequest{}
	b.build(syncup)
	withMigrationSides(slicesOf(syncup))
	poolGet := &pb.GetStoragePoolReply{}
	b.build(poolGet)
	withMigrationSides(poolGet.GetSliceList())
	noText := &pb.SyncupCntlrReply{}
	b.build(noText)

	want := widestPoolShape(withRepairs)
	for _, c := range []struct {
		name   string
		slices []*pb.Slice
	}{
		{"the cntlr syncup", slicesOf(syncup)},
		{"the pool get reply", poolGet.GetSliceList()},
	} {
		if got := shapeOf(c.slices); got != want {
			t.Errorf("%s holds %+v, want the widest pool %+v", c.name, got, want)
		}
	}
	clones := cloneGrpShape(withRepairs)
	var cloneGrps []*pb.Slice
	for _, clone := range syncup.GetCloneList() {
		cloneGrps = append(cloneGrps, &pb.Slice{
			MetaGrpList: []*pb.Group{clone.GetMetaGrp()},
		})
	}
	if got := shapeOf(cloneGrps); got.groups != clones.groups ||
		got.legs != clones.legs || got.spares != clones.spares ||
		got.sides != clones.sides {
		t.Errorf("the cntlr syncup's clones hold %+v, want the widest "+
			"clone meta groups %+v", got, clones)
	}
	repairs := 0
	if withRepairs {
		repairs = want.groups + clones.groups
	}
	info := noText.GetCntlrInfo()
	thinRows := 0
	for _, thin := range info.GetTdIdToThinInfo() {
		thinRows += len(thin.GetSliceIdToDmThin())
	}
	chunkIds := 0
	for _, bm := range noText.GetBmInfoList() {
		chunkIds += len(bm.GetChunkIdList())
	}
	newLegs := 0
	for _, rep := range syncup.GetNameToRepair() {
		newLegs += len(rep.GetNewLegList())
	}
	for _, c := range []struct {
		name      string
		got, want int
	}{
		// A leg row per leg of every group of the pool, the clones' groups
		// included, and per spare or new leg, as many as the pool with
		// spares has spares.
		{"leg rows", len(info.GetLegIdToLeg()),
			widestPoolShape(false).legs + widestPoolShape(false).spares +
				cloneGrpShape(false).legs + cloneGrpShape(false).spares},
		// Every side of a leg with two sides: twice the legs a migration
		// gave a second side.
		{"path rows", len(info.GetSideIdToPath()),
			2 * (want.sides - want.legs - want.spares)},
		{"group rows", len(info.GetGrpIdToMdRaid()),
			want.groups + clones.groups},
		{"thin rows", thinRows, len(syncup.GetTdList()) * want.slices},
		{"clone chunk ids", chunkIds,
			len(syncup.GetCloneList()) * want.slices * common.MaxCloneBmCnt},
		{"repairs in the syncup", len(syncup.GetNameToRepair()), repairs},
		{"new legs of the repairs", newLegs, repairs},
		{"repair names in the pool get reply",
			len(poolGet.GetSpConf().GetRepairNameList()), repairs},
		{"repair verdicts", len(info.GetRepIdToRepairInfo()), repairs},
		{"group readings", len(info.GetGrpIdToGrpInfo()),
			want.groups + clones.groups},
	} {
		if c.got != c.want {
			t.Errorf("the messages hold %d %s, want %d", c.got, c.name,
				c.want)
		}
	}

	withText := &pb.SyncupCntlrReply{}
	newWidestBuilder(t, rowTextAllowance, withRepairs).build(withText)

	for _, c := range []struct {
		name string
		msg  proto.Message
		// spare is how many times over the message must fit the limit.
		spare int
	}{
		{"the cntlr syncup", syncup, 2},
		{"the pool get reply", poolGet, 2},
		{"the cntlr report with no row text", noText, 2},
		{fmt.Sprintf("the cntlr report with %d bytes of text per row",
			rowTextAllowance), withText, 1},
	} {
		size := proto.Size(c.msg)
		t.Logf("%s: %s", c.name, mib(size))
		if c.spare*size > common.GrpcMaxMsgSize {
			t.Errorf("%s is %s; %d of it must fit GrpcMaxMsgSize, %s",
				c.name, mib(size), c.spare, mib(common.GrpcMaxMsgSize))
		}
	}
}

// TestFlipProvisionedAtTheSliceCeiling commits the transaction that SIZES
// EtcdMaxRequestBytes (dnv-worker.md, Constants this document owns): the
// provisioned flip (dnv-worker.md RW18) of one side in each of
// MaxSliceCntPerSp widest slices, which writes every slice of the pool in one
// transaction. It needs both halves of the requirement: the test etcd's
// --max-request-bytes and the client's send limit, which etcdutil.New sets
// (dnv-worker.md EU1). It writes those slices twice, tens of MiB each run, so
// it runs on an etcd of its own (newPrivateTestClient).
func TestFlipProvisionedAtTheSliceCeiling(t *testing.T) {
	cli := newPrivateTestClient(t)
	cid := testCid(t)
	ctx := context.Background()
	const shard = uint32(7)
	const spId = uint64(math.MaxUint64)

	// The pool with spares: a group's two spares are wider than its
	// repair's name (widestCounts).
	b := newWidestBuilder(t, 0, false)
	slices := make([]*pb.Slice, common.MaxSliceCntPerSp)
	for i := range slices {
		slices[i] = &pb.Slice{}
		b.build(slices[i])
	}
	withMigrationSides(slices)
	var refs []SideRef
	payload := 0
	for i, slice := range slices {
		sliceId := math.MaxUint64 - uint64(i)
		// Every id the builder writes is the widest, and withMigrationSides
		// gives its copies the next one down; the side to flip gets ids of
		// its own, so the flip finds it and no other.
		leg := slice.GetMetaGrpList()[0].GetLegList()[0]
		leg.LegId = math.MaxUint64 - 1
		side := leg.GetSideList()[0]
		side.SideId = math.MaxUint64 - 2
		side.Provisioned = false
		refs = append(refs, SideRef{
			SliceId: sliceId, LegId: leg.GetLegId(), SideId: side.GetSideId(),
		})
		key := SliceKey(cid, spId, sliceId)
		payload += len(key) + proto.Size(slice)
		mustPut(t, cli, key, slice)
	}
	mustPut(t, cli, SpRevKey(shard, cid, spId), &pb.SpRev{
		SpName: "sp", Revision: 1,
	})
	// The fixture stays a reproduction: the slices it writes are above the
	// etcd client's own send limit, and so above etcd's own default request
	// cap, with room under EtcdMaxRequestBytes for the rest of the request.
	t.Logf("the flip writes %s of slices", mib(payload))
	if payload <= clientv3DefaultSendBytes ||
		payload+payload/8 > common.EtcdMaxRequestBytes {
		t.Fatalf("the slices are %s: they must be above the etcd client's "+
			"default %s and, with an eighth more for the rest of the "+
			"request, within EtcdMaxRequestBytes %s", mib(payload),
			mib(clientv3DefaultSendBytes), mib(common.EtcdMaxRequestBytes))
	}

	flipped, _, err := FlipProvisioned(ctx, cli, cid, shard, spId, refs)
	if err != nil {
		t.Fatalf("FlipProvisioned over %d widest slices: %v (\"trying to "+
			"send message larger than max\" is the client's send limit, "+
			"\"request is too large\" or \"received message larger than "+
			"max\" etcd's --max-request-bytes)", len(slices), err)
	}
	if len(flipped) != len(refs) {
		t.Errorf("flipped %d sides, want all %d", len(flipped), len(refs))
	}
	for i := range min(len(flipped), len(refs)) {
		if flipped[i] != refs[i] {
			t.Errorf("flipped[%d] = %+v, want %+v", i, flipped[i], refs[i])
		}
	}
	rev := &pb.SpRev{}
	if found, err := cli.Get(ctx, SpRevKey(shard, cid, spId), rev); err != nil || !found {
		t.Fatalf("SpRev: found=%v err=%v", found, err)
	}
	if rev.GetRevision() != 2 {
		t.Errorf("SpRev revision %d, want 2: the flip bumps it once",
			rev.GetRevision())
	}
	// One transaction wrote every slice: they share one mod revision.
	keys, _, err := cli.RangeKeys(ctx,
		prefixOf(common.DnvPrefix, kindSlice, idField(cid), idField(spId)))
	if err != nil {
		t.Fatalf("RangeKeys: %v", err)
	}
	if len(keys) != len(slices) {
		t.Fatalf("%d slice keys, want %d", len(keys), len(slices))
	}
	for _, key := range keys {
		if key.ModRev != keys[0].ModRev {
			t.Errorf("slice key %q at mod revision %d and %q at %d: the "+
				"flip took more than one transaction", key.Key, key.ModRev,
				keys[0].Key, keys[0].ModRev)
		}
	}
	got := &pb.Slice{}
	if found, err := cli.Get(ctx, SliceKey(cid, spId, refs[0].SliceId), got); err != nil || !found {
		t.Fatalf("Get the first slice: found=%v err=%v", found, err)
	}
	if side := findSideOfLeg(got, refs[0].LegId, refs[0].SideId); !side.GetProvisioned() {
		t.Errorf("the first slice's side reads %v after the flip, want "+
			"provisioned", side)
	}
}
