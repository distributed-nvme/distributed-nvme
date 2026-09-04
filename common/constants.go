package common

const (
	ValidStrPattern = `^[a-zA-Z0-9\-_/.:]+$`
	MaxStrSize      = 64
	// ValidNqnPattern requires a ':' after the domain part, so the
	// well-known discovery NQN "nqn.2014-08.org.nvmexpress.discovery" can
	// never validate (architecture.md §7).
	ValidNqnPattern = `^nqn\.\d{4}-(0[1-9]|1[0-2])\.[A-Za-z0-9\.-]+:.+$`
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

	MaxDmPoolDataBlockSize     = 1 * 1024 * 1024 * 1024
	MinDmPoolDataBlockSize     = 64 * 1024
	DefaultDmPoolDataBlockSize = 1 * 1024 * 1024
	MaxDmRaid0StripeSize       = 64 * 1024 * 1024
	MinDmRaid0StripeSize       = 4 * 1024
	DefaultDmRaid0StripeSize   = 64 * 1024

	// Chunk and region have similar meaning.
	// It is chunk in md, It is region in dm.
	MinChunkBlockCnt      = 1
	MaxChunkBlockCnt      = 1024
	DefaultChunkBlockCnt  = 128
	MinRegionBlockCnt     = 1
	MaxRegionBlockCnt     = 1024
	DefaultRegionBlockCnt = 128

	MaxDnCntPerCluster   = 1024
	MaxCnCntPerCluster   = 1024
	MaxSpCntPerCluster   = 4096
	MaxTdCntPerSp        = 1024
	MaxSsCntPerSp        = 4
	MaxNsCntPerSs        = 4
	MaxHostCntPerSs      = 8
	MaxSliceCntPerSp     = 16
	MaxCntlrCntPerSp     = 4
	MinCntlrCntPerSp     = 1
	DefaultCntlrCntPerSp = 2
	MaxCloneCntPerSp     = 64
	MaxXferCntPerSp      = 4
	MaxMigrCntPerSp      = 4
	MaxSideCntPerDn      = 1024
	MaxCntlrCntPerCn     = 256
	MaxLegPerGrp         = 8
	MaxSpareLegPerGrp    = 2

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
	// registry is the kind-`b` wrapper dm tables themselves. No LVM
	// (update_01.md U3).
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

	DefaultLocalStorPrefix = "/var/tmp"

	IdKeyFmt     = "%016x"
	FreeSpaceFmt = "%016x"
	BinIdxFmt    = "%01x"
	BmIdxFmt     = "%02x"

	CmdSoftTimeout = 3
	CmdHardTimeout = 5

	MaxCloneThreshold     = 8
	DefaultCloneThreshold = 1
	MaxCloneBatchSize     = 4
	DefaultCloneBatchSize = 1
	MaxCloneBmCnt         = 16

	MaxMigrThreshold     = 8
	DefaultMigrThreshold = 1
	MaxMigrBatchSize     = 4
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

	// The single nvmet port every node exports (architecture.md §3.1/§3.2).
	NvmetPortId = 1

	// The three fixed ANA groups on every node's port (architecture.md
	// [D4]). Group 1 always exists in nvmet and defaults to optimized;
	// groups 2 and 3 are created at port setup. Group states are written
	// once and never changed; every ANA transition rewrites a namespace's
	// ana_grpid instead.
	AnaGrpIdOptimized    = 1
	AnaGrpIdNonOptimized = 2
	AnaGrpIdInaccessible = 3

	// AgentReply.code values (dnagent.md §2.5). 0 = OK. Callers only ever
	// branch on code != 0; the specific values exist for details/log
	// readability and tests.
	ReplyCodeStaleRevision = 1
	ReplyCodeUnknownObject = 2

	// Seconds between background retries of a pending migration-destination
	// nvme connect (dnagent.md DN8).
	DnMigrConnectRetryInterval = 5

	// Side provisioning ([D15], update_01.md U4, architecture.md §9.4,
	// dnagent.md DN9): the background zeroing goroutine zeroes
	// DnZeroBatchExtCnt logical extents per `blkdiscard --zeroout`
	// command, through the side's dm-linear, and persists that batch's
	// `zeroed_bits` after each success. The batch size assumes fast
	// hardware Write Zeroes: batch × ext_size must stay inside
	// CmdSoftTimeout. A failed or timed-out batch is retried no sooner
	// than DnZeroRetryInterval seconds later — the zeroing twin of
	// DnMigrConnectRetryInterval, never a hot loop.
	DnZeroBatchExtCnt   = 10
	DnZeroRetryInterval = 5

	// CN base state (architecture.md §3.2): the tmpfs that carries the
	// clone-metadata arena file, sized 2 × CnCloneMetaAreaSize so that even
	// a fully materialized arena plus slack never hits ENOSPC on the mount.
	// The file itself is sparse: pages appear as dm-clone writes metadata
	// and are released again by the allocator's hole-punch discard
	// (update_01.md U3).
	DefaultCnTmpfsSize = 2 * 1024 * 1024 * 1024

	// Seconds between two §3.6 leg health-probe rounds on a primary
	// cntlr, and how long one probe IO may stay in flight before the leg
	// is reported stalled (cnagent.md CN11). Probes are single-flight per
	// leg and run outside every lock.
	CnLegProbeInterval     = 5
	CnLegProbeStallSeconds = 15

	// The §3.6 health block: the last 4 KiB of the leg's meta region.
	// Payload = magic + writer id + timestamp, never interpreted on read
	// ([D6]).
	LegHealthBlockSize = 4096
	LegHealthMagic     = "DNVHLTH1"

	// Seconds between background retries of a pending cn outbound nvme
	// connect — leg side connections and clone source connections
	// (cnagent.md CN10/CN18); the cn twin of DnMigrConnectRetryInterval.
	CnConnectRetryInterval = 5

	// SuspendSeconds is the §11.2 src-cutover grace window: a migration
	// source's per-CN dm-linears are held suspended for at least this long
	// before they are reloaded onto their dm-errors, so IO the old primary
	// still had in flight is absorbed rather than immediately failed. The
	// deferred bios are released against the dm-error table the reload
	// installs, so they error at the end of the window instead of replaying
	// onto the side's data ([D12]). It is a floor, not a deadline: the
	// reload happens on the first converge at or after it.
	SuspendSeconds = 60
)
