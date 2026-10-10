package gateway

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The loop of GW9 on its own, with no etcd: candidateUnit re-runs a unit that
// answers errCandidateChanged, and stops at the first other answer, at the
// end of the request's ctx, or after common.MaxCandidateRoundCnt rounds.

// cuWantAborted asserts an ABORTED "did not settle" and hands the message
// back.
func cuWantAborted(t *testing.T, err error) string {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Aborted {
		t.Fatalf("want ABORTED, got %v", err)
	}
	if !strings.Contains(st.Message(), "did not settle") {
		t.Errorf("message %q does not say the unit did not settle",
			st.Message())
	}
	return st.Message()
}

// TestCandidateUnitStopsAtTheRoundCap pins the round cap: a unit that answers
// candidate changed for ever runs exactly MaxCandidateRoundCnt times and then
// answers ABORTED, naming the cap, on a ctx that never ends. Without the cap
// this test would never return.
func TestCandidateUnitStopsAtTheRoundCap(t *testing.T) {
	calls := 0
	err := candidateUnit(context.Background(), func() error {
		calls++
		return errCandidateChanged
	})
	if calls != common.MaxCandidateRoundCnt {
		t.Errorf("the unit ran %d times, want %d", calls,
			common.MaxCandidateRoundCnt)
	}
	msg := cuWantAborted(t, err)
	want := fmt.Sprintf("candidate allocation did not settle in %d rounds",
		common.MaxCandidateRoundCnt)
	if msg != want {
		t.Errorf("message: got %q, want %q", msg, want)
	}
	if isCandidateChanged(err) {
		t.Errorf("the internal sentinel reached the caller")
	}
}

// TestCandidateUnitReturnsTheFirstOtherAnswer pins the other two exits: a
// unit that settles after some changed rounds is run exactly until it does,
// and any answer but candidate changed — a commit or a refusal — comes back
// unchanged after the round that gave it.
func TestCandidateUnitReturnsTheFirstOtherAnswer(t *testing.T) {
	t.Run("settles on round three", func(t *testing.T) {
		calls := 0
		err := candidateUnit(context.Background(), func() error {
			calls++
			if calls < 3 {
				return errCandidateChanged
			}
			return nil
		})
		if err != nil {
			t.Fatalf("got %v, want the third round's nil", err)
		}
		if calls != 3 {
			t.Errorf("the unit ran %d times, want 3", calls)
		}
	})
	t.Run("a refusal on round one", func(t *testing.T) {
		calls := 0
		refusal := errExhausted("no room")
		err := candidateUnit(context.Background(), func() error {
			calls++
			return refusal
		})
		if err != refusal {
			t.Errorf("got %v, want the unit's own refusal unchanged", err)
		}
		if calls != 1 {
			t.Errorf("the unit ran %d times, want 1", calls)
		}
	})
	t.Run("a refusal on the last round", func(t *testing.T) {
		calls := 0
		refusal := errPrecondition("refused")
		err := candidateUnit(context.Background(), func() error {
			calls++
			if calls < common.MaxCandidateRoundCnt {
				return errCandidateChanged
			}
			return refusal
		})
		if err != refusal {
			t.Errorf("got %v, want the last round's refusal, not the cap",
				err)
		}
		if calls != common.MaxCandidateRoundCnt {
			t.Errorf("the unit ran %d times, want %d", calls,
				common.MaxCandidateRoundCnt)
		}
	})
}

// TestCandidateUnitStopsWhenTheCtxEnds pins the ctx exit, which comes first: a
// unit whose ctx has ended answers ABORTED after the round that found it so,
// naming the ctx's error rather than the cap, however few rounds ran — and
// when the ctx ends in the very round that reaches the cap, it is still the
// ctx that is named.
func TestCandidateUnitStopsWhenTheCtxEnds(t *testing.T) {
	for _, endRound := range []int{2, common.MaxCandidateRoundCnt} {
		t.Run(fmt.Sprintf("in round %d", endRound), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			err := candidateUnit(ctx, func() error {
				calls++
				if calls == endRound {
					cancel()
				}
				return errCandidateChanged
			})
			if calls != endRound {
				t.Errorf("the unit ran %d times, want %d: no round runs "+
					"after the ctx ended", calls, endRound)
			}
			msg := cuWantAborted(t, err)
			if !strings.Contains(msg, context.Canceled.Error()) {
				t.Errorf("message %q must name the ended ctx", msg)
			}
		})
	}
}
