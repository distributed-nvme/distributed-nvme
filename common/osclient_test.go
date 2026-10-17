package common

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// RunCommand (osclient.md, RunCommand)
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

// A command runs in the C locale whatever the caller's (osclient.md,
// RunCommand), so its error words are the ones its callers read; the rest of
// the caller's environment reaches it as it is.
func TestRunCommandRunsInTheCLocale(t *testing.T) {
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Setenv("LANG", "de_DE.UTF-8")
	t.Setenv("LANGUAGE", "de")
	t.Setenv("DNV_OSCLIENT_TEST", "kept")
	client := NewLimitedOsClient(0)

	stdout, _, _, err := client.RunCommand(context.Background(), "sh",
		[]string{"-c", `printf '%s|%s|%s' "$LC_ALL" "$LANG" ` +
			`"$DNV_OSCLIENT_TEST"`}, "")
	if err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if want := "C|de_DE.UTF-8|kept"; stdout != want {
		t.Errorf("the command saw LC_ALL|LANG|DNV_OSCLIENT_TEST = %q, "+
			"want %q", stdout, want)
	}
}

// The caller owns the deadline (osclient.md, RunCommand): the ctx
// firing SIGTERMs the process.
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
// Concurrency limit (osclient.md, Construction and concurrency limit)
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
	if names, err := client.ReadDir(ctx, t.TempDir()); err != context.Canceled || names != nil {
		t.Errorf("ReadDir = (%q, %v), want (nil, context.Canceled)", names, err)
	}
	// A listing waits for a slot like every other call: with the only slot
	// held, it gives up at its deadline and lists nothing.
	short, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShort()
	if names, err := client.ReadDir(short, t.TempDir()); err != context.DeadlineExceeded || names != nil {
		t.Errorf("ReadDir with the only slot held = (%q, %v), "+
			"want (nil, context.DeadlineExceeded)", names, err)
	}
	if mtime, err := client.ReadMtime(ctx, t.TempDir()); err != context.Canceled || !mtime.IsZero() {
		t.Errorf("ReadMtime = (%v, %v), want (zero, context.Canceled)", mtime, err)
	}
	// An mtime read waits for a slot like every other call.
	shortMtime, cancelShortMtime := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShortMtime()
	if mtime, err := client.ReadMtime(shortMtime, t.TempDir()); err != context.DeadlineExceeded || !mtime.IsZero() {
		t.Errorf("ReadMtime with the only slot held = (%v, %v), "+
			"want (zero, context.DeadlineExceeded)", mtime, err)
	}
	if devNo, err := client.ReadDevNo(ctx, "/dev/null"); err != context.Canceled || devNo != "" {
		t.Errorf("ReadDevNo = (%q, %v), want (\"\", context.Canceled)", devNo, err)
	}
	// A device-number read waits for a slot like every other call.
	shortDevNo, cancelShortDevNo := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShortDevNo()
	if devNo, err := client.ReadDevNo(shortDevNo, "/dev/null"); err != context.DeadlineExceeded || devNo != "" {
		t.Errorf("ReadDevNo with the only slot held = (%q, %v), "+
			"want (\"\", context.DeadlineExceeded)", devNo, err)
	}
	if err := client.WriteFile(ctx, filepath.Join(t.TempDir(), "f"), "x"); err != context.Canceled {
		t.Errorf("WriteFile err = %v, want context.Canceled", err)
	}

	// A semaphore-acquire failure logs nothing: the operation never happened.
	if recs := capture.records(t); len(recs) != 0 {
		t.Errorf("blocked calls logged %d records: %s", len(recs), capture.buf.String())
	}
}

// endsOnErrCtx is a ctx that ends the moment it is asked whether it has: its
// Done channel stays open until Err is called, and Err closes it and answers
// context.Canceled, as a ctx canceled just before that call would.
// semaphore.Acquire hands out a free slot without calling Err, so the ctx
// ends after the call took its slot and before its read, where only the
// call's own ctx check can see it.
type endsOnErrCtx struct {
	context.Context
	once sync.Once
	done chan struct{}
}

func newEndsOnErrCtx() *endsOnErrCtx {
	return &endsOnErrCtx{
		Context: context.Background(),
		done:    make(chan struct{}),
	}
}

func (c *endsOnErrCtx) Done() <-chan struct{} { return c.done }

func (c *endsOnErrCtx) Err() error {
	c.once.Do(func() { close(c.done) })
	return context.Canceled
}

// TestInProcessReadsCheckTheCtxAfterTheirSlot: an mtime read and a
// device-number read check the ctx once more after they take their slot
// (osclient.md, ReadMtime and ReadDevNo; dnagent.md SH13). A ctx that ended
// while the call took its slot returns the ctx error, performs nothing and
// logs nothing, and the slot goes back. TestLimitBlocksAndCanceledCtxDoesNotRun
// cannot show this: a ctx that ended before the call never gets a slot.
func TestInProcessReadsCheckTheCtxAfterTheirSlot(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, tc := range []struct {
		name string
		read func(*LimitedOsClient, context.Context) (bool, error)
	}{
		{"ReadMtime", func(c *LimitedOsClient, ctx context.Context) (bool, error) {
			mtime, err := c.ReadMtime(ctx, dir)
			return mtime.IsZero(), err
		}},
		{"ReadDevNo", func(c *LimitedOsClient, ctx context.Context) (bool, error) {
			devNo, err := c.ReadDevNo(ctx, file)
			return devNo == "", err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := captureLogs(t)
			client := NewLimitedOsClient(1)

			if empty, err := tc.read(client, newEndsOnErrCtx()); err != context.Canceled || !empty {
				t.Errorf("%s = (empty %v, %v), want (empty, context.Canceled)",
					tc.name, empty, err)
			}
			if recs := capture.records(t); len(recs) != 0 {
				t.Errorf("a read whose ctx ended after its slot logged %d "+
					"records: %s", len(recs), capture.buf.String())
			}
			// The read gave the only slot back.
			short, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancelShort()
			if _, err := client.ReadFile(short, file); err != nil {
				t.Errorf("a call made after the read = %v, want nil: a read "+
					"whose ctx ended kept the only slot", err)
			}
		})
	}
}

// TestReadDirHoldsItsSlotUntilItReturns: a listing holds its slot for the
// whole read, so listings count against the limit like every other call, and
// gives it back when it returns, after a listing that failed as well. A
// listing that keeps its slot leaves an agent, after as many listings as it
// has slots, with every call waiting for a slot that never comes.
func TestReadDirHoldsItsSlotUntilItReturns(t *testing.T) {
	client := NewLimitedOsClient(1)
	dir := t.TempDir()

	entered := make(chan struct{})
	release := make(chan struct{})
	prev := dirNames
	t.Cleanup(func() { dirNames = prev })
	dirNames = func(f *os.File) ([]string, error) {
		close(entered)
		<-release
		return prev(f)
	}
	done := make(chan error, 1)
	go func() {
		_, err := client.ReadDir(context.Background(), dir)
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("ReadDir returned %v without reading the directory", err)
	}
	// The listing reads: the only slot is its own.
	short, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShort()
	if _, err := client.ReadFile(short, filepath.Join(dir, "x")); err != context.DeadlineExceeded {
		t.Errorf("a call made while a listing reads = %v, want "+
			"context.DeadlineExceeded: the listing holds the only slot", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	dirNames = prev

	for _, path := range []string{dir, filepath.Join(dir, "missing"), dir} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := client.ReadDir(ctx, path)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ReadDir(%s) found no free slot: an earlier listing "+
				"kept it", path)
		}
	}
}

// TestReadMtimeGivesItsSlotBack: an mtime read holds one slot like a listing
// and gives it back when it returns, after a read that failed, as "not
// exist" or otherwise, as well as after one that answered. A read that keeps
// its slot leaves an agent, after as many age reads as it has slots, with
// every call waiting for a slot that never comes.
func TestReadMtimeGivesItsSlotBack(t *testing.T) {
	client := NewLimitedOsClient(1)
	dir := t.TempDir()
	file := filepath.Join(dir, "attr_serial")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, path := range []string{
		dir,
		filepath.Join(dir, "missing"),
		filepath.Join(file, "below"),
		dir,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := client.ReadMtime(ctx, path)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ReadMtime(%s) found no free slot: an earlier read "+
				"kept it", path)
		}
	}
	// Every other call finds the slot free as well.
	short, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShort()
	if _, err := client.ReadFile(short, file); err != nil {
		t.Errorf("a call made after the mtime reads = %v, want nil: an "+
			"mtime read kept the only slot", err)
	}
}

// TestReadDevNoGivesItsSlotBack: a device-number read holds one slot like an
// mtime read and gives it back when it returns, after a read that failed, as
// "not exist" or otherwise, as well as after one that answered. A read that
// keeps its slot leaves an agent, after as many size reads of its disk as it
// has slots, with every call waiting for a slot that never comes.
func TestReadDevNoGivesItsSlotBack(t *testing.T) {
	client := NewLimitedOsClient(1)
	dir := t.TempDir()
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, path := range []string{
		blockDevForTest(t),
		filepath.Join(dir, "missing"),
		file,
		blockDevForTest(t),
	} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := client.ReadDevNo(ctx, path)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ReadDevNo(%s) found no free slot: an earlier read "+
				"kept it", path)
		}
	}
	// Every other call finds the slot free as well.
	short, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShort()
	if _, err := client.ReadFile(short, file); err != nil {
		t.Errorf("a call made after the device-number reads = %v, want "+
			"nil: a device-number read kept the only slot", err)
	}
}

// ---------------------------------------------------------------------------
// File and proto I/O (osclient.md, ReadFile / WriteFile / WriteFileDirect;
// ReadProto / WriteProto)
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

// TestAtomicWriteSyncsTheDirAfterTheRename: the rename that publishes a
// replacement is a change to the DIRECTORY, and nothing makes it survive a
// crash until the directory is fsynced — before that the old file can come
// back, or, for a first write, no file at all, whatever the temp file's own
// fsync promised. So every WriteFile and WriteProto fsyncs the file's own
// directory exactly once, after the rename (the seam finds the new bytes
// already at path), and a directory fsync that fails fails the write.
func TestAtomicWriteSyncsTheDirAfterTheRename(t *testing.T) {
	client := NewLimitedOsClient(0)
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "state")

	type dirSync struct {
		dir  string
		held string // what path held when the directory was fsynced
	}
	var syncs []dirSync
	var failWith error
	prev := syncDir
	t.Cleanup(func() { syncDir = prev })
	syncDir = func(d string) error {
		held, err := os.ReadFile(path)
		if err != nil {
			held = []byte("<" + err.Error() + ">")
		}
		syncs = append(syncs, dirSync{dir: d, held: string(held)})
		if failWith != nil {
			return failWith
		}
		return prev(d)
	}
	expectOneSync := func(what string, want string) {
		t.Helper()
		if len(syncs) != 1 {
			t.Fatalf("%s: %d directory fsyncs, want exactly 1", what,
				len(syncs))
		}
		if syncs[0].dir != dir {
			t.Errorf("%s: fsynced %q, want the file's own directory %q",
				what, syncs[0].dir, dir)
		}
		if syncs[0].held != want {
			t.Errorf("%s: at the directory fsync the file held %q, want "+
				"%q — the fsync ran before the rename", what,
				syncs[0].held, want)
		}
		syncs = nil
	}

	if err := client.WriteFile(ctx, path, "first version"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	expectOneSync("first write", "first version")
	if err := client.WriteFile(ctx, path, "second version"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	expectOneSync("overwrite", "second version")

	msg := &pb.SyncupDnRequest{ClusterId: 1, DnId: 3, Revision: 7}
	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := client.WriteProto(ctx, path, msg); err != nil {
		t.Fatalf("WriteProto: %v", err)
	}
	expectOneSync("WriteProto", string(raw))

	// The rename happened, but nothing says it survives a crash, and the
	// callers' crash protocols depend on exactly that.
	failWith = errors.New("injected directory fsync failure")
	if err := client.WriteFile(ctx, path, "third version"); !errors.Is(
		err, failWith) {
		t.Errorf("a failed directory fsync returned %v, want it", err)
	}
	expectOneSync("failed directory fsync", "third version")
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
// ReadDir (osclient.md, ReadDir)
// ---------------------------------------------------------------------------

// TestReadDirSortsAndKeepsEveryName: a listing returns every name of the
// directory as it is, of any kind of entry, dot names and spaces at either end
// included, in byte order, the order `ls` prints in the C locale: upper case
// before lower case, "nvme-subsys10" before "nvme-subsys2". The entries are
// created in another order. A FIFO and a link that leads nowhere sit among
// them: a listing that opened its entries would wait on the one, and one that
// stat'ed them would fail on the other. A symlink to the directory, the shape
// of a sysfs class entry, lists the directory.
func TestReadDirSortsAndKeepsEveryName(t *testing.T) {
	client := NewLimitedOsClient(0)
	ctx := context.Background()
	dir := t.TempDir()

	for _, name := range []string{
		"nvme-subsys2", "trail ", ".hidden", "a b", "Zed", "md127", "B",
		"_under", "10", "a", " lead", "a.b", "b2", "..dots",
		"nvme-subsys1", "é", "9", "nvme-subsys10", "A", "b10",
	} {
		path := filepath.Join(dir, name)
		var err error
		switch name {
		case "md127":
			err = os.Mkdir(path, 0o755)
		case "nvme-subsys2":
			err = os.Symlink(filepath.Join(dir, "gone"), path)
		case "9":
			err = syscall.Mkfifo(path, 0o600)
		default:
			err = os.WriteFile(path, nil, 0o644)
		}
		if err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}
	want := []string{
		" lead", "..dots", ".hidden", "10", "9", "A", "B", "Zed", "_under",
		"a", "a b", "a.b", "b10", "b2", "md127", "nvme-subsys1",
		"nvme-subsys10", "nvme-subsys2", "trail ", "é",
	}

	names, err := client.ReadDir(ctx, dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if !slices.Equal(names, want) {
		t.Errorf("ReadDir = %q\nwant      %q", names, want)
	}

	link := filepath.Join(t.TempDir(), "nvme-subsys0")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if names, err := client.ReadDir(ctx, link); err != nil ||
		!slices.Equal(names, want) {
		t.Errorf("ReadDir through a symlink to the directory = (%q, %v), "+
			"want the directory's names", names, err)
	}
}

// TestReadDirMissingIsNotExist: a missing directory is the one failure a
// caller reads as absent, so its error keeps fs.ErrNotExist in the chain —
// whether the directory, its parent or the target of a link to it is
// missing — and no names come with it.
func TestReadDirMissingIsNotExist(t *testing.T) {
	client := NewLimitedOsClient(0)
	ctx := context.Background()
	dir := t.TempDir()
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "gone"), dangling); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	for _, path := range []string{
		filepath.Join(dir, "missing"),
		filepath.Join(dir, "missing", "below"),
		dangling,
	} {
		names, err := client.ReadDir(ctx, path)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("ReadDir(%s) err = %v, want one wrapping fs.ErrNotExist",
				path, err)
		}
		if names != nil {
			t.Errorf("ReadDir(%s) returned %q with its error", path, names)
		}
	}
}

// TestReadDirRefusesANonDirectory: a path that is not a directory, or that
// runs through one that is not, is an error other than fs.ErrNotExist, so no
// caller reads it as absent, and it comes back at once. A FIFO is the case
// that could block: a plain open of one waits until a writer comes, and a
// listing never waits on what sits at its path.
func TestReadDirRefusesANonDirectory(t *testing.T) {
	client := NewLimitedOsClient(0)
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("create: %v", err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	type result struct {
		names []string
		err   error
	}
	for _, path := range []string{file, filepath.Join(file, "below"), fifo} {
		done := make(chan result, 1)
		go func() {
			names, err := client.ReadDir(ctx, path)
			done <- result{names: names, err: err}
		}()
		var got result
		select {
		case got = <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("ReadDir(%s) has not returned after 2s", path)
			// A writer ends a blocked open of a FIFO, so the call returns
			// inside the test.
			if w, err := os.OpenFile(
				path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				w.Close()
			}
			select {
			case got = <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("ReadDir(%s) has not returned after a writer came", path)
			}
		}
		if got.err == nil || errors.Is(got.err, fs.ErrNotExist) {
			t.Errorf("ReadDir(%s) err = %v, want an error that is not "+
				"fs.ErrNotExist", path, got.err)
		}
		if got.names != nil {
			t.Errorf("ReadDir(%s) returned %q with its error", path, got.names)
		}
	}
}

// TestReadDirPartWayFailureReturnsNothing: a read that fails after some names
// returns none of them, only the error. Part of a listing taken for the whole
// would make a present entry read as absent.
func TestReadDirPartWayFailureReturnsNothing(t *testing.T) {
	client := NewLimitedOsClient(0)
	ctx := context.Background()
	dir := t.TempDir()
	for _, name := range []string{"md125", "md126", "md127"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	injected := errors.New("injected read failure")
	prev := dirNames
	t.Cleanup(func() { dirNames = prev })
	dirNames = func(f *os.File) ([]string, error) {
		names, err := prev(f)
		if err != nil {
			return names, err
		}
		// What a read that fails part-way returns: the names read before
		// the failure, and the error.
		return names[:2], injected
	}

	names, err := client.ReadDir(ctx, dir)
	if !errors.Is(err, injected) {
		t.Errorf("ReadDir err = %v, want the failed read's error", err)
	}
	if names != nil {
		t.Errorf("a listing that failed part-way returned %q", names)
	}
}

// TestReadDirClosesTheDirectory: a listing closes the directory it opened
// before it returns, one that fails part-way included. A directory left open
// holds its descriptor until a garbage collection closes it, and an agent
// lists many directories in one check round.
func TestReadDirClosesTheDirectory(t *testing.T) {
	client := NewLimitedOsClient(0)
	ctx := context.Background()
	dir := t.TempDir()

	var opened *os.File
	var failWith error
	prev := dirNames
	t.Cleanup(func() { dirNames = prev })
	dirNames = func(f *os.File) ([]string, error) {
		opened = f
		names, err := prev(f)
		if failWith != nil {
			return names, failWith
		}
		return names, err
	}

	for _, fail := range []error{nil, errors.New("injected read failure")} {
		opened, failWith = nil, fail
		if _, err := client.ReadDir(ctx, dir); !errors.Is(err, fail) {
			t.Fatalf("ReadDir err = %v, want %v", err, fail)
		}
		if opened == nil {
			t.Fatal("ReadDir read no directory")
		}
		// A second close fails with os.ErrClosed; a close that succeeds
		// here closes what the listing left open.
		if err := opened.Close(); !errors.Is(err, os.ErrClosed) {
			t.Errorf("the directory of a listing (read failure: %v) is "+
				"still open after ReadDir returned", fail)
		}
	}
}

// ---------------------------------------------------------------------------
// ReadMtime (osclient.md, ReadMtime)
// ---------------------------------------------------------------------------

// TestReadMtimeOfARealDirectory runs an mtime read on a real directory: it
// answers the time the directory holds, to the nanosecond the file system
// keeps, not rounded to the second. It reads the path itself and follows no
// link: a link to the directory answers the link's own time, and a link that
// leads nowhere answers its time too, never "not exist". A missing path, or
// one under a missing directory, is the one failure a caller reads as
// absent: its error wraps fs.ErrNotExist. A path that runs through a file is
// an error that is not, so no caller reads it as absent. Every failure comes
// with the zero time.
func TestReadMtimeOfARealDirectory(t *testing.T) {
	client := NewLimitedOsClient(0)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "subsys")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Old, and not a whole second: a read that followed a link, or one that
	// rounded, would not answer exactly this.
	want := time.Date(2001, 2, 3, 4, 5, 6, 789012345, time.Local)
	if err := os.Chtimes(dir, want, want); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	got, err := client.ReadMtime(ctx, dir)
	if err != nil || !got.Equal(want) {
		t.Fatalf("ReadMtime(%s) = (%v, %v), want (%v, nil)", dir, got, err,
			want)
	}

	for _, target := range []string{dir, filepath.Join(dir, "gone")} {
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		info, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("lstat: %v", err)
		}
		got, err := client.ReadMtime(ctx, link)
		if err != nil || !got.Equal(info.ModTime()) {
			t.Errorf("ReadMtime of a link to %s = (%v, %v), want the "+
				"link's own time (%v, nil)", target, got, err,
				info.ModTime())
		}
	}

	for _, path := range []string{
		filepath.Join(dir, "missing"),
		filepath.Join(dir, "missing", "below"),
	} {
		got, err := client.ReadMtime(ctx, path)
		if !errors.Is(err, fs.ErrNotExist) || !got.IsZero() {
			t.Errorf("ReadMtime(%s) = (%v, %v), want (zero, an error "+
				"wrapping fs.ErrNotExist)", path, got, err)
		}
	}

	file := filepath.Join(dir, "attr_serial")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err = client.ReadMtime(ctx, filepath.Join(file, "below"))
	if err == nil || errors.Is(err, fs.ErrNotExist) || !got.IsZero() {
		t.Errorf("ReadMtime through a file = (%v, %v), want (zero, an "+
			"error that is not fs.ErrNotExist)", got, err)
	}
}

// ---------------------------------------------------------------------------
// ReadDevNo (osclient.md, ReadDevNo)
// ---------------------------------------------------------------------------

// blockDevForTest is the /dev node of a block device of this machine, the
// first sysfs lists that has one; "/dev/null", a character device, when
// none has, so a caller that needs a block device skips on its answer.
func blockDevForTest(t *testing.T) string {
	t.Helper()
	names, err := os.ReadDir("/sys/class/block")
	if err != nil {
		return "/dev/null"
	}
	for _, entry := range names {
		dev := filepath.Join("/dev", entry.Name())
		if info, err := os.Stat(dev); err == nil &&
			info.Mode()&os.ModeDevice != 0 &&
			info.Mode()&os.ModeCharDevice == 0 {
			return dev
		}
	}
	return "/dev/null"
}

// TestReadDevNoOfARealDevice runs a device-number read on this machine's
// block devices: each answers the number sysfs prints in its "dev" file, and
// so does a link to it, since the read follows links as the /dev/disk/by-*
// names of a disk need. A missing path, or a link that leads nowhere, is the
// one failure that wraps fs.ErrNotExist. A path that is not a block device —
// a plain file, a directory, a character device — is an error that does not,
// so no caller takes it for a device that is gone. Every failure comes with
// "".
func TestReadDevNoOfARealDevice(t *testing.T) {
	client := NewLimitedOsClient(0)
	ctx := context.Background()
	names, err := os.ReadDir("/sys/class/block")
	if err != nil {
		t.Skipf("no /sys/class/block: %v", err)
	}
	read := 0
	for _, entry := range names {
		dev := filepath.Join("/dev", entry.Name())
		if _, err := os.Stat(dev); err != nil {
			continue
		}
		want, err := os.ReadFile(filepath.Join(
			"/sys/class/block", entry.Name(), "dev"))
		if err != nil {
			t.Fatalf("read %s's dev: %v", entry.Name(), err)
		}
		got, err := client.ReadDevNo(ctx, dev)
		if err != nil || got != strings.TrimSpace(string(want)) {
			t.Errorf("ReadDevNo(%s) = (%q, %v), want (%q, nil)", dev, got,
				err, strings.TrimSpace(string(want)))
		}
		link := filepath.Join(t.TempDir(), "by-id")
		if err := os.Symlink(dev, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if got, err := client.ReadDevNo(ctx, link); err != nil ||
			got != strings.TrimSpace(string(want)) {
			t.Errorf("ReadDevNo of a link to %s = (%q, %v), want (%q, nil)",
				dev, got, err, strings.TrimSpace(string(want)))
		}
		read++
	}
	if read == 0 {
		t.Log("no block device of this machine has a /dev node: only " +
			"the failures are read")
	}

	dir := t.TempDir()
	nowhere := filepath.Join(dir, "nowhere")
	if err := os.Symlink(filepath.Join(dir, "gone"), nowhere); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	for _, path := range []string{
		filepath.Join(dir, "missing"),
		filepath.Join(dir, "missing", "below"),
		nowhere,
	} {
		got, err := client.ReadDevNo(ctx, path)
		if !errors.Is(err, fs.ErrNotExist) || got != "" {
			t.Errorf("ReadDevNo(%s) = (%q, %v), want (\"\", an error "+
				"wrapping fs.ErrNotExist)", path, got, err)
		}
	}

	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, path := range []string{file, dir, "/dev/null"} {
		got, err := client.ReadDevNo(ctx, path)
		if err == nil || errors.Is(err, fs.ErrNotExist) || got != "" {
			t.Errorf("ReadDevNo(%s) = (%q, %v), want (\"\", an error that "+
				"is not fs.ErrNotExist)", path, got, err)
		}
	}
}

// TestDevNoStringDecodesLinuxDevT pins the decoding of a device number
// against the C library's makedev: the major's low 12 bits sit at bit 8 and
// the rest at bit 44, the minor's low 8 bits at bit 0 and the rest at bit 20.
// A number past either low part — an NVMe namespace's major, a minor past
// 255 — reads back whole.
func TestDevNoStringDecodesLinuxDevT(t *testing.T) {
	makedev := func(major, minor uint64) uint64 {
		return minor&0xff | (major&0xfff)<<8 |
			(minor&^uint64(0xff))<<12 | (major&^uint64(0xfff))<<32
	}
	for _, tc := range []struct{ major, minor uint64 }{
		{7, 0}, {8, 17}, {253, 4}, {259, 3}, {259, 1048575},
		{4095, 255}, {4096, 256}, {1<<32 - 1, 1<<32 - 1},
	} {
		want := fmt.Sprintf("%d:%d", tc.major, tc.minor)
		if got := devNoString(makedev(tc.major, tc.minor)); got != want {
			t.Errorf("devNoString(makedev(%d, %d)) = %q, want %q",
				tc.major, tc.minor, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Logging (osclient.md, Logging)
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

	// 6. directory listings: every name, never truncated, as an array; an
	// empty listing logs an empty array, and a failed one its error
	listed := filepath.Join(dir, "listed")
	empty := filepath.Join(dir, "empty")
	for _, d := range []string{listed, empty} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	// More names, and a longer one, than TruncForLog keeps; byte order puts
	// the long name first.
	names := []string{strings.Repeat("l", LogStrDataLimit+50)}
	for i := 0; i < 40; i++ {
		names = append(names, fmt.Sprintf("md%03d", i))
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(listed, name), nil, 0o644); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	if _, err := client.ReadDir(ctx, listed); err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if _, err := client.ReadDir(ctx, empty); err != nil {
		t.Fatalf("ReadDir of an empty directory: %v", err)
	}
	missing := filepath.Join(dir, "missing")
	if _, err := client.ReadDir(ctx, missing); err == nil {
		t.Fatal("listing a missing directory succeeded")
	}
	dirRecs := capture.withMsg(t, "os read dir")
	if len(dirRecs) != 3 {
		t.Fatalf("%d os read dir records for 3 listings\n%s", len(dirRecs),
			capture.buf.String())
	}
	for i, want := range []struct {
		path   string
		names  []string
		failed bool
	}{
		{path: listed, names: names},
		{path: empty},
		{path: missing, failed: true},
	} {
		rec := dirRecs[i]
		hasAttrs(t, rec, "path", "count", "names", TraceIdLogKey)
		if rec["path"] != want.path {
			t.Errorf("os read dir: path = %v, want %s", rec["path"], want.path)
		}
		if rec["count"] != float64(len(want.names)) {
			t.Errorf("os read dir %s: count = %v, want %d", want.path,
				rec["count"], len(want.names))
		}
		got, ok := rec["names"].([]any)
		if !ok {
			t.Errorf("os read dir %s: names = %v, want an array", want.path,
				rec["names"])
		} else if !slices.Equal(got, toAny(want.names)) {
			t.Errorf("os read dir %s: names = %q, want every name %q",
				want.path, got, want.names)
		}
		if _, has := rec["error"]; has != want.failed {
			t.Errorf("os read dir %s: error attr present = %v, want %v: %v",
				want.path, has, want.failed, rec)
		}
	}

	// 7. mtime reads: the path and the time read, and a failed one its
	// error
	stamped := time.Date(2001, 2, 3, 4, 5, 6, 789012345, time.UTC)
	if err := os.Chtimes(listed, stamped, stamped); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if _, err := client.ReadMtime(ctx, listed); err != nil {
		t.Fatalf("ReadMtime: %v", err)
	}
	if _, err := client.ReadMtime(ctx, missing); err == nil {
		t.Fatal("reading the mtime of a missing directory succeeded")
	}
	mtimeRecs := capture.withMsg(t, "os read mtime")
	if len(mtimeRecs) != 2 {
		t.Fatalf("%d os read mtime records for 2 reads\n%s", len(mtimeRecs),
			capture.buf.String())
	}
	for i, want := range []struct {
		path   string
		failed bool
	}{
		{path: listed},
		{path: missing, failed: true},
	} {
		rec := mtimeRecs[i]
		hasAttrs(t, rec, "path", "mtime", TraceIdLogKey)
		if rec["path"] != want.path {
			t.Errorf("os read mtime: path = %v, want %s", rec["path"],
				want.path)
		}
		if _, has := rec["error"]; has != want.failed {
			t.Errorf("os read mtime %s: error attr present = %v, want %v: "+
				"%v", want.path, has, want.failed, rec)
		}
	}
	if logged, err := time.Parse(time.RFC3339Nano,
		fmt.Sprint(mtimeRecs[0]["mtime"])); err != nil ||
		!logged.Equal(stamped) {
		t.Errorf("os read mtime: mtime = %v, want %v", mtimeRecs[0]["mtime"],
			stamped)
	}

	// 8. device-number reads: the path and the number read, "" and the
	// error for a failed one
	dev := blockDevForTest(t)
	devNo, devNoErr := client.ReadDevNo(ctx, dev)
	if _, err := client.ReadDevNo(ctx, missing); err == nil {
		t.Fatal("reading the device number of a missing path succeeded")
	}
	devNoRecs := capture.withMsg(t, "os read devno")
	if len(devNoRecs) != 2 {
		t.Fatalf("%d os read devno records for 2 reads\n%s", len(devNoRecs),
			capture.buf.String())
	}
	for i, want := range []struct {
		path   string
		devNo  string
		failed bool
	}{
		{path: dev, devNo: devNo, failed: devNoErr != nil},
		{path: missing, failed: true},
	} {
		rec := devNoRecs[i]
		hasAttrs(t, rec, "path", "devno", TraceIdLogKey)
		if rec["path"] != want.path || rec["devno"] != want.devNo {
			t.Errorf("os read devno: (path, devno) = (%v, %v), want (%s, %q)",
				rec["path"], rec["devno"], want.path, want.devNo)
		}
		if _, has := rec["error"]; has != want.failed {
			t.Errorf("os read devno %s: error attr present = %v, want %v: "+
				"%v", want.path, has, want.failed, rec)
		}
	}
}

// toAny is a []string as a decoded JSON array holds it.
func toAny(ss []string) []any {
	out := make([]any, 0, len(ss))
	for _, s := range ss {
		out = append(out, s)
	}
	return out
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
	// An unset ReadDirFn is an empty listing, never a missing directory.
	if names, err := fake.ReadDir(ctx, "/x"); names != nil || err != nil {
		t.Errorf("unset ReadDirFn = (%q, %v)", names, err)
	}
	// An unset ReadMtimeFn fails, and not as a missing path: a zero time
	// reads as older than any age, and a missing path as gone.
	if mtime, err := fake.ReadMtime(ctx, "/x"); err == nil ||
		errors.Is(err, fs.ErrNotExist) || !mtime.IsZero() {
		t.Errorf("unset ReadMtimeFn = (%v, %v), want (zero, an error that "+
			"is not fs.ErrNotExist)", mtime, err)
	}
	if devNo, err := fake.ReadDevNo(ctx, "/x"); devNo != "" || err != nil {
		t.Errorf("unset ReadDevNoFn = (%q, %v)", devNo, err)
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

	var gotPath string
	fake.ReadDirFn = func(_ context.Context, path string) ([]string, error) {
		gotPath = path
		return []string{"md127"}, nil
	}
	if names, err := fake.ReadDir(ctx, "/sys/block"); err != nil ||
		!slices.Equal(names, []string{"md127"}) || gotPath != "/sys/block" {
		t.Errorf("stubbed ReadDir → (%q, %v), stub saw %q", names, err, gotPath)
	}
	// A stubbed failure reaches the caller as the stub returns it, so a
	// missing directory stays one.
	fake.ReadDirFn = func(_ context.Context, path string) ([]string, error) {
		return nil, fmt.Errorf("list %s: %w", path, fs.ErrNotExist)
	}
	if names, err := fake.ReadDir(ctx, "/sys/block"); !errors.Is(err, fs.ErrNotExist) ||
		names != nil {
		t.Errorf("stubbed missing directory → (%q, %v), want (nil, an error "+
			"wrapping fs.ErrNotExist)", names, err)
	}

	stamp := time.Unix(1000, 5)
	fake.ReadMtimeFn = func(_ context.Context, path string) (time.Time, error) {
		gotPath = path
		return stamp, nil
	}
	if mtime, err := fake.ReadMtime(ctx, "/sys/kernel/config"); err != nil ||
		!mtime.Equal(stamp) || gotPath != "/sys/kernel/config" {
		t.Errorf("stubbed ReadMtime → (%v, %v), stub saw %q", mtime, err,
			gotPath)
	}

	fake.ReadDevNoFn = func(_ context.Context, path string) (string, error) {
		gotPath = path
		return "259:3", nil
	}
	if devNo, err := fake.ReadDevNo(ctx, "/dev/nvme0n1"); err != nil ||
		devNo != "259:3" || gotPath != "/dev/nvme0n1" {
		t.Errorf("stubbed ReadDevNo → (%q, %v), stub saw %q", devNo, err,
			gotPath)
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
// ReadBlock / WriteBlock (osclient.md, ReadBlock / WriteBlock —
// architecture.md [D13])
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

// osclient.md, ReadFile / WriteFile / WriteFileDirect: WriteFileDirect creates
// a missing file and overwrites an existing one in place, with no temp file
// left behind, and emits its own "os write file direct" record.
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
// Raw helpers — WriteBlockAt / ReadBlockDirectAt (osclient.md, Exported raw
// helpers and the probe-IO carve-out):
// exported, unlogged, semaphore-free
// ---------------------------------------------------------------------------

// TestReadBlockDirectAtIsCloseOnExec: the descriptor the direct read opens is
// close-on-exec. The probe can sit in its pread for as long as the leg queues
// IO, and every command the agent forks meanwhile — the nvme disconnect that
// will release the probe among them — would otherwise inherit the leg wrapper
// open; one that outlives the probe keeps it open, and the teardown's dmsetup
// remove fails EBUSY until that command exits. The flags are read off the
// live descriptor through the rawOpen seam, before the helper closes it.
func TestReadBlockDirectAtIsCloseOnExec(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(path, make([]byte, 4096), 0o644); err != nil {
		t.Fatalf("create backing file: %v", err)
	}
	var fdFlags []uintptr
	prev := rawOpen
	t.Cleanup(func() { rawOpen = prev })
	rawOpen = func(p string, mode int, perm uint32) (int, error) {
		fd, err := prev(p, mode, perm)
		if err == nil {
			flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL,
				uintptr(fd), syscall.F_GETFD, 0)
			if errno != 0 {
				t.Errorf("fcntl(F_GETFD): %v", errno)
			}
			fdFlags = append(fdFlags, flags)
		}
		return fd, err
	}

	_, err := ReadBlockDirectAt(path, 0, 4096)
	if errors.Is(err, syscall.EINVAL) {
		t.Skipf("the temp dir does not support O_DIRECT: %v", err)
	}
	if err != nil {
		t.Fatalf("ReadBlockDirectAt: %v", err)
	}
	if len(fdFlags) != 1 {
		t.Fatalf("%d opens through the seam, want exactly 1", len(fdFlags))
	}
	if fdFlags[0]&syscall.FD_CLOEXEC == 0 {
		t.Errorf("the direct-read descriptor is not close-on-exec "+
			"(F_GETFD = %#x): a child forked while the probe blocks "+
			"inherits the leg wrapper open", fdFlags[0])
	}
}

// TestReadBlockDirectAt covers the exported raw helpers of osclient.md,
// Exported raw helpers and the probe-IO carve-out: the write + O_DIRECT
// read-back the leg health probe needs (architecture.md, Group on-leg
// layout: meta region, data region, health block), which are package
// functions outside the OsClient. t.TempDir() may sit on
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
	// own (osclient.md, Exported raw helpers and the probe-IO carve-out —
	// the prober's "probe write block" / "probe read block direct").
	if recs := capture.records(t); len(recs) != 0 {
		t.Errorf("the raw helpers logged %d records: %s",
			len(recs), capture.buf.String())
	}
}
