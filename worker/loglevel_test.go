package worker

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// pinnedWarnReasons are the skip reasons that hold until an operator acts
// (log.md R6; dnv-worker.md, Log records), written out here so a change of
// the set in the code fails this test: no failover can happen, nothing is
// left to allocate, the pool cannot grow, a group with no copy or no healthy
// leg has a bad leg, a repair holds its finish for a kept retired leg, or the
// primary refuses a take-back.
var pinnedWarnReasons = map[string]bool{
	"no candidate":            true,
	"shared_state":            true,
	"same_error":              true,
	"candidate_refuses":       true,
	"no_healthy_leg":          true,
	"redund_none":             true,
	"meta_ladder_cap":         true,
	"grp_list_full":           true,
	"cn free_ext_cnt too low": true,
	"no id left":              true,
	"retired legs kept":       true,
	"take_back_refused":       true,
}

// pinnedSkipLevel is the level word a skip record with reason carries.
func pinnedSkipLevel(reason string) string {
	if pinnedWarnReasons[reason] {
		return "WARN"
	}
	return "INFO"
}

// log.md R6: skipLevel gives Warn to exactly the pinned reasons, and Info to
// every other reason the worker or a model op gives: a wait the worker ends
// alone, a hold an operator already set, a transient failure.
func TestSkipLevel(t *testing.T) {
	info := []string{
		"grow_pending", "grp_row_error", "grp_row_missing", "grp_row_unknown",
		"grp_row_provisioning", "grp_row_pending", "grp_row_absent",
		"grp_row_stale", "op_failed", "grp_forced_start",
		"grp_operator_in_charge", "grp_has_repair", "leg_has_two_sides",
		"migration_copy_not_whole", "data_covered", "no_data_group",
		"candidate changed", "stale revision", "retired leg carries keep",
		"clone_deleting", "abort_flag_clear", "grp_not_found",
		"grp_not_whole", "grp_leg_unhealthy", "cutover started", "md_check_waits", "grp_leg_bad_blocks", "grp_every_leg_bad_blocks",
		"cutover pending", "migration gone", "repair exists",
		"repair not found", "retired leg not found", "retired leg list full",
		"sp level suppresses reactions", "sp level builds no group array",
		"",
	}
	for _, reason := range info {
		if got := skipLevel(reason); got != slog.LevelInfo {
			t.Errorf("skipLevel(%q) = %v, want INFO", reason, got)
		}
	}
	for reason := range pinnedWarnReasons {
		if got := skipLevel(reason); got != slog.LevelWarn {
			t.Errorf("skipLevel(%q) = %v, want WARN", reason, got)
		}
	}
	if len(warnSkipReasons) != len(pinnedWarnReasons) {
		t.Errorf("warnSkipReasons holds %d reasons, the pin %d",
			len(warnSkipReasons), len(pinnedWarnReasons))
	}
}

// log.md R6: a rev key or value a shard worker cannot parse leaves its object
// undriven until an operator mends the record, so its record is at Warn; it
// is logged once per scan or watch event, so the level floods nothing.
func TestMalformedRevRecordsAreWarn(t *testing.T) {
	logs := captureLogs(t)
	w := &shardWorker{kind: dnKind(), shard: testShard,
		ctx: context.Background()}
	if _, ok := w.parseKey("not a rev key"); ok {
		t.Fatal("a key that does not parse was taken")
	}
	if _, ok := w.parseKey(model.DnRevKey(testShard+1, 1, 10)); ok {
		t.Fatal("another shard's key was taken")
	}
	if _, ok := w.parseValue("k", nil); ok {
		t.Fatal("a value that did not decode was taken")
	}
	if _, ok := w.parseValue("k", &pb.CnRev{}); ok {
		t.Fatal("a value of another kind was taken")
	}
	for msg, want := range map[string]int{
		"rev key malformed": 2, "rev value malformed": 2,
	} {
		recs := logs.withMsg(msg)
		if len(recs) != want {
			t.Errorf("%d %q records, want %d", len(recs), msg, want)
		}
		for _, rec := range recs {
			if rec["level"] != "WARN" {
				t.Errorf("%q at %v, want WARN", msg, rec["level"])
			}
		}
	}
}

// log.md R6 (CM5): a failed close of the etcd client at the stop is an event
// the program cannot act on, at Info, as in the gateway and the cdc. The ctx
// is cancelled before Run, so it stops at once, and the client was closed
// before, so its last step's close fails.
func TestFailedCloseAtStopIsInfo(t *testing.T) {
	logs := captureLogs(t)
	cli, err := etcdutil.New(context.Background(), []string{"127.0.0.1:1"},
		time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, cli, testConfig(common.WorkerRoleDn)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	recs := logs.withMsg("etcd client close failed")
	if len(recs) != 1 || recs[0]["level"] != "INFO" {
		t.Errorf("close records %v, want one at INFO", recs)
	}
}
