package worker

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// HL1 — the dn and cn node tables, row by row
// ---------------------------------------------------------------------------

// TestHealthDnTable walks the HL1 table for a DN: an ERROR row in disk_info,
// meta_info or port_info sets; a clean reply clears; PROVISIONING and MISSING
// neither set nor clear; agent_reply.code != 0 neither sets nor clears.
func TestHealthDnTable(t *testing.T) {
	cases := []struct {
		name    string
		code    uint32
		info    *pb.DnInfo
		want    healthObs
		wantRes string
	}{
		{
			name: "clean",
			info: &pb.DnInfo{
				DiskInfo: resOk("disk"),
				MetaInfo: resOk("meta"),
				PortInfo: resOk("port"),
			},
			want: healthClean,
		},
		{
			name: "no info at all is a clean round (HL5)",
			info: nil,
			want: healthClean,
		},
		{
			name: "disk error",
			info: &pb.DnInfo{
				DiskInfo: resErr("disk", "io error"),
				MetaInfo: resOk("meta"),
			},
			want:    healthErrorRow,
			wantRes: "disk",
		},
		{
			name: "meta error: disk lacks Write Zeroes is a plain ERROR",
			info: &pb.DnInfo{
				DiskInfo: resOk("disk"),
				MetaInfo: resErr("meta", "disk lacks Write Zeroes"),
			},
			want:    healthErrorRow,
			wantRes: "meta",
		},
		{
			name: "port error",
			info: &pb.DnInfo{
				DiskInfo: resOk("disk"),
				PortInfo: resErr("port", "nvmet"),
			},
			want:    healthErrorRow,
			wantRes: "port",
		},
		{
			// PROVISIONING and MISSING are not ERROR rows, so the round is
			// HL1 row 3's clean one: it CLEARS a set err_epoch and never sets
			// one. That is the reading HL1 row 4 and architecture.md §9.5
			// require — "never sets err_epoch, never counts as bad for the
			// §5.6 capacity keys" ([D15]) — and healthNone here would instead
			// freeze a stale epoch on a healthy node.
			name: "provisioning and missing are not ERROR rows: a clean round",
			info: &pb.DnInfo{
				DiskInfo: resStatus(
					"disk", pb.ResStatus_RES_STATUS_PROVISIONING,
				),
				MetaInfo: resStatus("meta", pb.ResStatus_RES_STATUS_MISSING),
			},
			want: healthClean,
		},
		{
			name: "code != 0",
			code: common.ReplyCodeUnknownObject,
			info: &pb.DnInfo{DiskInfo: resErr("disk", "io error")},
			want: healthNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs, res := dnObservation(tc.code, tc.info)
			if obs != tc.want {
				t.Fatalf("obs = %v, want %v", obs, tc.want)
			}
			if res != tc.wantRes {
				t.Fatalf("res_name = %q, want %q", res, tc.wantRes)
			}
		})
	}
}

// TestHealthCnTable walks the HL1 table for a CN: port_info, tmpfs_info,
// tmp_file_info and loop_dev_info are the ERROR sources.
func TestHealthCnTable(t *testing.T) {
	cases := []struct {
		name    string
		code    uint32
		info    *pb.CnInfo
		want    healthObs
		wantRes string
	}{
		{
			name: "clean",
			info: &pb.CnInfo{PortInfo: resOk("port")},
			want: healthClean,
		},
		{
			name:    "port error",
			info:    &pb.CnInfo{PortInfo: resErr("port", "x")},
			want:    healthErrorRow,
			wantRes: "port",
		},
		{
			name:    "tmpfs error",
			info:    &pb.CnInfo{TmpfsInfo: resErr("tmpfs", "x")},
			want:    healthErrorRow,
			wantRes: "tmpfs",
		},
		{
			name:    "tmp file error",
			info:    &pb.CnInfo{TmpFileInfo: resErr("tmp_file", "x")},
			want:    healthErrorRow,
			wantRes: "tmp_file",
		},
		{
			name:    "loop dev error",
			info:    &pb.CnInfo{LoopDevInfo: resErr("loop", "x")},
			want:    healthErrorRow,
			wantRes: "loop",
		},
		{
			name: "provisioning is not an ERROR row: a clean round",
			info: &pb.CnInfo{
				PortInfo: resStatus(
					"port", pb.ResStatus_RES_STATUS_PROVISIONING,
				),
			},
			want: healthClean,
		},
		{
			name: "code != 0",
			code: common.ReplyCodeStaleRevision,
			info: &pb.CnInfo{PortInfo: resErr("port", "x")},
			want: healthNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs, res := cnObservation(tc.code, tc.info)
			if obs != tc.want {
				t.Fatalf("obs = %v, want %v", obs, tc.want)
			}
			if res != tc.wantRes {
				t.Fatalf("res_name = %q, want %q", res, tc.wantRes)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// HL2 — the sp-object tables
// ---------------------------------------------------------------------------

// TestHealthCntlrTable checks HL2's cntlr row: any ERROR row other than
// leg_id_to_leg, which belongs to the legs.
func TestHealthCntlrTable(t *testing.T) {
	clean := &pb.CntlrInfo{
		SliceIdToDmPool: map[uint64]*pb.ResInfo{1: resOk("pool")},
		LegIdToLeg:      map[uint64]*pb.ResInfo{5: resOk("leg")},
	}
	if obs, _ := cntlrObservation(0, clean); obs != healthClean {
		t.Fatalf("clean cntlr = %v", obs)
	}
	legOnly := &pb.CntlrInfo{
		SliceIdToDmPool: map[uint64]*pb.ResInfo{1: resOk("pool")},
		LegIdToLeg:      map[uint64]*pb.ResInfo{5: resErr("leg", "io")},
	}
	if obs, _ := cntlrObservation(0, legOnly); obs != healthClean {
		t.Fatalf("a leg error must not set Cntlr.err_epoch, got %v", obs)
	}
	bad := &pb.CntlrInfo{
		SliceIdToDmPool: map[uint64]*pb.ResInfo{1: resErr("pool", "x")},
	}
	obs, res := cntlrObservation(0, bad)
	if obs != healthErrorRow || res != "pool" {
		t.Fatalf("obs = %v, res = %q", obs, res)
	}
	// A thin row is a cntlr row too.
	thin := &pb.CntlrInfo{
		TdIdToThinInfo: map[uint64]*pb.CntlrInfo_ThinInfo{
			9: {SliceIdToDmThin: map[uint64]*pb.ResInfo{
				1: resErr("thin", "x"),
			}},
		},
	}
	if obs, res := cntlrObservation(0, thin); obs != healthErrorRow ||
		res != "thin" {
		t.Fatalf("thin obs = %v, res = %q", obs, res)
	}
	if obs, _ := cntlrObservation(1, bad); obs != healthNone {
		t.Fatalf("code != 0 must neither set nor clear, got %v", obs)
	}
}

// TestHealthSideTable checks HL2's side row.
func TestHealthSideTable(t *testing.T) {
	clean := &pb.SideInfo{SideDevInfo: resOk("side")}
	if obs, _ := sideObservation(0, clean); obs != healthClean {
		t.Fatalf("clean side = %v", obs)
	}
	cases := []struct {
		name string
		info *pb.SideInfo
		res  string
	}{
		{
			name: "side dev",
			info: &pb.SideInfo{SideDevInfo: resErr("side", "x")},
			res:  "side",
		},
		{
			name: "dm error row",
			info: &pb.SideInfo{
				CnIdToDmError: map[uint64]*pb.ResInfo{2: resErr("dmerr", "x")},
			},
			res: "dmerr",
		},
		{
			name: "dm linear row",
			info: &pb.SideInfo{
				CnIdToDmLinear: map[uint64]*pb.ResInfo{2: resErr("lin", "x")},
			},
			res: "lin",
		},
		{
			name: "nvmeof row",
			info: &pb.SideInfo{
				CnIdToNvmeof: map[uint64]*pb.ResInfo{2: resErr("nvmeof", "x")},
			},
			res: "nvmeof",
		},
		{
			name: "migration source row",
			info: &pb.SideInfo{
				MigrSrcInfo: &pb.SideInfo_MigrSrcInfo{
					NvmeofInfo: resErr("migrsrc", "x"),
				},
			},
			res: "migrsrc",
		},
		{
			name: "migration destination row",
			info: &pb.SideInfo{
				MigrDstInfo: &pb.SideInfo_MigrDstInfo{
					DmCloneInfo: resErr("migrdst", "x"),
				},
			},
			res: "migrdst",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs, res := sideObservation(0, tc.info)
			if obs != healthErrorRow || res != tc.res {
				t.Fatalf("obs = %v, res = %q, want error_row/%s",
					obs, res, tc.res)
			}
		})
	}
	provisioning := &pb.SideInfo{
		SideDevInfo: resStatus(
			"side", pb.ResStatus_RES_STATUS_PROVISIONING,
		),
	}
	if obs, _ := sideObservation(0, provisioning); obs != healthClean {
		t.Fatalf("PROVISIONING must not set Side.err_epoch, got %v", obs)
	}
}

// TestHealthLegTable checks HL2's leg row: only the primary's probe result
// matters, ERROR sets, OK clears, everything else neither.
func TestHealthLegTable(t *testing.T) {
	info := &pb.CntlrInfo{
		LegIdToLeg: map[uint64]*pb.ResInfo{
			1: resErr("leg1", "io"),
			2: resOk("leg2"),
			3: resStatus("leg3", pb.ResStatus_RES_STATUS_PROVISIONING),
		},
	}
	if obs, res := legObservation(0, info, 1); obs != healthErrorRow ||
		res != "leg1" {
		t.Fatalf("leg 1 obs = %v, res = %q", obs, res)
	}
	if obs, _ := legObservation(0, info, 2); obs != healthClean {
		t.Fatalf("leg 2 obs = %v, want clean", obs)
	}
	if obs, _ := legObservation(0, info, 3); obs != healthNone {
		t.Fatalf("leg 3 obs = %v, want none", obs)
	}
	// A leg the reply does not mention at all.
	if obs, _ := legObservation(0, info, 4); obs != healthNone {
		t.Fatalf("unreported leg obs = %v, want none", obs)
	}
	if obs, _ := legObservation(1, info, 1); obs != healthNone {
		t.Fatalf("code != 0 obs = %v, want none", obs)
	}
}

// ---------------------------------------------------------------------------
// HL3 — transitions only
// ---------------------------------------------------------------------------

func healthTestDeps(t *testing.T) (*deps, *fakeHealthWriter) {
	t.Helper()
	writer := &fakeHealthWriter{}
	d := newTestDeps(
		testConfig(common.WorkerRoleDn), newFakeStore(), newFakeClock(),
	)
	d.health = writer
	return d, writer
}

// seedClusterConf installs a resolved ClusterConf in the RW21 cache. Every DN
// health write needs one: MD4 derives the DnCapacity key's bin index from
// dn_bin_conf, so newDnMonitor refuses to write without it (HL1).
func seedClusterConf(d *deps, cid uint64) {
	d.conf.mu.Lock()
	d.conf.entries[cid] = model.ResolveClusterConf(&pb.ClusterConf{
		CreationEpoch: 1,
	})
	d.conf.mu.Unlock()
}

// TestHealthTransitionsOnly checks HL3: a record is written only when the
// observed health changes, and the observations that neither set nor clear
// write nothing at all.
func TestHealthTransitionsOnly(t *testing.T) {
	logs := captureLogs(t)
	d, writer := healthTestDeps(t)
	seedClusterConf(d, 7)
	monitor := newDnMonitor(d, 7, 11, func() string { return "dn0:9520" })
	ctx := context.Background()

	// Unhealthy once.
	monitor.observe(ctx, healthUnreachable, "")
	monitor.observe(ctx, healthUnreachable, "")
	monitor.observe(ctx, healthErrorRow, "disk")
	if got := len(writer.all()); got != 1 {
		t.Fatalf("%d writes for one healthy -> unhealthy transition", got)
	}
	// Neither sets nor clears.
	monitor.observe(ctx, healthNone, "")
	if got := len(writer.all()); got != 1 {
		t.Fatalf("healthNone wrote something: %v", writer.all())
	}
	// Recovery, then more clean rounds.
	monitor.observe(ctx, healthClean, "")
	monitor.observe(ctx, healthClean, "")
	writes := writer.all()
	if len(writes) != 2 {
		t.Fatalf("writes = %v, want set then clear", writes)
	}
	if writes[0].record != healthRecordDn || writes[0].epoch == 0 {
		t.Fatalf("first write = %+v, want a nonzero dn epoch", writes[0])
	}
	if writes[1].epoch != 0 || writes[1].addr != "dn0:9520" {
		t.Fatalf("second write = %+v, want a clear on dn0:9520", writes[1])
	}
	recs := logs.withMsg(msgHealthChanged)
	if len(recs) != 2 {
		t.Fatalf("%d health changed records, want 2", len(recs))
	}
	first := recs[0]
	if first["role"] != common.WorkerRoleDn ||
		first["record"] != healthRecordDn ||
		first["reason"] != reasonUnreachable {
		t.Fatalf("first record = %v", first)
	}
	if cid, _ := first["cluster_id"].(float64); uint64(cid) != 7 {
		t.Fatalf("cluster_id = %v", first["cluster_id"])
	}
	if dnId, _ := first["dn_id"].(float64); uint64(dnId) != 11 {
		t.Fatalf("dn_id = %v", first["dn_id"])
	}
	if _, has := first["res_name"]; has {
		t.Fatalf("an unreachable record carries a res_name: %v", first)
	}
	if recs[1]["reason"] != reasonRecovered {
		t.Fatalf("second reason = %v", recs[1]["reason"])
	}
	if epoch, _ := recs[1]["err_epoch"].(float64); epoch != 0 {
		t.Fatalf("recovery err_epoch = %v, want 0", recs[1]["err_epoch"])
	}
}

// TestHealthErrorRowCarriesResName checks the res_name attribute of the §12
// record.
func TestHealthErrorRowCarriesResName(t *testing.T) {
	logs := captureLogs(t)
	d, _ := healthTestDeps(t)
	monitor := newCnMonitor(d, 3, 4, func() string { return "cn0:9620" })
	monitor.observe(context.Background(), healthErrorRow, "tmpfs")
	recs := logs.withMsg(msgHealthChanged)
	if len(recs) != 1 || recs[0]["res_name"] != "tmpfs" ||
		recs[0]["reason"] != reasonErrorRow {
		t.Fatalf("record = %v", recs)
	}
	if recs[0]["record"] != healthRecordCn {
		t.Fatalf("record kind = %v", recs[0]["record"])
	}
}

// TestHealthWriteFailureIsRetried checks that a failed write leaves the
// in-memory state untouched, so the next round retries the transition.
func TestHealthWriteFailureIsRetried(t *testing.T) {
	captureLogs(t)
	d, writer := healthTestDeps(t)
	seedClusterConf(d, 1)
	writer.err = errors.New("etcd down")
	monitor := newDnMonitor(d, 1, 2, func() string { return "dn0:9520" })
	monitor.observe(context.Background(), healthUnreachable, "")
	writer.err = nil
	monitor.observe(context.Background(), healthUnreachable, "")
	if got := len(writer.all()); got != 1 {
		t.Fatalf("%d writes, want the retry to land exactly once", got)
	}
}

// TestHealthDnWriteNeedsClusterConf checks HL1 against MD6's SetDnErrEpoch
// row: the op maintains the DnCapacity key in the same STM, and MD4 computes
// that key's bin index from the cluster's dn_bin_conf. A cluster deleted from
// the RW21 cache between the loop's RW9 gate and this write must NOT be
// papered over with ResolveDnBinConf's default 0/4/8/12 shifts — that leaves
// the real key undeleted and writes a duplicate at the wrong bin, which §6.3's
// bin scan then hands out as an allocation candidate for a DN this very write
// is flagging unhealthy. The write is skipped and retried next round (RW12).
func TestHealthDnWriteNeedsClusterConf(t *testing.T) {
	logs := captureLogs(t)
	d, writer := healthTestDeps(t)
	monitor := newDnMonitor(d, 7, 11, func() string { return "dn0:9520" })
	ctx := context.Background()

	monitor.observe(ctx, healthErrorRow, "disk")
	if got := writer.all(); len(got) != 0 {
		t.Fatalf("wrote %v with no cluster conf in the cache", got)
	}
	if got := len(logs.withMsg("health write failed")); got != 1 {
		t.Fatalf("%d health write failed records, want 1", got)
	}
	if got := len(logs.withMsg(msgHealthChanged)); got != 0 {
		t.Fatalf("a skipped write logged a health change")
	}
	// Nothing was remembered (HL3), so the next round retries the transition
	// once the conf is back.
	seedClusterConf(d, 7)
	monitor.observe(ctx, healthErrorRow, "disk")
	writes := writer.all()
	if len(writes) != 1 || writes[0].record != healthRecordDn ||
		writes[0].epoch == 0 {
		t.Fatalf("writes after the conf came back = %v", writes)
	}
}

// TestHealthSpMonitorsCarryTheirIds pins the HL2 monitors' log identities,
// which the sp role (§8.4) reuses.
func TestHealthSpMonitorsCarryTheirIds(t *testing.T) {
	logs := captureLogs(t)
	d, writer := healthTestDeps(t)
	ctx := context.Background()

	newCntlrMonitor(d, 1, 2, 3).observe(ctx, healthUnreachable, "")
	newLegMonitor(d, 1, 2, 4, 5).observe(ctx, healthErrorRow, "leg")
	newSideMonitor(d, 1, 2, 4, 6).observe(ctx, healthErrorRow, "side")

	writes := writer.all()
	if len(writes) != 3 {
		t.Fatalf("writes = %v", writes)
	}
	if writes[0].record != healthRecordCntlr || writes[0].objId != 3 {
		t.Fatalf("cntlr write = %+v", writes[0])
	}
	if writes[1].record != healthRecordLeg || writes[1].sliceId != 4 ||
		writes[1].objId != 5 {
		t.Fatalf("leg write = %+v", writes[1])
	}
	if writes[2].record != healthRecordSide || writes[2].objId != 6 {
		t.Fatalf("side write = %+v", writes[2])
	}
	recs := logs.withMsg(msgHealthChanged)
	if len(recs) != 3 {
		t.Fatalf("%d records, want 3", len(recs))
	}
	for _, rec := range recs {
		if rec["role"] != common.WorkerRoleSp {
			t.Fatalf("record role = %v, want sp (HL6)", rec["role"])
		}
		if _, has := rec["sp_id"]; !has {
			t.Fatalf("record without sp_id: %v", rec)
		}
	}
}

// TestHealthMarkUnknownClearsStaleErrors checks §9.5: while a stream is dead
// the worker records RES_STATUS_UNKNOWN on its own copy of the info, so a
// stale ERROR row does not outlive the stream that reported it.
func TestHealthMarkUnknownClearsStaleErrors(t *testing.T) {
	dn := &pb.DnInfo{DiskInfo: resErr("disk", "io")}
	markDnUnknown(dn)
	if dn.GetDiskInfo().GetStatus() != pb.ResStatus_RES_STATUS_UNKNOWN {
		t.Fatalf("disk status = %v", dn.GetDiskInfo().GetStatus())
	}
	if obs, _ := dnObservation(0, dn); obs != healthClean {
		t.Fatalf("obs after markUnknown = %v, want clean", obs)
	}
	cn := &pb.CnInfo{TmpfsInfo: resErr("tmpfs", "x")}
	markCnUnknown(cn)
	if obs, _ := cnObservation(0, cn); obs != healthClean {
		t.Fatalf("cn obs after markUnknown = %v", obs)
	}
	side := &pb.SideInfo{
		SideDevInfo:   resErr("side", "x"),
		CnIdToNvmeof:  map[uint64]*pb.ResInfo{1: resErr("nvmeof", "x")},
		CnIdToDmError: map[uint64]*pb.ResInfo{1: resErr("dmerr", "x")},
	}
	markSideUnknown(side)
	if obs, _ := sideObservation(0, side); obs != healthClean {
		t.Fatalf("side obs after markUnknown = %v", obs)
	}
	cntlr := &pb.CntlrInfo{
		SliceIdToDmPool: map[uint64]*pb.ResInfo{1: resErr("pool", "x")},
		TdIdToThinInfo: map[uint64]*pb.CntlrInfo_ThinInfo{
			2: {SliceIdToDmThin: map[uint64]*pb.ResInfo{
				1: resErr("thin", "x"),
			}},
		},
	}
	markCntlrUnknown(cntlr)
	if obs, _ := cntlrObservation(0, cntlr); obs != healthClean {
		t.Fatalf("cntlr obs after markUnknown = %v", obs)
	}
}

// TestHealthMonitorAttrsAreStable guards the §12 attribute names the
// integration suite greps.
func TestHealthMonitorAttrsAreStable(t *testing.T) {
	d, _ := healthTestDeps(t)
	monitor := newDnMonitor(d, 1, 2, func() string { return "dn0:9520" })
	want := []string{"dn_id"}
	if len(monitor.attrs) != len(want) {
		t.Fatalf("attrs = %v", monitor.attrs)
	}
	for i, attr := range monitor.attrs {
		if attr.Key != want[i] {
			t.Fatalf("attr %d = %s, want %s", i, attr.Key, want[i])
		}
	}
	var _ slog.Attr = monitor.attrs[0]
}
