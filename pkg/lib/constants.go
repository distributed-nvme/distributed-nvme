package lib

const (
	SchemaPrefixDefault = "dnv"
	DeviceMapperPrefixDefault = "dnv"

	RpcSucceedCode = 0
	RpcSucceedMsg = "succeed"
	RpcInternalErrCode = 1
	RpcDupResErrCode = 2
	RpcUnknownResErrCode = 3

	NodeSucceedMsg = "succeed"
	NodeUninitCode = 1
	NodeUnreachableCode = 2

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
)
