package agent

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// bootFake is an OsClient whose boot id and store files the test sets.
type bootFake struct {
	bootId  string
	bootErr error
	files   map[string]string
	saveErr error
	saves   int
	// badFile is a store file that does not load.
	badFile string
}

func (b *bootFake) client() *common.FakeOsClient {
	return &common.FakeOsClient{
		ReadFileFn: func(ctx context.Context, path string) (string, error) {
			if path != BootIdPath {
				return "", os.ErrNotExist
			}
			return b.bootId + "\n", b.bootErr
		},
		ReadProtoFn: func(ctx context.Context, path string, msg proto.Message) error {
			if path == b.badFile {
				return errors.New("proto: cannot parse")
			}
			value, ok := b.files[path]
			if !ok {
				return os.ErrNotExist
			}
			msg.(*wrapperspb.StringValue).Value = value
			return nil
		},
		WriteProtoFn: func(ctx context.Context, path string, msg proto.Message) error {
			b.saves++
			if b.saveErr != nil {
				return b.saveErr
			}
			b.files[path] = msg.(*wrapperspb.StringValue).GetValue()
			return nil
		},
	}
}

// TestReadAndSaveBoot pins SH33's boot check: the first start reads no reboot
// and returns the boot id to save; a start under the saved id reads none and
// has nothing to save; one under another reads a reboot, and so does every
// start after it until the id is saved, as the end of a start saves it; a
// boot id that cannot be read, or is empty, reads none, with the error; a
// file that does not load reads none and returns the id; each role keeps its
// own file.
func TestReadAndSaveBoot(t *testing.T) {
	ctx := context.Background()
	b := &bootFake{bootId: "aaaa", files: map[string]string{}}
	s := NewStore(b.client(), "/srv/store")
	path := "/srv/store/" + StoreFileBootDn
	id, rebooted, err := s.ReadBoot(ctx, StoreFileBootDn)
	if err != nil || rebooted || id != "aaaa" || b.saves != 0 {
		t.Fatalf("first start: id %q, rebooted %v, err %v, saves %d; want "+
			"the id to save, no reboot, no save yet", id, rebooted, err,
			b.saves)
	}
	if err := s.SaveBoot(ctx, StoreFileBootDn, id); err != nil ||
		b.files[path] != "aaaa" {
		t.Fatalf("the save: err %v, file %q", err, b.files[path])
	}
	if id, rebooted, err := s.ReadBoot(ctx, StoreFileBootDn); err != nil ||
		rebooted || id != "" {
		t.Fatalf("same boot: id %q, rebooted %v, err %v; want nothing",
			id, rebooted, err)
	}
	if err := s.SaveBoot(ctx, StoreFileBootDn, ""); err != nil || b.saves != 1 {
		t.Fatalf("an empty id saved: err %v, saves %d", err, b.saves)
	}
	b.bootId = "bbbb"
	for range 2 {
		// The start that dies before its end saves nothing, so the next
		// start reads the reboot too.
		if id, rebooted, err := s.ReadBoot(ctx, StoreFileBootDn); err != nil ||
			!rebooted || id != "bbbb" {
			t.Fatalf("new boot: id %q, rebooted %v, err %v; want a reboot",
				id, rebooted, err)
		}
	}
	if err := s.SaveBoot(ctx, StoreFileBootDn, "bbbb"); err != nil {
		t.Fatalf("the save after the reboot: %v", err)
	}
	if _, rebooted, _ := s.ReadBoot(ctx, StoreFileBootDn); rebooted {
		t.Fatalf("a restart after the saved reboot read a reboot")
	}
	if id, rebooted, _ := s.ReadBoot(ctx, StoreFileBootCn); rebooted ||
		id != "bbbb" {
		t.Fatalf("the cn role's first start: id %q, rebooted %v; want its "+
			"own file and no reboot", id, rebooted)
	}
	b.bootErr = errors.New("no proc")
	if id, rebooted, err := s.ReadBoot(ctx, StoreFileBootDn); err == nil ||
		rebooted || id != "" {
		t.Fatalf("unreadable boot id: id %q, rebooted %v, err %v", id,
			rebooted, err)
	}
	b.bootErr = nil
	b.bootId = "  "
	if _, rebooted, err := s.ReadBoot(ctx, StoreFileBootDn); err == nil ||
		rebooted {
		t.Fatalf("empty boot id: rebooted %v, err %v", rebooted, err)
	}
	b.bootId = "cccc"
	b.badFile = path
	if id, rebooted, err := s.ReadBoot(ctx, StoreFileBootDn); err != nil ||
		rebooted || id != "cccc" {
		t.Fatalf("a file that does not load: id %q, rebooted %v, err %v; "+
			"want the id and no reboot", id, rebooted, err)
	}
}

// TestStartupStampsTheReboot pins the stamp of SH33: every reply carries the
// agent's start and whether it began after a reboot.
func TestStartupStampsTheReboot(t *testing.T) {
	at := time.Unix(1_000_000, 0)
	var s Startup
	s.Begin(at, true)
	stamp := s.Stamped(&pb.ObsStamp{})
	if !stamp.GetAgentRebooted() || !stamp.GetAgentStartTime().AsTime().Equal(at) {
		t.Fatalf("stamp %v, want the start and the reboot", stamp)
	}
	s.Begin(at, false)
	if s.Stamped(&pb.ObsStamp{}).GetAgentRebooted() {
		t.Fatalf("a restart stamped a reboot")
	}
}
