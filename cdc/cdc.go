// Package cdc is dnv-cdc's engine (cdc.md): the etcd watcher over the
// {p} cdc keys (§4), the per-active-host view registry (§3) and the NVMe/TCP
// discovery controller that answers hosts on the well-known discovery NQN
// (§5).
//
// It reads etcd through etcdutil and NEVER writes it (WV6), serves and dials
// no gRPC (grpc.md records "cdc: none"), and never touches the local kernel's
// nvmet: a discovery controller built on kernel referrals cannot filter per
// host, which is the whole reason this package exists (§0 #1).
package cdc

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
)

// The normative msg strings of §7. The §9 suite greps them, so they are
// constants and never formatted.
const (
	msgCdcStarting     = "cdc starting"
	msgScanComplete    = "cdc scan complete"
	msgWatchRestarting = "cdc watch restarting"
	msgEntryApplied    = "cdc entry applied"
	msgEntrySkipped    = "cdc entry skipped"

	msgHostConnected    = "host connected"
	msgHostDisconnected = "host disconnected"
	msgViewChanged      = "view changed"
	msgAenSent          = "aen sent"
	msgPduError         = "pdu error"

	msgCdcStopping = "cdc stopping"
)

// The `cdc entry skipped` reasons of §7.
const (
	skipMalformedKey   = "malformed_key"
	skipMalformedValue = "malformed_value"
	skipForeignTrType  = "foreign_tr_type"
	skipForeignAdrFam  = "foreign_adr_fam"
)

// The `host disconnected` reasons of §7.
const (
	reasonClosed    = "closed"
	reasonKeepAlive = "keep_alive"
	reasonPduError  = "pdu_error"
	reasonShutdown  = "shutdown"
)

// ---------------------------------------------------------------------------
// Configuration (CM1)
// ---------------------------------------------------------------------------

// Config is everything cmd/dnv-cdc passes to Run. Every value is already
// validated there (CM2); this package trusts it.
type Config struct {
	// Ranges are the --range digits, each claiming the sixteen shard codes
	// h0…hf (DS2). Non-empty and duplicate-free.
	Ranges []uint32
	// TrType, AdrFam, TrAddr and TrSvcId are the listen endpoint. TrType is
	// always common.DefaultCdcTrType (§0 #2); AdrFam is reported in no log
	// entry of dnv-cdc's own — it describes the LISTEN address, while each
	// discovery log entry carries the address family of the CN port it
	// names (DS3).
	TrType  string
	AdrFam  string
	TrAddr  string
	TrSvcId string
	// Endpoints are the etcd endpoints the client was built with. dnv-cdc
	// never dials them itself; they exist for the `cdc starting` record.
	Endpoints []string
	// RescanInterval overrides common.DefaultCdcRescanInterval (WV5). Zero
	// means the default; only the §8 tests set it.
	RescanInterval time.Duration
}

// listenAddr is the (TrAddr, TrSvcId) pair as a dial string.
func (c Config) listenAddr() string {
	return net.JoinHostPort(c.TrAddr, c.TrSvcId)
}

// rescanInterval is WV5's retry cadence.
func (c Config) rescanInterval() time.Duration {
	if c.RescanInterval > 0 {
		return c.RescanInterval
	}
	return common.DefaultCdcRescanInterval * time.Second
}

// rangeStrings renders the owned ranges for the `cdc starting` record, in the
// hex spelling --range uses.
func (c Config) rangeStrings() []string {
	out := make([]string, 0, len(c.Ranges))
	for _, digit := range c.Ranges {
		out = append(out, fmt.Sprintf("%x", digit))
	}
	return out
}

// ---------------------------------------------------------------------------
// Clock (testability)
// ---------------------------------------------------------------------------

// clock is the package's single source of time: the keep-alive deadlines of
// NP10 and the rescan cadence of WV5 both go through it, so the §8 tests drive
// them without sleeping.
type clock interface {
	now() time.Time
	after(d time.Duration) <-chan time.Time
}

// realClock is the production clock.
type realClock struct{}

func (realClock) now() time.Time { return time.Now() }

func (realClock) after(d time.Duration) <-chan time.Time { return time.After(d) }

// ---------------------------------------------------------------------------
// Dependencies
// ---------------------------------------------------------------------------

// etcdStore is the etcd surface this package uses (EU2, EU3).
// *etcdutil.Client implements it as-is; the §8 tests substitute an in-memory
// fake. There is no write method on purpose: WV6 forbids one.
type etcdStore interface {
	Range(ctx context.Context, prefix string) ([]etcdutil.KV, int64, error)
	Decode(ctx context.Context, kv etcdutil.KV, msg proto.Message) error
	WatchTyped(
		ctx context.Context,
		prefix string,
		fromRev int64,
		newMsg func() proto.Message,
	) (<-chan etcdutil.Event, <-chan error)
}

// deps is what the watcher, the registry and the server share.
type deps struct {
	cfg   Config
	store etcdStore
	clk   clock
}

// ---------------------------------------------------------------------------
// Run (CM3, CM4, CM5)
// ---------------------------------------------------------------------------

// Run starts the three parts of one dnv-cdc instance and blocks until ctx
// ends (CM4): the listener first, so that a busy port fails the process
// before anything else exists; then the watcher; then the accept loop.
//
// On shutdown it stops accepting, closes every connection, joins the watcher
// and closes the etcd client, then logs `cdc stopping` (CM5). It returns an
// error only when the listener could not be opened — nothing about a shutdown
// is an error.
func Run(ctx context.Context, cli *etcdutil.Client, cfg Config) error {
	d := &deps{cfg: cfg, store: cli, clk: realClock{}}
	slog.InfoContext(ctx, msgCdcStarting,
		slog.Any("ranges", cfg.rangeStrings()),
		slog.Any("endpoints", cfg.Endpoints),
		slog.String("tr_addr", cfg.TrAddr),
		slog.String("tr_svc_id", cfg.TrSvcId),
	)
	reg := newRegistry()
	srv, err := newServer(ctx, d, reg)
	if err != nil {
		if closeErr := cli.Close(); closeErr != nil {
			slog.ErrorContext(ctx, "etcd client close failed",
				slog.String("error", closeErr.Error()),
			)
		}
		return err
	}
	w := newWatcher(d, reg, cfg.Ranges)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.run(ctx)
	}()

	srv.run(ctx)
	wg.Wait()

	if err := cli.Close(); err != nil {
		slog.ErrorContext(ctx, "etcd client close failed",
			slog.String("error", err.Error()),
		)
	}
	slog.InfoContext(ctx, msgCdcStopping)
	return nil
}

// endpointSerial derives Identify's SN from the listen endpoint (NP7): a
// stable, 16 character hex digest, so two twins of one range are told apart in
// a host's controller listing while one instance keeps the same serial across
// restarts.
func endpointSerial(trAddr string, trSvcId string) string {
	h := fnv.New64a()
	// hash.Hash.Write never returns an error.
	h.Write([]byte(trAddr))
	h.Write([]byte(":"))
	h.Write([]byte(trSvcId))
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], h.Sum64())
	return hex.EncodeToString(raw[:])
}
