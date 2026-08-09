const (
	DnvPrefix = "dnv"
	DmPrefix = "dnv"
	NqnPrefix = "nqn.2024-01.io.dnv"

	ValidStrPattern = `^[a-zA-Z0-9\-_/.:]+$`
	MaxStrSize = 64
	ValidNqnPattern = `^nqn\.\d{4}-(0[1-9]|1[0-2])\.[A-Za-z0-9\.-]+:` +
		`(uuid:[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{` +
		`4}-[0-9a-fA-F]{12}|discovery|.+)$`
	MaxNqnLength = 223

	DnExtSize = 1 * 1024 * 1024 * 1024
	DmBlockSize = 128 * 1024 * 1024
	DmStripeSizie = 16 * 1024
	ShardBucketSize = 256
	ShardCodeFmt = "%02x"

	MaxDescSize = 1024
	MaxDnCntPerCluster = 1024
	MaxSegmentCntPerDn = 16
	SegmentIdxFmt = "%02x"
	MaxExtCntPerSegment = 4096
	MaxCnCntPerCluster = 1024
	MaxSpCntPerCluster = 4096
	MaxTdCntPerSp = 1024
	MaxSsCntPerSp = 8
	MaxNsCntPerSs = 8
	MaxHostCntPerSs = 128
	MaxLegCntPerSp = 16
	MaxCntlrCntPerSp = 4
	MaxCloneCntPerSp = 2
	MaxMoveCntPerSp = 2
	MaxLdCntPerDn = 4096
	MaxCntlrCntPerCn = 256

	CmdSoftTimeout = 3
	CmdHardTimeout = 5

	DnBatchSize = 16
	CnBatchSize = 16

	DnBinLevel0 = 1 * DnExtSize
	DnBinLevel1 = 16 * DnExtSize
	DnBinLevel2 = 256 * DnExtSize
	DnBinLevel3 = 4096 * DnExtSize

	// The cntlr_id is a per host per connection resoruce.
	// To work in the worst case, we should make sure:
	// MaxCntlrCntPerCn * MaxSsCntPerSp * MaxHostCntPerSs < CntlrIdStep
	// To make sure the re-connection works well, we should dobule the left:
	// MaxCntlrCntPerCn * MaxSsCntPerSp * MaxHostCntPerSs * 2 < CntlrIdStep
	// We can't satisfy this math requirement, hope the allocation algorithm
	// could distribute the workload evenly and the typically the
	// ss_cnt_per_sp and exp_cnt_per_ss should be 1.
	CntlrIdBase = 10000
	CntlrIdStep = 8192

	DefaultClusterName = "default"
	DefaultCnCap =  4 * 1024 * 1024 * 1024 * 1024
	MaxCnCap = 64 * 1024 * 1024 * 1024 * 1024
	DefaultListCnt = 64
	MaxListCnt = 1024

	RedunMetaSize = 1 * DmBlockSize
	ThinPoolMetaSize = 1 * DnExtSize

	IdKeyFmt = "%016x"
	BinIdxFmt = "%1x"
	FreeSpaceFmt = "%016x"
)