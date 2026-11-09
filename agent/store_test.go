package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// ---------------------------------------------------------------------------
// The store's two states: the latest save failed, and the disk refused a
// write (dnagent.md SH4, SH30, DN18)
// ---------------------------------------------------------------------------

// scriptedWrites is a store whose writes answer the errors of next in turn,
// nil for a write that holds, and record the paths written.
type scriptedWrites struct {
	next  []error
	paths []string
	msgs  []proto.Message
	// removed lists the paths each rm named, one rm a line.
	removed []string
}

func (w *scriptedWrites) store() *Store {
	oc := &common.FakeOsClient{
		WriteProtoFn: func(
			ctx context.Context, path string, msg proto.Message,
		) error {
			w.paths = append(w.paths, path)
			w.msgs = append(w.msgs, msg)
			if len(w.next) == 0 {
				return nil
			}
			err := w.next[0]
			w.next = w.next[1:]
			return err
		},
		RunCommandFn: func(
			ctx context.Context, name string, args []string, stdin string,
		) (string, string, int, error) {
			if name == "rm" {
				w.removed = append(w.removed, strings.Join(args, " "))
			}
			return "", "", 0, nil
		},
	}
	return NewStore(oc, "/srv/store")
}

// assertStates checks both states of s: SaveFailing, and Refusal with its
// text when wantText is not empty.
func assertStates(
	t *testing.T,
	label string,
	s *Store,
	wantFailing bool,
	wantText string,
) {
	t.Helper()
	if got := s.SaveFailing(); got != wantFailing {
		t.Errorf("%s: SaveFailing %v, want %v", label, got, wantFailing)
	}
	text, refused := s.Refusal()
	if refused != (wantText != "") || text != wantText {
		t.Errorf("%s: Refusal (%q, %v), want (%q, %v)", label, text,
			refused, wantText, wantText != "")
	}
}

// TestStoreRefusalFollowsTheDiskErrors: a save that fails with an error of
// the write itself sets both states and keeps the error's text; a later
// one refreshes the text; a save cut off by its ctx sets the first state
// only and leaves the second as it is; a save that holds clears both.
func TestStoreRefusalFollowsTheDiskErrors(t *testing.T) {
	noSpace := fmt.Errorf("write /srv/store/dn-1: %w", syscall.ENOSPC)
	readOnly := fmt.Errorf("write /srv/store/dn-1: %w", syscall.EROFS)
	cutOff := fmt.Errorf("save: %w", context.DeadlineExceeded)
	w := &scriptedWrites{next: []error{
		cutOff, nil, noSpace, cutOff, context.Canceled, readOnly, nil,
	}}
	s := w.store()
	ctx := context.Background()
	msg := wrapperspb.String("request")

	assertStates(t, "a new store", s, false, "")
	steps := []struct {
		label       string
		wantFailing bool
		wantText    string
	}{
		{"a cut-off save on a healthy store", true, ""},
		{"a save that holds", false, ""},
		{"a save the disk refused", true, noSpace.Error()},
		{"a cut-off save after the refusal", true, noSpace.Error()},
		{"a cancelled save after the refusal", true, noSpace.Error()},
		{"a second refusal", true, readOnly.Error()},
		{"a save that holds after the refusals", false, ""},
	}
	for _, step := range steps {
		_ = s.Save(ctx, "/srv/store/dn-1", msg)
		assertStates(t, step.label, s, step.wantFailing, step.wantText)
	}
}

// TestStoreProbeWritesOnlyWhileRefused: the probe writes nothing on a store
// whose disk took its latest write, cut-off saves included; while the disk
// refuses writes it writes one probe file per call at the path given, of
// random bytes at least as many as the refused save wrote; a refused probe
// keeps the refusal with the probe's error; a probe that holds clears the
// refusal alone, leaving the latest save failed until a save holds (SH30),
// after which the probe writes nothing again.
func TestStoreProbeWritesOnlyWhileRefused(t *testing.T) {
	noSpace := fmt.Errorf("write a: %w", syscall.ENOSPC)
	ioErr := fmt.Errorf("write b: %w", syscall.EIO)
	w := &scriptedWrites{}
	s := w.store()
	ctx := context.Background()
	const probe = "/srv/store/probe-dn-1"

	if err := s.Probe(ctx, probe); err != nil {
		t.Fatalf("a probe on a new store: %v", err)
	}
	w.next = []error{context.DeadlineExceeded}
	_ = s.Save(ctx, "/srv/store/dn-1", wrapperspb.String("request"))
	if err := s.Probe(ctx, probe); err != nil {
		t.Fatalf("a probe after a cut-off save: %v", err)
	}
	if len(w.paths) != 1 {
		t.Fatalf("%d writes, want only the cut-off save's: %q",
			len(w.paths), w.paths)
	}

	// A large chunk refused: the probes write at least as much.
	chunk := wrapperspb.Bytes(make([]byte, 10000))
	w.next = []error{noSpace, ioErr, context.Canceled, nil}
	_ = s.Save(ctx, "/srv/store/migr-bm-1", chunk)
	if err := s.Probe(ctx, probe); !errors.Is(err, syscall.EIO) {
		t.Fatalf("a refused probe returned %v, want its write's error", err)
	}
	assertStates(t, "a refused probe", s, true, ioErr.Error())
	_ = s.Probe(ctx, probe)
	assertStates(t, "a cut-off probe", s, true, ioErr.Error())
	if len(w.removed) != 0 {
		t.Fatalf("refused probes removed %q, want nothing", w.removed)
	}
	if err := s.Probe(ctx, probe); err != nil {
		t.Fatalf("a probe that holds returned %v", err)
	}
	assertStates(t, "a probe that holds", s, true, "")
	if len(w.removed) != 1 || w.removed[0] != "-f "+probe {
		t.Fatalf("the probe that held removed %q, want its file alone",
			w.removed)
	}
	if err := s.Probe(ctx, probe); err != nil {
		t.Fatalf("a probe on a healed store: %v", err)
	}
	_ = s.Save(ctx, "/srv/store/dn-1", wrapperspb.String("request"))
	assertStates(t, "a save after the probe", s, false, "")

	if len(w.paths) != 6 {
		t.Fatalf("%d writes, want 6, three of them probes: %q",
			len(w.paths), w.paths)
	}
	for i := 2; i < 5; i++ {
		if w.paths[i] != probe {
			t.Errorf("write %d went to %s, want the probe file %s", i,
				w.paths[i], probe)
			continue
		}
		if size := proto.Size(w.msgs[i]); size < proto.Size(chunk) {
			t.Errorf("probe %d wrote %d bytes, want at least the refused "+
				"chunk's %d", i, size, proto.Size(chunk))
		}
		value := w.msgs[i].(*wrapperspb.BytesValue).GetValue()
		if bytes.Count(value, []byte{0}) > len(value)/16 {
			t.Errorf("probe %d wrote %d zero bytes of %d, want random "+
				"bytes", i, bytes.Count(value, []byte{0}), len(value))
		}
	}
}

// TestStoreProbeKeepsANewerRefusal: a save the disk refuses while a probe is
// writing keeps the refusal, though the probe's own write holds: the probe
// proved room for the earlier save, not for this one.
func TestStoreProbeKeepsANewerRefusal(t *testing.T) {
	noSpace := fmt.Errorf("write a: %w", syscall.ENOSPC)
	newer := fmt.Errorf("write c: %w", syscall.ENOSPC)
	var s *Store
	ctx := context.Background()
	oc := &common.FakeOsClient{
		WriteProtoFn: func(
			ctx context.Context, path string, msg proto.Message,
		) error {
			switch path {
			case "/srv/store/side-1":
				return noSpace
			case "/srv/store/side-2":
				return newer
			case "/srv/store/probe-dn-1":
				_ = s.Save(ctx, "/srv/store/side-2",
					wrapperspb.String("request"))
			}
			return nil
		},
	}
	s = NewStore(oc, "/srv/store")
	_ = s.Save(ctx, "/srv/store/side-1", wrapperspb.String("request"))
	if err := s.Probe(ctx, "/srv/store/probe-dn-1"); err != nil {
		t.Fatalf("the probe returned %v", err)
	}
	assertStates(t, "a probe beside a newer refusal", s, true, newer.Error())
}
