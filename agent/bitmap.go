package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// Bitmap conventions (architecture.md, Bitmap push protocol and
// raid0 bitmap math):
//
//   - Every Gateway/etcd/Push*Bitmap bitmap is "1 = unwritten/skippable";
//     thin-pool metadata and the raid0 bitmap math formulas are
//     "written = 1". The inversion happens exactly once, where thin-pool
//     metadata is read — so the chunks that reach an agent are already in
//     wire convention and a set bit means "this region need not be copied".
//   - Bits are addressed LSB-first inside each byte: bit i lives in
//     bitmap[i/8] at mask 1<<(i%8). When the logical bit count is not a
//     multiple of 8 the trailing pad bits of the last byte are 0.
//   - Chunks are byte-aligned, so migration's bit-level concatenation
//     (chunk k starts at the summed bit length of chunks 0…k−1) is plain byte
//     concatenation — while every chunk but the last covers a number of
//     blocks that is a multiple of 8: a chunk made from a page whose block
//     count is not a multiple of 8 carries its pad bits in the middle of the
//     joined bitmap (architecture.md, Bitmap reads).
const bitsPerByte = 8

// BitmapBitCount is the number of bits a chunk carries.
//
// It is for **wire chunks only**, where every bit is meaningful but the zero
// pad bits of the last chunk's last byte, which read as written, the safe
// direction, as long as every chunk but the last covers a number of blocks
// that is a multiple of 8 (the rule above; architecture.md, Bitmap reads).
// It must never be used as the bit count of a "written = 1" bitmap whose
// logical length is not a multiple of 8: there its zero pad bits would count
// as real unwritten blocks.
func BitmapBitCount(bitmap []byte) uint64 {
	return uint64(len(bitmap)) * bitsPerByte
}

// BitmapBit reads one bit of a bitmap; out-of-range bits read as 0.
func BitmapBit(bitmap []byte, idx uint64) bool {
	byteIdx := idx / bitsPerByte
	if byteIdx >= uint64(len(bitmap)) {
		return false
	}
	return bitmap[byteIdx]&(1<<(idx%bitsPerByte)) != 0
}

// ChunkSet holds the migration bitmap chunks of one migration that the agent
// loaded from disk or received since. bm_idx is the append sequence, so the
// set is interpretable only as a ContiguousPrefix (SH23). The applied set
// reported through BitmapInfo.bm_idx_list is always derived from it, so it
// survives restarts (SH21). Clone chunks are addressed by the pair
// (src_slice_idx, bm_idx) and live in CloneChunkSet below.
type ChunkSet struct {
	chunks map[uint32][]byte
}

func NewChunkSet() *ChunkSet {
	return &ChunkSet{chunks: make(map[uint32][]byte)}
}

func (c *ChunkSet) Put(bmIdx uint32, bitmap []byte) {
	c.chunks[bmIdx] = bitmap
}

func (c *ChunkSet) Delete(bmIdx uint32) {
	delete(c.chunks, bmIdx)
}

// Get reads one chunk by its bm_idx. It places nothing: a migration chunk is
// interpretable only inside the ContiguousPrefix its lower neighbours build
// (SH23).
func (c *ChunkSet) Get(bmIdx uint32) ([]byte, bool) {
	bitmap, ok := c.chunks[bmIdx]
	return bitmap, ok
}

func (c *ChunkSet) Len() int {
	return len(c.chunks)
}

// Indexes lists every chunk present, ascending — the applied set of SH21.
func (c *ChunkSet) Indexes() []uint32 {
	out := make([]uint32, 0, len(c.chunks))
	for bmIdx := range c.chunks {
		out = append(out, bmIdx)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ContiguousPrefix concatenates the longest run of chunks starting at
// bm_idx 0. Migration chunks are interpretable only as such a prefix: chunk
// k's first bit sits at the summed bit length of chunks 0…k−1, so a gap makes
// every later chunk unplaceable (SH23). The worker's ascending, one-in-flight
// push makes gaps unreachable in practice; the applied set (Indexes) still
// reports every chunk in the set, not only this prefix.
func (c *ChunkSet) ContiguousPrefix() []byte {
	var out []byte
	for bmIdx := uint32(0); ; bmIdx++ {
		chunk, ok := c.chunks[bmIdx]
		if !ok {
			return out
		}
		out = append(out, chunk...)
	}
}

// CloneChunkKey addresses one clone bitmap chunk: the source slice it
// describes and its index within that slice's bitmap (architecture.md,
// Bitmap push protocol).
type CloneChunkKey struct {
	SliceIdx uint32
	BmIdx    uint32
}

// CloneChunkSet holds the clone bitmap chunks an agent currently has on disk
// for one clone. Chunk (s, b) carries bytes [b*CloneBmChunkBytes, …) of source
// slice s's bitmap, so every chunk is **self-positioned** by its key alone
// (SH22): chunks may arrive in any order, a chunk may be missing entirely, and
// one may keep growing in place up to its fixed capacity ([D8]). There is
// deliberately no ContiguousPrefix analogue — concatenating clone chunks would
// place them at the wrong offsets; the fold of architecture.md,
// raid0 bitmap math, reads them in place instead.
// The applied set reported through BitmapInfo.chunk_id_list is always derived
// from this set, so it survives restarts (SH21).
type CloneChunkSet struct {
	chunks map[CloneChunkKey][]byte
}

func NewCloneChunkSet() *CloneChunkSet {
	return &CloneChunkSet{chunks: make(map[CloneChunkKey][]byte)}
}

func (c *CloneChunkSet) Put(key CloneChunkKey, bitmap []byte) {
	c.chunks[key] = bitmap
}

func (c *CloneChunkSet) Get(key CloneChunkKey) ([]byte, bool) {
	bitmap, ok := c.chunks[key]
	return bitmap, ok
}

func (c *CloneChunkSet) Delete(key CloneChunkKey) {
	delete(c.chunks, key)
}

func (c *CloneChunkSet) Len() int {
	return len(c.chunks)
}

// Ids lists every chunk present, ascending by (SliceIdx, BmIdx) — the applied
// set of SH21. The order is deterministic only: clone chunks are
// order-independent, so nothing downstream may depend on it for placement.
func (c *CloneChunkSet) Ids() []CloneChunkKey {
	out := make([]CloneChunkKey, 0, len(c.chunks))
	for key := range c.chunks {
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SliceIdx != out[j].SliceIdx {
			return out[i].SliceIdx < out[j].SliceIdx
		}
		return out[i].BmIdx < out[j].BmIdx
	})
	return out
}

// SkipRange is one contiguous byte range of a dm-clone device that may be
// marked hydrated without copying.
type SkipRange struct {
	Offset uint64
	Length uint64
}

// SkipRanges turns a wire bitmap into coalesced dm-clone byte ranges.
//
// Bit i of the bitmap describes region shiftRegions+i of the device — the dn
// shift by the leg's meta_blocks, whose blocks are never skippable because
// the md superblock and write-intent bitmap must be copied verbatim
// (architecture.md, Migrations, and
// Group on-leg layout: meta region, data region, health block). regionSize is
// the dm-clone region (= the SP's block_size) and
// regionCnt bounds the device, so a bitmap longer than the device discards
// nothing beyond it.
func SkipRanges(
	bitmap []byte,
	shiftRegions uint64,
	regionCnt uint64,
	regionSize uint64,
) []SkipRange {
	if regionSize == 0 {
		return nil
	}
	var ranges []SkipRange
	limit := shiftRegions + BitmapBitCount(bitmap)
	if limit > regionCnt {
		limit = regionCnt
	}
	runStart := uint64(0)
	runLen := uint64(0)
	flush := func() {
		if runLen > 0 {
			ranges = append(ranges, SkipRange{
				Offset: runStart * regionSize,
				Length: runLen * regionSize,
			})
			runLen = 0
		}
	}
	for region := shiftRegions; region < limit; region++ {
		if !BitmapBit(bitmap, region-shiftRegions) {
			flush()
			continue
		}
		if runLen == 0 {
			runStart = region
		}
		runLen++
	}
	flush()
	return ranges
}

// cloneApplyDrainBound bounds each of ApplySkipRanges' two waits for the
// dm-clone's copies in flight to end, and cloneApplyPollInterval paces their
// status reads. The second wait holds the holders paused, so the bound is
// short. Variables, so the package's tests need not wait them out.
var (
	cloneApplyDrainBound   = common.CmdSoftTimeout * time.Second
	cloneApplyPollInterval = 100 * time.Millisecond
)

// ApplySkipRanges marks every skippable range of the dm-clone name, whose
// device is dev, as copied: a discard, which dm-clone with discard pass-down
// off takes as "copied" without touching the disk below (architecture.md,
// Bitmap push protocol). Re-applying is harmless: dm-clone ignores a discard
// of a region it holds as copied.
//
// dm-clone marks a discarded region without looking at its copies in
// flight, and a write to a marked region goes straight to the destination;
// a copy of that region still in flight would then land after the write and
// put the source's bytes over it. So the discards go only to a dm-clone no
// write can reach and no copy is in flight on, and the apply makes it so for
// the moment it needs. It stops the background copy when it runs and waits,
// for a short bound, until the status shows no copy in flight, and goes on
// when the bound passes; that wait holds no IO back and keeps the pause
// short. Then it pauses every holder of the dm-clone that is not suspended
// (a suspend waits for the IO the holder sent down, and a write waiting on
// a copy ends only after that copy), waits again, this time for a status
// with no copy in flight or the bound, sends the discards and waits for
// every one of them to leave the kernel, and only then resumes the holders
// it paused and starts the copy again. Once the holders resume, a marked
// region is never copied, since dm-clone copies only regions not marked.
//
// It sends nothing when the dm-clone does not exist or is suspended (a
// discard on a suspended device waits for the resume); the caller's
// converge applies the stored pieces again when it creates or resumes the
// dm-clone. A holder already suspended is left as it is: its IO waits above
// the dm-clone, and a resume could undo a fence in progress. Anything the
// apply cannot make sure of — a device mapper device below the dm-clone
// that is suspended, where a pause or the discard could wait for ever, IO
// in flight on the dm-clone once its holders are paused, which only a
// writer or reader that is no holder can have sent, a holder with a table
// waiting, a listing that does not answer or misses a device, a pause, a
// stop or a status read that fails, a copy still in flight at the second
// bound — sends no discard: the apply restores what it
// changed, even for a caller whose ctx ended, and returns the error, and the
// regions are copied, which is always safe. What it cannot restore it
// returns too, and the converge takes it back: a holder left suspended, a
// copy left stopped.
func ApplySkipRanges(
	ctx context.Context,
	dm *Dm,
	name string,
	dev string,
	ranges []SkipRange,
) (err error) {
	if len(ranges) == 0 {
		return nil
	}
	info, err := dm.InfoDevNo(ctx, name)
	if err != nil {
		return err
	}
	if info == nil || info.Suspended {
		return nil
	}
	if info.DevNo == "" {
		return fmt.Errorf("dm-clone %s: no device number", name)
	}
	holders, err := readCloneStack(ctx, dm, name, info.DevNo)
	if err != nil {
		return err
	}
	// The restore runs whatever becomes of the caller: a push cut off by
	// the worker's deadline must not leave the holders paused or the copy
	// stopped until the next converge. Each command keeps its own soft
	// timeout (cmdCtx).
	restoreCtx := context.WithoutCancel(ctx)
	stopped, err := stopCopy(ctx, dm, name)
	if stopped {
		defer func() {
			if startErr := dm.Message(restoreCtx, name, 0,
				"enable_hydration"); startErr != nil {
				err = errors.Join(err, startErr)
			}
		}()
	}
	if err != nil {
		return err
	}
	if err := awaitNoCopy(ctx, dm, name, false); err != nil {
		return err
	}
	paused, err := pauseHolders(ctx, dm, name, holders)
	defer func() {
		err = errors.Join(err, resumeHolders(restoreCtx, dm, paused))
	}()
	if err != nil {
		return err
	}
	if err := awaitNoCopy(ctx, dm, name, true); err != nil {
		return err
	}
	// IO the pause does not hold, a udev probe's read of the dm-clone node
	// itself among them, would make the discard wait first for the reads
	// of the dm-clone's own page cache, as long as they take, with the
	// holders paused and the object's lock held; such IO refuses the apply.
	reads, writes, err := dm.InFlight(ctx, info.DevNo)
	if err != nil {
		return err
	}
	if reads != 0 || writes != 0 {
		return fmt.Errorf("dm-clone %s: %d reads and %d writes in flight "+
			"with its holders paused", name, reads, writes)
	}
	discards := make([]common.DiscardRange, len(ranges))
	for i, r := range ranges {
		discards[i] = common.DiscardRange{Offset: r.Offset, Length: r.Length}
	}
	// DiscardRanges returns only once every discard it sent has left the
	// kernel (osclient.md, DiscardRanges): a discard still in the kernel
	// after the holders resume would mark its regions while writes reach
	// them.
	return dm.DiscardRanges(ctx, dev, discards)
}

// readCloneStack reads the device mapper devices around the dm-clone name,
// whose number is devNo, from one listing of every table and one of every
// device: it returns the holders, every device whose live table names
// devNo, sorted, and refuses a stack with a suspended device below the
// dm-clone, found by following the device numbers the tables name, down
// from the dm-clone's own. Below a suspended device a pause of a holder
// could wait on IO that never ends, and so could the discard itself, under
// the object's lock that the converge which resumes that device needs. A
// listing that does not answer or may miss a device is an error too.
func readCloneStack(
	ctx context.Context,
	dm *Dm,
	name string,
	devNo string,
) ([]string, error) {
	tables, err := dm.ListTables(ctx)
	if err != nil {
		return nil, err
	}
	infos, err := dm.ListInfo(ctx)
	if err != nil {
		return nil, err
	}
	if tables.Partial || infos.Partial {
		return nil, fmt.Errorf(
			"dm-clone %s: a device listing may miss a device", name)
	}
	var holders []string
	for holder, targets := range tables.Tables {
		if holder != name && tableNames(targets, devNo) {
			holders = append(holders, holder)
		}
	}
	sort.Strings(holders)
	byNo := make(map[string]string, len(infos.Devs))
	for dev, info := range infos.Devs {
		if info.DevNo != "" {
			byNo[info.DevNo] = dev
		}
	}
	seen := map[string]bool{name: true}
	queue := []string{name}
	for len(queue) > 0 {
		dev := queue[0]
		queue = queue[1:]
		for _, target := range tables.Tables[dev] {
			for _, arg := range target.Args {
				below, ok := byNo[arg]
				if !ok || seen[below] {
					continue
				}
				seen[below] = true
				if infos.Devs[below].Suspended {
					return nil, fmt.Errorf(
						"dm-clone %s: %s below it is suspended", name, below)
				}
				queue = append(queue, below)
			}
		}
	}
	return holders, nil
}

// pauseHolders suspends every holder that is not suspended already, in the
// order given, and returns the ones it suspended or tried to, in that order.
// A holder with a table waiting stops the pause with an error, as a suspend
// that fails does, and the holders it suspended or tried to by then come
// back for the caller to resume.
func pauseHolders(
	ctx context.Context,
	dm *Dm,
	name string,
	holders []string,
) ([]string, error) {
	var paused []string
	for _, holder := range holders {
		info, err := dm.Info(ctx, holder)
		if err != nil {
			return paused, err
		}
		if info == nil || info.Suspended {
			continue
		}
		if info.Waiting {
			return paused, fmt.Errorf(
				"dm-clone %s: holder %s has a table waiting", name, holder)
		}
		// A suspend that failed may still have taken effect, as one killed
		// after its ioctl returned, so the holder is resumed either way: a
		// resume of a device that is not suspended changes nothing.
		paused = append(paused, holder)
		if err := dm.Suspend(ctx, holder); err != nil {
			return paused, err
		}
	}
	return paused, nil
}

// tableNames reports whether a table names devNo among its targets'
// arguments, as every table dnv builds names a device below it.
func tableNames(targets []common.DmTarget, devNo string) bool {
	for _, target := range targets {
		for _, arg := range target.Args {
			if arg == devNo {
				return true
			}
		}
	}
	return false
}

// resumeHolders resumes the holders pauseHolders suspended, the last first,
// and returns every resume's error. A holder whose resume fails stays
// suspended for the converge to resume.
func resumeHolders(ctx context.Context, dm *Dm, paused []string) error {
	var errs []error
	for i := len(paused) - 1; i >= 0; i-- {
		if err := dm.Resume(ctx, paused[i]); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// stopCopy stops the dm-clone's background copy when its status shows it on,
// and says whether it sent the stop: the caller starts the copy again then,
// even when the stop failed, since a failed message may still have taken
// effect and a start of a copy that runs changes nothing. A status that does
// not read as a dm-clone's, or that shows metadata out of read-write mode,
// where dm-clone ignores a discard, is an error.
func stopCopy(ctx context.Context, dm *Dm, name string) (bool, error) {
	st, err := cloneStatusNow(ctx, dm, name)
	if err != nil {
		return false, err
	}
	if !st.HydrationEnabled {
		return false, nil
	}
	return true, dm.Message(ctx, name, 0, "disable_hydration")
}

// awaitNoCopy reads the dm-clone's status until it shows no copy in flight,
// for no longer than cloneApplyDrainBound. Past the bound a strict wait is
// an error and a wait that is not strict answers nil: the first wait only
// shortens the pause that follows it.
func awaitNoCopy(
	ctx context.Context,
	dm *Dm,
	name string,
	strict bool,
) error {
	deadline := time.Now().Add(cloneApplyDrainBound)
	for {
		st, err := cloneStatusNow(ctx, dm, name)
		if err != nil {
			return err
		}
		if st.HydratingRead && st.Hydrating == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			if !strict {
				return nil
			}
			return fmt.Errorf("dm-clone %s: %d copies still in flight "+
				"after %v", name, st.Hydrating, cloneApplyDrainBound)
		}
		timer := time.NewTimer(cloneApplyPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// cloneStatusNow reads the dm-clone's status without a commit and parses
// it; a line that is not a dm-clone's, or whose metadata is not in
// read-write mode, is an error.
func cloneStatusNow(
	ctx context.Context,
	dm *Dm,
	name string,
) (*common.CloneStatus, error) {
	raw, err := dm.StatusNoFlush(ctx, name)
	if err != nil {
		return nil, err
	}
	st, ok := common.ParseCloneStatus(raw)
	if !ok {
		return nil, fmt.Errorf("dm-clone %s: unparsable status %q", name, raw)
	}
	if st.MetadataMode != "" &&
		st.MetadataMode != common.CloneMetadataReadWrite {
		return nil, fmt.Errorf("dm-clone %s: metadata in %s mode",
			name, st.MetadataMode)
	}
	return st, nil
}
