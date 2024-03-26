package lib

const (
	DefaultEtcdPrefix = "dnv"

	AgentSucceedCode = 0
	AgentSucceedMsg = "succeed"
	AgentUninitCode = 1
	AgentUnreachableCode = 2
	AgentOsCmdErrCode = 3

	CpApiSucceedCode = 0
	CpApiSucceedMsg = "succeed"
	CpApiInternalErrCode = 1
	CpApiDupResErrCode = 2
	CpApiUnknownResErrCode = 3
	CpApiAgentConnErrCode = 4
	CpApiAgentGrpcErrCode = 5
	CpApiAgentReplyErrCode = 6

	AgentTimeoutSecondDefault = 3

	DataExtentSizeShiftMin = 20
	DataExtentSizeShiftMax = 40
	DataExtentSizeShiftDefault = 30
	DataExtentPerSetShiftMax = 15
	DataExtentPerSetShiftDefault = 9

	MetaExtentSizeShiftMin = 12
	MetaExtentSizeShiftMax = 34
	MetaExtentSizeShiftDefault = 20
	MetaExtentPerSetShiftMax = 13
	MetaExtentPerSetShiftDefault = 9

	ExtentSetCntShiftMax = 7

	ExtentRatioShiftMin = 1
	ExtentRatioShiftMax = 10
	ExtentRatioShiftDefault = 7

	LegCntMax = 64
	LegCntDefault = 2
	ActiveCntlrCntMax = 16
	ActiveCntlrCntDefault = 1
	StandbyCntlrCntMax = 4
	StandbyCntlrCntDefault = 1

	DnPortBase = 0
	DnPortSize = 4096
	CnPortBase = 0
	CnPortSize = 4096
	DnCntlidMin = 10000
	DnCntlidMax = 19999
	CnCntlidStart = 20000
	CnCntlidStep = 2000

	ShardSize = 16

	StringLengthMax = 256
	StringLengthMin = 1
	NqnLengthMax = 223
	PrefixDefault = "dnv"
)
