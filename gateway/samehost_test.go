package gateway

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file pins the rule of architecture.md, Migrations, that two disk
// nodes of one machine never hold the two sides of one leg: two dn agents of
// one machine share one kernel, and the two sides of a leg export the same
// subsystem and namespace there, so each would take them from the other.

// setDnHost gives a planted disk node the host name its agent reported.
func (e *volEnv) setDnHost(addrPort string, host string) {
	e.t.Helper()
	dn := e.dn(addrPort)
	dn.HostName = host
	mustPut(e.t, e.cli, model.DnConfKey(e.cid, addrPort), dn)
}

// TestCreateMigrationPassesOverTheOldSidesMachine pins the pass-over: dn-c
// shares dn-a's host name, the old side's machine, though its location is
// its own, so the destination is always dn-d; with dn-d gone the migration
// is refused RESOURCE_EXHAUSTED, the text naming the machine, and nothing is
// written. Records with no host name, from before the field, keep the old
// behaviour: dn-c is a candidate again.
func TestCreateMigrationPassesOverTheOldSidesMachine(t *testing.T) {
	for draw := 0; draw < volTier1DrawCnt; draw++ {
		env := newVolEnv(t)
		env.setDnHost(volDnA, "machine-1")
		env.setDnHost(volDnC, "machine-1")
		env.setDnHost(volDnD, "machine-2")
		got, err := volMigrationDst(env)
		if err != nil {
			t.Fatalf("CreateMigration: %v", err)
		}
		if got != volDnD {
			t.Fatalf("draw %d: destination on %s, want %s (dn-c is on "+
				"the old side's machine)", draw, got, volDnD)
		}
	}
	env := newVolEnv(t)
	env.setDnHost(volDnA, "machine-1")
	env.setDnHost(volDnC, "machine-1")
	env.dropDn(volDnD)
	before := env.dump()
	_, err := volMigrationDst(env)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("CreateMigration with only the old side's machine left: "+
			"%v, want RESOURCE_EXHAUSTED", err)
	}
	msg := status.Convert(err).Message()
	for _, want := range []string{`"machine-1"`, "one machine"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not say %q", msg, want)
		}
	}
	env.wantNoWrite("the refused CreateMigration", before)

	old := newVolEnv(t)
	old.dropDn(volDnD)
	got, err := volMigrationDst(old)
	if err != nil {
		t.Fatalf("CreateMigration over records with no host name: %v", err)
	}
	if got != volDnC {
		t.Errorf("destination on %s, want %s", got, volDnC)
	}
}

// TestCreateDiskNodeStoresTheHostName pins the stored host name: the node's
// record keeps the name its agent reported at the create.
func TestCreateDiskNodeStoresTheHostName(t *testing.T) {
	s := newTestServer(t)
	cluster := hnodeName("samehost")
	cid := mustCluster(t, s, cluster)
	addr := fakeAddrPort(t, "dn")
	startFakeAgent(t, addr, 64<<30).setHostName("machine-7")
	if _, err := s.CreateDiskNode(t.Context(), &pb.CreateDiskNodeRequest{
		ClusterName: cluster,
		AddrPort:    addr,
		NvmeTrConf:  testTrConf(addr),
	}); err != nil {
		t.Fatalf("CreateDiskNode: %v", err)
	}
	dn := &pb.DnConf{}
	hnodeGet(t, s, model.DnConfKey(cid, addr), dn)
	if dn.GetHostName() != "machine-7" {
		t.Errorf("stored host name %q, want machine-7", dn.GetHostName())
	}
}

// TestCreateMigrationPassOverIsNoLocation pins two edges of the pass-over:
// the old side's machine's other disk nodes join the black list but not the
// tier-1 locations, so dn-d, in dn-c's location though on another machine,
// stays a tier-1 candidate beside dn-e; and with no other node of the old
// side's host name, a refusal does not name the machine.
func TestCreateMigrationPassOverIsNoLocation(t *testing.T) {
	sawD := false
	for draw := 0; draw < volTier1DrawCnt && !sawD; draw++ {
		env := newVolEnv(t)
		env.putDn(volDnE, 9, 3, nil)
		env.setDnHost(volDnA, "machine-1")
		env.setDnHost(volDnC, "machine-1")
		env.setDnHost(volDnD, "machine-2")
		env.setDnHost(volDnE, "machine-3")
		env.relocateDn(volDnD, volDnC)
		got, err := volMigrationDst(env)
		if err != nil {
			t.Fatalf("CreateMigration: %v", err)
		}
		switch got {
		case volDnD:
			sawD = true
		case volDnE:
		default:
			t.Fatalf("draw %d: destination on %s, want dn-d or dn-e", draw,
				got)
		}
	}
	if !sawD {
		t.Errorf("dn-d never drawn in %d draws: dn-c's location was "+
			"excluded as if it were the group's", volTier1DrawCnt)
	}
	env := newVolEnv(t)
	env.setDnHost(volDnA, "machine-1")
	env.dropDn(volDnC)
	env.dropDn(volDnD)
	_, err := volMigrationDst(env)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("CreateMigration with no disk node left: %v, want "+
			"RESOURCE_EXHAUSTED", err)
	}
	if strings.Contains(status.Convert(err).Message(), "machine-1") {
		t.Errorf("message %q names the old side's machine, which no other "+
			"node shares", status.Convert(err).Message())
	}
}

// TestCreateDiskNodeStoresNoNameForAnUnnamedMachine pins localhost, one of
// the names every unnamed machine reports (storedHostName keeps none of
// them): no name is kept, so no migration passes the node over for it.
func TestCreateDiskNodeStoresNoNameForAnUnnamedMachine(t *testing.T) {
	s := newTestServer(t)
	cluster := hnodeName("unnamed")
	cid := mustCluster(t, s, cluster)
	addr := fakeAddrPort(t, "dn")
	startFakeAgent(t, addr, 64<<30).setHostName("localhost")
	if _, err := s.CreateDiskNode(t.Context(), &pb.CreateDiskNodeRequest{
		ClusterName: cluster,
		AddrPort:    addr,
		NvmeTrConf:  testTrConf(addr),
		Location:    "rack-1",
	}); err != nil {
		t.Fatalf("CreateDiskNode: %v", err)
	}
	dn := &pb.DnConf{}
	hnodeGet(t, s, model.DnConfKey(cid, addr), dn)
	if dn.GetHostName() != "" {
		t.Errorf("stored host name %q, want none", dn.GetHostName())
	}
}
