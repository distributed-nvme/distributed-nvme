package model

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestSpCnChargesAddsUpPerCn pins SpCnCharges: each CN is charged the
// group's ext_cnt once per cntlr of the SP it holds, in the order of its first
// cntlr, and a cntlr the map lacks contributes nothing.
func TestSpCnChargesAddsUpPerCn(t *testing.T) {
	conf := &pb.SpConf{CntlrIdList: []uint64{1, 2, 3, 4}}
	cntlrs := map[uint64]*pb.Cntlr{
		1: {AddrPort: "cn-b:1"},
		2: {AddrPort: "cn-a:1"},
		3: {AddrPort: "cn-b:1"},
	}
	order, charges := SpCnCharges(conf, cntlrs, 5)
	if len(order) != 2 || order[0] != "cn-b:1" || order[1] != "cn-a:1" {
		t.Fatalf("order = %v, want [cn-b:1 cn-a:1]", order)
	}
	if charges["cn-b:1"] != 10 || charges["cn-a:1"] != 5 {
		t.Fatalf("charges = %v, want cn-b 10 and cn-a 5", charges)
	}
}

// TestCnBudgetShort pins CnBudgetShort: a CN is short only when its free
// extents are below its charge, the first short CN of the order is named, and
// a CN the records lack is not judged.
func TestCnBudgetShort(t *testing.T) {
	order := []string{"cn-a:1", "cn-b:1", "cn-c:1"}
	charges := map[string]uint64{"cn-a:1": 4, "cn-b:1": 4, "cn-c:1": 4}
	cns := map[string]*pb.CnConf{
		"cn-a:1": {FreeExtCnt: 4},
		"cn-c:1": {FreeExtCnt: 3},
	}
	addr, free, short := CnBudgetShort(order, charges, cns)
	if !short || addr != "cn-c:1" || free != 3 {
		t.Fatalf("got (%q, %d, %v), want cn-c:1 short with 3 free",
			addr, free, short)
	}
	cns["cn-c:1"].FreeExtCnt = 4
	if addr, _, short := CnBudgetShort(order, charges, cns); short {
		t.Fatalf("%s short with free equal to its charge", addr)
	}
}
