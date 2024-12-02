package constants

import (
	"time"
)

const (
	SchemaPrefixDefault       = "dnv"
	DeviceMapperPrefixDefault = "dnv"
	NqnPrefixDefault          = "nqn.2024-01.io.dnv"

	ReplyCodeSucceed     = uint32(0)
	ReplyMsgSucceed      = "succeed"
	ReplyCodeDupRes      = uint32(1001)
	ReplyCodeUnknownRes  = uint32(1002)
	ReplyCodeInternalErr = uint32(1003)
	ReplyCodeAgentErr    = uint32(1004)
	ReplyCodeInvalidArg  = uint32(1005)
	ReplyCodeNotFound    = uint32(1006)
	ReplyCodeResBusy     = uint32(1007)
	ReplyCodeNoCapacity  = uint32(1008)
	ReplyCodeNeedMore    = uint32(1009)

	StatusCodeSucceed         = uint32(0)
	StatusMsgSucceed          = "succeed"
	StatusCodeUninit          = uint32(2001)
	StatusCodeUnreachable     = uint32(2002)
	StatusCodeInternalErr     = uint32(2003)
	StatusCodeDataMismatch    = uint32(2004)
	StatusCodeDeletedRevision = uint32(2005)
	StatusCodeOldRevision     = uint32(2006)
	StatusCodeNoConf          = uint32(2007)
	StatusCodeNotFound        = uint32(3001)

	AgentTimeoutDefault = 3 * time.Second
	WkrTimeoutDefault   = 3 * time.Second
	ShardInitWaitTime   = 3 * time.Second
	ShardDeleteWaitTime = 60 * time.Second
	RollbackTimeout     = 10 * time.Second
	// GrantTTL is in second by default
	GrantTTLDefault = 10
	AllocLockTTL    = 10

	ShardWorkerDelayDefault = 1 * time.Second

	DnRetryBase     = 1 * time.Second
	DnRetryPower    = 2
	DnRetryMax      = 64 * time.Second
	DnCheckInterval = 1 * time.Second

	CnRetryBase     = 1 * time.Second
	CnRetryPower    = 2
	CnRetryMax      = 64 * time.Second
	CnCheckInterval = 1 * time.Second

	SpRetryBase = 1 * time.Second

	DnAgentBgInterval = 1 * time.Second
	CnAgentBgInterval = 1 * time.Second

	EtcdPageSize = 100

	RevisionUninit  = 0
	RevisionDeleted = -1

	DataExtentSizeShiftMin       = 20
	DataExtentSizeShiftMax       = 40
	DataExtentSizeShiftDefault   = 30
	DataExtentPerSetShiftMax     = 15
	DataExtentPerSetShiftDefault = 9

	MetaExtentSizeShiftMin       = 12
	MetaExtentSizeShiftMax       = 34
	MetaExtentSizeShiftDefault   = 21
	MetaExtentPerSetShiftMax     = 13
	MetaExtentPerSetShiftDefault = 9

	ExtentSetCntShiftMax = 7

	ExtentRatioShiftMin     = 1
	ExtentRatioShiftMax     = 10
	ExtentRatioShiftDefault = 7

	ThinBlockSizeDefault      = 1 * 1024 * 1024
	ThinLowWaterMarkDefault   = 100
	ThinErrorIfNoSpaceDefault = false
	RaidMetaRegionSizeDefault = 1 * 1024 * 1024
	RaidDataRegionSizeDefault = 64 * 1024 * 1024
	Raid0ChunkSizeDefault     = 16 * 1024

	LegCntMax              = 64
	LegCntDefault          = 2
	ActiveCntlrCntMax      = 16
	ActiveCntlrCntDefault  = 1
	StandbyCntlrCntMax     = 4
	StandbyCntlrCntDefault = 1

	InternalCntlidMin   = 10000
	InternalCntlidMax   = 19999
	ExternalCntlidStart = 20000
	ExternalCntlidStep  = 2000
	DnInternalPortNum   = "4097"
	CnInternalPortNum   = "4098"
	ExternalPortBase    = 0
	ExternalPortSize    = 4096

	ShardShift           = 8
	ShardCnt             = 1 << ShardShift
	ShardMove            = 64 - ShardShift
	ShardIdFormat        = "%02x"
	ShardHighPrioCode    = "7"
	ShardMediumPrioCode  = "5"
	ShardLowPrioCode     = "3"
	ShardDefaultPrioCode = "5"
	ShardIgnorePrioCode  = "0"
	ShardHighPrioText    = "high"
	ShardMediumPrioText  = "medium"
	ShardLowPrioText     = "low"

	StringLengthMax = 256
	StringLengthMin = 1
	NqnLengthMax    = 223
	TagCntMax       = 16
	PortNumMax      = 65535

	TraceIdKey = "trace_id"

	LocalDataPathDefault = "."

	RaidDevMiss            = '-'
	RaidHealthAliveInSync  = 'A'
	RaidHealthAliveOutSync = 'a'
	RaidHealthDead         = 'D'
	RaidHealthMiss         = '-'
	RaidJournalWriteTh     = 'A'
	RaidJournalWriteBa     = 'a'
	RaidJournalDead        = 'D'
	RaidJournalNone        = '-'

	DevMajorNone = 0
	DevMinorNone = 0

	RedunTypeRaid1 = 1

	Uint32Max = uint32(0xffffffff)

	AllocateRetryCntDefault = 3
	NsBitSizeDefault        = 4096

	DefaultTagKey = "GrpcTarget"

	MetaRegionSizeDefault = uint64(64 * 1024)

	AnaGroupOptimized      = "optimized"
	AnaGroupNonOptimized   = "non-optimized"
	AnaGroupInaccessible   = "inaccessible"
	AnaGroupChange         = "change"
	AnaGroupPersistentLoss = "persistent-loss"
)
