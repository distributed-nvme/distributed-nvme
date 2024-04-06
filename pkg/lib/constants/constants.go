package constants

import (
	"time"
)

const (
	SchemaPrefixDefault       = "dnv"
	DeviceMapperPrefixDefault = "dnv"

	ReplyCodeSucceed     = 0
	ReplyMsgSucceed      = "succeed"
	ReplyCodeDupRes      = 1001
	ReplyCodeUnknownRes  = 1002
	ReplyCodeInternalErr = 1003
	ReplyCodeAgentErr    = 1004
	ReplyCodeInvalidArg  = 1005

	StatusCodeSucceed     = 0
	StatusMsgSucceed      = "succeed"
	StatusCodeUninit      = 2001
	StatusCodeUnreachable = 2002
	StatusCodeInternalErr = 2003

	AgentTimeoutDefault = 3 * time.Second
	ShardInitWaitTime   = 10 * time.Second
	ShardDeleteWaitTime = 60 * time.Second
	RollbackTimeout     = 10 * time.Second
	// GrantTTL is in second by default
	GrantTTLDefault = 10

	DataExtentSizeShiftMin       = 20
	DataExtentSizeShiftMax       = 40
	DataExtentSizeShiftDefault   = 30
	DataExtentPerSetShiftMax     = 15
	DataExtentPerSetShiftDefault = 9

	MetaExtentSizeShiftMin       = 12
	MetaExtentSizeShiftMax       = 34
	MetaExtentSizeShiftDefault   = 20
	MetaExtentPerSetShiftMax     = 13
	MetaExtentPerSetShiftDefault = 9

	ExtentSetCntShiftMax = 7

	ExtentRatioShiftMin     = 1
	ExtentRatioShiftMax     = 10
	ExtentRatioShiftDefault = 7

	LegCntMax              = 64
	LegCntDefault          = 2
	ActiveCntlrCntMax      = 16
	ActiveCntlrCntDefault  = 1
	StandbyCntlrCntMax     = 4
	StandbyCntlrCntDefault = 1

	CnPortBase    = 0
	CnPortSize    = 4096
	DnCntlidMin   = 10000
	DnCntlidMax   = 19999
	CnCntlidStart = 20000
	CnCntlidStep  = 2000

	ShardCnt             = 256
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
)
