package common

const (
	ValidStrPattern = `^[a-zA-Z0-9\-_/.:]+$`
	MaxStrSize      = 64
	MaxNoteSize     = 4 * 1024
	ValidNqnPattern = `^nqn\.\d{4}-(0[1-9]|1[0-2])\.[A-Za-z0-9\.-]+:` +
		`(uuid:[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{` +
		`4}-[0-9a-fA-F]{12}|discovery|.+)$`
	MaxNqnLength = 223

	ShardBucketSize = 256
	ShardCodeFmt    = "%02x"

	MaxDnExtSize = 1024 * 1024 * 1024 * 1024
	MinDnExtSize = 64 * 1024 * 1024
	DefaultDnExtSize   = 1 * 1024 * 1024 * 1024
	DefaultDnBin0Shift = 0
	DefaultDnBin1Shift = 4
	DefaultDnBin2Shift = 8
	DefaultDnBin3Shift = 12

	MinAllocDnBatchSize = 1
	MaxAllocDnBatchSize = 1024
	DefaultAllocDnBatchSize = 16
	MinAllocCnBatchSize = 1
	MaxAllocCnBatchSize = 1024
	DefaultAllocCnBatchSize = 16

	MaxDmPoolDataBlockSize = 1 * 1024 * 1024 * 1024
	MinDmPoolDataBlockSize = 64 * 1024
	DefaultDmPoolDataBlockSize = 1 * 1024 * 1024
	MaxDmPoolLowWaterMarkPct = 90
	MinDmPoolLowWaterMarkPct = 10
	DefaultDmPoolLowWaterMarkPct = 50
	MaxDmRaid0StripeSize = 64 * 1024 * 1024
	MinDmRaid0StripeSize = 4 * 1024
	DefaultDmRaid0StripeSize   = 64 * 1024

	// Chunk and region have samilar meaning.
	// It is chunk in md, It is region in dm.
	MinChunkBlockCnt = 1
	MaxChunkBlockCnt = 1024
	DefaultChunkBlockCnt = 128
	MinRegionBlockCnt = 1
	MaxRegionBlockCnt = 1024
	DefaultRegionBlockCnt = 128

	MaxDnCntPerCluster = 1024
	MaxCnCntPerCluster = 1024
	MaxSpCntPerCluster = 4096
	MaxTdCntPerSp      = 1024
	MaxSsCntPerSp      = 4
	MaxNsCntPerSs      = 4
	MaxHostCntPerSs    = 8
	MaxSliceCntPerSp   = 16
	MaxCntlrCntPerSp   = 4
	MinCntlrCntPerSp   = 1
	DefaultCntlrCntPerSp = 2
	MaxCloneCntPerSp   = 64
	MaxXferCntPerSp    = 4
	MaxMigrCntPerSp    = 4
	MaxSideCntPerDn    = 1024
	MaxCntlrCntPerCn   = 256
	MaxLegPerGrp       = 8
	MaxSpareLegPerGrp  = 2

	CnCntlidSlotBase   = 10000
	CnCntlidSlotStep   = 5000
	CnCntlidSlotCnt = 8

	DnCntlidSlotBase   = 10000
	DnCntlidSlotStep   = 5000
	DnCntlidSlotCnt = 8

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
	DefaultCloneVgPrefix = "dnv-clone-vg"
	DefaultCloneVgSize = 1 * 1024 * 1024 * 1024
	DefaultCloneVgExtSize = 4 * 1024 * 1024

	DefaultDnVgPrefix = "dnv-dn"

	DefaultMigrVgPrefix = "dnv-migr"
	DefaultMigrVgSize = 1 * 1024 * 1024 * 1024
	DefaultMigrVgExtSize = 4 * 1024 * 1024

	DefaultLocalStorPrefix = "/var/tmp"

	IdKeyFmt     = "%016x"
	FreeSpaceFmt = "%016x"
	BinIdxFmt    = "%01x"
	BmIdxFmt     = "%02x"

	CmdSoftTimeout = 3
	CmdHardTimeout = 5

	SideSwitchWait = 300

	MaxCloneThreshold = 8
	DefaultCloneThreshold = 1
	MaxCloneBatchSize = 4
	DefaultCloneBatchSize = 1
	MaxCloneBmCnt = 16
	MaxMigrThreshold = 8
	DefaultMigrThreshold = 1
	MaxMigrBatchSize = 4
	MaxMigrBmCnt = 4

	DefaultPrimaryUnhealthy = 5
	DefaultCntlrUnhealthy = 600
	DefaultSideUnhealthy = 600
	DefaultLegUnhealthy = 1200
	DefaultPoolLowWatermarkPct = 50

	DefaultNvmeFastIoFailTmo = 5
)
