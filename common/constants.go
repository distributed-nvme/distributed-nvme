package common

import "time"

const (
	ValidStrPattern = `^[a-zA-Z0-9\-_/.:]+$`
	MaxStrSize      = 64
	// ValidNqnPattern is "nqn.", a date, a lower-case domain, a ':' and a
	// suffix of letters, digits, '.', '_', ':' and '-': no '/' and no
	// whitespace, since an NQN names a configfs directory. The gateway
	// refuses ".." on top of it. The ':' after the domain part is required,
	// so the well-known discovery NQN "nqn.2014-08.org.nvmexpress.discovery"
	// can never validate (architecture.md, Common validation).
	ValidNqnPattern = `^nqn\.\d{4}-(0[1-9]|1[0-2])\.[a-z0-9][a-z0-9.-]*:[A-Za-z0-9._:-]+$`
	MaxNqnLength    = 223

	ShardBucketSize = 256
	ShardCodeFmt    = "%02x"

	MaxDnExtSize       = 1024 * 1024 * 1024 * 1024
	MinDnExtSize       = 64 * 1024 * 1024
	DefaultDnExtSize   = 1 * 1024 * 1024 * 1024
	DefaultDnBin0Shift = 0
	DefaultDnBin1Shift = 4
	DefaultDnBin2Shift = 8
	DefaultDnBin3Shift = 12

	MaxHealthCheckInterval     = 3600
	MinHealthCheckInterval     = 1
	DefaultHealthCheckInterval = 5

	MinAllocDnBatchSize     = 1
	MaxAllocDnBatchSize     = 1024
	DefaultAllocDnBatchSize = 16
	MinAllocCnBatchSize     = 1
	MaxAllocCnBatchSize     = 1024
	DefaultAllocCnBatchSize = 16

	// CreateClone checks a source's stripe and block size against these same
	// bounds (architecture.md, raid0 bitmap math), so every legal pool is a
	// legal clone source.
	MaxDmPoolDataBlockSize     = 1 * 1024 * 1024 * 1024
	MinDmPoolDataBlockSize     = 64 * 1024
	DefaultDmPoolDataBlockSize = 1 * 1024 * 1024
	MaxDmRaid0StripeSize       = 1 * 1024 * 1024
	MinDmRaid0StripeSize       = 4 * 1024
	DefaultDmRaid0StripeSize   = 64 * 1024

	// MinChunkBlockCnt, MaxChunkBlockCnt and DefaultChunkBlockCnt are the
	// bounds and the default of redund_md_raid1.bitmap_chunk_block_cnt, the
	// md bitmap chunk counted in pool data blocks (architecture.md, Common
	// validation). A dm-clone region has no parameter of its own: it is the
	// data_block_size of the SP a clone copies into or whose side a migration
	// moves.
	MinChunkBlockCnt     = 1
	MaxChunkBlockCnt     = 1024
	DefaultChunkBlockCnt = 128

	MaxDnCntPerCluster   = 1024
	MaxCnCntPerCluster   = 1024
	MaxSpCntPerCluster   = 4096
	MaxTdCntPerSp        = 1024
	MaxSsCntPerSp        = 4
	MaxNsCntPerSs        = 4
	MaxHostCntPerSs      = 8
	MaxSliceCntPerSp     = 32
	MaxCntlrCntPerSp     = 4
	MinCntlrCntPerSp     = 1
	DefaultCntlrCntPerSp = 2
	// DefaultSliceCntPerSp is the slice count CreateStoragePool substitutes
	// for a slice_cnt of 0, exactly as DefaultCntlrCntPerSp above is
	// substituted for a cntlr_cnt of 0 (the Defaults of architecture.md,
	// Storage pools). The help of `dnvctl sp create --slice-cnt` — and of
	// gatewayctl's own --slice-cnt — prints the number an operator gets for
	// that zero, and both build it from this constant with a %d rather than
	// typing a 2, so moving the default moves what the two CLIs say.
	DefaultSliceCntPerSp = 2
	MaxCloneCntPerSp     = 64
	MaxXferCntPerSp      = 4
	MaxMigrCntPerSp      = 4
	MaxSideCntPerDn      = 1024
	MaxCntlrCntPerCn     = 256
	MaxLegPerGrp         = 8
	MaxSpareLegPerGrp    = 2
	// MaxGrpCntPerSlice is the most groups EACH of a slice's two group lists
	// (meta_grp_list, data_grp_list) holds. A group's two md names carry its
	// index in its list as two hex digits (%02x, architecture.md, md names), and
	// that fixed width is what keeps "md_" + CnMdDevName within the kernel's
	// DISK_NAME_LEN; index 256 would widen to three digits. GrowSlice, the
	// one op that appends a group to an existing slice, refuses at the
	// ceiling (model.GrpListFull), and common/name_fmt_test.go
	// TestMdNamesAtTheGroupCeiling is the tripwire that fails when the
	// ceiling is raised past what the names can carry.
	MaxGrpCntPerSlice = 255
	// MaxAllocLegPerGrp is the allocator's ACTUAL maximum legs per group, as
	// opposed to the aspirational and unenforced MaxLegPerGrp above: every
	// allocating path picks between 1 leg (RedundNone) and 2 (RedundMdRaid1),
	// and no allocating path builds a wider group.
	//
	// SPD1: it is CITED from all three places that make that choice —
	// gateway/alloc.go legCntOf, model/ops.go legCntOf and worker/reaction.go
	// legCnt — and from the sp-drain batch budget tripwire
	// (gateway/txnbudget_test.go TestSpDrainBatchBudget), so that widening the
	// allocator's group shape without revisiting MaxDelGrpPerTxn fails a test
	// instead of a deployment.
	MaxAllocLegPerGrp = 2
	// MaxDelGrpPerTxn is the most groups ONE sp-drain batch removes from one
	// slice in a single transaction (dnv-worker.md SPD10). It bounds
	// transaction SIZE, not rate — strictly sequential batches are the
	// pacing — and it is a package constant rather than configuration for
	// exactly that reason. The SPD13 arithmetic it must satisfy is
	//
	//	6 + 6 x MaxDelGrpPerTxn x (MaxAllocLegPerGrp + MaxSpareLegPerGrp)
	//	    <= EtcdMaxTxnOps
	//
	// which gateway/txnbudget_test.go asserts from the named constants (SPD14).
	MaxDelGrpPerTxn = 20
	// MaxDelBmPerTxn is the most clone-bitmap chunk keys ONE clone-drain batch
	// deletes in a single transaction (dnv-worker.md CLD8). Like
	// MaxDelGrpPerTxn it bounds transaction SIZE, not rate, and is a package
	// constant rather than configuration for that reason.
	//
	// Chunk removal is ledger-free — no DN or CN accounting, pure point
	// deletes — so the arithmetic is one line. etcd caps a transaction at
	// max(len(Compare), len(Success), len(Failure)), and etcdutil's
	// serializable-snapshot STM compares every key it READ and every key it
	// WROTE, so the COMPARE count binds: 3 reads (SpConf, Clone, SpRev) plus
	// MaxDelBmPerTxn + 1 writes = MaxDelBmPerTxn + 4 = 68, against 65 success
	// ops. It is independent of every ceiling constant. Growing MaxCloneBmCnt or
	// MaxSliceCntPerSp therefore grows the batch COUNT and never the
	// transaction's legality; at the ceilings a maximum-shape drain is
	// ceil(MaxSliceCntPerSp x MaxCloneBmCnt / MaxDelBmPerTxn) batches. 68 also
	// fits etcd's DEFAULT --max-txn-ops of 128 — prose, not a tripwire: the
	// deployment requirement stays EtcdMaxTxnOps for the transactions that do
	// NOT fit 128, such as
	// CreateStoragePool's 967-compare maximum shape, the sp drain's
	// 486-compare D2 batch and the created flip's 514-compare transaction
	// (see EtcdMaxTxnOps and MaxFlipCreatedPerTxn below).
	MaxDelBmPerTxn = 64

	CnCntlidSlotBase = 10000
	CnCntlidSlotStep = 5000
	CnCntlidSlotCnt  = 8

	DnCntlidSlotBase = 10000
	DnCntlidSlotStep = 5000
	DnCntlidSlotCnt  = 8

	DefaultClusterName = "default"
	DefaultCnCap       = 4 * 1024 * 1024 * 1024 * 1024
	MaxCnCap           = 64 * 1024 * 1024 * 1024 * 1024
	MinCnCap           = 1024
	DefaultListCnt     = 64
	MaxListCnt         = 1024

	DnvPrefix = "dnv"
	DmPrefix  = "dnv"
	NqnPrefix = "nqn.2024-01.io.dnv"

	DefaultTmpfsPrefix = "/tmp/dnv-tmpfs"

	// The CN clone-metadata arena ([D14], cnagent.md CN5/CN18): one sparse
	// file (CnTmpFilePath) on the CN tmpfs, attached to a single loop
	// device, carved into fixed units by the CN slot allocator whose
	// registry is the kind-`cb` wrapper dm tables themselves. No LVM
	// ([D14]).
	// CnCloneMetaAreaSize is the `truncate` size of that file — and so the
	// arena the slot allocator carves, 256 units. CnCloneMetaUnit is the
	// allocation granularity, the cn twin of DnCloneMetaUnit; it is
	// deliberately NOT called an "extent", which everywhere else in dnv
	// means the 1 GiB DN/CN allocation unit (DefaultDnExtSize).
	CnCloneMetaAreaSize = 1 * 1024 * 1024 * 1024
	CnCloneMetaUnit     = 4 * 1024 * 1024

	// dnv DN disk format ([D13]). All byte offsets on the raw --disk device.
	DnHeaderOffset     = 0 // 4 KiB header block
	DnHeaderSize       = 4096
	DnTableSlotAOffset = 4 * 1024 * 1024  // volume-table slot A
	DnTableSlotBOffset = 20 * 1024 * 1024 // volume-table slot B
	DnTableSlotSize    = 16 * 1024 * 1024
	DnCloneMetaOffset  = 64 * 1024 * 1024  // dm-clone metadata slot area
	DnCloneMetaSize    = 192 * 1024 * 1024 // 48 units
	DnCloneMetaUnit    = 4 * 1024 * 1024   // slot allocation granularity
	DnDataOffset       = 256 * 1024 * 1024 // extent area start (fixed!)

	// DefaultLocalStorPrefix is the directory an agent keeps its local store
	// in when --local-store is not given (architecture.md, Agent local-store paths).
	// It must exist before the agent starts (dnagent.md SH3). Not /var/tmp, where the
	// stock tmpfiles rule of some distributions deletes files that nothing
	// has touched for 30 days: a store file is written only when a request for
	// its object is applied and read only at startup.
	DefaultLocalStorPrefix = "/var/lib/dnv"

	IdKeyFmt     = "%016x"
	FreeSpaceFmt = "%016x"
	BinIdxFmt    = "%01x"
	BmIdxFmt     = "%02x"

	CmdSoftTimeout = 3
	CmdHardTimeout = 5

	// The clone pair has no defaults: a clone's DmCloneConf is stored as sent
	// and forwarded to the cn agent untouched, so a zero there leaves the
	// dm-clone target's own default in place (model/ops.go,
	// ResolveEventThreshold's note). Only the migration pair is resolved, by
	// worker/sprole.go's migrCloneConf. validateDmCloneConf
	// (gateway/validate.go) holds both pairs to these two maxima.
	MaxCloneThreshold = 8
	MaxCloneBatchSize = 4
	// MaxCloneBmCnt is the number of chunks ONE source slice's bitmap may be
	// split into: a clone bitmap chunk is addressed (src_slice_idx, bm_idx)
	// and bm_idx < MaxCloneBmCnt (architecture.md, Bitmap push protocol).
	// It is NOT a cap on the source slice count — that is MaxSliceCntPerSp,
	// enforced by CreateClone. 16 chunks × CloneBmChunkBytes = 16 MiB per slice.
	MaxCloneBmCnt = 16
	// CloneBmChunkBytes is the fixed capacity of one clone bitmap chunk and
	// the quantum that positions it: chunk (s, b) holds bytes
	// [b*CloneBmChunkBytes, b*CloneBmChunkBytes+len) of source slice s's
	// bitmap. 1 MiB keeps a grown chunk value plus the rev-bump put inside
	// etcd's default ~1.5 MiB request cap, and every
	// PushCloneBitmap message inside gRPC's default 4 MiB. It is a clone
	// positioning quantum only — migration appends have no byte cap.
	CloneBmChunkBytes = 1 << 20

	DefaultMigrThreshold = 1
	DefaultMigrBatchSize = 1
	MaxMigrBmCnt         = 4

	DefaultPrimaryUnhealthy    = 5
	DefaultCntlrUnhealthy      = 600
	DefaultSideUnhealthy       = 600
	DefaultLegUnhealthy        = 1200
	DefaultPoolLowWatermarkPct = 50

	DefaultNvmeFastIoFailTmo = 5

	// Default cap on the number of in-flight OsClient operations
	// (commands + file I/O + proto I/O combined). See osclient.md.
	DefaultOsClientLimit = 32

	// Maximum number of characters of string file data included in a log
	// record (see log.md R11).
	LogStrDataLimit = 128

	// The DEFAULT configfs id of the nvmet port an agent converges
	// (architecture.md, Disk node; Controller node, common: exactly one port
	// per agent).
	// `dnv-agent --nvmet-port-id` overrides it, which is what lets several
	// agents share one node's kernel, each converging its own port.
	NvmetPortId = 1

	// The three fixed ANA groups on every nvmet port (architecture.md
	// [D4]). Group 1 always exists in nvmet and defaults to optimized;
	// groups 2 and 3 are created at port setup. Each group's state is fixed
	// — EnsurePort writes it probe-first and rewrites a drifted one — and
	// every ANA transition rewrites a namespace's ana_grpid instead.
	AnaGrpIdOptimized    = 1
	AnaGrpIdNonOptimized = 2
	AnaGrpIdInaccessible = 3

	// AgentReply.code values (dnagent.md SH8, SH9). 0 = OK. Codes 1-3 are
	// rejections: the request was not applied, so the worker reads no
	// verdict from the reply's rows. ReplyCodeLeftover (4) is accepted: the
	// worker reads the rows exactly as for 0 (worker/health.go accepted).
	// Any code != 0 in a Check* reply makes the worker issue a Syncup*
	// (dnv-worker.md RW4 step 5), and in a Push*Bitmap reply ends that push
	// plan. Among the rejections the specific value only sets the level of
	// the worker's `syncup rejected` record (a stale revision logs at Error)
	// and otherwise serves details and tests.
	ReplyCodeStaleRevision = 1
	ReplyCodeUnknownObject = 2
	// ReplyCodeInvalidConf refuses a request whose conf carries a value the
	// control plane cannot have written — a proto3 zero where architecture.md,
	// Common validation, requires a concrete geometry. The object is known and
	// the revision is current; it is the conf that is unusable, which is why it
	// is neither of the two above.
	ReplyCodeInvalidConf = 3
	// ReplyCodeLeftover reports an ACCEPTED request with residue: the
	// desired state is stored and every wanted object was converged, but the
	// node still holds objects the desired state does not want, or an
	// enumeration of what exists did not answer. It is the one thing that
	// travels in agent_reply rather than in the *Info rows, because a
	// leftover by definition has no row — nothing wanted names it. An agent
	// also reports a few conditions this way so that the worker re-sends the
	// Syncup* whose converge acts on them (architecture.md, Teardown by sweep):
	// on a dn a disk identity not yet confirmed or a side with extents to zero and
	// nothing zeroing it, on a cn a piece of the node's base state that a
	// CheckCn round's or GetCnInfo's probe read absent, or an ANA group it
	// read in a state other than its fixed one on a port whose transport
	// attributes match.
	//
	// It is not a rejection: the worker evaluates the reply's rows exactly as
	// for code 0 and re-issues the Syncup* every round (RW12, no backoff)
	// until the code changes. "Pending" is never stored anywhere; the code is
	// recomputed by enumerating the node on every Syncup* and every Check*.
	ReplyCodeLeftover = 4

	// Seconds between background retries of a migration destination whose
	// build stopped short, at its nvme connect or at another step
	// (dnagent.md DN13; the loop is SH27's "DN8 retry", so nicknamed for the
	// DN8-gated converge it re-runs).
	DnMigrConnectRetryInterval = 5

	// The migration destination's wait for the source namespace (dnagent.md
	// DN13 step (3)). The kernel returns from `nvme connect` once the
	// controller is live and only QUEUES the namespace scan that adds the
	// namespace node, so right after a connect that succeeded the pass
	// re-reads the subsystem until the device is there, pausing
	// DnMigrDstNsPause between reads and DnMigrDstNsWait in all (the reads
	// themselves are not counted). It is not a connect retry: one connect per
	// pass stays DN13's rule, and the DN8 loop above stays the retry. Unlike
	// the whole-second integers above them, these two are time.Durations.
	DnMigrDstNsWait  = 1 * time.Second
	DnMigrDstNsPause = 50 * time.Millisecond

	// DnExportOrphanGrace is the age an unattributable :2: export must exceed
	// before a sweep may remove it (dnagent.md DN6): one with no namespace,
	// linked to no nvmet port or only to this agent's own. That shape is
	// ALSO every export's shape between its subsystem `mkdir` and its
	// namespace `mkdir`, and on a kernel shared by several dn agents the
	// build in that window may be a sibling's, whose request this agent
	// cannot see; only age tells an abandoned one from one in flight. The
	// age is read from the node — the subsystem directory's mtime, `stat -c
	// %Y`, in whole seconds — and never remembered. A time.Duration, like the
	// two above.
	DnExportOrphanGrace = 30 * time.Second

	// Side provisioning ([D15], architecture.md, Side provisioning protocol,
	// dnagent.md DN9): the background zeroing goroutine zeroes at most
	// DnZeroBatchExtCnt logical extents per `blkdiscard --zeroout`
	// command, through the side's dm-linear, and persists that batch's
	// `zeroed_bits` after each success. The batch size assumes fast
	// hardware Write Zeroes: batch × ext_size should zero inside
	// CmdSoftTimeout at the disk's rate split DnZeroConcurrency ways. A
	// failed or timed-out batch is retried no sooner than
	// DnZeroRetryInterval seconds later — the zeroing twin of
	// DnMigrConnectRetryInterval, never a hot loop.
	DnZeroBatchExtCnt   = 10
	DnZeroRetryInterval = 5
	// DnZeroConcurrency caps the zeroing batches one agent runs at once,
	// however many of its sides are zeroing: N concurrent batches split
	// the disk's Write Zeroes rate N ways, so with no cap a busy DN would
	// have every batch killed at CmdSoftTimeout and redone for ever. A batch
	// the soft timeout killed halves the side's next batch, a success
	// doubles it again up to DnZeroBatchExtCnt, and DnZeroKillBackoff
	// kills in a row drop the side to one extent per batch. That rate
	// control lives in the side's goroutine only — a restart begins again
	// at DnZeroBatchExtCnt — and never decides what is zeroed: the bits
	// do.
	DnZeroConcurrency = 2
	DnZeroKillBackoff = 2

	// CN base state (architecture.md, Controller node, common): the tmpfs that
	// carries the clone-metadata arena file, sized 2 × CnCloneMetaAreaSize so
	// that even a fully materialized arena plus slack never hits ENOSPC on the
	// mount.
	// The file itself is sparse: pages appear as dm-clone writes metadata
	// and are released again by the allocator's hole-punch discard
	// (CN18).
	DefaultCnTmpfsSize = 2 * 1024 * 1024 * 1024

	// Seconds between two leg health-probe rounds (architecture.md, Group
	// on-leg layout: meta region, data region, health block) on a primary
	// cntlr, and how long one probe IO may stay in flight before the leg
	// is reported stalled (cnagent.md CN11). Probes are single-flight per
	// leg and run outside every lock.
	CnLegProbeInterval     = 5
	CnLegProbeStallSeconds = 15

	// The leg health block (architecture.md, Group on-leg layout: meta region,
	// data region, health block): the last 4 KiB of the leg's meta region.
	// Payload = magic + writer id + timestamp, never interpreted on read
	// ([D6]).
	LegHealthBlockSize = 4096
	LegHealthMagic     = "DNVHLTH1"

	// Seconds between background retries of a cn cntlr's converge after any
	// of CN10's triggers (cnagent.md CN10); the cn twin of
	// DnMigrConnectRetryInterval.
	CnConnectRetryInterval = 5

	// The connect step's one wait budget per converge pass (cnagent.md CN10,
	// CN18). A pass — one convergeCntlr, whether a SyncupCntlr, the startup
	// reconcile or an attempt of the background retry above runs it — gets
	// one budget of CnConnectPassBudget, shared by every leg and every clone
	// source it connects, and exactly three things draw on it: a failed
	// `nvme connect`'s own elapsed time; the CnConnectRetryPause before each
	// in-pass retry of a failed connect; and the CnNsScanPause steps of the
	// wait for the multipath namespace head after a connect this pass made.
	// A pause starts only while it fits in what is left; a failed connect is
	// charged after it ran, so a slow one can overdraw the budget, and none
	// of this bounds a side's first connect, which every pass makes. Once it
	// is spent the pass behaves as it did without it: the leg or the clone
	// source fails with the same error and registers the background retry.
	// Unlike the whole-second integers above them, these three are
	// time.Durations.
	CnConnectPassBudget = 1 * time.Second
	CnConnectRetryPause = 100 * time.Millisecond
	CnNsScanPause       = 50 * time.Millisecond

	// ResDetailsSpLevel is the details of a CntlrInfo row the sp_level
	// suppresses (cnagent.md CN19): RES_STATUS_MISSING that says the
	// resource must not exist, where every other MISSING the cn agent reports
	// says it does not exist yet. The cn agent writes it and the worker's
	// settle reads it (dnv-worker.md HL2), so the two take it from here.
	ResDetailsSpLevel = "sp_level"

	// SuspendSeconds is the src-cutover grace window (architecture.md,
	// Migration, src step 2): a migration
	// source's per-CN dm-linears are held suspended for at least this long
	// before they are reloaded onto their dm-errors, so IO the old primary
	// still had in flight is absorbed rather than immediately failed. The
	// deferred bios are released against the dm-error table the reload
	// installs, so they error at the end of the window instead of replaying
	// onto the side's data ([D12]). It is a floor, not a deadline: the
	// reload happens on the first converge at or after it, and sooner where
	// an export above the linear is to go, since disabling a namespace waits
	// for its in-flight IO, which a suspended device does not complete
	// (dnagent.md DN12).
	SuspendSeconds = 60

	// dnv-worker (dnv-worker.md, Constants this document owns), plus one
	// constant this block holds for another document: EtcdMaxTxnOps is the
	// addition of gateway.md, Constants this document owns, and the
	// arithmetic tripwired against it is that section's for CreateStoragePool
	// and DeleteThinDevice, dnv-worker.md's for the created flip (RW19) and for
	// the two drains — SPD13/SPD14 for the sp drain, CLD11 for the clone drain.
	//
	// Seconds between two refreshes of a worker's registry key (VW2); a
	// registration not refreshed for 2 × this is dead (VW3).
	DefaultVoteWorkerInterval = 10
	// Seconds an observed membership transition must hold before it is
	// committed into the effective membership (VW5).
	DefaultVoteWorkerGraceTime = 60
	// Per-call deadlines of the worker's agent RPCs (RW5, BM3).
	DefaultWorkerSyncupTimeout = 60
	DefaultWorkerPushTimeout   = 60
	// etcd client: dial, and per plain operation / per whole
	// transaction, every retry included (EU1, EU5).
	DefaultEtcdDialTimeout = 5
	DefaultEtcdOpTimeout   = 10
	// EtcdMaxTxnOps is a DEPLOYMENT REQUIREMENT, not a client setting: every
	// etcd serving dnv MUST run with --max-txn-ops=1024 or higher; etcd's
	// default is 128. etcd caps on max(len(Compare), len(Success), …), and
	// etcdutil's serializable-snapshot STM compares every key it read AND
	// every key it wrote, so that sum is what has to fit.
	//
	// The transaction this number is SIZED by is CreateStoragePool at its
	// widest shape, 967 COMPARES. Its STM costs 7 fixed (ClusterConf, SpConf
	// and SpGlobal read; SpConf, SpName, SpRev and SpGlobal put) + one put
	// per slice + 7 per distinct DN (DnConf, the scan's dn_capacity key and
	// DnRev read; DnConf, the dn_capacity del and put, and DnRev written)
	// + 8 per CN (the DN seven CN-keyed, plus that cntlr's own put), and
	// every factor of the widest shape is a ceiling constant:
	//
	//	7 + MaxSliceCntPerSp
	//	  + 7 x (2 x MaxSliceCntPerSp x MaxAllocLegPerGrp)
	//	  + 8 x MaxCntlrCntPerSp                        <= EtcdMaxTxnOps
	//
	// 32 slices x 2 groups per slice (planSpGroups) x MaxAllocLegPerGrp = 2
	// legs is 128 DNs, all distinct (the growing black list of architecture.md,
	// Per-operation allocation), and
	// MaxCntlrCntPerSp = 4 cntlrs on 4 distinct CNs, so it is
	// 7 + 32 + 7x128 + 8x4 = 967, which 1024 clears by 57. init_ext_cnt never
	// enters that count — it moves ExtCnt VALUES, not key counts — so the
	// shape is bounded by those three ceilings alone, and
	// gateway/txnbudget_test.go's TestCreateStoragePoolBudget asserts the
	// arithmetic from them. The headroom is thin in the DN dimension: one
	// more key read or written per DN inside that STM costs 128 compares.
	//
	// Other bounded transactions exceed etcd's default 128 too, without
	// sizing this number. The sp drain's D2 batch is one:
	// 6 + 6·MaxDelGrpPerTxn·(MaxAllocLegPerGrp + MaxSpareLegPerGrp) = 486
	// COMPARES at the maximum shape — SPD13's arithmetic, asserted at the
	// named constants by gateway/txnbudget_test.go's SPD14 tripwire and
	// committed against a real etcd by model/drain_test.go's
	// TestDrainSpSliceAtTheCeiling (dnv-worker.md SPD14). The created flip's
	// transaction is another, 2 x MaxFlipCreatedPerTxn + 2 = 514 COMPARES
	// (dnv-worker.md RW19; TestFlipCreatedTxnBudget,
	// TestFlipCreatedAtTheTdCeiling; see MaxFlipCreatedPerTxn below).
	//
	// DeleteThinDevice stays below all three — the create, D2 and the flip —
	// whatever the td count: its deciding STM commits having read the fixed
	// resolution and token keys, the target td and one key per subsystem and
	// per clone of the SP, so with its three writes it is
	// 7 + MaxSsCntPerSp + MaxCloneCntPerSp = 75 COMPARES at the ceilings
	// (gateway/txnbudget_test.go's TestDeleteThinDeviceBudget). The
	// walk over every td for uncreated snapshots is a read-only plan outside
	// the transaction, verified inside it by the pool's identity and revision
	// (architecture.md, Thin devices), and TestDeleteThinDeviceAtTheTdCeiling commits a
	// delete in a pool of MaxTdCntPerSp tds. A walk inside the STM would cost
	// one compare per td and put a delete from a full pool past the budget.
	//
	// The clone drain's batches, MaxDelBmPerTxn + 4 ops each, fit the default
	// (dnv-worker.md CLD11).
	//
	// The Go test etcd launchers pass it from here; the shell suites that
	// start an etcd cannot import common, so they read it at preflight from
	// workerctl's constants subcommand.
	EtcdMaxTxnOps = 1024
	// MaxFlipCreatedPerTxn is the most candidates ONE created-flip
	// transaction carries (dnv-worker.md RW19). model.FlipCreated
	// commits its list this many at a time, in list order, one STM each, and
	// each STM that wrote bumps SpRev once. The list is bounded only by
	// MaxTdCntPerSp: the sp worker folds every td one drain of its reports
	// completed into one call, and an SP whose tds were all created before
	// its pool came up can complete them in one reply. One STM over the whole
	// list would exceed EtcdMaxTxnOps at MaxTdCntPerSp candidates and be
	// refused on every round, so nothing would flip. Like MaxDelBmPerTxn it
	// bounds transaction SIZE, not rate, and is a package constant rather than
	// configuration for that reason.
	//
	// The arithmetic is one line. The STM compares every key it read and
	// every key it wrote: a Get of the td key per candidate, a Put per
	// candidate it flips, and the SpRev read and put. At worst every
	// candidate flips, so
	//
	//	2 x MaxFlipCreatedPerTxn + 2 = 514 <= EtcdMaxTxnOps
	//
	// which gateway/txnbudget_test.go's TestFlipCreatedTxnBudget asserts from
	// the named constants and model/ops_test.go's
	// TestFlipCreatedAtTheTdCeiling commits against a real etcd. 514 is over
	// etcd's default 128, so the created flip, like CreateStoragePool and the
	// sp drain's D2 batch, needs the raised flag. 256 keeps a whole
	// MaxTdCntPerSp of candidates to four transactions and at most four SpRev
	// bumps.
	MaxFlipCreatedPerTxn = 256

	WorkerRoleDn = "dn"
	WorkerRoleCn = "cn"
	WorkerRoleSp = "sp"

	// dnv-gateway (gateway.md, Constants this document owns). That
	// section adds three constants and
	// only this one lands here: CloneBmChunkBytes sits beside MaxCloneBmCnt
	// above, and EtcdMaxTxnOps in the dnv-worker block, whose header says so.
	//
	// DefaultGatewayAgentTimeout is the per-call budget of the gateway's
	// agent RPCs (GetDnSize/GetCnSize, the Get*Info behind Inspect* and
	// behind the force = false checks of DeleteClone/FinishMigration, the
	// Get*Bm bitmap reads), in seconds. Applied with context.WithTimeout
	// around each dial+call (AG2); chosen equal to DefaultEtcdOpTimeout so a
	// hung agent and a hung etcd bound an RPC alike.
	DefaultGatewayAgentTimeout = 10

	// dnv-cdc (cdc.md, Constants this document owns).
	//
	// NvmeDiscoveryNqn is the well-known discovery subsystem NQN every
	// host connects to (NP5). It deliberately fails ValidNqnPattern above:
	// no dnv object may ever be named it.
	NvmeDiscoveryNqn = "nqn.2014-08.org.nvmexpress.discovery"
	// The listen endpoint defaults of cmd/dnv-cdc (CM1). Only tcp is
	// accepted (CM2); 8009 is the IANA discovery port.
	DefaultCdcTrType  = "tcp"
	DefaultCdcAdrFam  = "ipv4"
	DefaultCdcTrSvcId = "8009"
	// CdcAdrFamIpv6 is the other legal --adr-fam value (CM2).
	CdcAdrFamIpv6 = "ipv6"
	// CdcRangeAll is the --range default: all sixteen ranges, so a
	// single-instance deployment needs no sharding flag (CM1). Range
	// digit h owns the sixteen shard codes h0…hf (DS2).
	CdcRangeAll = "0,1,2,3,4,5,6,7,8,9,a,b,c,d,e,f"
	// CdcMaxAdminSqSize is the admin SQ entry count: CAP.MQES is one less,
	// Connect's 0's-based SQSIZE is capped at CAP.MQES so the queue never
	// holds more entries than this (NP4), and it is the ASQSZ of every
	// discovery log entry (DS3). 32 is NVME_AQ_DEPTH, what the Linux host
	// asks for.
	CdcMaxAdminSqSize = 32
	// CdcAerl is Identify's AERL: up to CdcAerl + 1 outstanding AERs per
	// connection (NP11).
	CdcAerl = 3
	// CdcMaxH2CData is ICResp's MAXH2CDATA and the framing cap: readPdu
	// refuses any PDU whose PLEN exceeds the CapsuleCmd header plus this
	// value, before reading its payload. The 1024 B Connect data blob is the
	// only host-to-controller data dnv-cdc ever interprets — in-capsule data
	// on any other command is accepted and discarded (NP2, NP3). 8192 is
	// NVME_TCP_ADMIN_CCSZ, the host's own admin capsule budget.
	CdcMaxH2CData = 8192
	// The discovery log page geometry (DS9): a header block followed by
	// fixed-size entries, both as the specs lay them out.
	CdcDiscLogHeaderSize = 1024
	CdcDiscLogEntrySize  = 1024
	// CdcCntlIdMax is the top of the dynamic CNTLID range [1, CdcCntlIdMax]
	// assigned round-robin at Connect (NP5). 0xffff is the "dynamic
	// controller" wildcard and 0xfff0…0xfffe are reserved.
	CdcCntlIdMax = 0xffef
	// DefaultCdcKeepAliveGraceMs is added to a connection's KATO before it
	// is reaped (NP10), mirroring nvmet's grace.
	DefaultCdcKeepAliveGraceMs = 10000
	// DefaultCdcZeroKatoTmoMs is the idle cutoff of a KATO = 0 connection
	// (NP10) — a one-shot `nvme discover` is not immortal. It mirrors
	// nvmet's NVMET_DISC_KATO_MS.
	DefaultCdcZeroKatoTmoMs = 120000
	// DefaultCdcRescanInterval is the seconds between retries of a failed
	// etcd scan (WV5). The server keeps answering from the held state
	// meanwhile (DS10) — or, before the first scan has landed, answers no
	// host at all (CM4).
	DefaultCdcRescanInterval = 10
)
