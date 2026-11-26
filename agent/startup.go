package agent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// Startup is an agent's startup state (dnagent.md SH33): the time its
// process began to serve and whether it began after a boot of its machine
// that its last start did not see, which every reply's stamp carries, and
// whether its startup pass still runs. A server built without Begin, as a test that
// calls Reconcile itself builds it, is never starting. It is safe for use by
// every RPC goroutine and the startup pass at once.
type Startup struct {
	mu       sync.Mutex
	at       time.Time
	rebooted bool
	starting bool
}

// Begin marks the agent starting from at, after a reboot when rebooted
// (Store.SeeBoot), before its gRPC listener opens (Serve).
func (s *Startup) Begin(at time.Time, rebooted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.at = at
	s.rebooted = rebooted
	s.starting = true
}

// End marks the startup pass over: from then on every request is answered
// as SH25 and SH28 say.
func (s *Startup) End() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starting = false
}

// Starting reports whether the startup pass still runs.
func (s *Startup) Starting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starting
}

// Stamped sets the agent's start time, and whether it began after a reboot,
// on a reply's stamp and returns it; a server that never began leaves them
// unset.
func (s *Startup) Stamped(stamp *pb.ObsStamp) *pb.ObsStamp {
	s.mu.Lock()
	at, rebooted := s.at, s.rebooted
	s.mu.Unlock()
	if stamp != nil {
		stamp.AgentStartTime = Timestamp(at)
		stamp.AgentRebooted = rebooted
	}
	return stamp
}

// MsgBootSeen is the record of an agent's look at its machine's boot as it
// starts, and MsgBootSaveFailed that of a save of the boot id that failed
// (dnagent.md SH33).
const (
	MsgBootSeen       = "agent boot seen"
	MsgBootSaveFailed = "agent boot id save failed"
)

// SeeBoot is Store.ReadBoot with its record: at Info, whether the agent
// starts after a reboot; at Warn, with the error, when the boot id could not
// be read. It returns the answer Startup.Begin takes and the boot id KeepBoot
// saves once the start ends.
func SeeBoot(ctx context.Context, store *Store, name string) (bool, string) {
	bootId, rebooted, err := store.ReadBoot(ctx, name)
	if err != nil {
		slog.WarnContext(ctx, MsgBootSeen, slog.Bool("rebooted", rebooted),
			slog.String("error", err.Error()))
		return rebooted, bootId
	}
	slog.InfoContext(ctx, MsgBootSeen, slog.Bool("rebooted", rebooted))
	return rebooted, bootId
}

// KeepBoot saves the boot id SeeBoot returned, as the startup pass ends; a
// save that fails is logged at Warn, and the next start compares with the
// older id again (dnagent.md SH33).
func KeepBoot(ctx context.Context, store *Store, name, bootId string) {
	if err := store.SaveBoot(ctx, name, bootId); err != nil {
		slog.WarnContext(ctx, MsgBootSaveFailed, slog.String("error", err.Error()))
	}
}

// StartingReply is the AgentReply of a request the agent does not answer
// because its startup pass still runs (common.ReplyCodeStarting).
func StartingReply() *pb.AgentReply {
	return &pb.AgentReply{
		Code:    common.ReplyCodeStarting,
		Details: "the agent's startup pass still runs",
	}
}
