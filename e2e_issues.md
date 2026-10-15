# The e2e failures at 32 slices

This file describes the two problems that stop the e2e suite at 32 slices.
It is the starting point for one plan per problem. A reader needs only this
file, the repository's code and `doc/*.md`.

**The state it describes.** Branch `dev` at commit `33c07d3`, "A cntlr's
check round reads the node once, so 32 slices fit the round's wait". That
commit is the slice-32 fix. Before it, a cntlr's check round at 32 slices
ran a `dmsetup` and an `lsblk` for each device and took about 2 s, so the
worker's 2 s wait for its answer often ran out. Now the round lists the
whole node once.

The lab gate, the set of suite runs that checked the fix, ran on the
evening of 2026-10-14, local time (UTC−7). It ran commit `8bb1f25` with the
fix not yet committed: the suite runner (`integtest/runner.sh`, which runs
the suites one after another) reported "commit 8bb1f25, tree dirty". The fix
was committed as `33c07d3` after the lab gate. That the tree which ran is
byte for byte `33c07d3` is not verified.

Every time in this file is UTC on 2026-10-15, the date the logs carry, unless
the line names another date.

**The e2e suite by slice count.** smoke builds the run's pool, checks that
it is whole, and deletes it. ops runs the pool's other calls. copy moves
bytes with a transfer, a clone, a migration and a spare leg. react causes
the four reactions with real faults. cutoff stops the primary's agent and
expects a host to see only a pause.

| Slices | smoke | ops | copy | react | cutoff |
|---|---|---|---|---|---|
| 1 | pass | pass | pass | pass | pass |
| 4 | pass | pass | pass | pass | pass |
| 32 | pass | pass | fails at stage it-copy-04 | pass, run alone; the suite builds react's pool with 8 slices at most | fails at stage it-cutoff-05, run alone |

A stage is named here by its trace id. A suite stops at its first failing
case. So the full run at 32 slices ended in copy, and react and cutoff were
then run alone with `--only`.

**Round times since the fix.** The fix's commit message gives round times
from a run of the smoke case alone at 32 slices, made before the lab gate.
In that run, the check rounds that did not wait behind a converge answered
in 0.03 to 0.08 s on the standby and in 0.36 to 0.54 s on the primary. In
the lab gate's copy case at 32 slices, such rounds of the primary took 0.24
to 0.91 s.

**The other suites of the same lab run.** dn, cn, gateway, dnvctl and cdc
pass. The worker suite failed once at its drain stage: the worker logged
"sp drain failed", phase "final", error "etcd stm: context canceled", where
the suite waits for "sp drained". It passed when run again. This file does
not treat that failure.

## Words used here

These words are not in `doc/core_glossary.md`. Each is explained here in
core words, and each entry uses only words of the entries above it. "hold"
is a core word; it is listed because this file needs its two time bounds.

- **cn0, cn1, cn2, dn0 to dn3, host0, host1** — the e2e suite's names for the
  lab guests. The last section gives their addresses.
- **old side, new side** — the two sides of a migrating leg.
  `architecture.md` calls them the src side and the dst side, and numbers
  the steps of each.
- **ANA group** — ANA is how an NVMe-oF target tells a host which of its
  paths to a namespace serves IO. Every nvmet port of a dnv agent has three
  fixed groups: 1 is optimized, 2 is non-optimized and 3 is inaccessible.
  An agent moves a namespace by writing the group number into the
  namespace's `ana_grpid` file in configfs, so "ana_grpid 1" in a log means
  optimized. dnv never changes a group's state, only a namespace's group
  (`architecture.md`, [D4]). A host sends IO on an optimized path, falls
  back to a non-optimized path when no optimized one is usable, and never
  sends IO on an inaccessible path.
- **dm-error** — a device-mapper device whose every IO fails at once. dnv
  points a path at it to take the path out of service. On a controller node
  each thin device has one. On a disk node each side has one for each cntlr.
- **ns-dev** — the namespace's own dm-linear on a controller node. nvmet
  exports this device to hosts. Its table maps the thin device's raid0 when
  the namespace serves, and the thin device's dm-error when it does not, as
  on a standby. An ns-dev that maps dm-error is called parked. `cnagent.md`,
  CN16, lists the other devices a serving ns-dev can map, such as the
  dm-clone of a clone that fills the thin device.
- **side device** — the dm-linear on a disk node that joins the extents of
  one side into one device (`architecture.md`, Disk node).
- **per-CN export** — what a disk node builds for one side and one cntlr: a
  dm-linear and an nvmet subsystem with one namespace. The export to the
  primary maps the side device and is optimized. The export to a standby
  maps the side's dm-error and is non-optimized.
- **switch-over of a side** — what each side does at a failover, in one
  converge. It reloads the new primary's export onto the side device. It
  reloads the old primary's export onto dm-error; this is the fence. Then it
  moves the new primary's namespace to optimized and the old primary's to
  non-optimized (`architecture.md`, Failover, the sides' steps 1 to 4). The
  docs also call it the sides' flip.
- **provisioned flip** — the worker's write that marks a side provisioned
  once its zeroing is complete. It raises the pool's revision
  (`dnv-worker.md`, RW18).
- **provisioning-deferred** — the state of a thin device that the primary
  cannot build yet. It holds when, in some slice of the pool, the first meta
  group or the first data group still has a leg none of whose sides is
  provisioned. A thin device stripes over every slice, so one such slice
  holds back every thin device of the pool (`cnagent.md`, CN9). A side that
  zeroes elsewhere holds nothing back: a migration's new side, a group that
  a grow added after the first one, or a spare.
- **promotion** — the syncup that tells a standby it is now the primary.
- **thin volume** — the dm-thin device of one thin device in one slice. The
  thin device's raid0 stripes over its thin volumes in all slices.
- **stack** — what a primary builds on its legs: md arrays, thin pools,
  thin volumes, raid0s and ns-devs.
- **hydration** — dm-clone's background copy of the data from its source to
  its destination.
- **multipath head** — the one block device, such as `nvme35n1`, that the
  Linux nvme driver makes for a namespace over all its paths.
- **leg wrapper** — the dm-linear on a controller node over a leg's
  multipath head. md uses it as the array's member.
- **available leg** — a leg the primary may put into an md array in this
  converge: its connect and its leg wrapper worked in this pass, and one of
  its paths is live and optimized (`cnagent.md`, CN12). A non-optimized path
  does not count, because it is a standby's export, which maps dm-error.
- **the two waits of the hold** — the first wait comes only with a failover.
  The old primary gets its demotion first, and the sides wait until it
  reports the demotion applied or until 3 s have passed (`dnv-worker.md`,
  RW22). The second wait comes with every new revision: the cntlrs wait
  until every side that was healthy reports the revision applied, or until
  one check interval of a cntlr has passed, 2 s in this lab (`dnv-worker.md`,
  RW14). The docs call
  the first wait the demotion hold and the second the sides-first hold. A
  wait that its timer ends is logged as "sp demotion unsynced" or "sp sides
  unsynced"; the second carries the count of sides that had not reported.
- **settling** — the state of a cntlr from the moment it becomes primary
  until the worker sees it report its stack built and clean; the worker
  then clears it and logs "cntlr settled". A failover sets it, and so do a
  cntlr replacement that makes a primary, the creation of a pool for its
  first primary, and the enabling of a cntlr that is still primary
  (`dnv-worker.md`, HL2). While it is set, the worker judges the cntlr by
  the cntlr threshold instead of the primary threshold, when that is the
  longer, so a slow build does not cause another failover (`dnv-worker.md`,
  AR5).
- **background retry** — the cn agent's re-run of a cntlr's whole converge
  every 5 s. It runs for as long as the last pass left a reason to retry,
  such as a failed connect or a group member that was not available
  (`cnagent.md`, CN10). Each attempt has its own trace id.
- **leg prober** — a goroutine on the primary that writes a small health
  block at a fixed place on one leg every 5 s and reads it back, outside
  every lock. The leg's row reads pending, "health probe pending", until its
  first round completes (`cnagent.md`, CN11).
- **info read** — the gateway's read of one cntlr's state from its agent.
  `dnvctl cntlr inspect` uses it.
- **node lock, cntlr lock** — the cn agent's locks. A syncup, a check round,
  an info read or a background retry attempt of one cntlr holds the node
  lock for reading and that cntlr's own lock, for its whole run. A syncup of
  the controller node holds the node lock for writing. The leg probers
  take no lock (`cnagent.md`, CN1). A writer that waits for the node lock
  makes every later reader wait too; `architecture.md` calls this
  head-of-line blocking (`architecture.md`, Known limits).
- **reading** — what a check round or an info read of a cntlr takes from its
  controller node first. It is one `dmsetup info` and one `dmsetup table`
  listing of every device-mapper device, a walk of the nvme subsystems in
  sysfs, and a walk of the md arrays. The status reads of the thin pools come
  after it, one per pool (`cnagent.md`, CN28).
- **migration gap** — the time in which the primary has no usable path to a
  migrating leg. It starts with the old side's first step, which moves its
  per-CN namespaces to inaccessible. It ends when the primary's path to the
  new side is live and optimized (`glossary.md`, migration gap).
- **migration source export** — the nvmet subsystem the old side builds for
  the new side's disk node, so that the new side's dm-clone can read the
  leg's data (`architecture.md`, Migration, src side step 3).
- **cutover window** — the 60 s for which the old side keeps its per-CN
  dm-linears suspended before it reloads them onto dm-error. IO the old
  primary still had in flight is then failed, never written to the side
  (`architecture.md`, Migration, src side step 2, and [D12], which bounds
  every such suspension).
- **thin-pool commit** — dm-thin's write of its open metadata transaction.
  dm-thin first flushes the pool's data device, then writes the changed
  metadata blocks and flushes the metadata device. While a commit runs, the
  rest of that pool's IO waits.
- **requeue** — what the nvme multipath driver does with IO for a namespace
  that has a live controller but no usable path. It keeps the IO on a list,
  with no time limit of its own, and sends it again when a path becomes
  usable. The kernel logs "no usable path - requeuing I/O".
- **DNR** — the NVMe "do not retry" bit of an error status. A host does not
  retry such an IO at all, on this path or on another, so the application
  gets EIO.
- **nvme-stas** — the host daemons that follow the cdc's discovery log:
  stafd reads the log, and stacd connects and disconnects the paths it
  lists.
- **AEN** — the asynchronous event by which the cdc tells a connected host
  that its discovery log changed.
- **D state** — a Linux task in uninterruptible sleep. No signal ends it,
  SIGKILL included, before the IO it waits on completes.

## The early ANA move after a failover

**In one line.** At 32 slices, the new primary of a failover moved its
namespaces to ANA optimized while their ns-devs still mapped dm-error, so
host0's writes failed for about 12 s instead of waiting.

### How it shows

- The cutoff case fails at stage it-cutoff-05, "the new primary builds the
  stack, and host0's writes run again", after 7m56s with "host0's write loop
  recorded 214 FAILED write(s) after the resume."
- The write loop on host0 writes 4 KiB with `dd conv=fsync` to a scratch
  namespace about every 58 ms, and records each write's start, end and
  result. Write 198 waited 9960 ms and then failed. Writes 199 to 202 failed
  in 3.9 to 5.5 ms each. In all 930 writes ran, 716 ok and 214 failed. By the
  loop's counts, the failures are writes 198 to 411.
- host0's kernel log: `nvme1c2n1: I/O Cmd(0x1) @ LBA 1584, 8 blocks, I/O
  Error (sct 0x0 / sc 0x6) MORE DNR`. Status 0x6 is an internal error of the
  target, and DNR tells the host not to retry the IO, so dd gets EIO. Reads
  of block 0 fail the same way ("Buffer I/O error on dev nvme1n1,
  logical block 0, async page read"). They are probably udev or blkid
  reading the device after each write; this is inferred, not verified.

### Where and when

- The run: the cutoff case alone at 32 slices, from 03:33:43 to 03:41:39 by
  the runner's clock. There is one such run, so how often it fails is not
  known.
- The pool: 32 slices, 64 RAID1 groups and 128 legs of one side each, on
  180 disk nodes.
- The old primary is cntlr 1 on cn2 (192.168.122.77, controller node id 3).
  The new primary is cntlr 2 on cn1 (192.168.122.229, controller node id 2).
- The cutoff case's event thresholds: primary 4 s, cntlr 1800 s, side
  1800 s, leg 3600 s. A cntlr's check interval is 2 s. The first wait of the
  hold is 3 s.
- The write loop writes to namespace 2 of the subsystem, over thin device
  0x166 (`/dev/nvme1n1` on host0). Namespace 1 is over thin device 0x163.
  In the device names below, `c6` marks an ns-dev (`…-c6-…-0165` is
  namespace 1, `…-c6-…-0167` is namespace 2), `c4` a raid0 and `c3` a thin
  volume.

### Timeline

- **03:40:20.941**, cn2: the last line of the old primary's agent log. The
  suite stopped that agent with SIGSTOP at about 03:40:21.0. Its kernel
  objects go on serving host0.
- **03:40:23.437**, worker: a check round of cntlr 1 ended without an answer
  ("round timeout"), and the worker sets its health epoch with the reason
  "unreachable". The next round of cntlr 1 times out at 03:40:27.437.
- **03:40:27.499**, worker: `"reaction applied" kind failover old_cntlr_id 1
  new_cntlr_id 2 revision 23`, about 6.5 s after the stop. (The suite's own
  polls say 7 s.) In the same ms the cdc logs "view changed" and "aen sent"
  for host0.
- **03:40:27.597**, worker: sends the demotion to cntlr 1. The stopped agent
  never answers.
- **03:40:29**, host0: nvme-stas logs "nvme1 - Disconnect initiated" for
  192.168.122.77.
- **≈03:40:29.012**, host0 kernel: `nvme nvme1: Removing ctrl`.
- **03:40:29.034**, host0: write 198 starts.
- **≈03:40:29.036**, host0 kernel: `block nvme1n1: no usable path -
  requeuing I/O`. The only path left goes to cn1, where both namespaces were
  in group 3, inaccessible (cn1's agent read 3 at 03:40:29.114 and at
  03:40:31.183). This is the pause the case expects.
- **03:40:30.597**, worker: `"sp demotion unsynced"`: the first wait of the
  hold ran its full 3 s. In the same ms the worker sends revision 23 to all
  128 sides.
- **03:40:30.633 to 03:40:34.873**, disk nodes: the sides switch over, from
  the first suspend of a new primary's export to the last move of an old
  primary's export to non-optimized. The reloads of the new primary's
  exports ran from 03:40:30.635 to 03:40:32.114. The fences ran from
  03:40:30.774 to 03:40:33.304. host0 had already dropped its path to the
  old primary (03:40:29.012), so they cost no write. The new primary's
  exports turned optimized at different times on each guest (table below),
  the last at 03:40:34.763.
  The worker had 30 side replies by 03:40:32.598, 99 by 03:40:33.738 and
  all 128 by 03:40:34.920.
- **03:40:32.598**, worker: `"sp sides unsynced" side_cnt 98`: the second
  wait of the hold ended by its 2 s timer, with 98 sides not yet reported.
  The worker sends revision 23 (primary true, settling true) to cntlr 2.
  cn1 logs the promotion's arrival at 03:40:32.597 by its own clock, trace
  id `96919489-33cf7140c31df21e`. It is the only syncup cntlr 2 gets at
  revision 23.
- **03:40:32.618 to 03:40:33.684**, cn1: reads the ANA state of its 128 leg
  paths. Every controller is live. 87 paths read optimized and 41
  non-optimized.
- **03:40:33.738 to 03:40:38.102**, cn1: 60 `mdadm --assemble` calls. 27 are
  given both legs and start. 33 are given one leg and refuse: `mdadm:
  /dev/md/… assembled from 1 drive - need all 2 to start it (use --run to
  insist).` 4 more groups have "no available leg" and get no mdadm call.
- **from 03:40:38.148**, cn1: builds 7 of 32 thin pools and 14 of 64 thin
  volumes. Both raid0s fail, for example `dm device …-c3-…-0163-0000000000000003
  is not there`: a thin volume of the slice with id 3 is missing. Both
  ns-devs fail:
  `dm device …-c4-…-0163 is not there`, and the same for 0166. The ns-devs
  keep the table they had, which maps the thin device's dm-error.
- **03:40:38.985 and 03:40:38.989**, cn1: reads `ana_grpid` 3 and writes 1
  (optimized), first for namespace 1, then for namespace 2.
- **≈03:40:38.991**, host0 kernel: the first `I/O Error (sct 0x0 / sc 0x6)
  MORE DNR`.
- **03:40:38.994**, cn1: replies to the promotion with no error code. Its
  rows: groups 27 OK and 37 ERROR; pools 7 OK and 25 ERROR; thin volumes 14
  OK and 50 MISSING; both raid0s and both ns-devs ERROR; all 128 legs
  PENDING, "health probe pending".
- **03:40:38.995**, host0: write 198 ends, failed, after 9960 ms. Writes 199
  to 202 fail in 3.9 to 5.5 ms each.
- **03:40:39.004**, worker: sets cntlr 2's health epoch with the reason
  "error_row": the row of namespace 1's ns-dev read ERROR.
- **03:40:43.106 to 03:40:51.385**, cn1: one background retry attempt, trace
  id `58de53581abafc38`. 37 `mdadm --assemble` calls with both legs, all of
  which start (03:40:44.161 to 03:40:48.352). Then 25 thin pools and 50 thin
  volumes. The raid0 of thin device 0x163 comes at 03:40:51.247, that of
  0x166 at 03:40:51.315. The ns-dev of namespace 1 is reloaded onto its raid0
  at 03:40:51.323 to .333, and that of namespace 2 at 03:40:51.341 to .351.
  No ANA write is needed, and no further attempt runs.
- **03:40:45.218 and 03:40:49.219**, worker: two check rounds of cntlr 2 time
  out. On the agent they wait for the cntlr lock, which the retry attempt
  holds.
- **by ≈03:40:51.34 and by ≈03:40:51.40**, host0: the last failed write
  ends, and the first good write after it starts. The last failed write
  ended by the time cn1 suspended the ns-dev of namespace 2 for its reload
  onto the raid0 (03:40:51.34). A write that reached cn1 after that waited
  for the reload and then went to the raid0, so it did not meet dm-error.
  The first good write began one loop period, about 58 ms, after the last
  failed one began. Both times are bounds, not readings: the suite log does
  not hold that part of the loop's record.
- **03:40:51.688**, worker: a check round answered at 03:40:51.678 with the
  whole stack. The worker clears cntlr 2's health epoch and logs "cntlr
  settled".

Writes failed for about 12.3 s. From the path drop to the first good write
was about 22.4 s. The new primary was settling, so the worker judged it by
the 1800 s cntlr threshold, and no second failover came.

When the new primary's exports turned optimized, by disk-node guest:

| Guest | Sides | Optimized writes |
|---|---|---|
| dn1 (192.168.122.70) | 28 | 03:40:31.316 to 03:40:32.824 |
| dn0 (192.168.122.48) | 31 | 03:40:32.111 to 03:40:33.571 |
| dn2 (192.168.122.49) | 30 | 03:40:32.119 to 03:40:33.676 |
| dn3 (192.168.122.122) | 39 | 03:40:30.880 to 03:40:34.763 |

For example, the side of leg 7 on dn2 (TCP port 4328) started its reload at
03:40:31.41, resumed at 03:40:32.44 and wrote group 1 at 03:40:33.399.

### Why only 27 of 64 arrays started

- A leg counts as available only when one of its paths is live and
  optimized (`subsysView.available` in `agent/cnagent/leg.go`; the
  availability rule of `cnagent.md`, CN12). `assembleGroup`
  (`agent/cnagent/md.go`) uses only the available legs. With none it fails
  "no available leg" and calls no mdadm. With one leg that carries a
  superblock it runs `mdadm --assemble` on that leg alone.
- mdadm refuses to start a clean two-leg array from one leg. That refusal is
  on purpose: it keeps a stale leg from starting alone. Failover safety rests
  on it and on each side's one-pass switch-over (`architecture.md`, "Make
  sure all groups are available", and [D16]).
- At 03:40:32.618 to 03:40:33.684, 41 of the 128 leg paths read
  non-optimized: 27 on dn3, 8 on dn2, 5 on dn0 and 1 on dn1. For 40 of them
  the side had not yet moved the new primary's export to optimized. For leg
  0x31 it had, 13 ms before the read (below). A standby's path is
  non-optimized by design, because the disk node exports dm-error to a
  standby (`cnagent.md`, CN11, which reads non-optimized as a standby path's
  normal state). 41 is 33 groups with one available leg plus 4 groups with
  none, 33 + 2 × 4.
- cn1's check reply at 03:40:31.196 still showed rows such as
  "192.168.122.122:4317 live/non-optimized".
- Leg by leg, what cn1 read agrees with the time the disk node wrote the
  group, for 127 of 128 legs. Every leg that read non-optimized got its
  optimized write after the read, up to 1.775 s later. The one exception,
  leg 0x31 on dn3, read non-optimized 13 ms after its disk node wrote group
  1: cn1's kernel had not yet taken in the ANA change. A disk node reloads
  before it writes the group, so an optimized path maps the side device
  whenever its reload succeeded. Every reload did in this run: all 256
  reloads of the switch-over exited 0.
- The pending leg rows did not cause it. They come from probers that a
  promotion starts afresh (`legInfo` and `legProbeOutcome`), and a prober's
  result never decides whether a leg is available.
- The cause: the worker sent the promotion when the second wait of the hold
  ended by its 2 s timer, with 98 sides not yet reported. Switching all 128
  sides took about 4.3 s on these guests, from the worker's send at
  03:40:30.597 to the last side's last step at 03:40:34.873.
- The docs accept this race. The bounded waits make it "rarer, not
  impossible" (`architecture.md`, Failover, the new primary's step 1). Such
  a promotion "leaves the new primary's groups to the agent's own retry"
  (`dnv-worker.md`, RW14, which lets the cntlrs go after one interval
  whether or not every side has reported).

### The mechanism

**In the cn agent.**

- `buildSubsystems` (`agent/cnagent/plan.go`) sets each namespace's wanted
  group. It reads only the desired state, never the built devices. The group
  is optimized when all four of these hold, and inaccessible otherwise:
  - The cntlr is the primary and not disabled.
  - The pool's level has not reached its last step, which removes
    everything of the pool from the controller node (`SP_LEVEL_DISABLE`;
    `cnagent.md`, CN19).
  - The namespace is not suspended, by an operator or by a transfer.
  - Its thin device is not provisioning-deferred.
- `build` (`agent/cnagent/syncup_cntlr.go`) builds from the legs up. Its last
  loop, commented "ANA rewrites to optimized last", moves to optimized every
  namespace whose wanted group is optimized. The move to inaccessible comes
  earlier in the pass (`cnagent.md`, CN9, pre-step 1). The loop makes one
  exception, the clone hold. A clone hold keeps an ns-dev off its raid0
  while a dm-clone that the cntlr no longer wants may still write into that
  raid0 (`cnagent.md`, CN18, which calls it a hold). This file says clone
  hold to keep it apart from the worker's hold. A namespace under a clone
  hold keeps its group while its ns-dev serves, and goes inaccessible when
  its ns-dev is parked.
- `ensureNsDev` (`agent/cnagent/td.go`) fails when the raid0 it should map is
  missing ("dm device … is not there", from `dmDevNo`). It returns
  `nsDevConverged` with the error. That value means that no clone hold
  applies, so the ANA loop goes ahead. The ns-dev keeps its old table, which
  maps dm-error.
- So at 03:40:38.985 and 03:40:38.989 both namespaces went from group 3 to
  group 1, over dm-error.

**In the docs.** The code does what `architecture.md` and `cnagent.md` say,
except for one time bound:

- The ANA rule makes a namespace optimized when the cntlr is the primary
  and not disabled, the namespace is not suspended, its thin device is not
  provisioning-deferred, and the level has not reached its last step
  (`cnagent.md`, CN16). It reads the desired state, not the stack.
- The build order ends with "ANA rewrites to optimized last" (`cnagent.md`,
  CN9).
- The new primary's step 4 "does not wait for steps 1 to 3 to succeed". It
  moves the namespaces to optimized "over the td's `CnErrorName`" (the thin
  device's dm-error), and host IO on that path fails "until the agent's
  retry has built the stack" (`architecture.md`, Failover).
- "What hosts see at a failover" says the same (`architecture.md`,
  Failover).
- The `cnagent.md` known limit "A promotion that outruns the sides' flip
  serves IO errors until the retry builds the stack" names what is missing.
  It says: "An ANA conjunct in the style of `architecture.md`, [D15] —
  optimized only once the ns-dev is on the backing it wants — is not done
  here" (`cnagent.md`, Known limits). A conjunct is one more condition of
  the ANA rule. [D15] is the decision that a new side is zeroed, where the
  design assumes zeros, before its first export, and used only once it is
  provisioned. The provisioning-deferred condition of the ANA rule comes
  from it (`architecture.md`, [D15]). The known limit has said the same
  since commit `f6cf673` (2026-09-27). The wording quoted here dates from
  commit `ac2bbef` (2026-10-02).
- The same known limit bounds the errors, and this run breaks that bound.
  The limit says that the errors last until the first background retry
  attempt after the sides' flips reach the controller node builds the
  stack, "within about one `CnConnectRetryInterval` of that", which is 5 s.
  Here the last flip landed at 03:40:34.763, and the errors ended by about
  03:40:51.34, about 16.6 s later. Two things add up. The promotion's
  converge registered the retry when its group step ended, at about
  03:40:38.1, and the first attempt comes one interval after that, at
  03:40:43.106 (`connectRetryLoop` in `agent/cnagent/leg.go` starts a 5 s
  ticker). The attempt itself then took about 8.3 s at 32 slices.
- `cnagent.md`, CN9, calls "promoting a namespace over a non-existent
  device" the hazard that the provisioning-deferred part of the ANA rule
  prevents. A promotion that outruns the switch-over meets the same hazard
  with no such guard.

The code contradicts the e2e cutoff case. That case says "Not one write may
fail while the loop runs", and that the writes "must run again once the new
primary has built its stack and its namespaces have turned optimized"
(`e2e_integtest.md`, The cases). It also contradicts that doc's known limit
"The cutoff case's pause lasts the new primary's build" (`e2e_integtest.md`,
Known limits). Since `33c07d3`, that doc also says that the cutoff case's
failover runs at the run's own slice count, and so at the widest shape the
suite builds (`e2e_integtest.md`, "Purpose — what the suite proves", and
Known limits, "react is capped below the ceiling"). That shape is 32 slices
with RAID1, at two cntlrs. 32 is the most slices a pool may have
(`MaxSliceCntPerSp`).

**On the host.** From the kernel source (`drivers/nvme/host/multipath.c`,
`drivers/nvme/host/core.c` and `drivers/nvme/target/`):

- The driver sends IO on an optimized path, else on a non-optimized one. It
  skips inaccessible paths and paths whose controller is not live
  (`nvme_find_path`, `__nvme_find_path`).
- With no usable path but a controller that is live, resetting or
  connecting, it requeues the IO, with no time limit
  (`nvme_ns_head_submit_bio`, `nvme_available_path`).
- It sends the queued IO again when a path on a live controller turns
  optimized or non-optimized (`nvme_update_ns_ana_state`,
  `nvme_mpath_set_live`, `nvme_requeue_work`). An `ana_grpid` write on the
  target sends the host an ANA change notice (`nvmet_send_ana_event`).
- IO that reaches an inaccessible namespace gets an ANA error
  (`nvmet_check_ana_state`). The host treats it as a path error: it requeues
  the IO and reads the ANA log again (`nvme_failover_req`). The application
  sees no error.
- IO that reaches an optimized namespace over dm-error fails in the block
  layer. nvmet turns that into an internal error with DNR
  (`blk_to_nvme_status`), the "sc 0x6 … DNR" of host0's log. The host
  completes it as failed, with no retry (`nvme_decide_disposition`), and dd
  gets EIO.
- So, had the namespaces stayed inaccessible until 03:40:51.35, the queued
  write would have waited about 22 s and then completed. This is inferred
  from the code.

### Why it is older than the slice-32 fix

- The slice-32 commit changes nothing under `worker/` (the hold and the
  syncups), `cdc/` or `gateway/`. Under `agent/dnagent/` it changes only
  tests. In `common/constants.go` it changes one comment: the retry interval
  (5 s), the first wait of the hold (3 s) and the check interval (2 s) are
  the same.
- At `8bb1f25` every step of the mechanism above is the same. That holds
  for the availability test, the assembly with "no available leg", the
  retry for a late member (a group member that is not available in the
  pass), the ANA rule in `buildSubsystems`, the last ANA loop in `build`,
  and `ensureNsDev` returning `nsDevConverged` with an error. Only the error
  text differs: `8bb1f25` found the raid0's number with lsblk, not with
  `dmsetup info`.
- The dn agent still changed through shared code (`agent/store.go`,
  `agent/nvmehost.go`, `agent/oswrap.go`, `agent/dm.go`,
  `common/osclient.go`). It now lists sysfs and its local store in its own
  process, and both agents use one NVMe walk (the commit message's
  paragraphs 4 to 6). That may change how long a switch-over takes. It does
  not change the switch-over's order or the cn agent's ANA rule.
- The docs have described this behaviour since 2026-09-27.
- No cutoff run at 32 slices before the fix is known. The full e2e run at
  32 slices in the 2026-10-12 runner pass (commit `0ed4ae7`) failed at
  setup, at host0's connect (stage it-setup-10). On `8bb1f25`, a run of the
  smoke case alone failed at the same stage with 2-vCPU controller-node
  guests, and two such runs passed with 8-vCPU guests.

### Why the slice count matters

At a failover every side switches over in one converge on its disk node.
The pool has 4 sides at 1 slice, 16 at 4 slices and 128 at 32 slices. Each
disk-node guest has 2 vCPUs and switches its sides through its own dn agents.
Here the switch-over of 128 sides took about 4.3 s, and dn3, with 39 sides,
finished last. The second wait of the hold lets the cntlrs go after 2 s at
most, so the promotion reached cn1 while 98 sides had not yet reported. At
that moment 77 sides had not yet made the new primary's export optimized,
and when cn1 read its paths just after, 41 still read non-optimized.

At 1 and 4 slices the cutoff case passed. The suite's first look at the new
primary already showed every pool and group built while the leg rows were
still pending (the counts are of OK rows):

| Slices | Sides | First look at the new primary | Longest write | Failed writes |
|---|---|---|---|---|
| 1 | 4 | pools 1/1, groups 2/2, legs 0/4 | 1952 ms | none |
| 4 | 16 | pools 4/4, groups 8/8, legs 0/16 | 3084 ms | none |
| 32 | 128 | pools 7/32, groups 27/64, legs 128/128 | 9960 ms, then failed | 214 |

So at 1 and 4 slices the promotion's own converge most likely found every
leg available. No guest logs of those runs were kept, so this is inferred
from the suite's lines. Whether the second wait ended by the sides' reports
or by its timer in those runs is not known either.

### How to reproduce

Put this entry in a runner configuration file (the format is in the header
of `integtest/runner.conf`):

```
e2e-32-cutoff   integtest/e2e_test.sh --slice-cnt 32 --only cutoff \
        --cp yupeng@192.168.122.78 \
        --cn yupeng@192.168.122.125 --cn yupeng@192.168.122.229 --cn yupeng@192.168.122.77 \
        --dn yupeng@192.168.122.48 --dn yupeng@192.168.122.70 \
        --dn yupeng@192.168.122.49 --dn yupeng@192.168.122.122 \
        --host yupeng@192.168.122.193 --host yupeng@192.168.122.197
```

- Run `bash integtest/runner.sh --conf <that file> --out <a new or empty
  directory>`.
- It takes about 8 minutes and uses all ten guests. No other suite may run
  meanwhile (`e2e_integtest.md`, E2E9, one dnv suite at a time in the lab).
- After a failure the runner copies the guest logs to
  `<out>/01-e2e-32-cutoff.remote/` and then runs the suite's cleanup.
- It fails when the 128 sides take longer to switch over than the 2 s second
  wait of the hold.
- Signs in the logs: in the worker log, right after the failover's "sp
  demotion unsynced", an "sp sides unsynced" with a large `side_cnt`. In the
  new primary's agent log, inside the promotion's trace id: mdadm's "need all
  2 to start it", "dm device … is not there", and an `ana_grpid` write of 1.
  In host0's kernel log after the case's marker (a line the case writes
  into that log before the loop starts): "sc 0x6) MORE DNR".

### Options

1. *Optimized only over a serving ns-dev* (cn agent, a new condition in the
   ANA rule).
   - What changes: the build moves a namespace from inaccessible to
     optimized only in a pass that put its ns-dev on the device the desired
     state wants it to map. A namespace that is already optimized is not
     moved back on a passing error. A namespace kept inaccessible this way
     registers the background retry.
   - Here: the first converge would have left both namespaces inaccessible
     and host0's write queued. The retry attempt would have moved them to
     optimized at about 03:40:51.35, and the write would have completed
     after about 22 s. This is inferred.
   - Check rounds: a round compares each namespace's group with the wanted
     group (`agent/cnagent/probe.go` calls `probeNamespaceObject`, in
     `agent/cnagent/td.go`, with `np.anaGrpId`). Either the round learns the
     same condition, or the namespace's row reads ERROR with the details
     "ana_grpid is 3, want 1". The ns-dev's row reads ERROR in this window
     already.
   - Docs to change: the ANA rule (`cnagent.md`, CN16); the build order
     (`cnagent.md`, CN9); the new primary's step 4 and "What hosts see at a
     failover" (`architecture.md`, Failover); the `cnagent.md` known limit
     on a promotion that outruns the sides' flip. The e2e cutoff text then
     holds as written.
   - Cost: a small change in `build`, with tests in both directions.
   - Risks: when a stack can never be built, a host waits without limit
     instead of getting EIO. The scope needs a decision on two cases. One is
     the error window of clone crash recovery, the primary's rebuild of a
     clone whose dm-clone or its metadata was lost: in that window a
     recovering clone's namespace stays optimized over dm-error until
     hydration is enabled (`cnagent.md`, CN18 step 4, and CN16 rule 5). The
     other is transfer namespaces (`cnagent.md`, CN17). It must keep the
     no-thin-pool level as it is. At that level step, which removes the thin
     pools, the ns-dev maps dm-error and the namespace stays optimized on
     purpose, so that a host sees IO errors rather than a missing device
     (`cnagent.md`, CN19, and CN16 rule 3). The ANA rule then reads the built
     devices, not only the desired state.
2. *No move to optimized from an incomplete pass* (cn agent, the pass as a
   whole).
   - What changes: a pass that registers the background retry moves no
     namespace to optimized.
   - Docs to change: as for *Optimized only over a serving ns-dev*.
   - Cost: smaller than *Optimized only over a serving ns-dev*.
   - Risk: it is coarse. A late member also registers the retry when the
     array runs without it. So a new primary whose stack is built but
     degraded keeps its namespaces inaccessible, for example after a disk
     node died. That lasts until the leg returns or leg repair replaces it.
     Leg repair first waits out the leg threshold, or the side threshold when
     the leg's side is unhealthy too (`dnv-worker.md`, AR8).
3. *A second step from the worker* (worker).
   - What changes: the promotion keeps the namespaces inaccessible, through
     a new field of the cntlr record or through the cn agent reading
     `settling`, which it ignores today. When the worker settles the cntlr
     (`dnv-worker.md`, HL2, which clears settling once the cntlr reports its
     stack built and clean), it commits a second revision that releases
     them.
   - Docs to change: `architecture.md` (the etcd data model and Failover);
     `dnv-worker.md`, RW14 (the sides-first wait), RW22 (the demotion hold),
     HL2 (settling) and AR5 (failover); `cnagent.md`, CN16 (the ANA rule),
     and its known limit on a promotion that outruns the sides' flip.
   - Cost: every failover gets one more revision, one more round of syncups
     and a check round, about 2 to 4 s.
   - Risks: a new primary's namespaces then wait for a second commit in
     etcd. If etcd goes away between the two commits, they stay inaccessible
     until it returns. Today the data plane goes on serving what it last
     applied (`architecture.md`, Running without the control plane). And
     the cn agent reading `settling` keeps the namespaces inaccessible in
     the other cases that set it too: a cntlr replacement that makes a
     primary, a pool's creation and the enabling of a cntlr that is still
     primary (`dnv-worker.md`, HL2). Whether that is wanted in those cases
     is not decided.
4. *A longer second wait for a promotion* (worker).
   - What changes: the promotion's syncup waits for every side's report,
     with no bound, or with a bound that grows with the number of sides.
   - Docs to change: `dnv-worker.md`, RW14 (the sides-first wait);
     `architecture.md`, [D16] (both waits bounded, neither needed for
     correctness) and Failover; the time bound of the `cnagent.md` known
     limit on a promotion that outruns the sides' flip (In the docs).
   - Cost: a small worker change.
   - Risks: it only narrows the race, because a controller node's kernel
     takes in an ANA change some milliseconds after the disk node writes it
     (leg 0x31 above). A side that stops answering would hold the promotion;
     that is why both waits are bounded and neither is needed for
     correctness (`architecture.md`, [D16]). Every large failover gets
     longer.
5. *The promotion's converge waits for its late legs* (cn agent).
   - What changes: the converge waits a bounded time for member paths that
     are not yet optimized, or reads availability again just before each
     group's assembly. Here the group loop ran from 03:40:33.738 to
     03:40:38.102, and the last optimized write landed at 03:40:34.763.
   - Docs to change: `cnagent.md`, CN10 (the connect step and its wait
     budget) and CN12 (availability); the time bound of the `cnagent.md`
     known limit on a promotion that outruns the sides' flip (In the docs).
   - Cost: small.
   - Risks: it only narrows the race, so *Optimized only over a serving
     ns-dev* is still needed for no failed writes. The converge holds the
     cntlr lock longer, and check rounds wait behind it.
6. *An early first retry after a promotion* (cn agent).
   - What changes: the first background retry attempt runs at once, not 5 s
     later.
   - Effect here: writes would have failed for about 8 s instead of 12 s.
     The errors remain.
   - Docs to change: `cnagent.md`, CN10 (the background retry and its
     interval); the time bound of the `cnagent.md` known limit on a
     promotion that outruns the sides' flip (In the docs).
   - Cost: small.
   - Risk: more waiting for the cntlr lock. One retry attempt held it for
     about 8.3 s at 32 slices.
7. *Keep the design, change the case* (doc and suite only).
   - What changes: the cutoff case accepts failed writes when the promotion
     outruns the switch-over, or it builds its pool with fewer slices, as
     react does with `REACT_SLICE_CAP`.
   - Docs to change: `e2e_integtest.md`, The cases (the cutoff case) and
     Known limits; the time bound of the `cnagent.md` known limit on a
     promotion that outruns the sides' flip (In the docs).
   - Cost: doc and suite only.
   - Risk: the suite's statement "A primary cut off from the control plane
     fails over as a pause" (`e2e_integtest.md`, Purpose — what the suite
     proves) then no longer holds for large pools on slow disk nodes, and
     nothing tests the failover at the widest shape.

Rejected, because they are not safe:

- Starting a group degraded with `mdadm --run`. Failover safety rests on
  mdadm refusing exactly that (`architecture.md`, [D16]).
- Counting a non-optimized path as available. That path is a standby's
  export, which maps dm-error (`cnagent.md`, CN12).
- Parking the ns-dev by suspending it. A suspended device-mapper device
  queues IO forever with no timeout, and dnv bounds every suspension
  (`architecture.md`, [D12]).
- Using the ANA change state. dnv uses three fixed groups only
  (`architecture.md`, [D4]). And a Linux host resets a controller whose
  namespace stays in the change state longer than twice the target's ANA
  transition time, 2 × 10 s with nvmet (`nvme_read_ana_log` in
  `drivers/nvme/host/multipath.c`; `id->anatt = 10` in
  `drivers/nvme/target/admin-cmd.c`). The pause here was about 22 s, longer
  than those 20 s.

### Suggestion

*Optimized only over a serving ns-dev*, kept narrow. The build moves a
namespace from inaccessible to optimized only in a pass that put its ns-dev
on the backing that `cnagent.md`, CN16, selects for a serving namespace.
That is the dm-clone while a clone fills the thin device (`cnagent.md`,
CN16 rule 5), else the thin device's raid0 (`cnagent.md`, CN16 rule 6).
Once the level makes the pool read-only, either one sits under a dm-flakey
that fails writes (`cnagent.md`, CN16 rule 7). The build leaves a namespace
that is already optimized where it is. It keeps the states that are
dm-error and optimized on purpose as they are. One is the no-thin-pool
level (`cnagent.md`, CN16 rule 3). The other is a namespace whose thin
device a clone fills, at the level step that removes clone stacks
(`cnagent.md`, CN16 rule 4). The error window of clone crash recovery is
decided on its own. This makes the failover a pause, as the cutoff case
promises (`e2e_integtest.md`, The cases). `architecture.md`, Failover, "What
hosts see at a failover", accepts IO errors in this case today and changes
with it, and so does the `cnagent.md` known limit that names this change.
*The promotion's converge waits for its late legs* or *An early first retry
after a promotion* can follow to shorten the pause; alone they do not remove
the errors.

### Questions for the owner

1. *The requirement.* Which statement is the requirement: the cutoff case's
   "not one write may fail", or the accepted limit that a promotion which
   outruns the sides' switch-over serves IO errors until the retry builds
   the stack? Either way, that limit's time bound needs a fix, since this
   run broke it (In the docs).
2. *The fix.* If the cutoff statement wins, which option: per namespace
   (*Optimized only over a serving ns-dev*), per pass (*No move to
   optimized from an incomplete pass*) or *A second step from the worker*?
3. *A stack that is never built.* When a stack can never be built, should a
   host's IO wait without limit (namespace inaccessible) or fail with EIO
   (namespace optimized over dm-error)?
4. *The scope.* Should the same rule cover the error window of clone crash
   recovery and transfer namespaces?
5. *A shorter pause.* Should the pause also get shorter, by *A longer second
   wait for a promotion*, *The promotion's converge waits for its late legs*
   or *An early first retry after a promotion*? Is a pause of about 22 s at
   32 slices on these guests acceptable?

## The migration-gap wedge

**In one line.** During a migration at 32 slices, the primary's cntlr
blocked for about 31 minutes. A check round read the status of the migrated
slice's thin pool, which made dm-thin commit the pool. The commit's flush
waited on the migrating leg, which had no usable path. The round held the
cntlr lock while it waited, and the syncup that would have connected the
primary to the new side needed that lock.

### How it shows

- The copy case fails at stage it-copy-04, "migr create, append-bm, finish —
  then a second one, cancelled". In that title, append-bm is `dnvctl migr
  append-bm`, which gives the migration one chunk of its skip bitmap: the
  leg's blocks that were never written, which the new side need not copy.
  The failure message is: "timed out after 600s waiting for: cn0 to hold
  ANY nvme controller for
  nqn.2024-01.io.dnv:2:f4e35cb180bd77b0:0000000000000001:000000000000000a:0000000000000001
  on its path to 192.168.122.122:4334 (there is no ANA state without one)".
  That NQN is leg 10's export to the primary's controller node. The suite
  waits for the primary to connect to the new side.
- The suite's polls of the new side saw its dm-clone row go PROVISIONING,
  then MISSING, then OK (last poll 01:41:46.796). The new side was ready
  within seconds.
- No host saw anything. The copy case issues no host IO in this window on
  purpose: its own comment in `integtest/e2e_test.sh` warns that a host read
  there blocks in D state.
- The suite's diagnostics then blocked for about 21 minutes (below), and the
  whole failing run took 61m43s.

### Where and when

- The run: the full e2e run at 32 slices, case copy. There is one such run;
  no earlier copy run at 32 slices is known.
- The pool: 32 slices, 64 RAID1 groups, 128 legs, on 180 disk nodes. The
  copy case's event thresholds: primary 1800 s, cntlr 1800 s, side 1800 s,
  leg 3600 s.
- The primary is cntlr 1 on cn0 (192.168.122.125, controller node id 1). The
  standby is cntlr 2 on cn2 (192.168.122.77, controller node id 3). cn1
  (192.168.122.229) holds no cntlr.
- The migrated leg is leg 10 of data group 9 in the slice with id 3, which is
  the pool's first slice. Its old side is side 11 on dn agent 39 of dn2
  (192.168.122.49:29939; nvmet port 40, TCP port 4339). Its new side is side
  363 on dn agent 34 of dn3 (192.168.122.122:29934; nvmet port 35, TCP port
  4334). The migration is m0, with id 362.
- On cn0, leg 10's multipath head is `nvme35n1`, with one controller,
  `nvme35`, to 192.168.122.49:4339. Its leg wrapper is `dm-17`. md123 is the
  RAID1 of legs 10 and 12; md124 is that of legs 5 and 7. The slice's thin
  pool, `…-c2-0000000000000001-0000000000000003`, keeps its data on md123 and
  its metadata on md124.

### Timeline

- **01:41:36.281**, gateway: CreateMigration m0 for side 11. The pool goes to
  revision 42. The worker sends revision 42 to the new side at 01:41:36.491.
  The new side's agent logs the request at 01:41:36.514. The new side is
  still not provisioned, so it only zeroes. The worker sends revision 42 to
  the old side at 01:41:36.648, and the old side serves as before.
- **01:41:37.226 to 01:41:40.778**, cn0: the converge of revision 42, 3.55 s.
  It skips the unprovisioned new side. Its status read of slice 3's pool at
  01:41:39.507 returns normally: the gap has not started.
- **01:41:39.567**, worker: `"flip applied" kind provisioned side 363
  revision 43`: the worker marks the new side provisioned.
- **01:41:39.64**, both sides receive revision 43. The old side's request
  carries `dst_provisioned: true`, the new side's `provisioned: true`.
- **01:41:40.243**, old side: step 1, `ana_grpid` 3 (inaccessible) for leg
  10's export to cn0, and at 01:41:40.336 for the export to cn2. **The gap
  starts.**
- **01:41:40.465**, old side: step 2a suspends its dm-linear for cn0, and at
  01:41:40.538 the one for cn2: the cutover window.
- **01:41:40.777**, new side: its first connect to the old side's migration
  source export fails ("could not add new controller: failed to write to
  nvme-fabrics device"). That export is linked to its port only at
  01:41:41.258.
- **01:41:40.781**, cn0: check round `021d4406-042201f1a6fe71e4`, at revision
  42, opens 3 ms after the converge replied. The worker's round timer had
  run out during the converge.
- **01:41:40.785 to 01:41:40.818**, cn0: the round's reading. Then its first
  `dmsetup status`, of slice 3's pool. That command has no further record
  until 02:12:57.428.
- **01:41:41.638**, worker: `"sp sides unsynced" revision 43 side_cnt 57`:
  the second wait of the hold ended by its timer. The worker sends revision
  43 to both cntlrs.
- **01:41:41.640**, cn0: receives the syncup of revision 43, trace id ending
  `496eb0c871901bb6`. It logs nothing more until its reply at 02:12:57.445:
  it waits for the cntlr lock.
- **01:41:42.036**, cn2 (standby): its connect to the new side works, after
  two refused tries at 01:41:41.732 and 01:41:41.881. It replies at
  01:41:42.784.
- **≈01:41:43.8**, cn0: the agent sends SIGTERM to the blocked `dmsetup` at
  the 3 s soft timeout. No record shows the signal; the time comes from the
  code. The process does not end, so it is most likely in D state.
- **01:41:45.966**, new side: its retry connects to the old side's export.
  The dm-clone is created at 01:41:46.076. The export to cn0 moves to group 1
  (optimized) at 01:41:46.277, the one to cn2 to group 2 at 01:41:46.286. For
  a connected primary, the gap would have ended here, 6.03 s after it
  started.
- **≈01:41:47 to 01:51:47**, suite: the 600 s wait for cn0's controller to
  192.168.122.122:4334 runs out.
- **01:42:40.36**, old side: step 2b, 60 s after step 2a, reloads its
  dm-linear for cn0 onto dm-error and resumes it.
- **01:42:41.638**, worker: the syncup of revision 43 to cntlr 1 ends with
  DeadlineExceeded, after 60 s. At the same moment the worker drops round
  `…042201f1a6fe71e4`. That round had been abandoned when the syncup started,
  so it gave no health verdict (`dnv-worker.md`, RW6). While the syncup was
  in flight, the worker sent cntlr 1 no check round.
- **01:42:45.643**, worker: round `…85546b3f9163b661`, opened at 01:42:43.640,
  gets no answer, since on the agent it waits for the cntlr lock. The worker
  sets cntlr 1's health epoch with the reason "unreachable". After that a
  round comes about every 4 s, and each one times out.
- **01:51:47.8 onwards**, suite diagnostics: `dnvctl cntlr inspect --id 1`
  fails, because the gateway's info read needs the same lock (it timed out
  at 01:51:58.447). The standby's leg 10 row reads "192.168.122.49:4339
  live/inaccessible,192.168.122.122:4334 live/non-optimized". cn0's own
  diagnostics start at about 01:52:02 and run `dmsetup status` over every
  device without `--noflush`. They block on the same pool until about
  02:12:57, although they run under `timeout 60`, and never print cn0's
  kernel log.
- **02:12:46.747**, worker: failover, revision 44, 1801.1 s after the health
  epoch; the copy case's primary threshold is 1800 s. cn0 receives it at
  02:12:46.808 and waits for the lock.
- **02:12:48.765**, worker: cntlr replacement, revision 45. Cntlr 1 is
  replaced by cntlr 364 on cn1.
- **02:12:49.806**, worker: `"sp demotion unsynced"` for revision 45: the
  blocked cntlr 1 could not apply its demotion. The old side gets revision 45
  at 02:12:49.818; it never saw revision 44.
- **02:12:50.480 to 02:12:52.280**, old side: removes its export of leg 10 to
  cn0: port unlink at 02:12:50.480, namespace removed at 02:12:50.941,
  subsystem removed at 02:12:51.157, dm-linear at 02:12:51.681 and its
  dm-error at 02:12:52.280.
- **02:12:56.905**, cn0: leg 10's prober gets EIO. Its last good write was at
  01:41:35.407. Its next one, due about 5 s later and so inside the gap,
  returns only now. The queued IO was failed.
- **02:12:57.428**, cn0: the `dmsetup status` returns with exit code −1 and
  "signal: terminated".
- **02:12:57.445**, cn0: the syncup of revision 43 replies with code 4, the
  leftover code (`ReplyCodeLeftover`), by which the agent says that it
  cannot call the node clean. Here it could not list the node: "enumeration
  failed: dmsetup ls: … context canceled; … md arrays …; … nvme subsystems
  …; … nvmet subsystems …: context canceled". Its deadline had passed at
  01:42:41.638, so it connected nothing.
- **02:13:09.944**, cn0: the converge of revision 44 replies OK.

The block on cn0 lasted about 31 min 17 s.

### Why there is a gap at all

- The old side runs its migration steps only once its request says the new
  side is provisioned. With the flag false, it behaves as if there were no
  migration (`dnagent.md`, DN12, the `dst_provisioned` gate).
- The worker sets that flag in the same request build as the new side's own
  `provisioned` flag: the old side's request copies the new side's flag
  (`dnv-worker.md`, RW15; `worker/sprole.go`). The worker marks the new side
  provisioned once its zeroing is complete, and that write raises the pool's
  revision (`dnv-worker.md`, RW18).
- The worker hands a revision to the sides first. The cntlrs wait until the
  sides report it applied or until 2 s have passed (`dnv-worker.md`, RW14).
- So the old side cuts the primary's path before the primary even receives
  the revision that tells it to connect to the new side. The old side's step
  1 moves its per-CN namespaces to inaccessible (`architecture.md`,
  Migration, src side step 1). The primary connects to a provisioned side in
  its converge and skips one that is not provisioned (`cnagent.md`, CN10).
- The gap ends with the new side's step 5: it reloads the primary's
  dm-linear onto its dm-clone and moves that namespace to optimized
  (`architecture.md`, Migration, dst side step 5). Step 5 needs step 3, the
  connect to the migration source export, which exists only after the old
  side's step 3, and step 4, the dm-clone. The new side retries the connect
  until it works (`dnagent.md`, DN13).
- Without any block, the gap lasted 6.03 s in this run (01:41:40.243 to
  01:41:46.277) and 5.22 s on 2026-10-12. Most of it is the new side's 5 s
  retry: its first connect is refused, because the old side builds its
  export only after its steps 1 and 2a, the move to inaccessible and the
  suspend. Here the export was linked to its port 1.6 s after the old side
  received revision 43.
- During any gap, IO that reaches the leg waits, and so does every commit of
  that slice's pool. While a commit waits, every other IO of that pool waits
  too.

### The mechanism

**In dm-thin and md** (`drivers/md/dm-thin.c` and
`drivers/md/dm-thin-metadata.c`, unless a line names another file):

- A `dmsetup status` of a thin pool runs `pool_status`. For the status line
  it first commits the pool, unless the caller passed the noflush flag
  (`dmsetup status --noflush`) or the pool is suspended: `if
  (!(status_flags & DM_STATUS_NOFLUSH_FLAG) && !dm_suspended(ti)) (void)
  commit(pool);`.
- The commit (`commit`, `dm_pool_commit_metadata`, `__commit_transaction`)
  holds the pool's metadata lock for writing the whole time. It first runs
  `metadata_pre_commit_callback`, which is `blkdev_issue_flush` of the
  pool's data device. Then it writes the changed metadata blocks and flushes
  the metadata device. A pool whose metadata changed since it was opened
  does this on every commit, so a pool in use always does.
- The flush of the data device reaches every data group of the slice. md
  sends a flush to each member that is not faulty, and completes the array's
  flush only when every member has answered. A member's flush error is only
  logged (`md_flush_request` and `md_end_flush` in `drivers/md/md.c`).
- `blkdev_issue_flush` (`block/blk-flush.c`) waits in `submit_bio_wait`
  (`block/bio.c`), which sleeps in D state.
- While the commit waits, every IO of the pool's thin volumes waits for the
  metadata lock, and so does dm-thin's own worker.
- dm-thin also commits by itself. It does so about once a second after new
  blocks were mapped, and on a host flush to a thin device that changed. It
  also commits on every pool message (create_thin, create_snap, delete),
  before a metadata snapshot reserve, and when the pool is suspended. A
  dm-clone's status read commits too, but only when its metadata changed
  (`clone_status` and `commit_metadata` in `drivers/md/dm-clone-target.c`).

**In nvme multipath.** The controller node is the NVMe-oF host of its legs.

- A path is used only when its controller is live and its ANA state is
  optimized or non-optimized.
- With no usable path but a controller that is live, resetting or
  connecting, the driver requeues the IO, with no time limit. The IO goes
  out again when a path becomes usable, when a controller turns live, when
  `fast_io_fail_tmo` runs out after the controller started to reconnect, or
  when the head is removed.
- dnv connects legs with `--fast_io_fail_tmo 5 --ctrl-loss-tmo -1`
  (`agent/nvmehost.go`).
- So while the old side's controller is live and its namespace
  inaccessible, a flush or a write to the leg waits without limit.

**In the cn agent.**

- A check round of a cntlr takes the node lock for reading and then the
  cntlr lock, and holds both for the whole round (`checkCntlrRound`,
  `agent/cnagent/check.go`). A syncup, an info read and a background retry
  attempt of the same cntlr take the same two locks (`cnagent.md`, CN1).
- After its reading, the round reads the status of each thin pool in slice
  order: `probeCntlr` (`agent/cnagent/probe.go`) calls `probePool`
  (`agent/cnagent/pool.go`), which calls `Dm.Status` (`agent/dm.go`), which
  runs `dmsetup status <name>`. No Go file passes `--noflush`. The docs say
  the same: dnv reads status without the flag that skips the commit, so a
  status read commits the pool and waits on that IO (`dnagent.md`, SH17;
  `cnagent.md`, CN28).
- So round `…042201f1a6fe71e4` most likely committed slice 3's pool, the
  commit's flush went through md123 to leg 10 and was requeued, and the
  round's `dmsetup` sat in D state. These steps are inferred from the code,
  from the 2026-10-12 hung-task stack and from the SIGTERM that did not end
  the `dmsetup`; cn0's kernel log of this run was not captured. The round
  waited for the `dmsetup` while it held the cntlr lock: the code holds that
  lock for the whole round, and the syncup of revision 43 logged nothing
  until the `dmsetup` returned.
- The connect to a provisioned side is the first step of a converge's build
  (`ensureLegs`, `agent/cnagent/leg.go`; `cnagent.md`, CN10). So the syncup of
  revision 43, which carried the provisioned new side, waited for the cntlr
  lock and never connected. The background retry would have waited for the
  same lock.
- The cntlr lock is a plain Go mutex (`agent/locks.go`), so a waiter cannot
  give up when its deadline passes.

**Why no timeout ended it.**

- The agent sends SIGTERM to a command at the 3 s soft timeout, and SIGKILL
  2 s later (`common/osclient.go`, `agent/oswrap.go`). A process in D state
  acts on neither.
- Go's `cmd.Wait` waits until the child process exits.
- The first fatal signal sets the exit status. So the status read returned
  "signal: terminated" 31 minutes after the SIGTERM was sent, when the flush
  it waited on was failed.

**What ended it.**

- The cntlr replacement's revision 45 made the old side remove its export of
  leg 10 to cn0. About 6.4 s after the unlink, leg 10's prober got EIO, so
  the queued IO had been failed. That fits `fast_io_fail_tmo` of 5 s: at
  about 02:12:58 all 128 of cn0's controllers read "connecting", `nvme35`
  among them. Without cn0's kernel log this is not proven.
- md then completed the array's flush, since a member's flush error is only
  logged, and the commit finished. This is inferred from the kernel source;
  no log shows it. The status read returned at 02:12:57.428.
- The failover alone would not have ended it. A failover keeps cntlr 1 as a
  standby, and with it the old side's export to cn0 (inferred from the code;
  here the old side never received revision 44).

**Other calls that can commit the pool in the gap.** A check round is only
the case that was seen.

- Every converge reads the pool's status (`ensureSlice`,
  `agent/cnagent/pool.go`), and it may send thin messages (create_thin,
  create_snap, delete) or reload the pool.
- Reads through a thin metadata snapshot (`dumpThinMetadata`,
  `agent/cnagent/thinbm.go`), which reserve and release the snapshot. One
  kind is the bitmap reads, which tell which blocks of a thin device or of a
  leg were ever written. Another is the two thin-id sweeps, which delete the
  thin ids of a pool that no thin device owns: the activation sweep, and
  the sweep after a failed delete (`cnagent.md`, CN14). The last is clone
  crash recovery's read of the destination's bitmap.
- dm-clone status reads.
- dm-thin's own commits under host writes.
- `architecture.md` lists all of these but the dm-clone status reads and a
  pool reload (`architecture.md`, Known limits, "A pool commit in a
  migration gap can wedge the primary").

**A converge can block itself** (inferred from the code, not observed). A
converge in the gap whose connect to the new side fails within its 1 s wait
budget still goes on to its pool step: `build` does not stop at a failed
leg. Its status read would then block, and the background retry that would
connect later needs the same lock. Here the standby needed two refused tries
before its connect worked, so the first try is not sure to work.

**Wider effects seen.**

- 451 more check rounds opened on cn0 during the block. Each held the node
  lock for reading and waited for the cntlr lock.
- `dnvctl cntlr inspect --id 1` failed.
- The suite's own diagnostics on cn0 blocked for about 21 minutes, although
  each dump runs under `timeout 60`, and lost cn0's kernel log
  (*Diagnostics that keep the kernel log*, below).
- A syncup of the controller node arrived at 02:12:48.766 and waited about
  21 s for the node lock. The calls of cntlr 1 that held that lock for
  reading had to end first: the syncup of revision 43, the waiting check
  rounds and the info read, which all ended by 02:12:58.4, and last the
  syncup of revision 44, which replied at 02:13:09.944. The node syncup's
  first work record is at 02:13:09.953; it then ran for about 3 s and
  replied at 02:13:12.731. No other call reached cn0 meanwhile. The worker
  sends an object's check rounds and syncups one at a time
  (`dnv-worker.md`, RW1), so it sent no check round of the node while the
  node's syncup was in flight, and cntlr 1 was being replaced. So this run
  shows no head-of-line blocking.
- The first wait of the hold, after the failover and the replacement, ran
  out by its timer, because the blocked primary could not apply its
  demotion.

**At the default thresholds** (inferred from the rules and from this run;
not tested). The block lasted 31 minutes because the copy case uses quiet
thresholds of 1800 s. The defaults are 4 s for the primary and 600 s for a
cntlr (`common/constants.go`). With them, the same start would most likely
end like this:

- The worker sends cntlr 1 no check round while the syncup of revision 43
  is in flight, for up to 60 s. So, as here, the first round after the
  syncup's deadline times out and sets the health epoch, about 65 s into
  the gap.
- The next round times out too. With the 4 s primary threshold and two
  unhealthy rounds in a row, the worker fails cntlr 1 over, about 70 s into
  the gap (`dnv-worker.md`, AR5 and AR10).
- cntlr 1 cannot apply its demotion, so a host still on cn0's path can get
  IO errors from the fence (`architecture.md`, Known limits, "An old
  primary that cannot apply its demotion within the demotion hold fails IO
  from the fence").
- The failover keeps cntlr 1 as a standby, and with it the old side's export
  to cn0, so the block goes on. About 600 s after the health epoch, the
  worker replaces cntlr 1 (`dnv-worker.md`, AR7). The old side then removes
  its export of leg 10 to cn0, and the queued flush fails, as it did here at
  02:12:56.905.

So at the defaults a migration would most likely turn into an unplanned
failover about 70 s into the gap, and a cntlr replacement about 11 minutes
into it.

**Where the docs already describe it.**

- `architecture.md`, Known limits, has three entries on it. "A pool commit
  in a migration gap can wedge the primary" states the whole chain above,
  and what a cancel or a finish does. "The agent node lock has head-of-line
  blocking" states how a waiting node syncup holds up the node's other
  calls. "An old primary that cannot apply its demotion within the demotion
  hold fails IO from the fence" states what hosts still on its path would
  see.
- `dnagent.md`, SH17, and `cnagent.md`, CN23 and CN28, say that a status
  read of a thin pool commits it.
- The copy case's comment in `integtest/e2e_test.sh` describes the same trap
  for host IO.

### Why it is older than the slice-32 fix

- The same block was first seen on the lab on 2026-10-12, in e2e at 1 slice,
  at the same stage, on commit `0ed4ae7`, whose tree is the same as that of
  `8bb1f25`. A review had found it in the code on 2026-10-09. After the
  2026-10-12 sighting, the owner decided that it gets its own plan.
- The 2026-10-12 sequence, all on that date:
  - 07:16:58.807, worker: the new side is marked provisioned, revision 23.
  - 07:16:58.840, old side (on 192.168.122.70): moves its namespaces to
    inaccessible. The gap starts.
  - 07:16:58.906, primary (192.168.122.229): check round
    `e3354bbd-fdd0d971ffd71107`, at revision 22, opens on its normal 2 s
    timer. Its last read is at 07:16:58.934. Its `dmsetup status` of the
    pool returns only at 07:48:14.351, "signal: terminated".
  - 07:16:59.011, primary: the syncup of revision 23 arrives and waits. It
    times out on the worker at 07:17:59.012 and replies code 4 at
    07:48:14.351.
  - 07:17:04.064, new side: optimized.
  - 07:18:03.020, worker: the primary is unreachable.
  - 07:48:04.951 failover; 07:48:06.951 cntlr replacement.
- A hung-task report, the kernel's warning about a task blocked for minutes,
  was taken from that primary on kernel 7.0.0-38. In it, `dmsetup` waits in
  D state in `blkdev_issue_flush`, called from `metadata_pre_commit_callback`
  and `__commit_transaction`. The dm-thin worker is "blocked on an
  rw-semaphore likely owned by task dmsetup".
- In that run e2e at 4 slices passed, and e2e at 1 slice passed when run
  again on the same commit.
- How often it happens: by the lab history, e2e at 1 slice had passed four
  times before that sighting, and it passed again on the rerun that day and
  in the lab gate. So at 1 slice copy-04 failed this way in one of at least
  seven runs. At 32 slices it failed in the one run there has been. No run
  at 4 slices is known to have failed this way.
- The slice-32 change kept one `dmsetup status <pool>` per pool in every
  round, without `--noflush`. So the kind and the number of commits are the
  same. A round that the 2 s wait cut short before the change still read the
  first slice's pool first. So the change neither removed nor added the
  exposure.

### Why the slice count matters

This is reasoning. There is one sighting at 1 slice and one at 32 slices,
and copy has run once at 32 slices.

- The block needs a commit of the migrated slice's pool between the old
  side's step 1 and the primary's connect to the new side.
- From the old side's step 1 to the moment the primary received its syncup
  took 0.17 s at 1 slice and 1.40 s at 32 slices. At 32 slices the second
  wait of the hold ran into its 2 s timer, with 57 sides not reported. The
  worker log of the 1-slice run has no "sp sides unsynced" at all; the
  32-slice run has 15.
- In the copy case before the gap, a converge of the primary took 2.4 to
  4.6 s at 32 slices, against 0.08 to 0.39 s at 1 slice on 2026-10-12. The
  worker's round timer runs out during a long converge, so the next round
  starts as soon as the converge ends. Here that was 0.54 s into the gap.
- The copy case always migrates the first leg of the first data group of the
  pool's first slice, and a round reads that slice's pool first.
- A check round that did not wait behind a converge took 0.24 to 0.91 s at
  32 slices (the two just before the gap took 0.41 and 0.43 s), against
  0.06 to 0.12 s at 1 slice on 2026-10-12. A longer round holds the cntlr
  lock longer.

### How to reproduce

Put this entry in a runner configuration file:

```
e2e-32-copy     integtest/e2e_test.sh --slice-cnt 32 --only copy \
        --cp yupeng@192.168.122.78 \
        --cn yupeng@192.168.122.125 --cn yupeng@192.168.122.229 --cn yupeng@192.168.122.77 \
        --dn yupeng@192.168.122.48 --dn yupeng@192.168.122.70 \
        --dn yupeng@192.168.122.49 --dn yupeng@192.168.122.122 \
        --host yupeng@192.168.122.193 --host yupeng@192.168.122.197
```

- Run `bash integtest/runner.sh --conf <that file> --out <a new or empty
  directory>`. The full 32-slice run is the `e2e-32` entry of
  `integtest/runner.conf`.
- `--only copy` builds its own pool first (`e2e_integtest.md`, E2E11, which
  starts every case from an empty etcd and a new pool). Here setup took
  about 5 minutes, from the gateway's start at 01:35:39 to copy-01 at
  01:40:43, and copy-01 to copy-03 about 1 minute more, before copy-04 at
  01:41:35. The run uses all ten guests, and no other suite may run
  meanwhile (`e2e_integtest.md`, E2E9).
- Expected failure: copy-04 times out after 600 s on the primary's controller
  to the new side. The suite's diagnostics then block until the reactions,
  so a failing run takes about 39 minutes, and about 46 with the log copy
  and the cleanup.
- Expected rate: copy-04 failed in the one run at 32 slices there has been,
  and in one of at least seven runs at 1 slice (Why it is older than the
  slice-32 fix). So a single passing run at 1 slice proves little.
- Signs in cn0's agent log: a check round whose last records are reads,
  followed by a `dmsetup status …-c2-…-0000000000000003` that returns only
  with "signal: terminated", and a syncup with no records between its
  request and its reply.
- To see the kernel side during the 600 s wait, on cn0: `sudo dmesg | grep
  -E 'requeuing|blocked for more'`, and `sudo cat /proc/<pid>/stack` for
  each pid that `pgrep -x dmsetup` prints.
- To end the block by hand: cancel the migration with `dnvctl migr cancel
  --name m0` on the control-plane guest, with the gateway address, cluster
  and pool flags the suite passes to every dnvctl call. A cancel gives the
  primary back its path to the old side, so the waiting commit finishes
  (`architecture.md`, Known limits). A cancel is a write to etcd alone, so
  the blocked primary does not block it.

### Options

1. *Connect first* (worker, both agents, and the messages between worker
   and agents).
   - What changes: the worker no longer sets the old side's
     `dst_provisioned` in the same revision that marks the new side
     provisioned. The new side builds its per-CN exports on dm-error,
     inaccessible, as its step 1 does today. The primary connects to them in
     its next converge (`cnagent.md`, CN10). Only when the primary reports a
     live controller to the new side does the worker set `dst_provisioned`
     on the old side, in a revision of its own. That wait in the worker is
     called the gate below.
   - Effect: the gap stays, about 6 s here, set by the disk nodes. But its
     end needs no lock on the controller node. The new side's step 5 makes
     the already connected path usable, the kernel sends the requeued IO
     again, and the waiting commit finishes. Any commit in the gap then only
     waits for the gap, dm-thin's own commits under host writes included. It
     also removes the converge's self-block above.
   - Cost: the primary must report its connect to the new side. Today a
     primary's leg row carries only the wrapper and the prober's result; the
     per-path text is on a standby's row only. The worker must not parse a
     row's text either: details are for people, and the worker tells
     failures apart by a reason field (`cnagent.md`, CN29). So this needs a
     new field in the agent's reply message and a worker gate. Meanwhile the
     new side's rows read error (here the worker set and cleared side 363's
     health epoch, reason "error_row", from about 01:41:42 to 01:41:46.5),
     and they must read pending instead. Leg repair already skips a leg
     with two sides (`dnv-worker.md`, AR8), so the longer wait starts no
     repair. If the primary changes during the wait, the gate must wait for
     the new primary's report. Every migration starts one round trip later.
   - Risks: a migration whose primary never reports a live controller to
     the new side waits at the gate with no limit, for example when the path
     from the primary to the new side's disk node is broken. The old side
     keeps serving meanwhile, so no data is at risk, but the rows must show
     that wait, so that an operator sees it and can cancel the migration.
   - Not checked: whether the connect step's wait for the multipath head
     ends at once, since the head already exists through the old side's
     path.
   - Docs to change: `architecture.md`, Migration (Phase 0, where the new
     side zeroes first, and the paragraph on what the next syncups carry)
     and Known limits; `dnv-worker.md`, RW15 (the old side's request copies
     the new side's flag), RW18 (the provisioned flip) and a new rule for the
     gate; `cnagent.md`, CN28 (the rows); `dnagent.md`, DN13 (the new side's
     rows while it waits).
2. *No commit on the agent's pool status reads* (cn agent).
   - What changes: `probePool` and `ensureSlice` (`agent/cnagent/pool.go`)
     read the pool with `dmsetup status --noflush`. dmsetup(8) documents the
     flag: "do not commit thin-pool metadata when obtaining thin-pool
     status". dm-clone status reads keep their commit.
   - Effect: it removes the agent's own status commits in check rounds, info
     reads and converges. Both lab sightings were a check round's status
     read with no host IO in the gap, so this alone would very likely have
     avoided both. That is an inference.
   - What it leaves: thin messages (create_thin, create_snap, delete); the
     metadata snapshot reserve and release of bitmap reads, the two thin-id
     sweeps and clone crash recovery; a pool suspend or reload; dm-clone
     commits; dm-thin's own commits. A noflush status still takes the pool's
     metadata lock for reading. So with host writes in the gap, the agent's
     read waits behind dm-thin's own blocked commit, and the block can still
     form.
   - Cost: two call sites (`probePool` and `ensureSlice`) and their tests,
     three docs and one code comment (Docs to change, below).
   - Risk, the numbers: with noflush the status line includes the open
     transaction. Data block counts are current. Metadata blocks freed in the
     open transaction may still count as used until the next commit.
     Auto-grow reads these numbers (`dnv-worker.md`, AR6, which grows a slice
     whose thin pool fills up). Not tested in the lab.
   - A side gain: the slice-32 change kept one status read per pool only
     because each read commits (`cnagent.md`, CN28). With noflush, one status
     listing of the whole node could serve every pool.
   - Docs to change: `dnagent.md`, SH17 (dnv reads status without the flag
     that skips the commit); `cnagent.md`, CN23 (an info read commits each
     pool whose status it reads) and CN28 (status reads stay outside the
     reading); `architecture.md`, Known limits; the header comment of
     `agent/cnagent/probe.go`.
   - Not checked: that the lab guests' dmsetup takes `status --noflush` as
     the local man page says.
3. *Close the gap* (worker, both agents, and their messages).
   - What changes: the old side keeps serving until the primary has a usable
     path to the new side. The old side builds its migration source export
     before it cuts the primary's path (its steps 1 and 2). The new side
     connects, builds the dm-clone with hydration off and keeps its per-CN
     namespaces inaccessible. The primary connects. Then the old side cuts
     the primary's path, and the new side turns optimized and enables
     hydration.
   - Effect: the gap shrinks to the time between two ANA changes on two disk
     nodes. It also makes the pause of the leg's IO, which every migration
     has today, almost nothing.
   - Risks: a write that reaches the old side after the dm-clone copied its
     region is lost on the new side. That is why the old side fails, and
     never replays, IO that reaches it at the cutover (`architecture.md`,
     [D12]). So the new path must carry no IO before the old side has
     stopped taking IO, which its steps 1 and 2a do. A non-optimized new
     path is not safe for that: a host falls back to a non-optimized path
     whenever no optimized one is usable, for example while the old
     controller reconnects.
   - Cost: a new report and gate, and changes in the worker, both agents and
     their messages. Docs: `architecture.md`, Migration, [D12] (the bounded
     cutover suspension) and Known limits; `dnagent.md`, DN12 and DN13 (the
     old and the new side's migration steps); `dnv-worker.md`; `cnagent.md`,
     CN28 (the rows).
4. *Connect without the cntlr lock* (cn agent).
   - What changes: the connects a converge needs go to a registry that runs
     `nvme connect` with no lock held. The sweep's disconnects already work
     this way: the disconnect registry runs `nvme disconnect` off the locks,
     so a kernel wait cannot hold the cntlr lock (`cnagent.md`, CN10). The
     syncup handler and the retry start these connects before they take the
     cntlr lock. No message changes.
   - Effect: it ends the block. Once the primary has connected, the new
     side's step 5 releases the queued flush. The gap stays.
   - Cost: a connect registry beside the disconnect registry, its order
     against the disconnects, the demotion and the teardown, and tests for
     each order. No message changes.
   - Risks: it changes the lock rule (`cnagent.md`, CN1). It must be ordered
     against the sweep's disconnects, which work by NQN; today a converge
     refuses to connect while a disconnect of the same subsystem is in flight
     (`cnagent.md`, CN10). It must also be ordered against demotion and
     teardown. Inferred: rounds can still block for the length of the gap.
     A syncup abandons such a round with no health verdict (`dnv-worker.md`,
     RW6). But when no syncup comes, two round timeouts at the default 4 s
     primary threshold could fail the primary over (`dnv-worker.md`, AR5 and
     AR10, which need the threshold and two unhealthy rounds in a row).
   - Docs to change: `cnagent.md`, CN1 (the lock rule) and CN10 (the connect
     step and the disconnect registry); `architecture.md`, Known limits.
5. *A background pool prober* (cn agent).
   - What changes: like the leg probers, a goroutine reads each pool's
     status outside every lock, and rounds and info reads report its last
     result.
   - Effect: rounds stop blocking in the gap.
   - What it leaves: converges and bitmap reads still commit under the lock,
     so they can still cause the block. It fits best together with *No
     commit on the agent's pool status reads*.
   - Cost: one prober per pool with its start and stop, as the leg probers
     have, and rows that report its last result.
   - Risks: a prober blocked in `dmsetup status` holds one of the agent's 32
     command slots (`DefaultOsClientLimit`) until the command ends, which
     the leg probers avoid by design: they run their IO as direct syscalls
     outside that limit (`cnagent.md`, CN11). And without *No commit on the
     agent's pool status reads*, the prober itself still commits the pool in
     the gap, so the pool's IO and its other callers wait behind it, as
     today.
   - Docs to change: `cnagent.md`, CN1 (the lock rule), CN24 (the check
     round's loop), CN28 (what a round reads) and a new rule like CN11, the
     rule of the leg probers; `architecture.md`, Known limits.
6. *Skip committing calls on a pathless leg* (cn agent).
   - What changes: the agent skips the calls that commit a pool when its
     last sysfs walk shows a leg of that pool with a live controller but no
     usable path.
   - Effect: it narrows the race but does not close it: the ANA change can
     land between the walk and the call. It does not stop dm-thin's own
     commits. Rows and bitmap reads would need a pending answer.
   - Cost: a check before every call that commits a pool, in converges,
     check rounds, info reads and bitmap reads, each with its own pending
     answer.
   - Risk: the block can still form (Effect), and every skipped call leaves
     its work undone until the leg has a usable path again.
   - Docs to change: `cnagent.md`, CN28 (what a round reads and reports);
     `architecture.md`, Known limits.
7. *Diagnostics that keep the kernel log* (suite only).
   - What changes: the cn helper is the script that the suite writes out of
     `integtest/e2e_test.sh` and ships to each controller-node guest as
     `/var/tmp/dnv-e2e-helper.sh`. Its `diag` function runs `dmsetup status`
     over every device, without `--noflush`, and only then prints the kernel
     log. It would print the kernel log first, and read the status of thin
     pools last, with `--noflush`. It would run that read in the background
     with its output to a file on the guest, wait for it a bounded time, and
     then print the file and go on.
   - Why the background read: each dump runs as `timeout 60 bash
     /var/tmp/dnv-e2e-helper.sh diag` over ssh, and here that bound failed.
     The cn0 dump started at about 01:52:02 (it printed the agent's run time
     as 14:00, and the agent's log starts at 01:38:02). It ended at about
     02:12:57, cut off inside its `dmsetup status` listing. Why the bound
     failed is not verified. A likely cause: the `dmsetup` in D state
     outlived the signal of `timeout` and kept the ssh session's output
     open, so ssh waited for it. A read whose output goes to a file holds no
     part of the session.
   - Effect: it fixes nothing in dnv, but it keeps the evidence of the next
     sighting, and the dump stays within its bound. `--noflush` stops the
     diagnostics from committing a pool themselves. It does not stop the
     read from blocking here: a noflush status still takes the pool's
     metadata lock for reading (`dm_pool_get_metadata_transaction_id`), and
     the agent's blocked commit holds it for writing. The background read
     keeps that wait out of the dump.
   - Cost: the suite and one doc.
   - Risk: a status read left in the background on cn0 can still be blocked
     when the run's cleanup starts, as the agent's own read is today.
   - Docs to change: `e2e_integtest.md`, Diagnostics on failure, which says
     that every dump is tolerant and bounded. This run broke that bound.

### Suggestion

Take *No commit on the agent's pool status reads* and *Diagnostics that
keep the kernel log* now. Both are small and local. The first removes the
trigger of both sightings, a check round's own status read. The second
keeps cn0's kernel log the next time, and keeps the dump within its bound.
Then make one plan for the real fix.
*Connect first* lets every commit in the gap finish by itself once the new
side serves, dm-thin's own commits under host writes included, and it needs
no lock on the controller node. *Connect without the cntlr lock* reaches the
same end inside the cn agent alone, but it changes the lock rule. *Close the
gap* removes the gap, but it touches the cutover's data-safety rule.

### Questions for the owner

1. *The fix.* Which fix for the plan: *Connect first* (a new message field
   and a worker gate), *Connect without the cntlr lock* (cn agent only, a
   change to the lock rule) or *Close the gap* (the largest change, close to
   the cutover's data-safety rule)?
2. *A first step.* Is *No commit on the agent's pool status reads*
   acceptable as a first step, given that the status numbers then include
   the open transaction, and that auto-grow reads them?
3. *The scope.* Should the plan cover only the agent's status reads, or also
   the other commits in the gap: thin device create and delete, bitmap
   reads, and dm-thin's own commits under host writes?
4. *Head-of-line blocking.* Should the node lock's head-of-line blocking be
   part of this plan, or a plan of its own? This run did not show it: no
   other call reached cn0 while its node syncup waited (Wider effects seen).
   With more pools on one controller node, a node syncup that waits behind
   a blocked cntlr would hold up the calls of every other cntlr on that
   node. That is inferred from the lock rule, not seen.
5. *The diagnostics.* Should *Diagnostics that keep the kernel log* go in
   now, apart from the plan?
6. *Host IO in the copy case.* Should the copy case also write from a host
   across the gap, so that a fix is tested against dm-thin's own commits?
   Today it issues no host IO there on purpose.
7. *Which plan first.* At the default thresholds the block would most
   likely turn a migration into an unplanned failover and then a cntlr
   replacement (At the default thresholds; inferred). Does that make this
   plan more urgent than the early ANA move?

## The lab and the evidence

**The guests.** The lab runs Ubuntu with kernel 7.0.0-38. The logs show
that version only as the controller nodes' nvmet firmware string, and the
hung-task report of 2026-10-12 names it too. The vCPU counts below are the
lab's setup and are not recorded in the logs.

- Control plane: 192.168.122.78. It runs etcd, the gateway, the worker, the
  cdc and dnvctl.
- Controller nodes, 8 vCPUs each: 192.168.122.125 (cn0), 192.168.122.229
  (cn1), 192.168.122.77 (cn2).
- Disk nodes, 2 vCPUs each: 192.168.122.48 (dn0), 192.168.122.70 (dn1),
  192.168.122.49 (dn2), 192.168.122.122 (dn3). Each runs 45 dn agents, each
  on its own loop device and its own nvmet port, so the cluster has 180 disk
  nodes.
- Hosts: 192.168.122.193 (host0, which runs nvme-stas in the cutoff case)
  and 192.168.122.197 (host1).

**The kernel source.** The mechanisms above were read in the Ubuntu kernel
source Ubuntu-7.0.0-34.34 (Linux 7.0.14). The lab runs 7.0.0-38. That the
nvme multipath, nvmet and dm-thin code is the same in both is assumed, not
verified. The hung-task report of 2026-10-12, taken on 7.0.0-38, shows the
same dm-thin call chain. host0's own kernel version is not in the evidence.

**The runs.** The runner first ran the whole of `integtest/runner.conf`: the
dn, cn, worker, gateway, dnvctl and cdc suites, then e2e at 1, 4 and 32
slices. A second runner pass ran the worker suite again, then e2e at 32
slices with only react, then with only cutoff. The 2026-10-12 sighting comes
from an earlier pass of the whole of `integtest/runner.conf` on commit
`0ed4ae7`, with its guest logs and a hung-task report taken from its
primary's kernel log.

**The evidence.**

- Each run's suite log: the suite's own output and, after a failure, its
  diagnostics. These hold the failure context, each guest's kernel, device,
  nvmet and log state, and for the cutoff case host0's write-loop records
  and kernel log.
- The guest logs the runner copied after each failure: the `*.log` files
  under each guest's work directory, which are the JSON logs of the agents,
  the worker, the gateway and the cdc.
- All times in this file come from these logs, except the times marked as
  the runner's clock, which come from the runner's start and end lines in
  its output, in UTC. Guest clocks agreed within a few ms. host0's kernel
  log times were turned into UTC with one audit record (`[515212.233625]` =
  `1792035630.093`), so they are good to a few ms.

**What the evidence lacks.**

- cn0's kernel log for the migration-gap wedge. The suite's diagnostics
  block before they print it, and the runner copies only `*.log` files.
- The middle of host0's write-loop record for the cutoff run. The suite log
  holds only the first five failures and the last 20 lines.
- Guest logs of the passing 1- and 4-slice runs.
