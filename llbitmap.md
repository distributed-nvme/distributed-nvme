# Lockless write-intent bitmap (`--bitmap=lockless`)

Notes from enabling the experimental **lockless bitmap** (bitmap superblock
major version 6, `BITMAP_MAJOR_LOCKLESS`) on a stock Ubuntu testbed VM
(`yupeng@192.168.122.125`, kernel `7.0.0-29-generic`).

The lockless bitmap is a new write-intent bitmap implementation by Yu Kuai that
removes the global bitmap lock contention point. It is **experimental**: mdadm
prints `Experimental lockless bitmap, use at your own risk!` on create. It
requires both **kernel** support and a **newer mdadm** than most distros ship.

## 1. Check kernel support

The kernel must be built with `CONFIG_MD_LLBITMAP=y` (it also needs
`CONFIG_MD_BITMAP=y` and `CONFIG_BLK_DEV_MD=y`, which are usual).

```bash
# preferred: read the built kernel config
grep -E "CONFIG_MD_LLBITMAP|CONFIG_MD_BITMAP|CONFIG_BLK_DEV_MD" /boot/config-$(uname -r)

# fallback if /boot/config-* is missing
zcat /proc/config.gz 2>/dev/null | grep -E "CONFIG_MD_LLBITMAP|CONFIG_MD_BITMAP"
```

On the testbed this returned:

```
CONFIG_MD=y
CONFIG_BLK_DEV_MD=y
CONFIG_MD_BITMAP=y
CONFIG_MD_LLBITMAP=y        # <-- the one that matters for lockless
```

If `CONFIG_MD_LLBITMAP` is missing or `=n`, the kernel cannot use a lockless
bitmap and you must run a kernel that has it (or build one) before continuing.

## 2. Check / build a mdadm that supports `--bitmap=lockless`

### Check the stock mdadm

```bash
mdadm --version
# stock Ubuntu 24.x/26.x ships mdadm 4.5, which rejects lockless:
#   mdadm: --bitmap value must be 'internal', 'clustered' or 'none'
```

Stock mdadm 4.5 (and older) **does not** accept `--bitmap=lockless`. You need
upstream mdadm **4.6** (2026-07-22) or newer, where the lockless code lives in:

- `mdadm.c:61`  — `strcmp(val, "lockless") == 0` -> `s->btype = BitmapLockless`
- `bitmap.h:17` — `#define BITMAP_MAJOR_LOCKLESS 6`
- `Create.c:542` — `else if (s->btype == BitmapLockless) major_num = BITMAP_MAJOR_LOCKLESS;`
- `super1.c`    — load/validate path for major 6
- `Assemble.c`, `Incremental.c`, `Grow.c` — assemble/grow handling

### Build upstream mdadm (does not replace the distro binary)

```bash
# install build deps (git was already present on the testbed)
sudo apt-get update -qq
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
    build-essential pkg-config libudev-dev git

# clone and build
cd /tmp
rm -rf mdadm
git clone --depth 1 https://git.kernel.org/pub/scm/utils/mdadm/mdadm.git
cd mdadm
make -j"$(nproc)" mdadm

# verify
./mdadm --version    # -> mdadm - v4.6 - 2026-07-22
```

Notes from the build:

- The repeated `fatal: No names found, cannot describe anything.` lines during
  `make` are just `git describe` failing to find a tag for the version string
  (shallow clone). They are harmless; the link step at the end succeeds and
  produces the `mdadm` binary.
- Run the built binary in place as `/tmp/mdadm/mdadm` (or `sudo make install`
  to replace `/usr/sbin/mdadm`). Leaving the stock binary untouched is safer.
- The build needs `libudev-dev` (links `-ludev`) and `pkg-config`.

## 3. Create the resources

All commands run on the target host (`yupeng@192.168.122.125`).

### 3a. Two 1G backing files

```bash
sudo truncate -s 1G /tmp/loop0.img /tmp/loop1.img
```

### 3b. Two loop devices on them

```bash
sudo losetup --find --show /tmp/loop0.img   # -> /dev/loop0
sudo losetup --find --show /tmp/loop1.img   # -> /dev/loop1
```

The kernel assigns loop indices dynamically; on the testbed it chose
`/dev/loop0` and `/dev/loop1` (not `loop240`/`loop241`). Use whatever
`--find --show` prints.

### 3c. Create the raid1 with a lockless bitmap

Use the **built** mdadm, not the stock one. `--run` skips the interactive
`Continue creating array? [y/N]` prompt (this build has no `--assume-yes` /
`-y` flag for `--create`):

```bash
sudo /tmp/mdadm/mdadm --create /dev/md0 \
    --level=1 \
    --raid-devices=2 \
    /dev/loop0 /dev/loop1 \
    --bitmap=lockless \
    --run
```

Expected output:

```
mdadm: Note: this array has metadata at the start and
    may not be suitable as a boot device.  If you plan to
    store '/boot' on this device please ensure that
    your boot-loader understands md/v1.x metadata, or use
    --metadata=0.90
mdadm: Experimental lockless bitmap, use at your own risk!
mdadm: Defaulting to version 1.2 metadata
mdadm: array /dev/md0 started.
```

Lockless bitmap requires 1.2 metadata (mdadm enforces this). It does **not**
support a bitmap chunk size (`super1.c:2498: lockless bitmap doesn't support
chunksize`); mdadm picks the chunk size itself.

## 4. Verify

```bash
cat /proc/mdstat
```

```
Personalities : [raid1]
md0 : active raid1 loop1[1] loop0[0]
      1046528 blocks super 1.2 [2/2] [UU]
      bitmap: 5/5 pages [20KB], 0B chunk
unused devices: <none>
```

```bash
sudo /tmp/mdadm/mdadm --detail /dev/md0 | grep -iE "Version|Bitmap|State|Consistency"
```

```
           Version : 1.2
     Intent Bitmap : Internal
             State : clean
Consistency Policy : bitmap
```

Confirm the bitmap superblock is major version 6 (lockless):

```bash
sudo /tmp/mdadm/mdadm --examine-bitmap /dev/loop0
```

```
        Filename : /dev/loop0
           Magic : 6d746962
         Version : 6
```

`Magic : 6d746962` (`mdmb` / `mtbi` little-endian) and `Version : 6`
(`BITMAP_MAJOR_LOCKLESS`) are the confirmation. Note: the *display* path in
this mdadm build still prints `mdadm: unknown bitmap version 6, either the
bitmap file is corrupted or you need to upgrade your tools` before dumping the
fields — that warning is from the `--examine-bitmap` pretty-printer not knowing
major 6; the **create/assemble/kernel** paths all accept it fine, so the array
is healthy.

## 5. Delete / tear down the resources

Teardown order matters: stop the array, zero the superblocks, detach the
loops, then delete the backing files. Use the **built** mdadm for stop/zero
(stock 4.5 may choke on the v6 bitmap superblock).

```bash
# 1. stop the array
sudo /tmp/mdadm/mdadm --stop /dev/md0

# 2. wipe the md superblocks (and the embedded bitmap) from both members
sudo /tmp/mdadm/mdadm --zero-superblock /dev/loop0 /dev/loop1

# 3. detach the loop devices
sudo losetup -d /dev/loop0 /dev/loop1

# 4. remove the backing files (root-owned because truncate ran under sudo)
sudo rm -f /tmp/loop0.img /tmp/loop1.img

# 5. (optional) unload the raid1 module if nothing else uses it
sudo modprobe -r raid1
```

Verify it is all gone:

```bash
cat /proc/mdstat            # -> "Personalities : " + "unused devices: <none>"
sudo losetup -l             # -> no loop0/loop1
ls -l /tmp/loop0.img /tmp/loop1.img   # -> No such file or directory
```

### Remove the built mdadm and build dependencies (full cleanup)

```bash
# remove the built binary + source tree
rm -rf /tmp/mdadm

# remove the build deps installed earlier (git was already present, keep it)
sudo apt-get purge -y build-essential pkg-config libudev-dev
sudo apt-get autoremove -y --purge
```

On the testbed, after purge + autoremove, `pkg-config` and `libudev-dev` were
removed; `gcc` and `make` were retained by apt because other installed packages
still depend on them. The **stock `/usr/sbin/mdadm` (v4.5) was never touched**
by any of the above.

## 6. Caveats / gotchas

- **Both kernel and mdadm must support it.** Kernel: `CONFIG_MD_LLBITMAP=y`.
  mdadm: upstream 4.6+. Stock distro mdadm 4.5 rejects `--bitmap=lockless`
  outright (`--bitmap value must be 'internal', 'clustered' or 'none'`).
- **Metadata must be 1.2.** mdadm forces this for lockless.
- **No bitmap chunk size may be specified** for lockless (`lockless bitmap
  doesn't support chunksize`); mdadm selects it.
- **`--create` is interactive.** Use `--run` to skip the `Continue creating
  array? [y/N]` prompt; there is no `--assume-yes`/`-y` for `--create` in this
  build.
- **`--examine-bitmap` prints a false "unknown bitmap version 6" warning** in
  this mdadm build (the pretty-printer wasn't updated for major 6). The
  `Magic: 6d746962` / `Version: 6` fields it still dumps are the real signal;
  the running array is fine.
- **`mdadm --detail` reports `Intent Bitmap : Internal` and
  `Consistency Policy : bitmap`** for a lockless bitmap — it does not say
  "lockless" there. The only way to confirm lockless is `--examine-bitmap`
  showing `Version : 6`.
- **Use the built mdadm for stop/zero too**, not just create, to avoid the
  stock 4.5 parser tripping on the v6 bitmap superblock during teardown.
