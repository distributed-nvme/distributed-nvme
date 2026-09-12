package common

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// RunCommand (osclient.md §8.1, §8.2)
// ---------------------------------------------------------------------------

func TestRunCommandExitCodes(t *testing.T) {
	client := NewLimitedOsClient(0)
	ctx := context.Background()

	if _, _, exitCode, err := client.RunCommand(
		ctx, "sh", []string{"-c", "exit 0"}, "",
	); exitCode != 0 || err != nil {
		t.Errorf("exit 0 → (%d, %v), want (0, nil)", exitCode, err)
	}

	if _, _, exitCode, err := client.RunCommand(
		ctx, "sh", []string{"-c", "exit 3"}, "",
	); exitCode != 3 || err == nil {
		t.Errorf("exit 3 → (%d, %v), want (3, non-nil)", exitCode, err)
	}

	if _, _, exitCode, err := client.RunCommand(
		ctx, "dnv-no-such-binary-xyz", nil, "",
	); exitCode != -1 || err == nil {
		t.Errorf("missing binary → (%d, %v), want (-1, non-nil)", exitCode, err)
	}
}

func TestRunCommandStreams(t *testing.T) {
	client := NewLimitedOsClient(0)
	ctx := context.Background()

	stdout, stderr, exitCode, err := client.RunCommand(ctx, "cat", nil, "hello")
	if err != nil || exitCode != 0 {
		t.Fatalf("cat → (%d, %v)", exitCode, err)
	}
	if stdout != "hello" || stderr != "" {
		t.Errorf("cat stdout=%q stderr=%q, want %q and empty", stdout, stderr, "hello")
	}

	stdout, stderr, _, err = client.RunCommand(
		ctx, "sh", []string{"-c", "echo out; echo err 1>&2"}, "",
	)
	if err != nil {
		t.Fatalf("sh → %v", err)
	}
	if stdout != "out\n" {
		t.Errorf("stdout = %q, want %q", stdout, "out\n")
	}
	if stderr != "err\n" {
		t.Errorf("stderr = %q, want %q", stderr, "err\n")
	}
}

// The caller owns the deadline (architecture.md §7): the ctx firing SIGTERMs
// the process.
func TestRunCommandSoftTimeoutSigterm(t *testing.T) {
	client := NewLimitedOsClient(0)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, exitCode, err := client.RunCommand(ctx, "sleep", []string{"10"}, "")
	elapsed := time.Since(start)

	if err == nil {
		t.Error("a killed command reported success")
	}
	if exitCode != -1 {
		t.Errorf("exit_code = %d, want -1 for a signal-killed process", exitCode)
	}
	if elapsed > 2*time.Second {
		t.Errorf("SIGTERM path took %v, want ~100ms", elapsed)
	}
}

// A process that ignores SIGTERM is SIGKILLed WaitDelay later, i.e. at the
// hard timeout relative to the soft one (CmdHardTimeout-CmdSoftTimeout).
func TestRunCommandHardTimeoutSigkill(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: waits out cmd.WaitDelay")
	}
	client := NewLimitedOsClient(0)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	waitDelay := time.Duration(CmdHardTimeout-CmdSoftTimeout) * time.Second
	start := time.Now()
	_, _, _, err := client.RunCommand(
		ctx, "sh", []string{"-c", `trap "" TERM; sleep 30`}, "",
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Error("a SIGKILLed command reported success")
	}
	if elapsed < waitDelay {
		t.Errorf("returned after %v, want at least the %v grace", elapsed, waitDelay)
	}
	if elapsed > waitDelay+2*time.Second {
		t.Errorf("returned after %v, want ~%v", elapsed, waitDelay)
	}
}

// ---------------------------------------------------------------------------
// Concurrency limit (osclient.md §8.4, §4.1)
// ---------------------------------------------------------------------------

func TestInFlightLimit(t *testing.T) {
	client := NewLimitedOsClient(2)
	ctx := context.Background()

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, _, err := client.RunCommand(
				ctx, "sleep", []string{"0.2"}, "",
			); err != nil {
				t.Errorf("sleep failed: %v", err)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	if elapsed < 400*time.Millisecond {
		t.Errorf("3 x sleep 0.2 with limit 2 took %v, want >= 400ms "+
			"(the third call must wait for a slot)", elapsed)
	}
}

func TestLimitBlocksAndCanceledCtxDoesNotRun(t *testing.T) {
	capture := captureLogs(t)
	client := NewLimitedOsClient(1)

	// Hold the only slot.
	if err := client.sem.Acquire(context.Background(), 1); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer client.sem.Release(1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stdout, stderr, exitCode, err := client.RunCommand(ctx, "echo", []string{"hi"}, "")
	if err != context.Canceled {
		t.Errorf("RunCommand err = %v, want context.Canceled", err)
	}
	if stdout != "" || stderr != "" || exitCode != -1 {
		t.Errorf("RunCommand ran anyway: (%q, %q, %d)", stdout, stderr, exitCode)
	}
	if _, err := client.ReadFile(ctx, "/etc/hostname"); err != context.Canceled {
		t.Errorf("ReadFile err = %v, want context.Canceled", err)
	}
	if err := client.WriteFile(ctx, filepath.Join(t.TempDir(), "f"), "x"); err != context.Canceled {
		t.Errorf("WriteFile err = %v, want context.Canceled", err)
	}

	// A semaphore-acquire failure logs nothing: the operation never happened.
	if recs := capture.records(t); len(recs) != 0 {
		t.Errorf("blocked calls logged %d records: %s", len(recs), capture.buf.String())
	}
}

// ---------------------------------------------------------------------------
// File and proto I/O (osclient.md §8.5, §8.6)
// ---------------------------------------------------------------------------

func TestFileRoundTrip(t *testing.T) {
	client := NewLimitedOsClient(0)
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "state")

	if err := client.WriteFile(ctx, path, "first version"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	data, err := client.ReadFile(ctx, path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if data != "first version" {
		t.Errorf("read back %q", data)
	}

	if err := client.WriteFile(ctx, path, "second version"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if data, _ = client.ReadFile(ctx, path); data != "second version" {
		t.Errorf("after overwrite read back %q", data)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %v, want 0644", perm)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "state" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("leftover temp files: %v", names)
	}

	if _, err := client.ReadFile(ctx, filepath.Join(dir, "missing")); err == nil {
		t.Error("reading a missing file succeeded")
	}
}

func TestProtoRoundTrip(t *testing.T) {
	client := NewLimitedOsClient(0)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "side-0-0-0-0")

	want := &pb.SyncupSideRequest{
		ClusterId:   16981786240730056190,
		DnId:        3,
		SidePointer: &pb.SidePointer{SpId: 17, LegId: 21, SideId: 22},
		Revision:    9,
		SideConf: &pb.SyncupSideRequest_SideConf{
			ExtCnt:        10,
			PrimaryCnId:   5,
			StandbyIdList: []uint64{6, 7},
			SpLevel:       pb.SpLevel_SP_LEVEL_READONLY,
		},
	}
	if err := client.WriteProto(ctx, path, want); err != nil {
		t.Fatalf("WriteProto: %v", err)
	}

	got := &pb.SyncupSideRequest{}
	if err := client.ReadProto(ctx, path, got); err != nil {
		t.Fatalf("ReadProto: %v", err)
	}
	if !proto.Equal(want, got) {
		t.Errorf("round trip changed the message:\nwant %v\ngot  %v", want, got)
	}

	if err := client.ReadProto(ctx, path+"-missing", &pb.SyncupSideRequest{}); err == nil {
		t.Error("ReadProto of a missing file succeeded")
	}
}

// ---------------------------------------------------------------------------
// Logging (osclient.md §8.7, §4.5)
// ---------------------------------------------------------------------------

func hasAttrs(t *testing.T, rec map[string]any, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if _, ok := rec[key]; !ok {
			t.Errorf("record %v is missing attr %q", rec, key)
		}
	}
}

func TestOsClientLogRecords(t *testing.T) {
	capture := captureLogs(t)
	client := NewLimitedOsClient(0)
	ctx := WithTraceId(context.Background(), "os-trace")
	dir := t.TempDir()

	// 1. command
	if _, _, _, err := client.RunCommand(
		ctx, "sh", []string{"-c", "echo out; echo err 1>&2"}, "stdin data",
	); err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	cmdRec := capture.onlyMsg(t, "os command")
	hasAttrs(t, cmdRec, "cmd", "args", "stdin", "stdout", "stderr", "exit_code", TraceIdLogKey)
	if cmdRec["cmd"] != "sh" || cmdRec["stdin"] != "stdin data" ||
		cmdRec["stdout"] != "out\n" || cmdRec["stderr"] != "err\n" ||
		cmdRec["exit_code"] != float64(0) {
		t.Errorf("os command record = %v", cmdRec)
	}
	if args, ok := cmdRec["args"].([]any); !ok || len(args) != 2 || args[0] != "-c" {
		t.Errorf("args rendered as %v", cmdRec["args"])
	}
	if _, ok := cmdRec["error"]; ok {
		t.Errorf("successful command carries an error attr: %v", cmdRec)
	}
	if cmdRec[TraceIdLogKey] != "os-trace" {
		t.Errorf("trace_id = %v, want os-trace", cmdRec[TraceIdLogKey])
	}

	// 2. failing command: same record, plus error
	if _, _, _, err := client.RunCommand(ctx, "sh", []string{"-c", "exit 4"}, ""); err == nil {
		t.Fatal("exit 4 reported success")
	}
	failRec := capture.withMsg(t, "os command")[1]
	if failRec["exit_code"] != float64(4) {
		t.Errorf("exit_code = %v, want 4", failRec["exit_code"])
	}
	if _, ok := failRec["error"]; !ok {
		t.Errorf("failed command has no error attr: %v", failRec)
	}

	// 3. file write/read, with truncation of long data (R11)
	longData := strings.Repeat("z", LogStrDataLimit+50)
	path := filepath.Join(dir, "long")
	if err := client.WriteFile(ctx, path, longData); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := client.ReadFile(ctx, path); err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	for _, msg := range []string{"os write file", "os read file"} {
		rec := capture.onlyMsg(t, msg)
		hasAttrs(t, rec, "path", "size", "data", TraceIdLogKey)
		if rec["path"] != path {
			t.Errorf("%s: path = %v", msg, rec["path"])
		}
		if rec["size"] != float64(len(longData)) {
			t.Errorf("%s: size = %v, want %d", msg, rec["size"], len(longData))
		}
		data, _ := rec["data"].(string)
		if !strings.HasSuffix(data, "...(178 chars total)") {
			t.Errorf("%s: data not truncated: %q", msg, data)
		}
		if len([]rune(data)) != LogStrDataLimit+len("...(178 chars total)") {
			t.Errorf("%s: data kept %d runes", msg, len([]rune(data)))
		}
	}

	// 4. proto write/read: bytes fields are logged as sizes only (R10)
	protoPath := filepath.Join(dir, "migr-bm")
	msg := &pb.PushMigrBitmapRequest{
		ClusterId:   16981786240730056190,
		DnId:        3,
		SidePointer: &pb.SidePointer{SpId: 17, LegId: 21, SideId: 22},
		Revision:    9,
		MigrId:      30,
		Bitmap:      []byte{1, 2, 3, 4},
	}
	if err := client.WriteProto(ctx, protoPath, msg); err != nil {
		t.Fatalf("WriteProto: %v", err)
	}
	if err := client.ReadProto(ctx, protoPath, &pb.PushMigrBitmapRequest{}); err != nil {
		t.Fatalf("ReadProto: %v", err)
	}
	serialized, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, name := range []string{"os write proto", "os read proto"} {
		rec := capture.onlyMsg(t, name)
		hasAttrs(t, rec, "path", "size", "data", TraceIdLogKey)
		if rec["path"] != protoPath {
			t.Errorf("%s: path = %v", name, rec["path"])
		}
		if rec["size"] != float64(len(serialized)) {
			t.Errorf("%s: size = %v, want %d", name, rec["size"], len(serialized))
		}
		data, ok := rec["data"].(map[string]any)
		if !ok {
			t.Fatalf("%s: data = %T, want a decoded map", name, rec["data"])
		}
		if data["bitmap"] != "<4 bytes>" {
			t.Errorf("%s: bitmap = %v, want <4 bytes>", name, data["bitmap"])
		}
		if _, ok := data["side_pointer"].(map[string]any); !ok {
			t.Errorf("%s: side_pointer = %v", name, data["side_pointer"])
		}
	}

	// 5. failing file read still emits exactly one record, carrying the error
	if _, err := client.ReadFile(ctx, filepath.Join(dir, "nope")); err == nil {
		t.Fatal("reading a missing file succeeded")
	}
	readRecs := capture.withMsg(t, "os read file")
	failRead := readRecs[len(readRecs)-1]
	if _, ok := failRead["error"]; !ok {
		t.Errorf("failed read has no error attr: %v", failRead)
	}
	if failRead["size"] != float64(0) {
		t.Errorf("failed read size = %v, want 0", failRead["size"])
	}
}

func TestFakeOsClientDefaultsAndOverrides(t *testing.T) {
	ctx := context.Background()
	fake := &FakeOsClient{}

	stdout, stderr, exitCode, err := fake.RunCommand(ctx, "anything", nil, "")
	if stdout != "" || stderr != "" || exitCode != 0 || err != nil {
		t.Errorf("unset RunCommandFn = (%q, %q, %d, %v)", stdout, stderr, exitCode, err)
	}
	if data, err := fake.ReadFile(ctx, "/x"); data != "" || err != nil {
		t.Errorf("unset ReadFileFn = (%q, %v)", data, err)
	}
	if err := fake.WriteFile(ctx, "/x", "d"); err != nil {
		t.Errorf("unset WriteFileFn = %v", err)
	}
	if data, err := fake.ReadBlock(ctx, "/x", 0, 8); data != nil || err != nil {
		t.Errorf("unset ReadBlockFn = (%v, %v)", data, err)
	}
	if err := fake.WriteBlock(ctx, "/x", 0, []byte{1}); err != nil {
		t.Errorf("unset WriteBlockFn = %v", err)
	}
	if err := fake.ReadProto(ctx, "/x", &pb.SidePointer{}); err != nil {
		t.Errorf("unset ReadProtoFn = %v", err)
	}
	if err := fake.WriteProto(ctx, "/x", &pb.SidePointer{}); err != nil {
		t.Errorf("unset WriteProtoFn = %v", err)
	}

	var gotName string
	var gotArgs []string
	fake.RunCommandFn = func(_ context.Context, name string, args []string, stdin string) (string, string, int, error) {
		gotName, gotArgs = name, args
		return "table\n", "", 0, nil
	}
	fake.ReadProtoFn = func(_ context.Context, _ string, target proto.Message) error {
		proto.Merge(target, &pb.SidePointer{SpId: 7})
		return nil
	}

	if stdout, _, _, _ := fake.RunCommand(ctx, "dmsetup", []string{"table"}, ""); stdout != "table\n" {
		t.Errorf("stubbed RunCommand returned %q", stdout)
	}
	if gotName != "dmsetup" || len(gotArgs) != 1 || gotArgs[0] != "table" {
		t.Errorf("stub saw (%q, %v)", gotName, gotArgs)
	}
	target := &pb.SidePointer{}
	if err := fake.ReadProto(ctx, "/x", target); err != nil || target.GetSpId() != 7 {
		t.Errorf("stubbed ReadProto → (%v, %v)", target, err)
	}

	var blockOff uint64
	var blockData []byte
	fake.WriteBlockFn = func(_ context.Context, _ string, offset uint64, data []byte) error {
		blockOff, blockData = offset, data
		return nil
	}
	fake.ReadBlockFn = func(_ context.Context, _ string, _ uint64, length uint64) ([]byte, error) {
		return make([]byte, length), nil
	}
	if err := fake.WriteBlock(ctx, "/disk", 4096, []byte("DNVDISK1")); err != nil {
		t.Errorf("stubbed WriteBlock = %v", err)
	}
	if blockOff != 4096 || string(blockData) != "DNVDISK1" {
		t.Errorf("stub saw (%d, %q)", blockOff, blockData)
	}
	if got, err := fake.ReadBlock(ctx, "/disk", 0, 5); err != nil || len(got) != 5 {
		t.Errorf("stubbed ReadBlock → (%v, %v)", got, err)
	}
}

// ---------------------------------------------------------------------------
// ReadBlock / WriteBlock (osclient.md §8.9, §4.6 — architecture.md [D13])
// ---------------------------------------------------------------------------

func TestBlockRoundTrip(t *testing.T) {
	capture := captureLogs(t)
	client := NewLimitedOsClient(0)
	ctx := WithTraceId(context.Background(), "block-trace")
	path := filepath.Join(t.TempDir(), "disk.img")

	// A block device always exists at full size; model that with a
	// pre-sized regular file, since WriteBlock never extends one.
	if err := os.WriteFile(path, make([]byte, 4096*4), 0o644); err != nil {
		t.Fatalf("create backing file: %v", err)
	}

	head := []byte("DNVDISK1\x01\x00\x00\x00")
	if err := client.WriteBlock(ctx, path, 0, head); err != nil {
		t.Fatalf("WriteBlock at 0: %v", err)
	}
	tail := []byte{0xde, 0xad, 0xbe, 0xef}
	if err := client.WriteBlock(ctx, path, 8192, tail); err != nil {
		t.Fatalf("WriteBlock at 8192: %v", err)
	}

	got, err := client.ReadBlock(ctx, path, 0, uint64(len(head)))
	if err != nil {
		t.Fatalf("ReadBlock at 0: %v", err)
	}
	if string(got) != string(head) {
		t.Errorf("read back %q, want %q", got, head)
	}
	if got, err = client.ReadBlock(
		ctx, path, 8192, uint64(len(tail))); err != nil ||
		string(got) != string(tail) {
		t.Errorf("read at 8192 → (%v, %v)", got, err)
	}

	// A write into an existing region replaces exactly that region and
	// leaves its neighbours alone.
	if err := client.WriteBlock(ctx, path, 8192, []byte{0x00, 0x00}); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if got, err = client.ReadBlock(ctx, path, 8192, 4); err != nil ||
		got[0] != 0 || got[1] != 0 || got[2] != 0xbe || got[3] != 0xef {
		t.Errorf("partial overwrite → (%v, %v)", got, err)
	}
	if got, err = client.ReadBlock(
		ctx, path, 0, uint64(len(head))); err != nil ||
		string(got) != string(head) {
		t.Errorf("the header was disturbed: (%q, %v)", got, err)
	}

	// An unwritten region reads as zeros, not as an error.
	if got, err = client.ReadBlock(ctx, path, 12288, 16); err != nil {
		t.Fatalf("ReadBlock of an untouched region: %v", err)
	}
	for i, b := range got {
		if b != 0 {
			t.Fatalf("untouched byte %d = %#x, want 0", i, b)
		}
	}

	// Logging: one record per call, with path/offset/length and never data.
	rec := capture.withMsg(t, "os write block")[0]
	hasAttrs(t, rec, "path", "offset", "length", TraceIdLogKey)
	if rec["path"] != path || rec["offset"] != float64(0) ||
		rec["length"] != float64(len(head)) ||
		rec[TraceIdLogKey] != "block-trace" {
		t.Errorf("os write block record = %v", rec)
	}
	if _, ok := rec["data"]; ok {
		t.Errorf("os write block logged the payload: %v", rec)
	}
	rec = capture.withMsg(t, "os read block")[0]
	hasAttrs(t, rec, "path", "offset", "length", TraceIdLogKey)
	if _, ok := rec["data"]; ok {
		t.Errorf("os read block logged the payload: %v", rec)
	}
}

// A short read is an error, never a silently truncated buffer.
func TestReadBlockShortReadIsError(t *testing.T) {
	capture := captureLogs(t)
	client := NewLimitedOsClient(0)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "small.img")
	if err := os.WriteFile(path, make([]byte, 100), 0o644); err != nil {
		t.Fatalf("create: %v", err)
	}

	if data, err := client.ReadBlock(ctx, path, 0, 4096); err == nil {
		t.Errorf("a short read succeeded with %d bytes", len(data))
	}
	if data, err := client.ReadBlock(ctx, path, 4096, 8); err == nil {
		t.Errorf("a read past the end succeeded with %d bytes", len(data))
	}
	if _, err := client.ReadBlock(
		ctx, filepath.Join(t.TempDir(), "absent"), 0, 8); err == nil {
		t.Error("reading a missing file succeeded")
	}
	// A nonsense length is rejected before anything is allocated, so a
	// corrupt on-disk length field cannot turn into an OOM.
	if _, err := client.ReadBlock(ctx, path, 0, 1<<40); err == nil {
		t.Error("a 1 TiB read of a 100-byte file succeeded")
	}
	if _, err := client.ReadBlock(
		ctx, path, ^uint64(0)-4, 8); err == nil {
		t.Error("an offset+length overflow succeeded")
	}
	if err := client.WriteBlock(
		ctx, filepath.Join(t.TempDir(), "absent"), 0, []byte{1}); err == nil {
		t.Error("writing a missing file succeeded")
	}
	for _, rec := range capture.withMsg(t, "os read block") {
		if _, ok := rec["error"]; !ok {
			t.Errorf("a failed read block has no error attr: %v", rec)
		}
	}
}

// osclient.md §8.8: WriteFileDirect creates a missing file and overwrites an
// existing one in place, with no temp file left behind, and emits its own
// "os write file direct" record.
func TestWriteFileDirect(t *testing.T) {
	capture := captureLogs(t)
	client := NewLimitedOsClient(0)
	ctx := WithTraceId(context.Background(), "direct-trace")
	dir := t.TempDir()
	path := filepath.Join(dir, "ana_state")

	if err := client.WriteFileDirect(ctx, path, "optimized"); err != nil {
		t.Fatalf("WriteFileDirect: %v", err)
	}
	if data, _ := client.ReadFile(ctx, path); data != "optimized" {
		t.Errorf("read back %q", data)
	}
	if err := client.WriteFileDirect(ctx, path, "inaccessible"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if data, _ := client.ReadFile(ctx, path); data != "inaccessible" {
		t.Errorf("after overwrite read back %q", data)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "ana_state" {
		t.Errorf("direct write left extra files: %v", entries)
	}

	rec := capture.withMsg(t, "os write file direct")[0]
	hasAttrs(t, rec, "path", "size", "data", TraceIdLogKey)
	if rec["path"] != path || rec["data"] != "optimized" ||
		rec[TraceIdLogKey] != "direct-trace" {
		t.Errorf("os write file direct record = %v", rec)
	}

	// A path whose directory does not exist fails, and says so.
	err = client.WriteFileDirect(
		ctx, filepath.Join(dir, "missing", "x"), "v")
	if err == nil {
		t.Error("writing into a missing directory succeeded")
	}
	failRec := capture.withMsg(t, "os write file direct")[2]
	if _, ok := failRec["error"]; !ok {
		t.Errorf("failed direct write has no error attr: %v", failRec)
	}
}

// ---------------------------------------------------------------------------
// Raw helpers — WriteBlockAt / ReadBlockDirectAt (osclient.md §4.5.1):
// exported, unlogged, semaphore-free
// ---------------------------------------------------------------------------

// TestReadBlockDirectAt covers the exported raw helpers of osclient.md
// §4.5.1: the write + O_DIRECT read-back the §3.6 leg health probe
// needs, now package functions outside the OsClient. t.TempDir() may sit on
// tmpfs, which rejects O_DIRECT outright, so the round trip is skipped with a
// diagnostic there — the alignment rejection and the log silence are checked
// regardless, since neither reaches the filesystem.
func TestReadBlockDirectAt(t *testing.T) {
	capture := captureLogs(t)
	path := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(path, make([]byte, 4096*4), 0o644); err != nil {
		t.Fatalf("create backing file: %v", err)
	}

	// Misaligned offsets and lengths are rejected before the open, so a
	// caller can never get a partial or page-cached answer.
	for _, bad := range []struct{ offset, length uint64 }{
		{1, 4096}, {0, 4095}, {0, 0}, {4096, 8192 + 1},
	} {
		if _, err := ReadBlockDirectAt(
			path, bad.offset, bad.length); err == nil {
			t.Errorf("ReadBlockDirectAt(off=%d,len=%d) was accepted",
				bad.offset, bad.length)
		}
	}
	// The rejection happens before the open, so a missing path is
	// irrelevant to it.
	if _, err := ReadBlockDirectAt(
		filepath.Join(t.TempDir(), "absent"), 0, 4096); err == nil {
		t.Error("ReadBlockDirectAt of a missing path succeeded")
	}

	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte(i)
	}
	// WriteBlockAt is the write half of the same probe round: a plain
	// O_WRONLY open of a pre-sized file, one pwrite, one fsync. It never
	// creates the target, so a missing path is an error, not an empty file.
	if err := WriteBlockAt(
		filepath.Join(t.TempDir(), "absent"), 0, payload); err == nil {
		t.Error("WriteBlockAt of a missing path succeeded")
	}
	if err := WriteBlockAt(path, 4096, payload); err != nil {
		t.Fatalf("WriteBlockAt: %v", err)
	}
	got, err := ReadBlockDirectAt(path, 4096, 4096)
	switch {
	case errors.Is(err, syscall.EINVAL):
		t.Logf("skipping the O_DIRECT round trip: %v "+
			"(the temp dir does not support it)", err)
	case err != nil:
		t.Fatalf("ReadBlockDirectAt: %v", err)
	default:
		if string(got) != string(payload) {
			t.Errorf("read back %d bytes that differ from the write",
				len(got))
		}
		// A read past the end is a short read, never a truncated buffer.
		if _, err := ReadBlockDirectAt(path, 4096*4, 4096); err == nil {
			t.Error("a read past the end succeeded")
		}
	}

	// The raw helpers are silent by construction: they hold no semaphore
	// slot and emit no record, which is why every direct caller MUST log its
	// own (osclient.md §4.5.1 — the prober's "probe write block" /
	// "probe read block direct").
	if recs := capture.records(t); len(recs) != 0 {
		t.Errorf("the raw helpers logged %d records: %s",
			len(recs), capture.buf.String())
	}
}
