package gateway

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestObsAge pins the age an Inspect* reply derives from the agent's stamp
// (architecture.md, Live-state reporting): the answer time less the
// observation time, both the node's, to the nanosecond; unset when either is
// unset, since an agent with no observation has no age to give and a zero
// would read as a fresh one; and zero, never negative, when the observation
// time sits after the answer time.
func TestObsAge(t *testing.T) {
	obsTime := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	ts := timestamppb.New
	// unset is the want of a case whose age must be left unset.
	const unset = time.Duration(-1)
	cases := []struct {
		name  string
		stamp *pb.ObsStamp
		want  time.Duration
	}{
		{"no stamp", nil, unset},
		{"an empty stamp", &pb.ObsStamp{}, unset},
		{"no observation time",
			&pb.ObsStamp{AnswerTime: ts(obsTime)}, unset},
		{"no answer time",
			&pb.ObsStamp{ObsTime: ts(obsTime)}, unset},
		{"an observation 1.5 s old",
			&pb.ObsStamp{
				ObsTime:    ts(obsTime),
				AnswerTime: ts(obsTime.Add(1500 * time.Millisecond)),
			}, 1500 * time.Millisecond},
		{"a nanosecond old",
			&pb.ObsStamp{
				ObsTime:    ts(obsTime),
				AnswerTime: ts(obsTime.Add(time.Nanosecond)),
			}, time.Nanosecond},
		{"answered at the observation time",
			&pb.ObsStamp{ObsTime: ts(obsTime), AnswerTime: ts(obsTime)},
			0},
		{"an observation time after the answer time",
			&pb.ObsStamp{
				ObsTime:    ts(obsTime.Add(time.Nanosecond)),
				AnswerTime: ts(obsTime),
			}, 0},
		// The converge start and the trace id play no part in the age.
		{"under a converge",
			&pb.ObsStamp{
				ObsTime:       ts(obsTime),
				ObsTraceId:    "t",
				AnswerTime:    ts(obsTime.Add(time.Second)),
				ConvergeStart: ts(obsTime.Add(-time.Hour)),
			}, time.Second},
	}
	for _, tc := range cases {
		got := obsAge(tc.stamp)
		switch {
		case tc.want == unset && got != nil:
			t.Errorf("%s: obsAge = %v, want unset", tc.name, got.AsDuration())
		case tc.want != unset && got == nil:
			t.Errorf("%s: obsAge unset, want %v", tc.name, tc.want)
		case tc.want != unset && got.AsDuration() != tc.want:
			t.Errorf("%s: obsAge = %v, want %v",
				tc.name, got.AsDuration(), tc.want)
		}
	}
}
