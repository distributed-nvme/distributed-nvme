package gateway

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is gateway.md §9.2: every row of architecture.md §7 as a
// table-driven case against the pure validators of validate.go. It does no
// I/O at all — no etcd, no clock, no agent — so it runs (and must keep
// running) even when TestMain found no etcd binary.
//
// Every case asserts the gRPC CODE, never merely "an error came back". §7
// violations are GW7's INVALID_ARGUMENT and nothing else, and a client
// branches on the code: a validator that refused with, say, ABORTED would
// make a malformed request look like a lost race and be retried for ever.

// ---------------------------------------------------------------------------
// Shared assertions
// ---------------------------------------------------------------------------

// validateWantCode asserts the code one validator produced. codes.OK is the
// "accepted" case, because status.Code(nil) is exactly that, so acceptance and
// refusal share one table column and no case can silently stop asserting.
func validateWantCode(
	t *testing.T,
	what string,
	err error,
	want codes.Code,
) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Errorf("%s: code %s (err %v), want %s", what, got, err, want)
	}
}

// validateWantMsg asserts a refusal names what the caller must fix. §7 messages
// are the only thing a human operator sees, so the field name (and, for the
// pattern row, the pattern itself) being in the text is part of the contract.
func validateWantMsg(t *testing.T, what string, err error, want string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: no error, want one mentioning %q", what, want)
		return
	}
	if msg := status.Convert(err).Message(); !strings.Contains(msg, want) {
		t.Errorf("%s: message %q does not mention %q", what, msg, want)
	}
}

// validateNqnOfLen builds a pattern-valid NQN of exactly n bytes, so that the
// MaxNqnLength boundary is pinned on its own: a hand-written long NQN would
// trip the pattern too and the test would pass for the wrong reason.
func validateNqnOfLen(t *testing.T, n int) string {
	t.Helper()
	const head = "nqn.2024-01.io.dnv:"
	if n <= len(head) {
		t.Fatalf("cannot build a valid NQN of %d bytes", n)
	}
	return head + strings.Repeat("a", n-len(head))
}

// ---------------------------------------------------------------------------
// Names: MaxStrSize and ValidStrPattern (§7 row 1)
// ---------------------------------------------------------------------------

// TestValidateName pins the required-name row of §7: non-empty, at most
// MaxStrSize BYTES, and drawn from ValidStrPattern. The size limit is counted
// in bytes and not in runes, which is why a 33-character two-byte string is
// refused at 66 bytes.
func TestValidateName(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  codes.Code
	}{
		{"empty", "", codes.InvalidArgument},
		{"single char", "a", codes.OK},
		{"every allowed class", "aZ9-_/.:", codes.OK},
		{"an addr_port", "192.168.0.17:9000", codes.OK},
		{"a path-like location", "/rack/1.a", codes.OK},
		{
			"at MaxStrSize",
			strings.Repeat("a", common.MaxStrSize),
			codes.OK,
		},
		{
			"one byte over MaxStrSize",
			strings.Repeat("a", common.MaxStrSize+1),
			codes.InvalidArgument,
		},
		{
			"64 runes but 128 bytes",
			strings.Repeat("é", common.MaxStrSize),
			codes.InvalidArgument,
		},
		{"space", "a b", codes.InvalidArgument},
		{"at sign", "a@b", codes.InvalidArgument},
		{"bang", "sp!", codes.InvalidArgument},
		{"star", "sp*", codes.InvalidArgument},
		{"comma", "a,b", codes.InvalidArgument},
		{"tab", "a\tb", codes.InvalidArgument},
		{"embedded newline", "a\nb", codes.InvalidArgument},
		{"trailing newline", "abc\n", codes.InvalidArgument},
		{"non ascii", "café", codes.InvalidArgument},
		{"nul byte", "a\x00b", codes.InvalidArgument},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			validateWantCode(t, "validateName",
				validateName("sp_name", item.value), item.want)
		})
	}
}

// TestValidateOptionalName pins the one difference of the optional row: an
// empty cluster_name / location is legal and defaults at use time, while a
// non-empty one obeys the same size and pattern rules.
func TestValidateOptionalName(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  codes.Code
	}{
		{"empty is legal", "", codes.OK},
		{"non-empty is checked", "ok-1", codes.OK},
		{
			"over MaxStrSize",
			strings.Repeat("a", common.MaxStrSize+1),
			codes.InvalidArgument,
		},
		{"bad character", "bad name", codes.InvalidArgument},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			validateWantCode(t, "validateOptionalName",
				validateOptionalName("cluster_name", item.value), item.want)
		})
	}
}

// TestValidateNameMessages pins what a §7 name refusal tells the caller: the
// request field it came from, and — for the pattern row — the pattern itself,
// so the message is actionable without the spec at hand.
func TestValidateNameMessages(t *testing.T) {
	validateWantMsg(t, "empty",
		validateName("td_name", ""), "td_name")
	validateWantMsg(t, "too long",
		validateName("td_name", strings.Repeat("a", common.MaxStrSize+1)),
		"td_name")
	validateWantMsg(t, "too long cites the maximum",
		validateName("td_name", strings.Repeat("a", common.MaxStrSize+1)),
		"65")
	validateWantMsg(t, "bad pattern cites the pattern",
		validateName("td_name", "bad name"), common.ValidStrPattern)
}

// ---------------------------------------------------------------------------
// NQNs (§7 row 2)
// ---------------------------------------------------------------------------

// TestValidateNqn pins the NQN row: non-empty, at most MaxNqnLength bytes and
// ValidNqnPattern — which requires a ':' after the domain part and a non-empty
// suffix behind it.
func TestValidateNqn(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  codes.Code
	}{
		{"empty", "", codes.InvalidArgument},
		{"the dnv form", "nqn.2024-01.io.dnv:sp-1", codes.OK},
		{"a host nqn", "nqn.2014-08.org.nvmexpress:uuid:abc", codes.OK},
		{"hyphenated domain", "nqn.2001-04.com.my-corp:x", codes.OK},
		{"month 01", "nqn.2024-01.io.dnv:x", codes.OK},
		{"month 12", "nqn.2024-12.io.dnv:x", codes.OK},
		{"month 00", "nqn.2024-00.io.dnv:x", codes.InvalidArgument},
		{"month 13", "nqn.2024-13.io.dnv:x", codes.InvalidArgument},
		{"single digit month", "nqn.2024-1.io.dnv:x", codes.InvalidArgument},
		{"three digit year", "nqn.202-01.io.dnv:x", codes.InvalidArgument},
		{"no nqn prefix", "iqn.2024-01.io.dnv:x", codes.InvalidArgument},
		{"no colon", "nqn.2024-01.io.dnv", codes.InvalidArgument},
		{"empty after colon", "nqn.2024-01.io.dnv:", codes.InvalidArgument},
		{"underscore in domain", "nqn.2024-01.io_dnv:x", codes.InvalidArgument},
		{"leading space", " nqn.2024-01.io.dnv:x", codes.InvalidArgument},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			validateWantCode(t, "validateNqn",
				validateNqn("nqn", item.value), item.want)
		})
	}
}

// TestValidateNqnLength pins the MaxNqnLength boundary exactly: 223 bytes is
// the longest NQN NVMe allows and 224 is one too many. The refusal is the size
// row and not the pattern row — both operands are pattern-valid.
func TestValidateNqnLength(t *testing.T) {
	atMax := validateNqnOfLen(t, common.MaxNqnLength)
	overMax := validateNqnOfLen(t, common.MaxNqnLength+1)
	validateWantCode(t, "at MaxNqnLength",
		validateNqn("nqn", atMax), codes.OK)
	validateWantCode(t, "one byte over MaxNqnLength",
		validateNqn("nqn", overMax), codes.InvalidArgument)
	validateWantMsg(t, "one byte over MaxNqnLength",
		validateNqn("nqn", overMax), "224")
}

// TestValidateNqnRejectsDiscoveryNqn pins the §7 sentence that saves every
// name-taking RPC a special case: ValidNqnPattern demands a ':' after the
// domain part, and the well-known discovery NQN has none, so it can never
// validate. No dnv object can be named it and CreateSubsystem needs no
// separate rejection — which is exactly why this assertion has to exist here,
// as the only place the impossibility is checked.
func TestValidateNqnRejectsDiscoveryNqn(t *testing.T) {
	err := validateNqn("nqn", common.NvmeDiscoveryNqn)
	validateWantCode(t, "discovery nqn", err, codes.InvalidArgument)
	validateWantMsg(t, "discovery nqn", err, "not a valid NQN")
	// Length is not what refuses it: it is well under MaxNqnLength, so only
	// the missing ':' can be doing the work.
	if len(common.NvmeDiscoveryNqn) > common.MaxNqnLength {
		t.Fatalf("the discovery NQN is %d bytes, so this test would pass "+
			"on the length row instead of the pattern row",
			len(common.NvmeDiscoveryNqn))
	}
	// And appending a ':' suffix turns it into a legal NQN, which pins that
	// the ':' really is the discriminator.
	validateWantCode(t, "discovery nqn plus a suffix",
		validateNqn("nqn", common.NvmeDiscoveryNqn+":x"), codes.OK)
}

// TestValidateHosts pins the allowed_hosts row: at most MaxHostCntPerSs
// entries, each a valid NQN. An absent list is legal — a subsystem with no
// allowed host simply admits nobody.
func TestValidateHosts(t *testing.T) {
	full := make([]string, 0, common.MaxHostCntPerSs+1)
	for idx := 0; idx <= common.MaxHostCntPerSs; idx++ {
		full = append(full, validateNqnOfLen(t, 40+idx))
	}
	cases := []struct {
		name  string
		hosts []string
		want  codes.Code
	}{
		{"nil", nil, codes.OK},
		{"empty", []string{}, codes.OK},
		{"one", full[:1], codes.OK},
		{"at MaxHostCntPerSs", full[:common.MaxHostCntPerSs], codes.OK},
		{"one over MaxHostCntPerSs", full, codes.InvalidArgument},
		{
			"a member that is not an NQN",
			[]string{full[0], "not-an-nqn"},
			codes.InvalidArgument,
		},
		{
			"an empty member",
			[]string{full[0], ""},
			codes.InvalidArgument,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			validateWantCode(t, "validateHosts",
				validateHosts("allowed_hosts", item.hosts), item.want)
		})
	}
}

// ---------------------------------------------------------------------------
// Bounded numerics and the "zero means unset" rule (§7 table)
// ---------------------------------------------------------------------------

// TestValidateBound pins the rule the whole numeric half of §7 rests on: on
// the REQUEST side a proto3 zero is "unset" and asks for the Default*, so zero
// is ALWAYS accepted here even when the minimum is 1, and only a non-zero
// value outside [min, max] is refused. Nothing in validate.go rewrites the
// value; the handler substitutes the default afterwards, once, and stores the
// concrete result. The model's stored-conf validators have no such escape
// hatch — a zero there is corruption — which is exactly why this side has to
// keep accepting one.
func TestValidateBound(t *testing.T) {
	cases := []struct {
		name  string
		value uint64
		want  codes.Code
	}{
		{"zero is unset, never refused", 0, codes.OK},
		{"just below min", 9, codes.InvalidArgument},
		{"at min", 10, codes.OK},
		{"inside", 50, codes.OK},
		{"at max", 100, codes.OK},
		{"just above max", 101, codes.InvalidArgument},
		{"far above max", ^uint64(0), codes.InvalidArgument},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			validateWantCode(t, "validateBound",
				validateBound("f", item.value, 10, 100), item.want)
		})
	}
}

// TestValidateDnBinConf pins the dn_bin_conf.extent_size row — [64 MiB, 1 TiB],
// zero unset — and the §6.2 shift ladder, which is all-or-nothing.
//
// All four shifts zero is the proto3 "unset" that asks for the 0/4/8/12
// default and is accepted; any other set must already BE a ladder
// 0 <= bin0 < bin1 < bin2 < bin3 <= 63. The stored ladder is what every
// capacity key in the cluster is written under for the cluster's whole life
// (§7: nothing resolves it again on read), so an operator who asks for a
// ladder that is not one has to be told here — quietly substituting the
// default would hand them a cluster binned differently from the one they
// asked for, and there is no UpdateCluster RPC to correct it with.
func TestValidateDnBinConf(t *testing.T) {
	cases := []struct {
		name string
		conf *pb.DnBinConf
		want codes.Code
	}{
		{"nil", nil, codes.OK},
		{"empty", &pb.DnBinConf{}, codes.OK},
		{
			"at MinDnExtSize",
			&pb.DnBinConf{ExtentSize: common.MinDnExtSize},
			codes.OK,
		},
		{
			"below MinDnExtSize",
			&pb.DnBinConf{ExtentSize: common.MinDnExtSize - 1},
			codes.InvalidArgument,
		},
		{
			"at MaxDnExtSize",
			&pb.DnBinConf{ExtentSize: common.MaxDnExtSize},
			codes.OK,
		},
		{
			"above MaxDnExtSize",
			&pb.DnBinConf{ExtentSize: common.MaxDnExtSize + 1},
			codes.InvalidArgument,
		},
		{
			// The all-zero set the "empty" and "nil" rows above also carry,
			// spelled out beside an explicit ladder so the two arms of the
			// rule sit next to each other: this one asks for the default.
			"the all-zero ladder asks for the default",
			&pb.DnBinConf{
				ExtentSize: common.DefaultDnExtSize,
				Bin0Shift:  0, Bin1Shift: 0, Bin2Shift: 0, Bin3Shift: 0,
			},
			codes.OK,
		},
		{
			"an explicit increasing ladder",
			&pb.DnBinConf{
				Bin0Shift: 1, Bin1Shift: 5, Bin2Shift: 9, Bin3Shift: 63,
			},
			codes.OK,
		},
		{
			// bin0 above bin1 and bin2 below both: not a ladder in any
			// reading, and the stored bins would be nonsense rather than
			// merely unusual.
			"absurd shifts are refused",
			&pb.DnBinConf{
				Bin0Shift: 99, Bin1Shift: 1, Bin2Shift: 0, Bin3Shift: 7,
			},
			codes.InvalidArgument,
		},
		{
			// Increasing, but bin3 past the 63 a uint64 level can be shifted
			// by: the ladder's top rung has to stay expressible.
			"bin3_shift above 63",
			&pb.DnBinConf{
				Bin0Shift: 0, Bin1Shift: 4, Bin2Shift: 8, Bin3Shift: 64,
			},
			codes.InvalidArgument,
		},
		{
			// Three of four set is still "not all zero", so the whole set is
			// held to the ladder and a zero bin3 fails it. There is no
			// shift-by-shift defaulting to fall back on.
			"a partially set ladder",
			&pb.DnBinConf{Bin0Shift: 0, Bin1Shift: 4, Bin2Shift: 8},
			codes.InvalidArgument,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			validateWantCode(t, "validateDnBinConf",
				validateDnBinConf(item.conf), item.want)
		})
	}
}

// TestValidateAllocConf pins both batch-size rows: [1, 1024], zero unset, and
// each member checked independently so a good dn_batch_size cannot mask a bad
// cn_batch_size.
func TestValidateAllocConf(t *testing.T) {
	cases := []struct {
		name string
		conf *pb.AllocConf
		want codes.Code
	}{
		{"nil", nil, codes.OK},
		{"both unset", &pb.AllocConf{}, codes.OK},
		{
			"both at min",
			&pb.AllocConf{
				DnBatchSize: common.MinAllocDnBatchSize,
				CnBatchSize: common.MinAllocCnBatchSize,
			},
			codes.OK,
		},
		{
			"both at max",
			&pb.AllocConf{
				DnBatchSize: common.MaxAllocDnBatchSize,
				CnBatchSize: common.MaxAllocCnBatchSize,
			},
			codes.OK,
		},
		{
			"dn above max",
			&pb.AllocConf{DnBatchSize: common.MaxAllocDnBatchSize + 1},
			codes.InvalidArgument,
		},
		{
			"cn above max, dn fine",
			&pb.AllocConf{
				DnBatchSize: 16,
				CnBatchSize: common.MaxAllocCnBatchSize + 1,
			},
			codes.InvalidArgument,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			validateWantCode(t, "validateAllocConf",
				validateAllocConf(item.conf), item.want)
		})
	}
}

// TestValidateHealthCheckConf pins the four round-interval rows: [1, 3600]
// seconds each, zero unset, each member checked on its own.
func TestValidateHealthCheckConf(t *testing.T) {
	const over = common.MaxHealthCheckInterval + 1
	cases := []struct {
		name string
		conf *pb.HealthCheckConf
		want codes.Code
	}{
		{"nil", nil, codes.OK},
		{"all unset", &pb.HealthCheckConf{}, codes.OK},
		{
			"all at min",
			&pb.HealthCheckConf{
				DnInterval:    common.MinHealthCheckInterval,
				CnInterval:    common.MinHealthCheckInterval,
				SideInterval:  common.MinHealthCheckInterval,
				CntlrInterval: common.MinHealthCheckInterval,
			},
			codes.OK,
		},
		{
			"all at max",
			&pb.HealthCheckConf{
				DnInterval:    common.MaxHealthCheckInterval,
				CnInterval:    common.MaxHealthCheckInterval,
				SideInterval:  common.MaxHealthCheckInterval,
				CntlrInterval: common.MaxHealthCheckInterval,
			},
			codes.OK,
		},
		{
			"dn over max",
			&pb.HealthCheckConf{DnInterval: over},
			codes.InvalidArgument,
		},
		{
			"cn over max",
			&pb.HealthCheckConf{CnInterval: over},
			codes.InvalidArgument,
		},
		{
			"side over max",
			&pb.HealthCheckConf{SideInterval: over},
			codes.InvalidArgument,
		},
		{
			"cntlr over max",
			&pb.HealthCheckConf{CntlrInterval: over},
			codes.InvalidArgument,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			validateWantCode(t, "validateHealthCheckConf",
				validateHealthCheckConf(item.conf), item.want)
		})
	}
}

// TestValidateDmCloneConf pins the hydration knob pair, which clone (§8.9) and
// migration (§8.11) share one bound table for: threshold [1, 8], batch size
// [1, 4], zero unset in both.
func TestValidateDmCloneConf(t *testing.T) {
	cases := []struct {
		name string
		conf *pb.DmCloneConf
		want codes.Code
	}{
		{"nil", nil, codes.OK},
		{"both unset", &pb.DmCloneConf{}, codes.OK},
		{
			"both at min",
			&pb.DmCloneConf{HydrationThreshold: 1, HydrationBatchSize: 1},
			codes.OK,
		},
		{
			"both at max",
			&pb.DmCloneConf{
				HydrationThreshold: common.MaxCloneThreshold,
				HydrationBatchSize: common.MaxCloneBatchSize,
			},
			codes.OK,
		},
		{
			"threshold over max",
			&pb.DmCloneConf{
				HydrationThreshold: common.MaxCloneThreshold + 1,
			},
			codes.InvalidArgument,
		},
		{
			"batch size over max",
			&pb.DmCloneConf{
				HydrationBatchSize: common.MaxCloneBatchSize + 1,
			},
			codes.InvalidArgument,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			validateWantCode(t, "validateDmCloneConf",
				validateDmCloneConf("dm_clone_conf", item.conf), item.want)
		})
	}
}

// TestValidateListCountClamp pins the last numeric row of §7 — list `count`,
// min 1, max MaxListCnt, default DefaultListCnt. The resolver lives in
// common.go's pageLimit rather than validate.go because it returns the
// resolved limit as well as the refusal, but it is a §7 row and this is the
// §9.2 table that owns it: 0 resolves to 64, 1024 is accepted verbatim and
// 1025 is refused.
func TestValidateListCountClamp(t *testing.T) {
	cases := []struct {
		name  string
		count uint32
		limit int
		want  codes.Code
	}{
		{"zero selects the default", 0, common.DefaultListCnt, codes.OK},
		{"one", 1, 1, codes.OK},
		{"below the default", 10, 10, codes.OK},
		{"at MaxListCnt", common.MaxListCnt, common.MaxListCnt, codes.OK},
		{"one over MaxListCnt", common.MaxListCnt + 1, 0,
			codes.InvalidArgument},
		{"absurd", ^uint32(0), 0, codes.InvalidArgument},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			limit, err := pageLimit(item.count)
			validateWantCode(t, "pageLimit", err, item.want)
			if limit != item.limit {
				t.Errorf("pageLimit(%d) = %d, want %d",
					item.count, limit, item.limit)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Transport configurations
// ---------------------------------------------------------------------------

// TestValidateTrConf pins that all four NvmeTrConf members obey the §7 name
// row, and that an entirely empty message is accepted here: the RPCs that
// require a non-empty one (CreateDiskNode, CreateControllerNode) say so
// themselves through trConfEmpty.
func TestValidateTrConf(t *testing.T) {
	good := func() *pb.NvmeTrConf {
		return &pb.NvmeTrConf{
			TrType:  "tcp",
			AdrFam:  "ipv4",
			TrAddr:  "192.168.0.17",
			TrSvcId: "4420",
		}
	}
	long := strings.Repeat("a", common.MaxStrSize+1)
	withTrType := good()
	withTrType.TrType = "bad type"
	withAdrFam := good()
	withAdrFam.AdrFam = long
	withTrAddr := good()
	withTrAddr.TrAddr = "192.168.0.17 "
	withTrSvcId := good()
	withTrSvcId.TrSvcId = "44 20"
	cases := []struct {
		name string
		conf *pb.NvmeTrConf
		want codes.Code
	}{
		{"nil", nil, codes.OK},
		{"empty is accepted here", &pb.NvmeTrConf{}, codes.OK},
		{"complete", good(), codes.OK},
		{"bad tr_type", withTrType, codes.InvalidArgument},
		{"over-long adr_fam", withAdrFam, codes.InvalidArgument},
		{"bad tr_addr", withTrAddr, codes.InvalidArgument},
		{"bad tr_svc_id", withTrSvcId, codes.InvalidArgument},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			validateWantCode(t, "validateTrConf",
				validateTrConf("nvme_tr_conf", item.conf), item.want)
		})
	}
}

// TestValidateTrConfEmpty pins the predicate CreateDiskNode and
// CreateControllerNode refuse on (§8.2): a message is empty exactly when all
// four members are, so any single member makes it non-empty.
func TestValidateTrConfEmpty(t *testing.T) {
	cases := []struct {
		name string
		conf *pb.NvmeTrConf
		want bool
	}{
		{"nil", nil, true},
		{"empty", &pb.NvmeTrConf{}, true},
		{"tr_type only", &pb.NvmeTrConf{TrType: "tcp"}, false},
		{"adr_fam only", &pb.NvmeTrConf{AdrFam: "ipv4"}, false},
		{"tr_addr only", &pb.NvmeTrConf{TrAddr: "10.0.0.1"}, false},
		{"tr_svc_id only", &pb.NvmeTrConf{TrSvcId: "4420"}, false},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := trConfEmpty(item.conf); got != item.want {
				t.Errorf("trConfEmpty = %v, want %v", got, item.want)
			}
		})
	}
}

// TestValidateTrConfList pins the list form CreateClone and UpdateCloneTrConf
// take: at least one entry, no entry empty, every member a valid §7 name. A
// clone with no source address could never connect, so an empty list is a §7
// violation and not a defaulted field.
func TestValidateTrConfList(t *testing.T) {
	good := &pb.NvmeTrConf{
		TrType:  "tcp",
		AdrFam:  "ipv4",
		TrAddr:  "192.168.0.17",
		TrSvcId: "4420",
	}
	cases := []struct {
		name string
		list []*pb.NvmeTrConf
		want codes.Code
	}{
		{"nil", nil, codes.InvalidArgument},
		{"empty", []*pb.NvmeTrConf{}, codes.InvalidArgument},
		{"one entry", []*pb.NvmeTrConf{good}, codes.OK},
		{"two entries", []*pb.NvmeTrConf{good, good}, codes.OK},
		{
			"an empty entry",
			[]*pb.NvmeTrConf{good, {}},
			codes.InvalidArgument,
		},
		{
			"a nil entry",
			[]*pb.NvmeTrConf{good, nil},
			codes.InvalidArgument,
		},
		{
			"an entry with a bad member",
			[]*pb.NvmeTrConf{good, {TrAddr: "10.0.0.1 "}},
			codes.InvalidArgument,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			validateWantCode(t, "validateTrConfList",
				validateTrConfList("src_tr_conf", item.list), item.want)
		})
	}
}

// ---------------------------------------------------------------------------
// BdevConf: bounds plus the two structural rules
// ---------------------------------------------------------------------------

// TestValidateBdevConf pins every bounded member of a BdevConf and the §7
// structural rule that bdev_feature_list MUST be empty in this version.
//
// low_water_mark_pct is the row with no upper bound: 0 selects
// DefaultPoolLowWatermarkPct and a value above 100 is ACCEPTED and switches
// the §10.4 auto-grow off, so neither may be refused here.
func TestValidateBdevConf(t *testing.T) {
	raid1 := func(cnt uint64) *pb.RedundConf {
		return &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundMdRaid1{
				RedundMdRaid1: &pb.RedundMdRaid1{BitmapChunkBlockCnt: cnt},
			},
		}
	}
	cases := []struct {
		name string
		conf *pb.BdevConf
		want codes.Code
	}{
		{"nil", nil, codes.OK},
		{"empty", &pb.BdevConf{}, codes.OK},
		{
			"bdev_feature_list must be empty",
			&pb.BdevConf{
				BdevFeatureList: []*pb.BdevFeature{{
					BdevFeatureKind: &pb.BdevFeature_BdevFeatureNone{
						BdevFeatureNone: &pb.BdevFeatureNone{},
					},
				}},
			},
			codes.InvalidArgument,
		},
		{
			"an empty-but-present feature entry still counts",
			&pb.BdevConf{BdevFeatureList: []*pb.BdevFeature{{}}},
			codes.InvalidArgument,
		},
		{
			"data_block_size at min",
			&pb.BdevConf{DmPoolConf: &pb.DmPoolConf{
				DataBlockSize: common.MinDmPoolDataBlockSize,
			}},
			codes.OK,
		},
		{
			"data_block_size below min",
			&pb.BdevConf{DmPoolConf: &pb.DmPoolConf{
				DataBlockSize: common.MinDmPoolDataBlockSize - 1,
			}},
			codes.InvalidArgument,
		},
		{
			"data_block_size at max",
			&pb.BdevConf{DmPoolConf: &pb.DmPoolConf{
				DataBlockSize: common.MaxDmPoolDataBlockSize,
			}},
			codes.OK,
		},
		{
			"data_block_size above max",
			&pb.BdevConf{DmPoolConf: &pb.DmPoolConf{
				DataBlockSize: common.MaxDmPoolDataBlockSize + 1,
			}},
			codes.InvalidArgument,
		},
		{
			"low_water_mark_pct 0 selects the default",
			&pb.BdevConf{DmPoolConf: &pb.DmPoolConf{LowWaterMarkPct: 0}},
			codes.OK,
		},
		{
			"low_water_mark_pct 100",
			&pb.BdevConf{DmPoolConf: &pb.DmPoolConf{LowWaterMarkPct: 100}},
			codes.OK,
		},
		{
			"low_water_mark_pct above 100 switches auto-grow off",
			&pb.BdevConf{DmPoolConf: &pb.DmPoolConf{LowWaterMarkPct: 4096}},
			codes.OK,
		},
		{
			"stripe_size at min",
			&pb.BdevConf{DmRaid0Conf: &pb.DmRaid0Conf{
				StripeSize: common.MinDmRaid0StripeSize,
			}},
			codes.OK,
		},
		{
			"stripe_size below min",
			&pb.BdevConf{DmRaid0Conf: &pb.DmRaid0Conf{
				StripeSize: common.MinDmRaid0StripeSize - 1,
			}},
			codes.InvalidArgument,
		},
		{
			"stripe_size at max",
			&pb.BdevConf{DmRaid0Conf: &pb.DmRaid0Conf{
				StripeSize: common.MaxDmRaid0StripeSize,
			}},
			codes.OK,
		},
		{
			"stripe_size above max",
			&pb.BdevConf{DmRaid0Conf: &pb.DmRaid0Conf{
				StripeSize: common.MaxDmRaid0StripeSize + 1,
			}},
			codes.InvalidArgument,
		},
		{
			"redund_none carries nothing to bound",
			&pb.BdevConf{RedundConf: &pb.RedundConf{
				RedunKind: &pb.RedundConf_RedundNone{
					RedundNone: &pb.RedundNone{},
				},
			}},
			codes.OK,
		},
		{
			"an unset redund oneof means redund_none",
			&pb.BdevConf{RedundConf: &pb.RedundConf{}},
			codes.OK,
		},
		{
			"bitmap_chunk_block_cnt unset",
			&pb.BdevConf{RedundConf: raid1(0)},
			codes.OK,
		},
		{
			"bitmap_chunk_block_cnt at min",
			&pb.BdevConf{RedundConf: raid1(common.MinChunkBlockCnt)},
			codes.OK,
		},
		{
			"bitmap_chunk_block_cnt at max",
			&pb.BdevConf{RedundConf: raid1(common.MaxChunkBlockCnt)},
			codes.OK,
		},
		{
			"bitmap_chunk_block_cnt above max",
			&pb.BdevConf{RedundConf: raid1(common.MaxChunkBlockCnt + 1)},
			codes.InvalidArgument,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			validateWantCode(t, "validateBdevConf",
				validateBdevConf(item.conf), item.want)
		})
	}
}

// ---------------------------------------------------------------------------
// EventThreshold: the one cross-field rule of §7
// ---------------------------------------------------------------------------

// TestValidateEventThreshold pins the leg_unhealthy > side_unhealthy rule, and
// pins that it is applied AFTER default resolution — that is the whole point
// of the row. §10.4's leg repair fires on the side threshold when the DN looks
// dead and on the leg threshold when only the cntlr's path is bad, so the leg
// wait is the longer one by construction.
//
// The cases that leave one member unset are the ones that matter: a request
// naming only side_unhealthy = 1200 is refused because leg_unhealthy resolves
// to DefaultLegUnhealthy = 1200 and 1200 is not greater than 1200. A validator
// that compared the raw fields would accept it (0 vs 1200 reads as "unset,
// nothing to compare") and the SP would be stored with an inverted ladder.
func TestValidateEventThreshold(t *testing.T) {
	cases := []struct {
		name      string
		threshold *pb.EventThreshold
		want      codes.Code
	}{
		{"nil resolves to 1200 > 600", nil, codes.OK},
		{"empty resolves to 1200 > 600", &pb.EventThreshold{}, codes.OK},
		{
			"both stated, leg greater",
			&pb.EventThreshold{SideUnhealthy: 600, LegUnhealthy: 601},
			codes.OK,
		},
		{
			"both stated, equal",
			&pb.EventThreshold{SideUnhealthy: 100, LegUnhealthy: 100},
			codes.InvalidArgument,
		},
		{
			"both stated, leg smaller",
			&pb.EventThreshold{SideUnhealthy: 100, LegUnhealthy: 99},
			codes.InvalidArgument,
		},
		{
			"leg unset, side at the leg default",
			&pb.EventThreshold{SideUnhealthy: common.DefaultLegUnhealthy},
			codes.InvalidArgument,
		},
		{
			"leg unset, side above the leg default",
			&pb.EventThreshold{SideUnhealthy: common.DefaultLegUnhealthy + 1},
			codes.InvalidArgument,
		},
		{
			"leg unset, side just below the leg default",
			&pb.EventThreshold{SideUnhealthy: common.DefaultLegUnhealthy - 1},
			codes.OK,
		},
		{
			"side unset, leg below the side default",
			&pb.EventThreshold{LegUnhealthy: common.DefaultSideUnhealthy - 1},
			codes.InvalidArgument,
		},
		{
			"side unset, leg at the side default",
			&pb.EventThreshold{LegUnhealthy: common.DefaultSideUnhealthy},
			codes.InvalidArgument,
		},
		{
			"side unset, leg above the side default",
			&pb.EventThreshold{LegUnhealthy: common.DefaultSideUnhealthy + 1},
			codes.OK,
		},
		{
			"the other two members never take part",
			&pb.EventThreshold{
				PrimaryUnhealthy: 100000,
				CntlrUnhealthy:   1,
				SideUnhealthy:    10,
				LegUnhealthy:     11,
			},
			codes.OK,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			validateWantCode(t, "validateEventThreshold",
				validateEventThreshold(item.threshold), item.want)
		})
	}
}

// TestValidateEventThresholdMessageIsResolved pins that the refusal quotes the
// RESOLVED numbers. A message echoing the request's zeros would tell an
// operator "leg_unhealthy 0 must exceed side_unhealthy 1200", which reads as a
// contradiction and hides which default did the work.
func TestValidateEventThresholdMessageIsResolved(t *testing.T) {
	err := validateEventThreshold(
		&pb.EventThreshold{SideUnhealthy: common.DefaultLegUnhealthy})
	validateWantCode(t, "leg unset, side at the leg default",
		err, codes.InvalidArgument)
	validateWantMsg(t, "resolved leg_unhealthy", err, "1200")
}

// ---------------------------------------------------------------------------
// CreateCluster's write-once conf (§8.1)
// ---------------------------------------------------------------------------

// TestValidateClusterConfInput pins that CreateCluster's single validation
// point really reaches every member it stores. ClusterConf is write-once, so a
// member this function forgets can never be corrected: a bad value would sit
// in etcd for the life of the cluster and be inherited by every SP created
// under it.
func TestValidateClusterConfInput(t *testing.T) {
	cases := []struct {
		name string
		req  *pb.CreateClusterRequest
		want codes.Code
	}{
		{"nil", nil, codes.OK},
		{"empty takes every default", &pb.CreateClusterRequest{}, codes.OK},
		{
			"named",
			&pb.CreateClusterRequest{ClusterName: "cluster-1"},
			codes.OK,
		},
		{
			"bad cluster_name",
			&pb.CreateClusterRequest{ClusterName: "cluster 1"},
			codes.InvalidArgument,
		},
		{
			"bad bdev_conf",
			&pb.CreateClusterRequest{BdevConf: &pb.BdevConf{
				DmRaid0Conf: &pb.DmRaid0Conf{StripeSize: 1},
			}},
			codes.InvalidArgument,
		},
		{
			"bad dn_bin_conf",
			&pb.CreateClusterRequest{
				DnBinConf: &pb.DnBinConf{ExtentSize: 1},
			},
			codes.InvalidArgument,
		},
		{
			"bad alloc_conf",
			&pb.CreateClusterRequest{
				AllocConf: &pb.AllocConf{
					CnBatchSize: common.MaxAllocCnBatchSize + 1,
				},
			},
			codes.InvalidArgument,
		},
		{
			"bad health_check_conf",
			&pb.CreateClusterRequest{
				HealthCheckConf: &pb.HealthCheckConf{
					CntlrInterval: common.MaxHealthCheckInterval + 1,
				},
			},
			codes.InvalidArgument,
		},
		{
			"qos_ratio is deliberately not range-checked",
			&pb.CreateClusterRequest{
				QosRatio: &pb.QosRatio{
					BytesPerIops: ^uint64(0),
					BytesPerBps:  ^uint64(0),
				},
			},
			codes.OK,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			validateWantCode(t, "validateClusterConfInput",
				validateClusterConfInput(item.req), item.want)
		})
	}
}

// ---------------------------------------------------------------------------
// cntlid slots (§8.4, §11.8)
// ---------------------------------------------------------------------------

// TestValidateCntlidSlotList pins the slot-list rules: every value below
// CnCntlidSlotCnt and no duplicates, because §11.8 gives the cntlrs of one SP
// distinct slots and a repeated entry would silently shrink the usable list.
//
// allowEmpty is the one difference between the two callers:
// CreateStoragePool defaults an empty list to [0..7], while
// UpdateStoragePoolCntlidSlotList refuses one — an SP with no slots can
// produce no side.
func TestValidateCntlidSlotList(t *testing.T) {
	all := make([]uint32, 0, common.CnCntlidSlotCnt)
	for slot := uint32(0); slot < common.CnCntlidSlotCnt; slot++ {
		all = append(all, slot)
	}
	cases := []struct {
		name       string
		slots      []uint32
		allowEmpty bool
		want       codes.Code
	}{
		{"nil, empty allowed", nil, true, codes.OK},
		{"empty, empty allowed", []uint32{}, true, codes.OK},
		{"nil, empty refused", nil, false, codes.InvalidArgument},
		{"empty, empty refused", []uint32{}, false, codes.InvalidArgument},
		{"slot 0 alone", []uint32{0}, false, codes.OK},
		{"the whole range", all, false, codes.OK},
		{"out of order", []uint32{5, 1, 7, 0}, false, codes.OK},
		{
			"at CnCntlidSlotCnt",
			[]uint32{common.CnCntlidSlotCnt},
			false,
			codes.InvalidArgument,
		},
		{
			"far above CnCntlidSlotCnt",
			[]uint32{0, 1000},
			false,
			codes.InvalidArgument,
		},
		{"a duplicate", []uint32{0, 1, 0}, false, codes.InvalidArgument},
		{
			"a duplicate is refused even when empty is allowed",
			[]uint32{3, 3},
			true,
			codes.InvalidArgument,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			validateWantCode(t, "validateCntlidSlotList",
				validateCntlidSlotList(item.slots, item.allowEmpty),
				item.want)
		})
	}
}

// ---------------------------------------------------------------------------
// Enums (GW7's "bad enum" row)
// ---------------------------------------------------------------------------

// TestValidateSpLevel pins that only a declared SpLevel is accepted. The
// levels are spaced 16 apart on purpose, so an undeclared number between two
// of them must be refused rather than rounded to a neighbour: a stored
// SP_LEVEL of 40 would make every §11.7 comparison ambiguous.
func TestValidateSpLevel(t *testing.T) {
	declared := []pb.SpLevel{
		pb.SpLevel_SP_LEVEL_READWRITE,
		pb.SpLevel_SP_LEVEL_READONLY,
		pb.SpLevel_SP_LEVEL_NO_CLONE,
		pb.SpLevel_SP_LEVEL_NO_THINPOOL,
		pb.SpLevel_SP_LEVEL_NO_REDUND,
		pb.SpLevel_SP_LEVEL_NO_MIGRATION,
		pb.SpLevel_SP_LEVEL_NO_SIDE,
		pb.SpLevel_SP_LEVEL_DISABLE,
	}
	for _, level := range declared {
		t.Run("declared "+level.String(), func(t *testing.T) {
			validateWantCode(t, "validateSpLevel",
				validateSpLevel(level), codes.OK)
		})
	}
	undeclared := []int32{1, 15, 17, 40, 113, 255, -1, 1 << 20}
	for _, value := range undeclared {
		t.Run("undeclared", func(t *testing.T) {
			validateWantCode(t, "validateSpLevel",
				validateSpLevel(pb.SpLevel(value)), codes.InvalidArgument)
		})
	}
}

// ---------------------------------------------------------------------------
// Node selectors
// ---------------------------------------------------------------------------

// TestValidateNodeSelector pins that both address lists of a NodeSelector go
// through the REQUIRED name rule: an entry is an addr_port and an empty one
// names no node at all, so it is refused rather than skipped.
func TestValidateNodeSelector(t *testing.T) {
	cases := []struct {
		name     string
		selector *pb.NodeSelector
		want     codes.Code
	}{
		{"nil", nil, codes.OK},
		{"empty", &pb.NodeSelector{}, codes.OK},
		{
			"both lists populated",
			&pb.NodeSelector{
				BlackList: []string{"10.0.0.1:9000", "10.0.0.2:9000"},
				WhiteList: []string{"10.0.0.3:9000"},
			},
			codes.OK,
		},
		{
			"an empty black_list entry",
			&pb.NodeSelector{BlackList: []string{""}},
			codes.InvalidArgument,
		},
		{
			"a black_list entry with a bad character",
			&pb.NodeSelector{BlackList: []string{"10.0.0.1 :9000"}},
			codes.InvalidArgument,
		},
		{
			"an over-long white_list entry",
			&pb.NodeSelector{
				WhiteList: []string{strings.Repeat("a", common.MaxStrSize+1)},
			},
			codes.InvalidArgument,
		},
		{
			"a good black_list does not excuse a bad white_list",
			&pb.NodeSelector{
				BlackList: []string{"10.0.0.1:9000"},
				WhiteList: []string{"bad entry"},
			},
			codes.InvalidArgument,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			validateWantCode(t, "validateNodeSelector",
				validateNodeSelector("dn_selector", item.selector), item.want)
		})
	}
}

// ---------------------------------------------------------------------------
// Namespace identity (§8.8)
// ---------------------------------------------------------------------------

// TestValidateDevIdentity pins the two identity shapes CreateNamespace accepts
// from a caller: the canonical dashed RFC 4122 uuid it would otherwise
// generate, and an NGUID as 16 bytes of hex. Either being empty means "you
// generate it", so the empty string is never a violation — but a supplied
// value the host would parse differently is, because the identity is what a
// multipath host uses to decide two paths are one namespace.
func TestValidateDevIdentity(t *testing.T) {
	const goodUuid = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	const goodNguid = "0123456789abcdef0123456789ABCDEF"
	cases := []struct {
		name  string
		uuid  string
		nguid string
		want  codes.Code
	}{
		{"both empty, both generated", "", "", codes.OK},
		{"both supplied", goodUuid, goodNguid, codes.OK},
		{"uuid only", goodUuid, "", codes.OK},
		{"nguid only", "", goodNguid, codes.OK},
		{"upper-case uuid", strings.ToUpper(goodUuid), "", codes.OK},
		{
			"uuid without dashes",
			strings.ReplaceAll(goodUuid, "-", ""),
			"",
			codes.InvalidArgument,
		},
		{"uuid too short", goodUuid[:len(goodUuid)-1], "",
			codes.InvalidArgument},
		{"uuid too long", goodUuid + "0", "", codes.InvalidArgument},
		{
			"uuid with a non-hex digit",
			"3f2504e0-4f89-41d3-9a0c-0305e82c330g",
			"",
			codes.InvalidArgument,
		},
		{
			"uuid in braces",
			"{" + goodUuid + "}",
			"",
			codes.InvalidArgument,
		},
		{
			"uuid with the dashes misplaced",
			"3f2504e04-f89-41d3-9a0c-0305e82c3301",
			"",
			codes.InvalidArgument,
		},
		{"nguid 31 hex", "", goodNguid[:31], codes.InvalidArgument},
		{"nguid 33 hex", "", goodNguid + "0", codes.InvalidArgument},
		{
			"nguid with a non-hex digit",
			"",
			"0123456789abcdef0123456789abcdeg",
			codes.InvalidArgument,
		},
		{
			"nguid in the dashed uuid form",
			"",
			goodUuid,
			codes.InvalidArgument,
		},
		{
			"a good uuid does not excuse a bad nguid",
			goodUuid,
			"xyz",
			codes.InvalidArgument,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			validateWantCode(t, "validateDevIdentity",
				validateDevIdentity(item.uuid, item.nguid), item.want)
		})
	}
}

// ---------------------------------------------------------------------------
// Clone source geometry (§8.9, §11.4)
// ---------------------------------------------------------------------------

// TestValidateCloneGeometry pins the three source-geometry bounds of
// CreateClone and the divisibility rule between two of them. The numbers
// describe the SOURCE SP, which this cluster cannot read, so §7 is the only
// place they are ever checked: src_slice_cnt ∈ [1, MaxSliceCntPerSp],
// src_stripe_size = i x 4 KiB with i ∈ [1, 256], src_block_size = j x 64 KiB
// with j ∈ [1, 16384], and src_block_size a multiple of src_stripe_size —
// without which a clone region would straddle a raid0 stripe boundary and the
// per-slice bitmaps of §8.13 would address the wrong bytes.
//
// Every case states all three numbers so that a bound and the divisibility
// rule can never be confused for one another: the only member under test is
// the one the case name mentions, the other two are always legal.
func TestValidateCloneGeometry(t *testing.T) {
	const kib4 = uint64(4 * 1024)
	const kib64 = uint64(64 * 1024)
	cases := []struct {
		name       string
		sliceCnt   uint32
		stripeSize uint64
		blockSize  uint64
		want       codes.Code
	}{
		{"the smallest legal geometry", 1, kib4, kib64, codes.OK},
		{
			"the largest legal geometry",
			common.MaxSliceCntPerSp, 256 * kib4, 16384 * kib64, codes.OK,
		},
		{"src_slice_cnt 0", 0, kib4, kib64, codes.InvalidArgument},
		{
			"src_slice_cnt above MaxSliceCntPerSp",
			common.MaxSliceCntPerSp + 1, kib4, kib64, codes.InvalidArgument,
		},
		{"src_stripe_size 0", 1, 0, kib64, codes.InvalidArgument},
		{
			"src_stripe_size not a multiple of 4 KiB",
			1, kib4 + 1, kib64, codes.InvalidArgument,
		},
		{
			"src_stripe_size at 256 x 4 KiB",
			1, 256 * kib4, 256 * kib4, codes.OK,
		},
		{
			"src_stripe_size at 257 x 4 KiB",
			1, 257 * kib4, 16384 * kib64, codes.InvalidArgument,
		},
		{"src_block_size 0", 1, kib4, 0, codes.InvalidArgument},
		{
			"src_block_size not a multiple of 64 KiB",
			1, kib4, kib64 + kib4, codes.InvalidArgument,
		},
		{
			"src_block_size at 16385 x 64 KiB",
			1, kib4, 16385 * kib64, codes.InvalidArgument,
		},
		{
			"src_block_size not a multiple of src_stripe_size",
			1, 3 * kib4, kib64, codes.InvalidArgument,
		},
		{
			"src_block_size equal to src_stripe_size",
			1, 16 * kib4, kib64, codes.OK,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			validateWantCode(t, "validateCloneGeometry",
				validateCloneGeometry(
					item.sliceCnt, item.stripeSize, item.blockSize),
				item.want)
		})
	}
}

// ---------------------------------------------------------------------------
// Bitmaps (§8.9, §8.11)
// ---------------------------------------------------------------------------

// TestValidateBitmap pins the one thing the gateway ever asserts about a
// bitmap. GW14 makes the payload opaque — Append*Bitmap stores the bytes
// verbatim and never inspects a bit — so the only §7 rule left is that an
// append must actually carry something.
func TestValidateBitmap(t *testing.T) {
	cases := []struct {
		name   string
		bitmap []byte
		want   codes.Code
	}{
		{"nil", nil, codes.InvalidArgument},
		{"empty", []byte{}, codes.InvalidArgument},
		{"one zero byte is still a bitmap", []byte{0}, codes.OK},
		{"arbitrary bytes", []byte{0xff, 0x00, 0x7f}, codes.OK},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			validateWantCode(t, "validateBitmap",
				validateBitmap(item.bitmap), item.want)
		})
	}
}

// ---------------------------------------------------------------------------
// GrowSlice exclusivity (§8.5)
// ---------------------------------------------------------------------------

// TestValidateGrowExclusivity pins the §8.5 rule that decides which of the two
// GrowSlice signals a request may carry. A meta grow takes its size from the
// meta ladder, so an ext_cnt alongside is_meta would be silently ignored and
// the caller would believe it had asked for something it did not get; a data
// grow has no ladder to fall back on, so a zero ext_cnt names no size at all.
// Both are refused rather than defaulted.
func TestValidateGrowExclusivity(t *testing.T) {
	cases := []struct {
		name   string
		isMeta bool
		extCnt uint64
		want   codes.Code
	}{
		{"meta grow with no ext_cnt", true, 0, codes.OK},
		{"meta grow with an ext_cnt", true, 1, codes.InvalidArgument},
		{"data grow with an ext_cnt", false, 4, codes.OK},
		{"data grow with no ext_cnt", false, 0, codes.InvalidArgument},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			validateWantCode(t, "validateGrowExclusivity",
				validateGrowExclusivity(item.isMeta, item.extCnt), item.want)
		})
	}
}
