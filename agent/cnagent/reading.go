package cnagent

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The reading of a check round and an info read (CN28), and where a
// device-mapper fact and a device number come from
//
// A check round and an info read first take one reading of the node
// (takeReading): one whole-node `dmsetup info` listing, one whole-node
// `dmsetup table` listing, one NVMe walk (CN10) and one md walk (CN12). Their
// rows and their verdict take from it which dm devices, NVMe subsystems and
// md arrays the node holds, and each dm device's attrs, number and table
// (readCntlr); cnReading says when it reads a dm name on its own instead.
// The rows' other reads are in the probe map (probe.go): the status reads,
// the matched array's attributes, each looked-up NVMe entry and the numbers
// in /sys/block among them. The verdict also lists the nvmet tree in
// configfs, and reads the namespace node and the number of each connection
// it checks against a live dm-clone (srcNqnsInUse). So such a call starts a
// few processes whatever the slice count, plus the status reads of its thin
// pools and dm-clones and the configfs `ls` of its exports (dnagent.md SH18).
//
// dmInfo, dmTable and dmDevNo read a dm device's attrs, its live table and
// its number for the helpers the converge and the probe share: from the
// pass's reading when the pass took one, else from one dmsetup run of that
// device. A dm device's number comes from dmsetup info (dmDevNo), and an md
// array's, an NVMe multipath head's or a loop device's from
// /sys/block/<kname>/dev (blockDevNo, mdDevNo); no number comes from lsblk.
// ---------------------------------------------------------------------------

// cnReading is what a check round or an info read takes of the node first
// (CN28): one whole-node `dmsetup info` listing (Dm.ListInfo), one
// `dmsetup table` listing (Dm.ListTables), one NVMe walk and one md walk
// (Md.Scan), taken under the call's locks and shared with nothing (CN23). A
// name a complete info listing lacks is not there. A name a partial info
// listing lacks, or a name the table listing lacks, is read on its own at its
// first lookup, and the answer is kept for the call, so no device is read
// twice.
type cnReading struct {
	dm *agent.Dm
	// infoList is nil when infoErr is set: the info listing did not answer.
	infoList *agent.DmInfoListing
	infoErr  error
	// tableList is nil when tableErr is set: the table listing did not
	// answer.
	tableList *agent.DmTableListing
	tableErr  error
	// walk is the call's NVMe walk, every lookup's (lookupSubsys) and the
	// verdict's list of connections (hostSubsys).
	walk *passWalk
	// md is the call's md walk, nil when mdErr is set: the listing of
	// /sys/block did not answer.
	md    *MdScan
	mdErr error
	// devAnswers and tableAnswers are the reading's own reads of names a
	// listing lacked, by name, made at the first lookup and kept for the
	// call, errors included.
	devAnswers   map[string]dmDevAnswer
	tableAnswers map[string]dmTableAnswer
}

// takeReading takes the reading, in this order: the info listing before the
// table listing, so a name the info listing holds and the table listing
// lacks went away between the two, and one only the table listing holds
// came after the info listing; then the NVMe walk and the md walk. Each part
// that did not answer keeps its error, which every row that reads that part
// takes, and the verdict names.
func (s *CnAgentServer) takeReading(ctx context.Context) *cnReading {
	rd := &cnReading{dm: s.dm}
	rd.infoList, rd.infoErr = s.dm.ListInfo(ctx)
	rd.tableList, rd.tableErr = s.dm.ListTables(ctx)
	rd.walk = s.newPassWalk(ctx)
	rd.md, rd.mdErr = s.md.Scan(ctx)
	return rd
}

// mdWalks is the reading's md walk as the group rows and the concat rows
// take it (cntlrPlan.mdWalks): the scan's walk view, or the listing's error.
// It reads nothing of its own.
func (rd *cnReading) mdWalks() *mdWalkOnce {
	walks := &mdWalkOnce{done: true, err: rd.mdErr}
	if rd.mdErr == nil {
		walks.walk = rd.md.Walk()
	}
	return walks
}

// readCntlr is what a check round and an info read take of the node (CN23,
// CN24), under the node read lock and the cntlr's lock their callers hold:
// one plan and one reading, then the rows (probeCntlr), then the verdict
// (sweepCntlr with nothing removed), both from that reading, so the verdict
// judges the node the rows read and a call answers what a round in the same
// state would.
//
// A stored conf the gate refuses (architecture.md, Common validation) takes
// no reading: its rows are empty, what a cntlr with nothing provable looks
// like, and its verdict is clean. Every table the rows compare is built from
// that conf — the pool's low_water_mark and block size and the raid0's
// stripe are its own arguments — so a probe with a zero would report a
// correctly built pool as needing a reload. And the converge refuses it
// before anything runs, so leftovers reported for a cntlr this agent
// deliberately did not build would name objects it may not remove.
func (s *CnAgentServer) readCntlr(
	ctx context.Context,
	st *cntlrState,
) (*pb.CntlrInfo, *agent.SweepResult) {
	req := st.loadReq()
	if err := agent.ValidateBdevConf(req.GetBdevConf()); err != nil {
		ptr := req.GetCntlrPointer()
		slog.ErrorContext(ctx, msgInvalidStoredConf,
			slog.Uint64("cluster_id", req.GetClusterId()),
			slog.Uint64("cn_id", req.GetCnId()),
			slog.Uint64("sp_id", ptr.GetSpId()),
			slog.Uint64("cntlr_id", ptr.GetCntlrId()),
			slog.String("error", err.Error()))
		return newCntlrInfo(), &agent.SweepResult{}
	}
	plan := newCntlrPlan(s.nf, req)
	rd := s.takeReading(ctx)
	plan.rd, plan.walk, plan.mdWalks = rd, rd.walk, rd.mdWalks()
	info := s.probeCntlr(ctx, st, plan)
	return info, s.sweepCntlr(ctx, st, plan, false)
}

// dmDevAnswer is one own `dmsetup info` of a name: the device, nil when it is
// not there, or the read's error.
type dmDevAnswer struct {
	dev *agent.DmDevInfo
	err error
}

// dmTableAnswer is one own `dmsetup table` of a name: its targets or the
// read's error.
type dmTableAnswer struct {
	targets []common.DmTarget
	err     error
}

// info is a device's attrs and number from the reading. An info listing that
// did not answer is the error of every lookup. A name the listing holds is
// its line, as listed: its DevNo is "" when the line's numbers did not parse,
// which dmDevNo turns into an error. A name a complete listing lacks is not
// there, with no read. A name a partial listing lacks is read on its own once
// (Dm.InfoDevNo), and that answer serves every later lookup of the pass.
func (rd *cnReading) info(
	ctx context.Context,
	name string,
) (*agent.DmDevInfo, error) {
	if rd.infoErr != nil {
		return nil, rd.infoErr
	}
	if dev, ok := rd.infoList.Devs[name]; ok {
		return dev, nil
	}
	if !rd.infoList.Partial {
		return nil, nil
	}
	if answer, ok := rd.devAnswers[name]; ok {
		return answer.dev, answer.err
	}
	dev, err := rd.dm.InfoDevNo(ctx, name)
	if rd.devAnswers == nil {
		rd.devAnswers = make(map[string]dmDevAnswer)
	}
	rd.devAnswers[name] = dmDevAnswer{dev: dev, err: err}
	return dev, err
}

// table is a device's live table from the reading. A table listing that did
// not answer is the error of every lookup. A name the listing holds is its
// record. A name it lacks, partial or not, is read on its own once
// (Dm.Table), and that answer serves every later lookup of the pass: a
// caller looks a table up only for a device its info lookup found, so a name
// the table listing lacks went away between the two listings, or the table
// listing is partial. A caller that needs an absence confirmed after a table
// read failed asks a fresh `dmsetup info`, never the reading.
func (rd *cnReading) table(
	ctx context.Context,
	name string,
) ([]common.DmTarget, error) {
	if rd.tableErr != nil {
		return nil, rd.tableErr
	}
	if targets, ok := rd.tableList.Tables[name]; ok {
		return targets, nil
	}
	if answer, ok := rd.tableAnswers[name]; ok {
		return answer.targets, answer.err
	}
	targets, err := rd.dm.Table(ctx, name)
	if rd.tableAnswers == nil {
		rd.tableAnswers = make(map[string]dmTableAnswer)
	}
	rd.tableAnswers[name] = dmTableAnswer{targets: targets, err: err}
	return targets, err
}

// dmInfo is a dm device's attrs: from the reading, or with no reading one
// `dmsetup info` of the device (Dm.Info). nil, nil is a device that is not
// there.
func (s *CnAgentServer) dmInfo(
	ctx context.Context,
	rd *cnReading,
	name string,
) (*agent.DmDevInfo, error) {
	if rd == nil {
		return s.dm.Info(ctx, name)
	}
	return rd.info(ctx, name)
}

// dmTable is a dm device's live table: from the reading, or with no reading
// one `dmsetup table` of the device (Dm.Table).
func (s *CnAgentServer) dmTable(
	ctx context.Context,
	rd *cnReading,
	name string,
) ([]common.DmTarget, error) {
	if rd == nil {
		return s.dm.Table(ctx, name)
	}
	return rd.table(ctx, name)
}

// dmDevNo is a dm device's number, "major:minor", the form a dm table names
// a device in: from the reading, or with no reading one `dmsetup info` of the
// device that prints its number (Dm.InfoDevNo). Both channels make the same
// three checks, so a check round and a converge write the same text for the
// same device: the read's error, a device that is not there
// (errDmNotThere), and a device listed with numbers that do not parse.
func (s *CnAgentServer) dmDevNo(
	ctx context.Context,
	rd *cnReading,
	name string,
) (string, error) {
	var dev *agent.DmDevInfo
	var err error
	if rd == nil {
		dev, err = s.dm.InfoDevNo(ctx, name)
	} else {
		dev, err = rd.info(ctx, name)
	}
	if err != nil {
		return "", err
	}
	if dev == nil {
		return "", errDmNotThere(name)
	}
	if dev.DevNo == "" {
		return "", fmt.Errorf(
			"dm device %s has no device number in dmsetup info", name)
	}
	return dev.DevNo, nil
}

// errDmNotThere is the error of a number read of a dm device that is not
// there, one text for both channels.
func errDmNotThere(name string) error {
	return fmt.Errorf("dm device %s is not there", name)
}

// blockDevNoPattern is the "major:minor" /sys/block/<kname>/dev holds.
var blockDevNoPattern = regexp.MustCompile(`^[0-9]+:[0-9]+$`)

// blockDevNo is the number of the block device the kernel names kname — an
// md array's node, an NVMe multipath head, a loop device — read from
// /sys/block/<kname>/dev under the soft timeout (SH15). A read that failed
// is its error and never an absence; a device whose file is not there, or a
// value that is not a device number, is an error too.
func (s *CnAgentServer) blockDevNo(
	ctx context.Context,
	kname string,
) (string, error) {
	path := sysfsBlockDir + "/" + kname + "/dev"
	value, ok, err := s.cmd.ReadAttr(ctx, path)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("block device %s is not there", kname)
	}
	if !blockDevNoPattern.MatchString(value) {
		return "", fmt.Errorf("%s reads %q, not a device number", path, value)
	}
	return value, nil
}
