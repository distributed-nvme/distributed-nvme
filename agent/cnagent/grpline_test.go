package cnagent

import (
	"testing"
)

// TestAConvergeGroupRowCarriesTheStateLine: the group row of a converge reads
// as a read's does (CN28): OK with the array's state line, degraded and its
// sync word included, not OK with empty details for the round until the next
// read.
func TestAConvergeGroupRowCarriesTheStateLine(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.node.setMdSync(f.mdDev, 1, "recover", "100 / 200")
	info := f.syncup(t, noRepairReq(2))
	row := info.GetGrpIdToMdRaid()[testDataGrp]
	read := checkRound(t, f.srv, 2).GetGrpIdToMdRaid()[testDataGrp]
	if row.GetDetails() == "" || row.GetDetails() != read.GetDetails() {
		t.Fatalf("the converge's group row details %q, want the read's %q",
			row.GetDetails(), read.GetDetails())
	}
}
