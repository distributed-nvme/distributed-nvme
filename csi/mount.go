package csi

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The node's side of filesystems and mounts (csi.md CSI17), all through the
// OS client: blkid, the direct read of a device's start, the filesystem
// makers, mount and umount, the kernel's mount table and the filesystem
// totals.
const (
	mountInfoPath = "/proc/self/mountinfo"
	// blankProofBytes is how much of a device's start a stage reads before
	// it formats the device (csi.md CSI17): the first MiB holds the
	// signatures of ext4 and xfs, the two filesystems the driver makes, and
	// of almost every other filesystem and partition table.
	blankProofBytes = 1 << 20
	// directReadAlign is the alignment of a direct read's offset and length
	// (osclient.md, ReadBlockDirect).
	directReadAlign = 4096
	// xfsProfile is the xfs maker's profile of the oldest kernel series a
	// Kubernetes node may run (csi.md CSI17): it names every feature the
	// maker knows, on or off, so the maker of the image formats nothing
	// that kernel cannot mount, whatever its own defaults. The image's
	// build checks that the file is there (deploy/Dockerfile).
	xfsProfile = "/usr/share/xfsprogs/mkfs/lts_6.1.conf"
)

type mounter struct {
	oc common.OsClient
}

// run runs one command and returns its stdout; a failure carries stderr.
func (m *mounter) run(ctx context.Context, name string, args ...string) (string, error) {
	stdout, stderr, _, err := m.oc.RunCommand(ctx, name, args, "")
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "),
			err, strings.TrimSpace(stderr))
	}
	return stdout, nil
}

// mountEntry is one line of the mount table: where something is mounted,
// what, of which filesystem, and whether the mount itself is read-only.
type mountEntry struct {
	target   string
	source   string
	fsType   string
	readOnly bool
}

// mounts reads the kernel's mount table.
func (m *mounter) mounts(ctx context.Context) ([]mountEntry, error) {
	data, err := m.oc.ReadFile(ctx, mountInfoPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", mountInfoPath, err)
	}
	return parseMountInfo(data), nil
}

// parseMountInfo reads mountinfo lines: the mount point is the fifth field
// and the mount's own options the sixth, and after the separator come the
// filesystem type and the source. A space in a path is escaped as an octal
// sequence.
func parseMountInfo(data string) []mountEntry {
	var out []mountEntry
	for _, line := range strings.Split(data, "\n") {
		before, after, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		fields := strings.Fields(before)
		tail := strings.Fields(after)
		if len(fields) < 6 || len(tail) < 2 {
			continue
		}
		out = append(out, mountEntry{
			target:   unescapeMount(fields[4]),
			fsType:   tail[0],
			source:   unescapeMount(tail[1]),
			readOnly: slices.Contains(strings.Split(fields[5], ","), "ro"),
		})
	}
	return out
}

// unescapeMount turns the octal escapes of mountinfo back into bytes.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// mountAt is the mount table's entry at a path, or nil.
func (m *mounter) mountAt(ctx context.Context, target string) (*mountEntry, error) {
	entries, err := m.mounts(ctx)
	if err != nil {
		return nil, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].target == target {
			return &entries[i], nil
		}
	}
	return nil, nil
}

// probe reads what a device carries, straight from the device: its
// filesystem and its partition table, both empty when blkid finds neither.
func (m *mounter) probe(ctx context.Context, dev string) (string, string, error) {
	stdout, stderr, code, err := m.oc.RunCommand(ctx, "blkid",
		[]string{"-p", "-s", "TYPE", "-s", "PTTYPE", "-o", "export", dev}, "")
	if err != nil {
		// blkid answers 2 with nothing printed when it found no
		// recognizable content, and also when a read of the device failed,
		// so this answer is "found nothing or did not read": the read of
		// the device's start (readStart) decides before any format. It
		// answers 2 as well for a device it cannot open, with a message on
		// stderr, which is no answer.
		if code == 2 && strings.TrimSpace(stdout) == "" && strings.TrimSpace(stderr) == "" {
			return "", "", nil
		}
		return "", "", fmt.Errorf("blkid %s: %w: %s", dev, err, strings.TrimSpace(stderr))
	}
	var fsType, ptType string
	for _, line := range strings.Split(stdout, "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch key {
		case "TYPE":
			fsType = value
		case "PTTYPE":
			ptType = value
		}
	}
	return fsType, ptType, nil
}

// readStart reads the start of a device past the page cache, as the device
// holds it now: the first blankProofBytes, or the whole device in whole 4 KiB
// blocks when it is smaller, since a direct read past the end fails. It
// returns no bytes for a device smaller than one block.
func (m *mounter) readStart(ctx context.Context, dev string) ([]byte, error) {
	size, err := m.blockSize(ctx, dev)
	if err != nil {
		return nil, err
	}
	n := min(uint64(blankProofBytes), size) / directReadAlign * directReadAlign
	if n == 0 {
		return nil, nil
	}
	data, err := m.oc.ReadBlockDirect(ctx, dev, 0, n)
	if err != nil {
		return nil, err
	}
	if uint64(len(data)) != n {
		return nil, fmt.Errorf("a direct read of %s returned %d bytes, want %d",
			dev, len(data), n)
	}
	return data, nil
}

// mkfs formats a blank device with the one of the two filesystems named:
// ext4 with the maker's defaults, which the oldest kernel series a node may
// run mounts, and xfs with the profile of that series (xfsProfile).
func (m *mounter) mkfs(ctx context.Context, fsType string, dev string) error {
	var err error
	switch fsType {
	case fsExt4:
		_, err = m.run(ctx, "mkfs.ext4", "-q", "-F", dev)
	case fsXfs:
		_, err = m.run(ctx, "mkfs.xfs", "-q", "-f", "-c", "options="+xfsProfile, dev)
	default:
		return fmt.Errorf("the driver formats %s and %s, not %q", fsExt4, fsXfs, fsType)
	}
	return err
}

// mkdir makes a directory and its parents.
func (m *mounter) mkdir(ctx context.Context, path string) error {
	_, err := m.run(ctx, "mkdir", "-p", path)
	return err
}

// touch makes an empty file, the target of a block volume's bind mount.
func (m *mounter) touch(ctx context.Context, path string) error {
	return m.oc.WriteFile(ctx, path, "")
}

// mount mounts a device at a target with a filesystem type and options.
func (m *mounter) mount(
	ctx context.Context,
	dev string,
	target string,
	fsType string,
	options []string,
) error {
	args := []string{"-t", fsType}
	if len(options) > 0 {
		args = append(args, "-o", strings.Join(options, ","))
	}
	args = append(args, dev, target)
	_, err := m.run(ctx, "mount", args...)
	return err
}

// bindMount bind-mounts a source at a target, read-only when asked, which
// takes a second mount: a bind mount's own options are the source's.
func (m *mounter) bindMount(ctx context.Context, source string, target string, readOnly bool) error {
	if _, err := m.run(ctx, "mount", "--bind", source, target); err != nil {
		return err
	}
	if !readOnly {
		return nil
	}
	return m.remountReadOnly(ctx, target)
}

// remountReadOnly makes a bind mount read-only.
func (m *mounter) remountReadOnly(ctx context.Context, target string) error {
	_, err := m.run(ctx, "mount", "-o", "remount,bind,ro", target)
	return err
}

// unmount unmounts a target.
func (m *mounter) unmount(ctx context.Context, target string) error {
	_, err := m.run(ctx, "umount", target)
	return err
}

// remove removes a mount target the plugin made: a directory with rmdir,
// never recursively, so a mount that is still there is refused rather than
// emptied; a file with rm. A target that is gone is done.
func (m *mounter) remove(ctx context.Context, target string) error {
	_, err := m.oc.ReadDir(ctx, target)
	switch {
	case err == nil:
		_, err = m.run(ctx, "rmdir", target)
		return err
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		_, err = m.run(ctx, "rm", "-f", target)
		return err
	}
}

// fsStats is a filesystem's totals in bytes and inodes.
type fsStats struct {
	totalBytes, freeBytes, availBytes    uint64
	totalInodes, freeInodes, availInodes uint64
}

// statfs reads a mounted filesystem's totals: the fragment size, the
// blocks, the free and the available blocks, the inodes and the free
// inodes.
func (m *mounter) statfs(ctx context.Context, path string) (fsStats, error) {
	out, err := m.run(ctx, "stat", "-f", "-c", "%S %b %f %a %c %d", path)
	if err != nil {
		return fsStats{}, err
	}
	fields := strings.Fields(out)
	if len(fields) != 6 {
		return fsStats{}, fmt.Errorf("stat -f %s answered %q, not six numbers", path, out)
	}
	var n [6]uint64
	for i, f := range fields {
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return fsStats{}, fmt.Errorf("stat -f %s answered %q: %w", path, out, err)
		}
		n[i] = v
	}
	return fsStats{
		totalBytes:  n[0] * n[1],
		freeBytes:   n[0] * n[2],
		availBytes:  n[0] * n[3],
		totalInodes: n[4],
		freeInodes:  n[5],
		availInodes: n[5],
	}, nil
}

// blockSize reads a block device's size in bytes.
func (m *mounter) blockSize(ctx context.Context, dev string) (uint64, error) {
	out, err := m.run(ctx, "blockdev", "--getsize64", dev)
	if err != nil {
		return 0, err
	}
	size, err := strconv.ParseUint(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("blockdev --getsize64 %s answered %q", dev, out)
	}
	return size, nil
}
