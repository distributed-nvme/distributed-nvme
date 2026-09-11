package gateway

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Error mapping (GW7)
// ---------------------------------------------------------------------------
//
// GW7 is the single error table of the component, and these are its only
// constructors. Handlers return them straight out of their STM closure:
// etcdutil.RunSTM hands an error returned by the callback back to the caller
// unchanged and without committing (EU4), so a status error raised deep inside
// a transaction reaches the client exactly as written and nothing is written
// to etcd.

// errInvalid is a §7 violation, a malformed page_token or a bad enum/oneof.
func errInvalid(format string, args ...any) error {
	return status.Errorf(codes.InvalidArgument, format, args...)
}

// errNotFound is an absent cluster, SP, or named / id-addressed object.
func errNotFound(format string, args ...any) error {
	return status.Errorf(codes.NotFound, format, args...)
}

// errExists is a create whose name key (or, for CreateCluster, a global) is
// already there.
func errExists(format string, args ...any) error {
	return status.Errorf(codes.AlreadyExists, format, args...)
}

// errPrecondition is a documented public precondition that does not hold.
func errPrecondition(format string, args ...any) error {
	return status.Errorf(codes.FailedPrecondition, format, args...)
}

// errExhausted is a cardinality ceiling: the Max*CntPerCluster / Max*CntPerSp
// gates, "too few candidates" (§6.5) and the Append*Bitmap count caps. GW7's
// dividing line: RESOURCE_EXHAUSTED is capacity or quota that could be freed
// or extended, FAILED_PRECONDITION the object's own state forbidding the
// operation — which is why the meta ladder cap left this list for
// errPrecondition.
func errExhausted(format string, args ...any) error {
	return status.Errorf(codes.ResourceExhausted, format, args...)
}

// errAborted is the §5.9 catch-all: STM-client, conflict-budget, etcd and
// proto errors, a missing invariant key, and the agent gRPC failures the RPC
// specs map here.
func errAborted(format string, args ...any) error {
	return status.Errorf(codes.Aborted, format, args...)
}

// msgStaleRevision is the one sentence a token mismatch ever produces. The
// integration suite matches on it, so it is a constant (§0 #7, GW6).
const msgStaleRevision = "stale revision"

// errStale is the ABORTED of GW6: the request token — including the 0 a nil
// token reads as — does not equal the stored revision.
func errStale() error {
	return status.Error(codes.Aborted, msgStaleRevision)
}

// isStatusErr reports whether err already carries a gRPC status, i.e. whether
// it is one of the constructors above rather than an etcd/proto failure.
func isStatusErr(err error) bool {
	var grpcErr interface{ GRPCStatus() *status.Status }
	return errors.As(err, &grpcErr)
}

// mapStmErr is what every handler wraps its RunSTM / Snapshot call in. A
// status error the closure raised passes through untouched; everything else —
// the STM client, the conflict budget, etcd itself, a (de)serialization
// failure — is §5.9's ABORTED.
func mapStmErr(err error) error {
	if err == nil {
		return nil
	}
	if isStatusErr(err) {
		return err
	}
	return errAborted("%v", err)
}

// mapModelErr maps an error returned by one of the three model ops the
// gateway shares with the worker — GrowSlice, CreateSpareLeg, SwitchSpareLeg,
// each of which runs its own STM — onto GW7.
//
// ReasonStaleRevision is the token check the gateway asked the op to run and
// is ABORTED; ReasonCandidateChanged never reaches a client (GW9: the caller
// re-runs the whole candidate unit) and is reported to the caller of this
// helper through isCandidateChanged instead; every other ErrPrecondition is a
// documented public precondition and therefore FAILED_PRECONDITION.
func mapModelErr(err error) error {
	if err == nil {
		return nil
	}
	var pre *model.ErrPrecondition
	if errors.As(err, &pre) {
		switch pre.Reason {
		case model.ReasonStaleRevision:
			return errStale()
		case model.ReasonCandidateChanged:
			return errCandidateChanged
		}
		return errPrecondition("%s", pre.Error())
	}
	if errors.Is(err, model.ErrNotFound) {
		return errNotFound("%v", err)
	}
	return mapStmErr(err)
}

// ---------------------------------------------------------------------------
// The candidate unit (GW9)
// ---------------------------------------------------------------------------

// errCandidateChanged is the sentinel an allocating STM closure returns when
// one pick's exact capacity key — the key the scan outside the transaction saw
// — is no longer there. It is never surfaced to a client (§0 #8).
var errCandidateChanged = errors.New("gateway: candidate changed")

// isCandidateChanged reports whether err is that sentinel.
func isCandidateChanged(err error) bool {
	return errors.Is(err, errCandidateChanged)
}

// candidateUnit runs one "scan candidates outside the STM, commit inside it"
// round over and over until the round commits, fails for any other reason, or
// the request context ends (GW9).
//
// The retry covers the WHOLE unit — the scan included — because a pick whose
// capacity key moved is stale information, not a transient write conflict:
// re-running only the transaction would re-validate the same dead pick for
// ever. etcdutil's own conflict retry is a different layer and is untouched.
func candidateUnit(ctx context.Context, unit func() error) error {
	for {
		err := unit()
		if !isCandidateChanged(err) {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errAborted("candidate allocation did not settle: %v", ctxErr)
		}
	}
}

// ---------------------------------------------------------------------------
// Resolution (GW5)
// ---------------------------------------------------------------------------

// clusterNameOf applies the §8 preamble default that every RPC carrying a
// cluster_name shares.
func clusterNameOf(name string) string {
	if name == "" {
		return common.DefaultClusterName
	}
	return name
}

// resolveCluster is the first read of every STM but CreateCluster's and
// ListClusters' (GW5, §5.8): ClusterConf is the only name-keyed message, and
// cluster_id = fnv64a(name ‖ creation_epoch) is the prefix of every other key
// the RPC will touch. Reading it INSIDE the transaction is what makes an RPC
// fail correctly when the cluster is concurrently deleted or recreated.
func resolveCluster(
	s etcdutil.STM,
	clusterName string,
) (uint64, *pb.ClusterConf, error) {
	name := clusterNameOf(clusterName)
	cc := &pb.ClusterConf{}
	if !s.Get(model.ClusterConfKey(name), cc) {
		return 0, nil, errNotFound("cluster %q not found", name)
	}
	return model.ClusterId(name, cc.GetCreationEpoch()), cc, nil
}

// resolveSp reads the SP an SP-scoped RPC names (GW5).
//
// rejectDeleting is true for every mutator except DeleteStoragePool: an SP
// whose teardown has begun accepts no further changes (§8 preamble). Nothing
// in v1 ever sets the flag, so the branch is exercised by the unit tests only
// (§10.18).
func resolveSp(
	s etcdutil.STM,
	cid uint64,
	spName string,
	rejectDeleting bool,
) (*pb.SpConf, error) {
	conf := &pb.SpConf{}
	if !s.Get(model.SpConfKey(cid, spName), conf) {
		return nil, errNotFound("storage pool %q not found", spName)
	}
	if rejectDeleting && conf.GetDeleting() {
		return nil, errPrecondition("storage pool %q is being deleted", spName)
	}
	return conf, nil
}

// spScope is what an SP-scoped handler resolves once and then carries: the
// cluster it lives in, the SP record, and — for a mutator — the SpRev whose
// token has already been checked.
type spScope struct {
	Cid  uint64
	Cc   *pb.ClusterConf
	Conf *pb.SpConf
	Rev  *pb.SpRev
}

// SpId is the resolved SP's id.
func (sc *spScope) SpId() uint64 { return sc.Conf.GetSpId() }

// Shard is the resolved SP's shard code, which addresses its SpRev and its
// CdcEntry keys.
func (sc *spScope) Shard() uint32 { return sc.Conf.GetShardCode() }

// openSp is the resolve-then-check-the-token opening of every SP-scoped
// mutator (GW5 + GW6, in that order). token is
// req.GetSpRev().GetRevision(), so a nil token message reads as 0 and can
// never match a stored revision that starts at 1 (§0 #7).
func openSp(
	s etcdutil.STM,
	clusterName string,
	spName string,
	token uint64,
) (*spScope, error) {
	return openSpFlags(s, clusterName, spName, token, true)
}

// openSpRead is openSp without a token: the opening of every SP-scoped
// read-only RPC.
func openSpRead(
	s etcdutil.STM,
	clusterName string,
	spName string,
) (*spScope, error) {
	cid, cc, err := resolveCluster(s, clusterName)
	if err != nil {
		return nil, err
	}
	conf, err := resolveSp(s, cid, spName, false)
	if err != nil {
		return nil, err
	}
	return &spScope{Cid: cid, Cc: cc, Conf: conf}, nil
}

// openSpFlags is openSp with the `deleting` gate made explicit, for
// DeleteStoragePool — the one mutator that must proceed on a deleting SP.
func openSpFlags(
	s etcdutil.STM,
	clusterName string,
	spName string,
	token uint64,
	rejectDeleting bool,
) (*spScope, error) {
	cid, cc, err := resolveCluster(s, clusterName)
	if err != nil {
		return nil, err
	}
	conf, err := resolveSp(s, cid, spName, rejectDeleting)
	if err != nil {
		return nil, err
	}
	rev, err := checkSpToken(s, cid, conf, token)
	if err != nil {
		return nil, err
	}
	return &spScope{Cid: cid, Cc: cc, Conf: conf, Rev: rev}, nil
}

// ---------------------------------------------------------------------------
// Token checks and revision bumps (GW6, §5.5)
// ---------------------------------------------------------------------------
//
// The token check runs immediately after resolution and before any other state
// check, so a stale client always sees ABORTED "stale revision" and never a
// precondition error computed against state it has not read. The
// addr_port / sp_name a client echoes back inside the token message is
// ignored: only `revision` participates.

// checkSpToken asserts the stored SpRev.revision equals want (GW6).
func checkSpToken(
	s etcdutil.STM,
	cid uint64,
	conf *pb.SpConf,
	want uint64,
) (*pb.SpRev, error) {
	key := model.SpRevKey(conf.GetShardCode(), cid, conf.GetSpId())
	rev := &pb.SpRev{}
	if !s.Get(key, rev) {
		return nil, errAborted("sp_rev key %q is missing", key)
	}
	if rev.GetRevision() != want {
		return nil, errStale()
	}
	return rev, nil
}

// checkDnToken asserts the stored DnRev.revision equals want (GW6).
func checkDnToken(
	s etcdutil.STM,
	cid uint64,
	dn *pb.DnConf,
	want uint64,
) (*pb.DnRev, error) {
	key := model.DnRevKey(dn.GetShardCode(), cid, dn.GetDnId())
	rev := &pb.DnRev{}
	if !s.Get(key, rev) {
		return nil, errAborted("dn_rev key %q is missing", key)
	}
	if rev.GetRevision() != want {
		return nil, errStale()
	}
	return rev, nil
}

// checkCnToken asserts the stored CnRev.revision equals want (GW6).
func checkCnToken(
	s etcdutil.STM,
	cid uint64,
	cn *pb.CnConf,
	want uint64,
) (*pb.CnRev, error) {
	key := model.CnRevKey(cn.GetShardCode(), cid, cn.GetCnId())
	rev := &pb.CnRev{}
	if !s.Get(key, rev) {
		return nil, errAborted("cn_rev key %q is missing", key)
	}
	if rev.GetRevision() != want {
		return nil, errStale()
	}
	return rev, nil
}

// bumpSp bumps the SP's revision once (§5.5). The key has already been read
// and matched by GW6's token check, so a failure here can only mean the key
// vanished inside the same transaction, which is §5.9's ABORTED.
func bumpSp(s etcdutil.STM, op string, sc *spScope) error {
	err := model.BumpSpRev(s, op, sc.Shard(), sc.Cid, sc.SpId())
	if err != nil {
		return errAborted("%v", err)
	}
	return nil
}

// bumpDn bumps one DN's revision once (§5.5).
func bumpDn(s etcdutil.STM, op string, cid uint64, dn *pb.DnConf) error {
	if err := model.BumpDnRev(s, op, cid, dn); err != nil {
		return errAborted("%v", err)
	}
	return nil
}

// bumpCn bumps one CN's revision once (§5.5).
func bumpCn(s etcdutil.STM, op string, cid uint64, cn *pb.CnConf) error {
	if err := model.BumpCnRev(s, op, cid, cn); err != nil {
		return errAborted("%v", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Cluster-scoped id minting (GW12, §5.4)
// ---------------------------------------------------------------------------

// mint is what one draw from a DnGlobal / CnGlobal / SpGlobal yields. The
// three messages have identical shapes but distinct Go types, so the rule
// lives here once and each caller writes the result back into its own message.
type mint struct {
	Id     uint64
	Shard  uint32
	NextId uint64
	Bucket []uint32
}

// mintClusterId allocates one id and one shard code (GW12, §5.4): the id is
// next_id (never reused), the shard code is the index of the smallest bucket
// value — the first index on ties — and that bucket is incremented.
//
// sum(bucket) before the increment is the cluster's live object count, which
// is why the Max*CntPerCluster gate needs no range query. A bucket of the
// wrong length — a global written before ShardBucketSize was what it is, or a
// hand-edited one — is normalized rather than trusted, so a shard code is
// always inside [0, ShardBucketSize).
func mintClusterId(
	nextId uint64,
	bucket []uint32,
	maxCnt int,
	what string,
) (mint, error) {
	sized := make([]uint32, common.ShardBucketSize)
	copy(sized, bucket)
	total := 0
	best := 0
	for idx, value := range sized {
		total += int(value)
		if value < sized[best] {
			best = idx
		}
	}
	if total >= maxCnt {
		return mint{}, errExhausted(
			"%s count %d has reached the per-cluster maximum %d",
			what, total, maxCnt,
		)
	}
	if nextId == 0 {
		// next_id "starts at 1" (§5.4); a proto3 zero is a global written
		// without it and must never mint the 0 that every id-valued result
		// reserves for "none".
		nextId = 1
	}
	sized[best]++
	return mint{
		Id:     nextId,
		Shard:  uint32(best),
		NextId: nextId + 1,
		Bucket: sized,
	}, nil
}

// releaseShard is the deletion half of GW12: the object's bucket is
// decremented and its id is never reused. A zero bucket is left alone rather
// than wrapped around.
func releaseShard(bucket []uint32, shard uint32) []uint32 {
	sized := make([]uint32, common.ShardBucketSize)
	copy(sized, bucket)
	if int(shard) < len(sized) && sized[shard] > 0 {
		sized[shard]--
	}
	return sized
}

// bucketSum is the live object count a global's shard_bucket encodes (§5.4).
func bucketSum(bucket []uint32) uint64 {
	total := uint64(0)
	for _, value := range bucket {
		total += uint64(value)
	}
	return total
}

// zeroBucket is the shard_bucket a fresh global carries: ShardBucketSize
// zeros (§5.4).
func zeroBucket() []uint32 {
	return make([]uint32, common.ShardBucketSize)
}

// ---------------------------------------------------------------------------
// Per-SP id minting (GW12, §5.4)
// ---------------------------------------------------------------------------

// spIdMinter hands out the per-SP sub-object ids of one transaction from the
// SpConf being written. Every id an SP owns — cntlr_id, slice_id, grp_id,
// leg_id, side_id, ss_id, ns_id, td_id, clone_id, xfer_id, migr_id — comes
// from the single next_id counter, and the counter is written back exactly
// once at the end of the STM.
//
// A minter is created inside the STM closure and thrown away with it, so a
// retried attempt re-reads next_id and mints exactly the same ids.
type spIdMinter struct {
	next uint64
}

// newSpIdMinter starts minting at the SP's next_id, clamped past the reserved
// 0 exactly as model.SpNextId does.
func newSpIdMinter(conf *pb.SpConf) *spIdMinter {
	return &spIdMinter{next: model.SpNextId(conf)}
}

// mint returns the next per-SP id.
func (m *spIdMinter) mint() uint64 {
	id := m.next
	m.next++
	return id
}

// commit writes the advanced counter back into the SpConf being stored.
func (m *spIdMinter) commit(conf *pb.SpConf) {
	conf.NextId = m.next
}

// nextDevId draws one thin-device dev_id from SpConf.next_dev_id (§5.4:
// starts at 1, never reused, 0 is the "no origin" sentinel of ori_id).
func nextDevId(conf *pb.SpConf) uint32 {
	devId := conf.GetNextDevId()
	if devId == 0 {
		devId = 1
	}
	conf.NextDevId = devId + 1
	return devId
}

// ---------------------------------------------------------------------------
// Pagination (GW10, §5.7)
// ---------------------------------------------------------------------------

// pageLimit resolves and validates a list `count` (§7): 0 selects
// DefaultListCnt, anything above MaxListCnt is refused.
func pageLimit(count uint32) (int, error) {
	if count == 0 {
		return common.DefaultListCnt, nil
	}
	if count > common.MaxListCnt {
		return 0, errInvalid(
			"count %d exceeds the maximum %d", count, common.MaxListCnt)
	}
	return int(count), nil
}

// decodePageToken turns a request token into the last key of the previous
// page (§5.7). An empty token is the start of the prefix; a token that is not
// valid base64 is INVALID_ARGUMENT.
func decodePageToken(token string) (string, error) {
	if token == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return "", errInvalid("malformed page_token %q", token)
	}
	return string(raw), nil
}

// encodePageToken is the token that continues a page after lastKey (§5.7).
func encodePageToken(lastKey string) string {
	return base64.StdEncoding.EncodeToString([]byte(lastKey))
}

// validatePageArgs is the pure half of GW10, hoisted so a list RPC can run it
// BEFORE the ClusterConf read its prefix needs (GW4: "Violations ⇒
// INVALID_ARGUMENT before any etcd read").
//
// Without it a bad `count` or `page_token` against a cluster that does not
// exist comes back as NOT_FOUND — the read having already happened — and the
// caller never learns which of the two was actually wrong. pageNames
// re-derives both values, so this is a check and not a hand-off.
func validatePageArgs(count uint32, token string) error {
	if _, err := pageLimit(count); err != nil {
		return err
	}
	_, err := decodePageToken(token)
	return err
}

// pageNames lists one page of the names under prefix (GW10).
//
// The range itself is not an STM read: §5.7 says the list RPCs never use one,
// and a transaction cannot range at all. It is a keys-only scan of the whole
// prefix, cut in Go at the token and the limit — every prefix a list RPC pages
// is bounded by a Max*CntPerCluster (1024 DNs, 1024 CNs, 4096 SPs), so a
// keys-only scan of one is cheap, and etcdutil deliberately exposes no
// "range from key, limited" primitive to build a server-side cut from.
//
// The returned names are the key suffixes after the prefix; the next token is
// empty exactly when the page was not full, which is what tells a client it
// has reached the end.
func pageNames(
	ctx context.Context,
	cli *etcdutil.Client,
	prefix string,
	count uint32,
	token string,
) ([]string, string, error) {
	limit, err := pageLimit(count)
	if err != nil {
		return nil, "", err
	}
	after, err := decodePageToken(token)
	if err != nil {
		return nil, "", err
	}
	keys, _, err := cli.RangeKeys(ctx, prefix)
	if err != nil {
		return nil, "", errAborted("%v", err)
	}
	// RangeKeys sorts ascending, so one linear pass is the page.
	names := make([]string, 0, limit)
	lastKey := ""
	for _, entry := range keys {
		if after != "" && entry.Key <= after {
			continue
		}
		if !strings.HasPrefix(entry.Key, prefix) {
			continue
		}
		names = append(names, strings.TrimPrefix(entry.Key, prefix))
		lastKey = entry.Key
		if len(names) >= limit {
			break
		}
	}
	if len(names) < limit {
		return names, "", nil
	}
	return names, encodePageToken(lastKey), nil
}

// ---------------------------------------------------------------------------
// Agent calls (AG2)
// ---------------------------------------------------------------------------

// withAgentConn dials one agent, runs f against it and closes the connection
// (AG2, §0 #5): dial per call, no cache in v1.
//
// Both interceptor chains are mandatory on every dnv connection (grpc.md §4);
// they are what forwards the request's trace id to the agent (T3), which is
// how the integration suite ties a script stage to an agent log record. The
// whole dial+call is bounded by common.DefaultGatewayAgentTimeout, so a hung
// agent bounds an RPC exactly as a hung etcd does.
//
// grpc.NewClient does not block, so a dead endpoint surfaces as f's error,
// never here.
func withAgentConn(
	ctx context.Context,
	addrPort string,
	f func(ctx context.Context, conn *grpc.ClientConn) error,
) error {
	if addrPort == "" {
		return errAborted("agent call: empty addr_port")
	}
	callCtx, cancel := context.WithTimeout(
		ctx, common.DefaultGatewayAgentTimeout*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(
		addrPort,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(common.GrpcUnaryClientInterceptor()),
		grpc.WithChainStreamInterceptor(common.GrpcStreamClientInterceptor()),
	)
	if err != nil {
		return fmt.Errorf("grpc client %s: %w", addrPort, err)
	}
	defer conn.Close()
	return f(callCtx, conn)
}

// withDnAgent is withAgentConn with the DiskNodeAgent stub built.
func withDnAgent(
	ctx context.Context,
	addrPort string,
	f func(ctx context.Context, client pb.DiskNodeAgentClient) error,
) error {
	return withAgentConn(ctx, addrPort,
		func(ctx context.Context, conn *grpc.ClientConn) error {
			return f(ctx, pb.NewDiskNodeAgentClient(conn))
		})
}

// withCnAgent is withAgentConn with the ControllerNodeAgent stub built.
func withCnAgent(
	ctx context.Context,
	addrPort string,
	f func(ctx context.Context, client pb.ControllerNodeAgentClient) error,
) error {
	return withAgentConn(ctx, addrPort,
		func(ctx context.Context, conn *grpc.ClientConn) error {
			return f(ctx, pb.NewControllerNodeAgentClient(conn))
		})
}

// ---------------------------------------------------------------------------
// Small shared shapes
// ---------------------------------------------------------------------------

// containsName reports whether list already holds name. Every list it guards
// is bounded by a §2.1 cardinality limit, so a linear scan is the right shape.
func containsName(list []string, name string) bool {
	for _, item := range list {
		if item == name {
			return true
		}
	}
	return false
}

// removeName drops one entry from a name list, preserving the order of the
// rest.
func removeName(list []string, name string) []string {
	kept := make([]string, 0, len(list))
	for _, item := range list {
		if item == name {
			continue
		}
		kept = append(kept, item)
	}
	return kept
}

// containsId reports whether list already holds id.
func containsId(list []uint64, id uint64) bool {
	for _, item := range list {
		if item == id {
			return true
		}
	}
	return false
}

// removeId drops one id from a list, preserving the order of the rest.
func removeId(list []uint64, id uint64) []uint64 {
	kept := make([]uint64, 0, len(list))
	for _, item := range list {
		if item == id {
			continue
		}
		kept = append(kept, item)
	}
	return kept
}

// trConfEqual is the identity two NvmeTrConf values are compared by
// everywhere a transport list is maintained: all four members.
func trConfEqual(a *pb.NvmeTrConf, b *pb.NvmeTrConf) bool {
	return a.GetTrType() == b.GetTrType() &&
		a.GetAdrFam() == b.GetAdrFam() &&
		a.GetTrAddr() == b.GetTrAddr() &&
		a.GetTrSvcId() == b.GetTrSvcId()
}

// ---------------------------------------------------------------------------
// Shared SP walks (used by more than one handler file)
// ---------------------------------------------------------------------------

// primaryCntlr is the SP's primary cntlr and its id, or ok = false when the
// SP has none. Every RPC that must reach "the" cntlr of an SP — InspectCntlr's
// neighbours, DeleteClone's hydration check, both bitmap reads — goes through
// the primary, because it is the one that builds §3.3 and therefore the one
// that knows about thin volumes, clones and legs.
func primaryCntlr(
	conf *pb.SpConf,
	cntlrs []*pb.Cntlr,
) (uint64, *pb.Cntlr, bool) {
	for idx, cntlr := range cntlrs {
		if cntlr.GetPrimary() {
			return conf.GetCntlrIdList()[idx], cntlr, true
		}
	}
	return 0, nil, false
}

// enabledCntlrTrConfs is the nvme_tr_conf of every ENABLED cntlr's CN, in
// cntlr_id_list order: exactly the `nvme_tr_conf_list` a CdcEntry advertises
// (§8.8). A disabled cntlr is not advertised — its namespaces are ANA
// inaccessible — which is why UpdateCntlrEnabled maintains the list too.
func enabledCntlrTrConfs(cntlrs []*pb.Cntlr) []*pb.NvmeTrConf {
	var list []*pb.NvmeTrConf
	for _, cntlr := range cntlrs {
		if cntlr.GetDisabled() {
			continue
		}
		list = append(list, cntlr.GetNvmeTrConf())
	}
	return list
}

// findNs locates one namespace of a subsystem by its ns_idx — the NVMe NSID,
// which is what every namespace RPC addresses it by.
func findNs(subsystem *pb.Subsystem, nsIdx uint32) *pb.Namespace {
	for _, ns := range subsystem.GetNsList() {
		if ns.GetNsIdx() == nsIdx {
			return ns
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// CdcEntry maintenance (§8.6, §8.8)
// ---------------------------------------------------------------------------
//
// One CdcEntry exists per Subsystem of the SP and advertises the transport
// address of every enabled cntlr's CN. Three RPCs move an address in or out of
// all of them at once — CreateCntlr, DeleteCntlr and UpdateCntlrEnabled — and
// they all go through these two helpers so the entries can never disagree
// about what is advertised.

// eachCdcEntry applies f to the CdcEntry of every subsystem of the SP and
// writes back whatever f changed.
//
// A listed Subsystem that is missing aborts the RPC: its CdcEntry key cannot
// be formed, so the entry would keep advertising a controller that no longer
// exists, and an RPC never writes half of what it owes. A Subsystem whose
// CdcEntry has not been written is skipped: there is nothing to rewrite, and
// inventing one here would guess at fields only CreateSubsystem knows.
func eachCdcEntry(
	stm etcdutil.STM,
	sc *spScope,
	f func(entry *pb.CdcEntry),
) error {
	for _, nqn := range sc.Conf.GetNqnList() {
		subsystem := &pb.Subsystem{}
		ssKey := model.SubsystemKey(sc.Cid, sc.SpId(), nqn)
		if !stm.Get(ssKey, subsystem) {
			return errAborted("subsystem key %q is missing", ssKey)
		}
		key := model.CdcEntryKey(
			sc.Cid, sc.Shard(), sc.SpId(), subsystem.GetSsId())
		entry := &pb.CdcEntry{}
		if !stm.Get(key, entry) {
			continue
		}
		f(entry)
		stm.Put(key, entry)
	}
	return nil
}

// addCdcTrConf appends one transport address to every CdcEntry of the SP,
// skipping the entries that already advertise it so the operation is
// idempotent.
func addCdcTrConf(
	stm etcdutil.STM,
	sc *spScope,
	tr *pb.NvmeTrConf,
) error {
	return eachCdcEntry(stm, sc, func(entry *pb.CdcEntry) {
		for _, item := range entry.GetNvmeTrConfList() {
			if trConfEqual(item, tr) {
				return
			}
		}
		entry.NvmeTrConfList = append(entry.NvmeTrConfList, tr)
	})
}

// dropCdcTrConf removes every occurrence of one transport address from every
// CdcEntry of the SP.
func dropCdcTrConf(
	stm etcdutil.STM,
	sc *spScope,
	tr *pb.NvmeTrConf,
) error {
	return eachCdcEntry(stm, sc, func(entry *pb.CdcEntry) {
		kept := make([]*pb.NvmeTrConf, 0, len(entry.GetNvmeTrConfList()))
		for _, item := range entry.GetNvmeTrConfList() {
			if trConfEqual(item, tr) {
				continue
			}
			kept = append(kept, item)
		}
		entry.NvmeTrConfList = kept
	})
}

// ---------------------------------------------------------------------------
// Resolved SP geometry
// ---------------------------------------------------------------------------

// spBdevConf is the bdev conf a stored SP is read with: its own, which
// CreateStoragePool wrote as the member-wise merge of the request over
// ClusterConf (§8.4 Defaults). It is deliberately NOT re-merged at read time —
// an SP's geometry is fixed when it is created, and a later cluster-level
// change must never move the stripe a thin device was sized against.
func spBdevConf(conf *pb.SpConf) *pb.BdevConf {
	return conf.GetBdevConf()
}

// spStripeSize is the SP's dm-raid0 stripe, the unit CreateThinDevice sizes
// against (§8.7). Resolution order is §7's: the SP's stored conf, then the
// constant.
func spStripeSize(conf *pb.SpConf) uint64 {
	size := spBdevConf(conf).GetDmRaid0Conf().GetStripeSize()
	if size == 0 {
		size = common.DefaultDmRaid0StripeSize
	}
	return size
}

// spSliceCnt is how many slices the SP has, which bounds every slice_idx a
// request may name.
func spSliceCnt(conf *pb.SpConf) int {
	return len(conf.GetSliceIdList())
}

// ---------------------------------------------------------------------------
// dm-clone status (the force = false hydration checks of §8.9 / §8.11)
// ---------------------------------------------------------------------------

// hydrationComplete decides whether a dm-clone has finished copying, from the
// raw `dmsetup status` line the agent puts in a ResInfo's details (§9.5).
//
// The line is
//
//	<start> <len> clone <meta block size> <used>/<total> <region size>
//	<hydrated>/<total> <hydrating> <#feature args> …
//
// so the fourth argument after the target type carries the progress. Anything
// that does not parse as a clone status, and a total of zero, is "not
// complete": DeleteClone and FinishMigration refuse unless they can PROVE the
// copy is done, and an unreadable status is not proof (§8.9).
//
// The gateway parses this itself rather than importing agent.ParseCloneStatus:
// gateway may import only common, pb, etcdutil and model (layout.md §3), and
// only the one number is needed here.
func hydrationComplete(details string) bool {
	for _, line := range strings.Split(details, "\n") {
		fields := strings.Fields(line)
		// <start> <len> clone then at least four target arguments.
		if len(fields) < 7 || fields[2] != "clone" {
			continue
		}
		hydrated, total, ok := strings.Cut(fields[6], "/")
		if !ok {
			continue
		}
		done, err1 := strconv.ParseUint(hydrated, 10, 64)
		want, err2 := strconv.ParseUint(total, 10, 64)
		if err1 != nil || err2 != nil || want == 0 {
			continue
		}
		return done >= want
	}
	return false
}
