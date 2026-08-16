package common

const (
	DnvPrefix = "dnv"
	DmPrefix  = "dnv"
	NqnPrefix = "nqn.2024-01.io.dnv"

	ValidStrPattern = `^[a-zA-Z0-9\-_/.:]+$`
	MaxStrSize      = 64
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
	DnPortBitmapSize   = 512
	CnPortBitmapSize   = 512

	// The cntlr_id is a per host per connection resoruce.
	// To work in the worst case, we should make sure:
	// MaxCntlrCntPerCn * MaxSsCntPerSp * MaxHostCntPerSs < CnNvmeIdStep
	// To make sure the re-connection works well, we should dobule the left:
	// MaxCntlrCntPerCn * MaxSsCntPerSp * MaxHostCntPerSs * 2 < CnNvmeIdStep
	// We can't satisfy this math requirement, hope the allocation algorithm
	// could distribute the workload evenly and the typically the
	// ss_cnt_per_sp and exp_cnt_per_ss should be 1.
	CnNvmeIdBase   = 10000
	CnNvmeIdStep   = 5000
	CnNvmeIdSetCnt = 8

	DnNvmeIdBase   = 10000
	DnNvmeIdStep   = 5000
	DnNvmeIdSetCnt = 8

	DefaultClusterName = "default"
	DefaultCnCap       = 4 * 1024 * 1024 * 1024 * 1024
	MaxCnCap           = 64 * 1024 * 1024 * 1024 * 1024
	MinCpCap           = 1024
	DefaultListCnt     = 64
	MaxListCnt         = 1024

	DefaultDnTmpfsPrefix = "/tmp/dnv-dn-tmpfs"
	DefaultDnTmpfsSize   = 2 * 1024 * 1024 * 1024
	DefaultDnLoopStart   = 10000
	DefaultDnLoopRange   = 1024
	DefaultCnTmpfsPrefix = "/tmp/dnv-cn-tmpfs"
	DefaultCnLoopStart   = 20000
	DefaultCnLoopRange   = 1024
	InitLoopPosition     = -3

	DefaultDnVgPrefix = "dnv-dn-"

	IdKeyFmt     = "%016x"
	FreeSpaceFmt = "%016x"

	CmdSoftTimeout = 3
	CmdHardTimeout = 5

	OldPirmaryWait = 5
	SideSwitchWait = 300
)
