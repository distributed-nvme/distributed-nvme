package agent

import (
	"sync"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// Startup is an agent's startup state (dnagent.md SH33): the time its
// process began to serve, which every reply's stamp carries, and whether its
// startup pass still runs. A server built without Begin, as a test that
// calls Reconcile itself builds it, is never starting. It is safe for use by
// every RPC goroutine and the startup pass at once.
type Startup struct {
	mu       sync.Mutex
	at       time.Time
	starting bool
}

// Begin marks the agent starting from at, before its gRPC listener opens
// (Serve).
func (s *Startup) Begin(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.at = at
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

// Stamped sets the agent's start time on a reply's stamp and returns it; a
// server that never began leaves it unset.
func (s *Startup) Stamped(stamp *pb.ObsStamp) *pb.ObsStamp {
	s.mu.Lock()
	at := s.at
	s.mu.Unlock()
	if stamp != nil {
		stamp.AgentStartTime = Timestamp(at)
	}
	return stamp
}

// StartingReply is the AgentReply of a request the agent does not answer
// because its startup pass still runs (common.ReplyCodeStarting).
func StartingReply() *pb.AgentReply {
	return &pb.AgentReply{
		Code:    common.ReplyCodeStarting,
		Details: "the agent's startup pass still runs",
	}
}
