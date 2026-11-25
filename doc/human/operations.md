# Operations

This document holds the procedures an operator follows by hand. The words
are those of the [core glossary](core_glossary.md). [The scope
page](scope.md) says what dnv promises and which limits each procedure
works around. The first procedure is how to read what is wrong across a
cluster, the place to start; then comes the forced-start guide: how to
bring back a group whose legs all failed.

A procedure has these parts, each where it applies: the symptom, or when to
use the procedure; how to confirm it; what dnv does alone and how long it
waits; the safe steps; the steps that can lose data; and when to stop and
ask for help. Each one says which test checked it, or that no test did.

The commands are `dnvctl` commands. Each one needs `--gateway-address`,
`--cluster` and `--sp`, which are left out below. A command that changes
a pool can also take `--rev`, a **revision token**: the pool's revision
as you last read it, which `dnvctl sp get` prints as `sp_rev.revision`.
`dnvctl dn delete` and `dnvctl dn set-disabled` take the disk node's
revision instead, `dn_rev.revision` of `dnvctl dn get --addr <addr>`, and
`dnvctl cn delete` and `dnvctl cn set-disabled` the controller node's,
`cn_rev.revision` of `dnvctl cn get --addr <addr>`. With `--rev`, the
gateway refuses the change with the error code ABORTED and the message
`stale revision` when the revision moved since you read it, and it
changes nothing: read the state again, check that the change is still
right, and send it again with the new revision. Without `--rev`, the
gateway checks nothing, so your change can undo a change that another
operator, or the worker, made after your read. That can also lose data: a
delete by name can delete a volume that another operator deleted and made
again under that name after your read, with its blocks, and `dnvctl ns
set-dev` can give the hosts a volume they never wrote. Most writes move
the revision, the worker's own steps among them, so read it again before
each write.
`dnvctl` refuses `--rev` on a command that takes none, such as a read.

Type an id as `dnvctl` prints it, in decimal. An id copied from a device
name or an NQN is in hex: type `0x` in front of it. `dnvctl` refuses a
number with zeros in front and no `0x`, such as `017` or
`0000000000000011`, and an id typed with an empty value. A command that
ends with exit status 3 was done by the gateway, and any change it asked
for was made, but its output was lost. Read the state again before you run
the command again.

`dnvctl cluster get-status` lists what is wrong across the cluster
([Read what is wrong](#read-what-is-wrong)). `dnvctl sp get` also lists
the pool's cntlrs under `cntlr_list`, and the one whose `primary` is true
is the pool's primary. That list shows no ids: its entries come in the
order of the ids in `sp_conf.cntlr_id_list`, so a cntlr's id stands at the
same place in that list. `dnvctl cntlr inspect --id <cntlr id>` shows a
cntlr's rows, and `<primary cntlr id>` below is the primary's id.

The sections, by subject:

- Where to start:
  - [Read what is wrong](#read-what-is-wrong)
- The forced-start guide, which brings back a group whose legs all failed:
  - [The forced-start guide](#the-forced-start-guide)
    - [Words of this guide](#words-of-this-guide)
    - [Reading a group](#reading-a-group)
    - [Before every forced start](#before-every-forced-start)
    - [Which leg to choose](#which-leg-to-choose)
    - [A group whose in-sync leg died during a
      copy](#a-group-whose-in-sync-leg-died-during-a-copy)
    - [Starting from a retired leg](#starting-from-a-retired-leg)
    - [A broken array that comes back by
      itself](#a-broken-array-that-comes-back-by-itself)
    - [A forced start that waits](#a-forced-start-that-waits)
    - [A forced start whose stop failed](#a-forced-start-whose-stop-failed)
    - [A lost clone meta group](#a-lost-clone-meta-group)
    - [A leg of another identity that the primary does not
      add](#a-leg-of-another-identity-that-the-primary-does-not-add)
    - [A group whose every leg was
      wiped](#a-group-whose-every-leg-was-wiped)
    - [An array create cut short](#an-array-create-cut-short)
    - [Keeping the old copies](#keeping-the-old-copies)
    - [Taking the group's repairs over](#taking-the-groups-repairs-over)
- Nodes:
  - [Prepare a node](#prepare-a-node)
  - [Install the udev rules on a node](#install-the-udev-rules-on-a-node)
  - [Check a node before it joins](#check-a-node-before-it-joins)
  - [Choose a node's location](#choose-a-nodes-location)
  - [The cluster's capacity](#the-clusters-capacity)
  - [Read a node's state](#read-a-nodes-state)
  - [A planned restart of an agent](#a-planned-restart-of-an-agent)
  - [Disable a node](#disable-a-node)
  - [Leftovers on a node](#leftovers-on-a-node)
  - [Remove a node](#remove-a-node)
  - [Reuse a disk from an old node](#reuse-a-disk-from-an-old-node)
  - [A disk leaves dnv](#a-disk-leaves-dnv)
  - [Replace a dead disk node](#replace-a-dead-disk-node)
  - [Replace a dead controller node](#replace-a-dead-controller-node)
  - [Gather a node's state for a bug
    report](#gather-a-nodes-state-for-a-bug-report)
- The networks:
  - [The networks](#the-networks)
- Pools:
  - [A pool's space](#a-pools-space)
  - [Change a pool's level](#change-a-pools-level)
  - [Repair a thin pool's metadata](#repair-a-thin-pools-metadata)
  - [A slow leg](#a-slow-leg)
  - [Move a leg to another disk node](#move-a-leg-to-another-disk-node)
    - [Ending a migration with force](#ending-a-migration-with-force)
  - [Repair a leg by hand](#repair-a-leg-by-hand)
  - [Retired legs](#retired-legs)
  - [Replace a cntlr by hand](#replace-a-cntlr-by-hand)
  - [Change a pool's cntlid slots](#change-a-pools-cntlid-slots)
  - [Stripes and discards](#stripes-and-discards)
  - [md's copy speed](#mds-copy-speed)
  - [The md check's mismatch count](#the-md-checks-mismatch-count)
  - [One pool for each volume](#one-pool-for-each-volume)
  - [A pool made before the stripe
    bound](#a-pool-made-before-the-stripe-bound)
  - [A pool made before the block
    bound](#a-pool-made-before-the-block-bound)
- Volumes:
  - [Change a volume's hosts](#change-a-volumes-hosts)
  - [Snapshots](#snapshots)
  - [Park a namespace](#park-a-namespace)
  - [Move a volume to another pool](#move-a-volume-to-another-pool)
    - [Read a volume's written blocks](#read-a-volumes-written-blocks)
  - [Clone from another cluster](#clone-from-another-cluster)
  - [A new volume refused for a thin id that holds
    data](#a-new-volume-refused-for-a-thin-id-that-holds-data)
- The control plane:
  - [Set up the control plane](#set-up-the-control-plane)
  - [Back up etcd](#back-up-etcd)
  - [Restore etcd from a backup](#restore-etcd-from-a-backup)
    - [Restore one record from a backup](#restore-one-record-from-a-backup)
  - [etcd refuses writes: its store is
    full](#etcd-refuses-writes-its-store-is-full)
  - [Upgrade dnv](#upgrade-dnv)
  - [While the control plane is down](#while-the-control-plane-is-down)
  - [Give hosts the cdc addresses](#give-hosts-the-cdc-addresses)
  - [The logs](#the-logs)
    - [Read the logs](#read-the-logs)
- Kubernetes:
  - [A Kubernetes volume the node plugin will not
    format](#a-kubernetes-volume-the-node-plugin-will-not-format)
    - [The read failed](#the-read-failed)
    - [Data at the start](#data-at-the-start)
  - [A Kubernetes node plugin that does not
    start](#a-kubernetes-node-plugin-that-does-not-start)
  - [Change the cdc addresses of the Kubernetes
    driver](#change-the-cdc-addresses-of-the-kubernetes-driver)
  - [A Kubernetes machine is lost](#a-kubernetes-machine-is-lost)
  - [A one-cntlr Kubernetes volume after a cntlr
    replacement](#a-one-cntlr-kubernetes-volume-after-a-cntlr-replacement)
  - [A Kubernetes mount that lost its last
    path](#a-kubernetes-mount-that-lost-its-last-path)
  - [A pool left by a Kubernetes volume whose name held
    ".."](#a-pool-left-by-a-kubernetes-volume-whose-name-held-)
- Hosts:
  - [Restart nvme-stas](#restart-nvme-stas)
  - [A host that keeps a withdrawn
    path](#a-host-that-keeps-a-withdrawn-path)
  - [A fault only a host sees](#a-fault-only-a-host-sees)

## Read what is wrong

Words: the **status read** is `dnvctl cluster get-status`, one read of the
gateway that lists what is wrong in a cluster, a page of storage pools at
a time; it reads etcd and the latest report of each pool's primary, and it
changes nothing. A **page token** is the text the read prints after
`--page-token` when pools are left to read; the next run with it reads the
next page. A **repair** is the record of one replacement of a bad leg of a
group with a new leg: the worker makes one for each leg repair, and an
operator can make one with `dnvctl repair create`. A **forced-start mark**
is the operator's mark on a group that tells the primary to start the
group's array from one leg ([the forced-start
guide](#the-forced-start-guide)).

When to use: first, whenever something seems wrong — a volume is slow or
stops, a host loses a path, or a command is refused for a reason you do
not see. Then follow the line that names the problem to the guide for it.

How to read it:

1. Run `dnvctl cluster get-status`. Each line is one thing that is wrong:
   the pool ("-" for a line about the workers or the nodes, which only
   the first page has), the object, named with its id
   (`cntlr 3 (primary, cn ...)`,
   `leg 17 (group 5, slice 2; side 18 on dn ...)`,
   `repair r1 (group 5, slice 2)`), the STATUS column, the age — how long
   it has been so, "-" where nothing records the time — and a text.
2. When the last line is `next page: --page-token <token>`, run the read
   again with `--page-token <token>`, until that line does not come. A
   page that meets a node that does not answer can end early; its token
   still leads on.
3. `nothing is wrong in the N pools read` on every page means the read
   found nothing wrong. A healthy cluster at rest lists nothing; a change
   in flight lists itself for a few seconds while it runs: a pool just
   made, or a cntlr made as its pool's primary, as when the worker
   replaces the cntlr of a pool with one cntlr, until its agent reports on
   it (`report/unknown`), a primary just disabled until its failover, a
   migration whose cutover is pending, and a pool or a clone being
   deleted until its drain ends. A new standby lists nothing, since the
   read asks only the primaries. A repair lists itself until it ends.
4. `--json` prints the same page as one JSON document, with each object's
   ids as fields, for a script.

The first word of the STATUS column says what kind of line it is; the
status and the reason of the row it shows follow where it has them:

- `lost-record`: a record the pool or a node needs is gone from etcd, the
  record that maps the pool's id to its name names another pool, or a
  node's record does not read. On a pool's line the object is the record's
  key. On a line about a node's own records, whose pool is "-", the object
  is `dn <address>` or `cn <address>`, and the text names the node's
  revision key when that key is gone.
- `report/unknown`: the read could not get the pool's state: its records
  do not read, it has no primary, or the primary's agent does not answer,
  holds nothing or is starting; or the primary's report is too old, which
  says either that no worker asks it for a read or that the agent's read
  does not end, and then a `health` line of the cntlr shows too; or the
  primary's report is behind the pool's latest change while its health
  epoch is set: either the change is still on its way to the primary, or
  its node refuses it, as when it cannot save what the worker sends; the
  read cannot tell which, and the worker's log names it.
- `pool`: the pool as a whole: being deleted, its level below read-write,
  its primary disabled, or a stored setting the worker refuses, such as a
  group whose sizes give its sides nothing to zero.
- `space`: a slice's thin pool is out of data space, so a write that needs
  a new block waits and then fails, or out of metadata space, so the pool
  is read-only.
- `grow`: auto-grow cannot add a group to a slice that is filling up; the
  text says why, for example that no disk node has room or that a
  controller node's budget, the extents it may still give the pools it
  serves, is short. A grow that waits is this kind too, whatever the
  slice's use: its new group has waited longer than the pool's side
  threshold for its disk node to prepare it ([A pool's
  space](#a-pools-space), step 3).
- `row`, as in `row/error`: a row of the primary's report reads
  `RES_STATUS_ERROR`, or a leg is kept out of its array; the text is the
  row's text. A subsystem's row, or a transfer's subsystem row, that reads
  `RES_STATUS_PENDING` shows as `row/pending`: a host taken off the list
  keeps its connection, because the controller node has not ended it yet
  or cannot. Its text says that while it does, the cntlr's converges move
  the subsystem's namespaces to ANA inaccessible, away from every host,
  and then gives the row's own text ([Change a volume's
  hosts](#change-a-volumes-hosts)).
- `health`: the worker has set a health epoch; the age is how long ago.
- `group`: a group's array is not whole, the primary refuses to add a leg,
  the operator is in charge of the group's repair, a forced-start mark
  stands, or md records bad blocks on a leg.
- `repair`, `migration`, `clone`: one that waits; the text says for what,
  and for a repair who acts next: the worker at its next pass, or the
  operator with `dnvctl repair cancel` or `dnvctl repair finish` — for the
  operator's own repairs, and for the worker's in a pool at a level that
  stops the worker's reactions, with a setting the worker refuses, or with
  a `lost-record` line for one of the pool's own records (not a node's
  record, nor the one that maps the pool's id to its name), since the
  worker then finishes and cancels none of its repairs there. With a
  `lost-record` line, ask for help before you run them, as When to stop
  below says: the gateway can refuse them too, for example when the lost
  record is the slice that holds the repaired group. A `lost-record` line
  for the pool's revision key, the etcd record that holds the pool's
  revision, is the case where no one can act: the worker and the gateway
  both read that record before they act on the pool, so no step another
  line of the pool names can run until the record is back, neither the
  worker's nor yours, such as `dnvctl repair cancel`, `dnvctl repair
  finish` or `dnvctl migr finish`. The record's own line says so, and the
  repair lines and the line of a migration whose copy is whole say they
  wait for it.
- `torn`: a thin device whose slices hold its data from different points
  in time.
- `worker`: no worker of a role (dn, cn or sp) has its record in etcd,
  the record a running worker writes again every few seconds; or one
  worker has not written its record for twice the default time between
  two writes, by the gateway's clock. This is only a hint: the gateway
  does not know a worker's own time between writes.
- `more`: the pool has more lines than the read shows for one pool; read
  them with `dnvctl sp get`, `dnvctl cntlr inspect` and
  `dnvctl group inspect`.

What dnv does alone: the read changes nothing, and dnv does not act on
it. The worker acts on the same facts by itself, each in its own time
([the scope page](scope.md), section 6), save the cases the end of this
section names: a health epoch on the primary
leads to a failover once the primary's event threshold passes; on a
standby, to a cntlr replacement once the cntlr's threshold passes; on a
leg of a RAID1 group, to leg repair once the leg's threshold passes, or
once its side has also been unhealthy for the side's threshold. A grow
refused for room waits until a disk node or a controller node has room;
one refused at a ceiling, a full list of groups or the metadata's
largest size, stays refused. A lost record, a `worker` line and a
`report/unknown` line get no reaction.

Safe steps: the read itself, any number of times. For a node named in
`report/unknown` or `health` lines, run `dnvctl cn inspect` or
`dnvctl dn inspect` on it, and look at the node and its agent. For a
`row` line, `dnvctl cntlr inspect --id <primary cntlr id>` shows the
whole report. For a `group` or a `repair` line, the forced-start guide
starts at [Reading a group](#reading-a-group).

Steps that can lose data: none in the read. Never remove a name from a
pool's lists to clear a `lost-record` line: the primary then deletes that
volume's data for good ([the scope page](scope.md), section 6.1, a pool
with a lost record).

When to stop: a `lost-record` line, or a `report/unknown` line that does
not go away while the node answers its inspect: ask for help before any
write to that pool.

What the read does not show: a standby's or a side's own trouble before the
worker sets its health epoch, since the read asks only the primaries and a
migration's new side; why the worker does not react: a failover of an
unhealthy primary with no enabled standby to take the role, a leg repair the
worker cannot create, for want of a disk node with room or of a healthy leg,
and a cntlr replacement that finds no controller node show only as a
`health` line whose age grows, and the worker's log names the reason, while
a disabled primary shows as a `pool` line, which says when no cntlr of the
pool is enabled or the pool's level runs no reaction; a subsystem no cdc
serves; and a worker that runs but whose watch has stopped. The scope
page, section 9, lists these.

Tested: the gateway's unit tests plant each kind of line and check it,
the order of a pool's lines and their cap, a node that hangs, the pages
and a page cut short by a client's deadline. The gateway suite's faults
case plants a leg fault, a thin pool that needs a check, a full thin pool,
a hung primary and a lost revision record, and reads the pages; the dnvctl
suite checks the table's shape; the e2e suite prints the table when a case
fails. The gateway and dnvctl suites passed in the lab with these stages
on 2026-11-19; the e2e suite has not yet run in the lab with its print.

## The forced-start guide

A group of a RAID1 pool keeps the same data on two legs. md runs the
group's array on the group's primary. When every leg of the group fails,
md marks the array broken, or the group has no array at all. The pool's
volumes then stop.

A forced start is the operator's way back. The operator writes a mark on
the group, the forced-start mark, that names one leg. The primary then
starts the array from that leg alone. It wipes the md superblock of every
other leg the group keeps: the legs of the group's list, and the new legs
of the group's open repair (see the words below), a retired leg the
repair took back among them. It does not wipe the open repair's bad leg,
nor the new leg of a repair created with its abort flag set. md then
copies the named leg onto each wiped leg. A forced start can lose data:
everything the wiped legs hold that the named leg does not hold.

### Words of this guide

- A group's **list** names its legs.
- A **repair** replaces a bad leg of a group with a new leg. A repair is
  **open** until it finishes or is cancelled. A group has one repair at a
  time.
- A **retired leg** is a leg that left its group at the end of a repair. It
  keeps its data until it is deleted. A repair can **take back** a retired
  leg: the retired leg becomes the repair's new leg.
- A **member** is a leg that md counts in the group's array.
- A member's **superblock** is md's record at the start of the leg. It
  holds the array's identity (a number md gives each array), md's change count (md calls it the event
  count), the member's place in the array, and its copy position when md
  was copying onto it.
- A **full member** has a place in the array and no copy position. A
  member with a copy position is **half-copied**: md copied only part of
  the data onto it.
- md keeps a **bad-block list** for each member: the sectors of the member
  that md could not read or write.
- A forced start gives the array a new **identity**. The legs it wipes
  join under the new identity once md copies onto them, and a leg it does
  not wipe, such as the open repair's bad leg or a retired leg, keeps the
  old identity. Change counts can be compared only between legs of the
  same identity.
- An **override** of a mark lets the start go on past one kind of
  refusal. There are three kinds, each named by a word: `older-leg`, a
  full member of the named leg's identity that the start wipes has a
  higher change count than the named leg (a leg of another identity, a
  half-copied leg or a leg with no superblock is never compared);
  `bad-blocks`, the named leg carries a bad-block list, which the start
  drops; and `partial`, a repair took the named leg back after md wrote
  onto it. A start passes a refusal only when the mark names its word, so
  an override given for one kind passes no other.
- The **keep flag** of a retired leg keeps the worker from deleting it.
- The **operator-in-charge flag** of a group is the operator's mark that
  keeps the worker's repairs off the group.

### Reading a group

`dnvctl group inspect --grp <group id>` asks the group's primary to read
every leg of the group at once. It changes nothing. For each leg it shows:

- whether the leg serves;
- its superblock: the identity, the change count, the place, the copy
  position and the length of the bad-block list;
- md's own state and place for it;
- its disk node, its health epochs and the primary's row for it.

It also shows the state of the array and the group's retired legs.

The dry run is a forced start that only says what it would do:
`dnvctl group inspect --grp <group id> --leg <leg id>`. It says which legs
the start would wipe, whether it would stop the array first, and its update:
`uuid`, or `uuid,force-no-bbl` when the start drops the named leg's
bad-block list. When a device holds the array open, as the pool's thin
pool does at a level that builds thin pools, it says that the start's stop
would be refused and the start would wait, and that a step of the pool's
level down to `NO_THINPOOL` comes first, or, at that level, that the
device that holds it must go first; its list of refusals is then empty,
since the start would stop before its checks. Or it says why the start
would refuse, in the same words as
the real start. Beside that, its `refusal_list` lists each refusal an
override passes that it meets, in this order: a leg a repair took back
after md wrote onto it (`FORCED_START_OVERRIDE_PARTIAL`, the word
`partial`), a bad-block list on the named leg
(`FORCED_START_OVERRIDE_BAD_BLOCKS`, `bad-blocks`), and each full member
of the named leg's identity that it wipes whose change count is above the
named leg's (`FORCED_START_OVERRIDE_OLDER_LEG`, `older-leg`). It compares
no other leg: a leg of another identity (rule 5), a half-copied leg and a
leg with no superblock are never listed, whatever they hold. Each entry
says whether the request passes it (`overridden`). Add `--override` with a
comma list of words, such as `--override bad-blocks`, to see what a start
that passes those refusals would do. A refusal of the primary's checks
that no override passes, such as a leg that does not serve, ends those
checks, and the list then holds only the refusals met before it. A refusal
the gateway makes on its own, such as a pool level at which no group array
is built, or a report of the primary with no row for the group, ends
nothing: the list still holds every refusal the primary's checks met. The
dry run runs nothing.

Tested: the cn agent suite, `integtest/cnagent_test.sh`, repair case,
stage `forced`. It runs the primary's read and dry run before a real
start, and checks that the real start does what the dry run said. The
dnvctl suite, `integtest/dnvctl_test.sh`, checks the command against a
stand-in gateway. The end-to-end suite's react case runs
`dnvctl group inspect` against a real gateway, its dry run only when the
named leg carries a bad-block list. The cn agent suite's stage `badstart` reads the list of
refusals, and so do its stage `forced` and the end-to-end suite's react
case when the named leg carries a bad-block list; a start that names one
override and is refused for another kind is tested only by unit tests:
those of the cn agent for `older-leg` and `bad-blocks`, and those of the
gateway for `partial`. The dry run over an array that a device holds open
is tested only by a unit test of the cn agent.

### Before every forced start

1. Step the pool's level to `NO_THINPOOL`:
   `dnvctl sp set-level --level NO_THINPOOL`. At this level the pool's
   thin pools and clones go away. Nothing then holds the group's array
   open, so the primary can stop it. No reaction runs, so no failover or
   repair moves the group while you work. Hosts get IO errors. Do not step
   further down: at `NO_REDUND`, `NO_MIGRATION`, `NO_SIDE` and `DISABLE` no
   group array is built, and the gateway refuses the mark.
2. Read the legs' rows and health epochs while the legs are still down.
   `dnvctl sp get` shows the health epochs on the group's legs. For a
   clone meta group, the group that holds a clone's copy records,
   `dnvctl clone get --name <clone>` shows them. The worker clears
   a leg's health epoch once the primary reads the leg well again, so you
   cannot read it later.
3. Read the group (above).

Tested: for a forced start, the suites step the level down before a leg
dies. Stepping it down after, as here, is tested only for "A broken array
that comes back by itself".

### Which leg to choose

Symptom: a group's array does not run, and the primary's row for the
group reads error.

Confirm: `dnvctl group inspect --grp <group id>`.

What dnv does alone: a broken array makes the primary unhealthy. Unless
the level stops reactions, the worker fails the primary over, about 6 to
10 seconds later at the default settings. The new primary assembles the
group once its legs are back, if md will start it. Without an operator,
md starts an array from one leg alone only when that leg's superblock
records the other legs as failed. dnv never chooses between legs that each
may hold the newest data.

Choose the leg with these rules:

1. Never a half-copied leg. The regions md never copied hold whatever that
   leg's disk held before, possibly another pool's bytes. The forced start
   refuses it.
2. Among full members of one identity, the leg with the highest change
   count holds the newest data.
3. Two full members of one identity can have the same change count. Then
   one of them may be a copy that md ended by marking it in sync when the
   leg it copied from died. That copy lacks the regions in its bad-block
   list. Name the leg md copied from, not the one it copied onto. The one
   it copied onto is the new leg of the group's latest repair; the legs'
   rows and health epochs, read while the legs were down, show which leg
   died while md copied. If you cannot tell, stop.
4. A named leg with a bad-block list is refused unless the mark passes
   `bad-blocks`. The start drops the list, and md then serves the listed
   regions from what the leg holds there. That is right for the leg md
   copied from, whose list the array's stop wrote, and wrong for the leg it
   copied onto. `dnvctl group inspect` shows each leg's list length.
5. A leg of another identity is from before an earlier forced start.
   Nothing compares it with the others: a mark naming it wipes the legs of
   the current identity, with no refusal and no warning in the dry run.
   Compare it by hand.
6. The named leg may be dead when you write the mark. The primary then
   waits for it and starts from it when it comes back, unless the start
   then meets a refusal the mark does not pass (step 4 of the safe steps).
   While the named leg does not serve, the primary checks nothing past
   it: the dry run's list holds no refusal but `partial`, which the
   gateway judges from the leg's record, and the dry run says that the
   start waits for the leg, or refuses it for `partial`. In the same way,
   while a leg the start would wipe does not serve (rule 7), the list can
   lack a refusal that the start meets once that leg serves.
7. The start waits for every other leg it would wipe. A leg that will
   never come back blocks it: see "A forced start that waits".

Safe steps:

1. Do the steps "Before every forced start".
2. Run the dry run from the leg you chose, with no `--override`, and read
   every refusal its list holds. Then compare by hand, in
   `dnvctl group inspect`, the named leg with every leg the start would
   wipe that is of another identity (rule 5): the list never holds such a
   leg. For each listed refusal you choose to go on past (rules 2 to 4),
   add its word to `--override`, run the dry run again with that list, and
   read what it would wipe and drop. Name no word whose refusal the list
   does not hold.
3. Write the mark:
   `dnvctl group force-start --grp <group id> --leg <leg id>`, with
   `--override` and the words of the last dry run of step 2, only when
   step 2 needed them.
   The gateway refuses it while the primary reads the group's array
   running. It also refuses it while the primary's report has no row for
   the group, or the primary does not answer. Then try again after a few
   seconds.
4. Wait until the group's row reads ok and the mark is gone. The worker
   clears the mark once the primary reports that it acted on it, and that
   the array runs with a member in sync on a leg whose row reads ok. If
   the mark stays while the named leg serves, run the dry run again, with
   the words the mark carries in `--override` (`dnvctl sp get` shows the
   mark's `override_list`, in the names the dry run's list uses, and
   `dnvctl clone get --name <clone>` for a clone's group). When its
   refusal starts `forced start refused`, the start meets a refusal the
   mark does not pass, as when the named leg came back with a bad-block
   list (rule 6), and it wipes nothing. Do not wait for the group's row
   to say so: it carries that text at most from a start the primary tried
   until its next check round, which finds no array of the group and
   reads the row missing. The primary's cn agent logs each such try as the
   record `forced start refused`. Follow step 2. To go on past what it
   lists, write the mark again with those words, as in step 3: a new mark
   replaces the one that stands.
5. Read the group again. md is copying onto the other legs. Wait until
   every leg is in sync. Check that no leg shows a bad-block list. If the
   named leg's list is still there after a start that passed `bad-blocks`,
   the start's second run, which drops it, did not happen, and hosts get IO
   errors over the list's regions: see "A forced start whose stop failed".
6. Step the level back:
   `dnvctl sp set-level --level READWRITE`, or the level the pool had.
7. Write to a volume from a host. If a thin pool comes back read-only, its
   row under `slice_id_to_dm_pool` in `dnvctl cntlr inspect` says why: for
   the reason `RES_REASON_POOL_NEEDS_CHECK`, follow
   [Repair a thin pool's metadata](#repair-a-thin-pools-metadata); for a
   row that reads ok while its details hold `ro` and no `needs_check`, the
   metadata is full: follow step 3 of [A pool's space](#a-pools-space); for
   any other, stop.

Steps that can lose data:

- Every forced start wipes the other legs the group keeps, the new legs
  of its open repair included. Whatever only they held is lost.
- A mark naming a leg of another identity wipes the legs of the current
  identity with no refusal: their newer data is lost.
- `--override older-leg` starts the array from a leg older than a leg it
  wipes. That leg serves and holds newer data, and you choose to give that
  data up. Use it only when the newer data is known to be bad.
- `--override bad-blocks` drops the named leg's bad-block list. If two legs
  have the same change count and you name the wrong one, hosts read the
  regions md never copied as whatever that leg's disk held, with no error.
- A start from an older copy restores this group alone. The pool's other
  groups keep their newer state, so the pool's volumes can mix old and new
  blocks. For a data group, a block first written since that copy can read
  another pool's bytes. A start of a slice's meta group from an older copy
  gives the slice's thin pool its records of that time, over data groups
  that kept their newer writes. A block a volume first wrote since that
  copy reads zeros. A block that a volume or its snapshot rewrote since,
  while the two shared it, reads its older bytes. A block a volume freed
  since, which the pool then gave out again, reads what the pool wrote
  there later, possibly another volume's bytes. None of these reads shows
  an error. A volume or a snapshot made since is lost: the primary reports
  it as an error with the reason `RES_REASON_THIN_ID_LOST`. Check the
  volumes from the hosts.

When to stop: no leg of the array's identity can come back; the dry run
refuses every leg; or two legs have the same change count and you cannot
tell which one md copied from.

Tested: the cn agent suite, repair case, stages `refuse` (a half-copied
leg refused, nothing wiped), `forced` (a start from the leg that kept the
data), `older` and `olderstart` (an older leg refused, then started
passing `older-leg`), and `badblocks` and `badstart` (the named leg's
bad-block list refused with no override, nothing wiped, the dry run
listing it alone, then dropped passing `bad-blocks`). The end-to-end
suite, `integtest/e2e_test.sh`, react case: its first forced start, whose
mark is written while the named leg is dead; once the leg is back, the
suite reads the leg's list length and, when the leg has a list, checks the
dry run's refusal and list, then writes the mark again passing the kinds
that list holds.

### A group whose in-sync leg died during a copy

Symptom: a repair added a new leg, md was copying onto it, and the leg
that kept the data died. The array is broken.

Confirm: `dnvctl group inspect --grp <group id>` shows the new leg
half-copied.

What dnv does alone: nothing that brings the group back. md does not start
from a half-copied leg, and neither does the primary. Unless the level
stops reactions, a failover moves the primary, about 6 to 10 seconds
later, and the new primary cannot start the array either.

Safe steps, in this order:

1. Step the level to `NO_THINPOOL` at once, before the dead leg comes
   back.
2. Write the mark naming the dead leg that kept the data, while it is
   still dead. The gateway refuses the mark while the primary's report
   reads the group's row ok, as it can for short moments while md retries
   its copy; then try again after a few seconds, while the leg is still
   dead, as in step 3 of "Which leg to choose". The primary stops the
   broken array and waits for that leg. The stop can take a few check
   rounds: md keeps retrying its copy, and the array reads as running for
   short moments while it does. Wait until
   `dnvctl group inspect --grp <group id>` shows no array state: the
   primary has stopped the array.
3. Bring the leg back. The primary starts the array from it and wipes the
   half-copied leg. md copies onto it again. If the leg comes back with a
   bad-block list, the start is refused instead and wipes nothing: see
   step 4 of "Which leg to choose".
4. Continue with steps 4 to 7 of "Which leg to choose".

Why this order: if the leg comes back before the primary stops the array,
md can end its copy by marking the half-copied leg in sync. md then writes
bad-block lists on both legs, and you can no longer tell the two legs
apart.

Steps that can lose data: if md has already marked the half-copied leg in
sync, a mark naming it is refused for its bad-block list only while it
does not pass `bad-blocks`. Passing it, hosts read the regions md never
copied as whatever that leg's disk held.

When to stop: the leg that kept the data will never come back. A retired
leg of the group may still hold an older copy: see "Starting from a
retired leg".

Tested: the cn agent suite, repair case, stages `copydeath`, `refuse` and
`forced`; the end-to-end suite, react case, its first forced start.

### Starting from a retired leg

Symptom: the leg that kept the group's data will never come back. The
other leg of the group's list still serves, but holds no whole copy, as a
half-copied leg does. A retired leg of the group holds an older copy.

Confirm: `dnvctl retired-leg list` shows the group's retired legs.

What dnv does alone: nothing. A mark cannot name a retired leg directly.

Safe steps:

1. Do the steps "Before every forced start".
2. If the group has an open repair, stop: the take-back needs a repair of
   its own.
3. Take the retired leg back with a repair whose bad leg is the dead leg:
   `dnvctl repair create --name <name> --grp <group id>
   --bad-legs <dead leg id> --retired-leg <retired leg id>`. The repair's
   new leg has the retired leg's id.
4. Read the group. If the taken-back leg's superblock is of another
   identity and you cannot tell how old it is, stop.
5. Run the dry run from that leg, then write the mark naming it:
   `dnvctl group force-start --grp <group id> --leg <retired leg id>`. If
   md once wrote onto that leg, as a repair that was cancelled can leave
   it, the gateway refuses the mark unless you add `--override partial`,
   and the dry run lists that refusal first. Such a leg is no longer the
   copy it was: stop instead. If the dry run lists a bad-block list on the
   leg, read rule 4 of "Which leg to choose". A retired leg can carry such
   a list from its time in the array, for example when md recorded bad
   blocks on it before a repair replaced it. `--override bad-blocks` drops
   the list, and md then serves those regions from what the leg holds
   there. Add `--override bad-blocks` to the dry run and to the mark only
   if you accept that; otherwise stop.
6. Once the array runs and `dnvctl repair get --name <name>` shows the
   repair started, finish it: `dnvctl repair finish --name <name>`. The
   new leg then takes the bad leg's place in the list.
7. Wait until every leg is in sync, then step the level back and write
   from a host, as in "Which leg to choose".

Steps that can lose data: this is a start from an older copy. Everything
written to the group since the leg retired is lost, while the pool's other
groups keep their newer state. Blocks first written since can read another
pool's bytes. For a slice's meta group, a start from an older copy costs
more: the steps that can lose data of "Which leg to choose" say what.

When to stop: as in steps 2, 4 and 5, the refusal for a bad-block list
included when you do not accept what `--override bad-blocks` does.

Tested: the cn agent suite, repair case, stage `restore`; the end-to-end
suite, react case, its second forced start.

### A broken array that comes back by itself

Symptom: every leg of a group failed under host IO at once, as on a switch
reload or a reboot of all its disk nodes, and the legs are back. The
group's row reads error with the state `broken`.

Confirm: `dnvctl group inspect --grp <group id>`.

What dnv does alone: the primary stops the broken array and assembles it
again, with no mark, when all of these hold:

- nothing holds the array open;
- every leg of the group serves, and its row does not read error;
- every leg's superblock reads as a full member of one identity, with no
  bad-block list, or the leg carries no superblock at all, and then md
  copies onto it afterwards;
- the group has no forced-start mark and no operator-in-charge flag.

md then starts the array from the newest legs. At the pool's normal levels
the thin pool holds every array, so the primary leaves the array as it is.
The failover or the cntlr replacement that the error brings then builds the
group on another cntlr: the failover about 6 to 10 seconds later at the
default settings. A pool with one cntlr waits for the cntlr replacement,
600 seconds at the default threshold.

Safe steps, when the group stays down:

1. Do not set the operator-in-charge flag: it stops this.
2. Step the level to `NO_THINPOOL`. That step takes the thin pool away. In
   the same step the primary assembles the array again, if the conditions
   above hold.
3. If the group's row still reads error, its text says why. Follow "Which
   leg to choose".
4. Wait until every leg is in sync, then step the level back and write
   from a host, as in "Which leg to choose".

Steps that can lose data: the assembly makes no choice of its own; mdadm
starts the array from the newest legs, and md copies the rest. When the
legs' paths died a few seconds apart rather than at once, md can have
completed a write on one leg alone with no record of that on the other
leg, so the legs look equal. md's copy after the assembly then copies the
leg in its first place over the other in the regions that were being
written, and such a write is lost when the leg that lacks it holds that
first place ([the scope page](scope.md#51-hosts-flushes-and-snapshots)).
The failure itself can lose data too: a data group that broke while the slice's metadata group took writes
brings the thin pool back read-only, and a metadata group that broke loses
the blocks mapped since its last commit (the scope page, section 4).

When to stop: the row names a half-copied leg, a bad-block list or two
identities. Follow "Which leg to choose".

Tested: the cn agent suite, repair case, stage `reassembly`: the array
breaks under the pool, the level step brings it back, and the host reads
its data unchanged (2026-11-16). md's stop and plain assembly were checked
on loop devices in the lab on 2026-11-15, kernel 7.0.0-42, mdadm 4.5.

### A forced start that waits

Symptom: a mark stands on the group, and nothing happens. `dnvctl sp get`
shows the mark on the group's record, or `dnvctl clone get --name <clone>`
for a clone's group.

Confirm: `dnvctl group inspect --grp <group id>` shows the array's state.

What dnv does alone: the primary acts on a mark only while the group's
array does not run. The worker clears only a mark the primary acted on.

Causes and steps:

- The array does not run, and the thin pool or a clone holds it open, so
  the primary cannot stop it. Step the level to `NO_THINPOOL`. The primary
  then stops the array and acts on the mark.
- The array runs. The mark was written over a running array, or the
  primary's agent restarted after the start and forgot that it acted on
  the mark. Cancel the mark at once:
  `dnvctl group force-start --grp <group id> --cancel`. Left standing, the
  mark acts at the array's next stop, from a leg that may be stale by
  then. The worker also makes no leg repair of the group while it stands.
  After a forced start whose stop failed, read "A forced start whose stop
  failed" first: there a mark whose leg md holds in sync can drop the
  bad-block lists.
- The named leg does not serve. The start waits for it. Bring the leg
  back, as in [A group whose in-sync leg died during a
  copy](#a-group-whose-in-sync-leg-died-during-a-copy).
- A leg the start would wipe does not serve. The start waits for it. Bring the
  leg back. If it will never come back, replace it with a repair:
  `dnvctl repair create --name <name> --grp <group id> --bad-legs <leg
  id>`. The start does not wait for an open repair's bad leg, but it waits
  until the repair's new leg serves. The group must have no other open
  repair. The repair is also refused while the group holds two retired
  legs, and every mark keeps them; deleting one may delete the copy you
  need, so stop instead.
- A start from a repair's new leg waits the same way for the leg of the
  list that is not the repair's bad leg.
- The dry run from the named leg, with the mark's words in `--override`,
  refuses with a text that starts `forced start refused`. The start meets
  a refusal the mark does not pass, such as a bad-block list on a named
  leg that was dead when the mark was written, and it wipes nothing. The
  group's row shows that text at most until the primary's next check
  round, which reads the row missing. Follow step 4 of the safe steps of
  "Which leg to choose".

Steps that can lose data: a mark left standing over a running array (see
above).

When to stop: the mark stays after a cancel, or the array runs and the
row still reads error.

Tested: the cn agent suite, `integtest/cnagent_test.sh`, repair case,
stages `badstart`, `older` and `olderstart`: a start refused for the named
leg's bad-block list and for an older leg, then passed; and the
end-to-end suite, `integtest/e2e_test.sh`, react case. Its first mark is
written while the named leg is dead, and the start waits until that leg's
disk node comes back; the case writes the mark again, from the dry run,
when the leg comes back with a bad-block list. These test the wait for the
named leg and the last cause. The other causes are not tested: the step
to `NO_THINPOOL` that lets the primary stop the array, since the suites
write every mark at that level; the cancel of a mark over a running array;
the wait for a leg the start would wipe, with its repair; and the wait of a
start from a repair's new leg for the list's other leg.

### A forced start whose stop failed

Symptom: a start that passed `bad-blocks` kept the named leg's bad-block
list. Such a start starts the array, stops it, and starts it again with the
list dropped; here the stop failed, which the lab saw only when a suite
held the array open on purpose. The group's row reads error for a moment,
with a text that says the stop to drop the list failed. At the worker's
next request the primary adds the other legs, and md copies onto them, but
not over the list's regions, which no member can read; those legs get the
regions in their own lists, and the row then reads ok. Hosts get IO errors
over the listed regions, though the named leg holds their data.

Confirm: `dnvctl group inspect --grp <group id>` shows a bad-block list on
the named leg and, once md has copied onto them, on the other legs.
`dnvctl cluster get-status` lists a `group` line for the bad blocks.

What dnv does alone: nothing that drops the lists. The worker sends the
primary its request again at the next check round, which adds the other
legs. It clears the mark once the row reads ok and the primary reports
that it acted on the mark. A primary whose agent restarted since the start
reports no such thing, and the mark then stands: it acts again at the
array's next stop. The worker makes no leg repair of the group, since
every leg now lists bad blocks, and a repair for bad blocks needs a leg
with none. From `NO_THINPOOL` down no reaction runs at all.

The ways, and what each costs:

1. Leave the group as it is, and go on from step 6 of "Which leg to
   choose", which steps the level back. Every region outside the lists
   reads and writes as before; a read over a listed region fails. Nothing
   more is lost. If `dnvctl sp get` (`dnvctl clone get --name <clone>` for
   a clone's group) still shows a `forced_start` on the group a few check
   rounds after the row reads ok, act on the mark. Either follow way 3 now:
   with the mark standing, it leads to "When the mark still stands", where
   the mark drops every list with nothing lost while md holds its leg in
   sync. Or cancel the mark, as "A forced start that waits" says: left
   standing, it acts at the array's next stop, from a leg that may be
   stale by then. Once the mark is gone, way 3 keeps only its four steps,
   at the cost they carry.
2. A new mark that names the same leg and passes `bad-blocks`, given while
   the row still reads error, then the level stepped to `NO_REDUND` and
   back, drops every list and loses nothing: the second start wipes the
   added legs again, and md copies them whole. But
   that moment ends with the worker's next request, one check round after
   the failed stop, 2 seconds at the default: no person can act in it.
   Once the row reads ok, the gateway refuses a new mark, and it refuses
   one at `NO_REDUND` too.
3. By hand with mdadm, which drops every leg's list. Before you start,
   with the level at `NO_THINPOOL`
   (`dnvctl sp set-level --level NO_THINPOOL` when it is not):
   - Read the group's md device, `res_name` of its row under
     `cntlr_info.grp_id_to_md_raid` in
     `dnvctl cntlr inspect --id <primary cntlr id>`. As root on the
     primary's controller node, `readlink -f <md device>` prints the
     array's kernel name, such as `/dev/md126`. Check that name once:
     `cat /sys/block/md126/md/array_state`, with the kernel name you read,
     must print a state of a running array: `clean`, `active`,
     `active-idle`, `write-pending`, `readonly` or `read-auto`. If it fails
     or prints another word, stop and ask for help.
   - Read the device of each leg md holds in the array: the legs of
     `leg_list` in `dnvctl group inspect --grp <group id>` whose
     `member.md_state` is not empty. A leg's device is
     `/dev/mapper/<res_name>` of its row under `cntlr_info.leg_id_to_leg`.
   - Check that `dnvctl sp get` (`dnvctl clone get --name <clone>` for a
     clone's group) shows no `forced_start` on the group. If it shows one,
     run no mdadm: follow "When the mark still stands" below in place of
     the four steps.

   The four steps:
   1. Step the level to `NO_REDUND`:
      `dnvctl sp set-level --level NO_REDUND`.
   2. Wait until the primary has stopped the array:
      `dnvctl cntlr inspect --id <primary cntlr id>` shows
      `applied_revision` at least the `sp_rev.revision` of `dnvctl sp get`,
      and on the primary's controller node
      `cat /sys/block/md126/md/array_state`, with the kernel name you read,
      prints `clear` or fails with `No such file or directory`. If the
      array still runs a minute later, its stop failed again: stop and ask
      for help.
   3. As root on the primary's controller node, in one command line, so
      that the array stops at once:
      `mdadm --assemble <md device> --metadata=1.2 --update=force-no-bbl <the device of each leg>; mdadm --stop <md device>`.
   4. Step the level back: `dnvctl sp set-level --level NO_THINPOOL`. The
      primary assembles the array again, with no list.
      `dnvctl group inspect --grp <group id>` must show no list on any leg.
      If a list is still there, stop and ask for help. Otherwise go on from
      step 6 of "Which leg to choose".

   When the mark still stands: the worker has not cleared it yet, or the
   primary's agent restarted after the start and forgot that it acted on
   the mark, and then the worker never clears it. The mark acts again at
   the array's next stop, and that drops every list with nothing lost, but
   only while md still holds the mark's leg in sync:
   1. Check that `dnvctl group inspect --grp <group id>` shows, for the leg
      the mark names, a `member.md_state` that holds the word `in_sync` and
      not the word `faulty`. If it does not, md no longer counts that leg
      whole, and the other legs may hold newer data: cancel the mark with
      `dnvctl group force-start --grp <group id> --cancel`, then stop and
      ask for help.
   2. Step the level to `NO_REDUND`, wait as in step 2 of the four steps,
      and step the level back to `NO_THINPOOL`. The mark acts: the start
      wipes the added legs, and md copies them whole.
   3. Wait until every leg is in sync and no leg shows a bad-block list, as
      in step 5 of "Which leg to choose". If the lists are still there, the
      worker cleared the mark first, or the start's stop failed again:
      start way 3 again. Otherwise go on from step 6 of "Which leg to
      choose". Run none of the four steps.

   The cost of the four steps: md then reads the listed regions from any
   leg as good data. The named leg holds their data. A leg added after the
   failed stop holds there whatever its disk held, since the forced start
   wipes only a leg's md superblock and md copied nothing onto those
   regions: for a leg that was a member of this group before, the group's
   data as md last wrote it there, which can be older; for a repair's new
   leg, whose disk node zeroed only its first blocks, possibly another
   pool's bytes. Hosts can read those bytes, with no error.

Steps that can lose data: the four steps of way 3, as their cost says.

When to stop: before the four steps of way 3, unless you know what each
added leg holds over the listed regions, as from its history in the group;
when the kernel name you read does not show a running array before you
start; when a mark still stands and md does not hold its leg in sync;
when the array still runs at `NO_REDUND`, since its stop failed again; or
when a leg still shows a bad-block list after step 4 of the four steps.
Ask for help.

Tested: the cn agent suite, repair case, stages `badstop` (the failed stop
made on purpose; a host read over the lists fails; the mark kept and the
level stepped down and up drops the lists), `badclear` (the mark cleared:
the same steps keep both lists), `badwindow` (a new mark while the row
reads error, then the level steps: both lists dropped) and `badmdadm` (the
mdadm steps of way 3, with the stop at `NO_REDUND` read from the cn
agent's check round, which reports the array as a leftover until it stops,
and the host's data read back), at the agent level. The suite plays the
worker and the gateway, so it times none of these against a worker. It
passed in the lab on 2026-11-21. Not tested: the wait of step 2 of the
four steps, through dnvctl and sysfs; the check of the kernel name; the
check that md holds the mark's leg in sync; a mark that an agent restart
left standing; and the four steps over a leg that holds another pool's
bytes.

### A lost clone meta group

Symptom: a clone's namespaces stay inaccessible and host IO waits. The
clone's meta group, the group that holds the clone's copy records, has no
running array: the primary's row for that group reads error.

Confirm: `dnvctl clone get --name <clone>` shows the group's id;
`dnvctl group inspect --grp <group id>` reads it.

What dnv does alone: the clone holds its group's array open, so the
primary cannot stop it. Unless the level stops reactions, the error brings
a failover, about 6 to 10 seconds later.

Safe steps: do the steps "Before every forced start". The level
`NO_THINPOOL` also takes the clones away, which frees the group's array.
Then follow "Which leg to choose" with the clone meta group's id, but only
from a leg with the newest copy records. When the level steps back, the
clone is built again from its copy records.

Steps that can lose data: a start from an older copy gives the clone older
copy records. They show regions as not yet copied that the clone had
copied, and hosts may have written there since. The clone then copies its
source over those regions again, and the hosts' writes are lost with no
error.

When to stop: the only leg you can start from holds an older copy of the
copy records.

Tested: no.

### A leg of another identity that the primary does not add

Words: a leg's **identity** is the array identity in its md superblock. A
forced start gives the array a new one. The legs it wipes join under the
new one once md copies onto them, and a leg it does not wipe, such as the
open repair's bad leg, keeps the old one.

Symptom: after a forced start from one leg, a leg that kept the old
identity, often a repair's bad leg, is never added back. After the next
failover or restart the group can stay down, its row naming two
identities.

Confirm: `dnvctl cntlr inspect --id <primary's cntlr id>` shows the
refusal in the primary's report for the group. While the array runs, the
leg's row reads `RES_STATUS_OK` with the reason
`RES_REASON_OTHER_IDENTITY`. Once the group is down with two identities,
each leg of either identity carries `RES_REASON_IDENTITY_UNDECIDED`, since
dnv cannot tell which identity is the current one.
`dnvctl group inspect --grp <group id>` shows each leg's identity.

What dnv does alone: the primary leaves that leg out, since it may hold
data the array lacks, and names it by that reason. The worker does not
replace it: the leg has no health epoch, and its leg repair waits for one.

Safe steps while the array runs, the leg's reason `RES_REASON_OTHER_IDENTITY`:

1. Replace the leg with a repair, with no force flag:
   `dnvctl repair create --name <name> --grp <group id> --bad-legs <leg id>`.
   The gateway reads the primary's report and takes the leg as the bad leg
   though it has no health epoch. The running array tells which identity
   is current, so the choice is dnv's. The gateway refuses the repair while
   the group holds two retired legs, and its message says to delete one. A
   retired leg can hold a copy you need, and the write of a forced start's
   mark sets the keep flag on the group's retired legs ("Keeping the old
   copies"): stop rather than delete one you cannot judge.
2. Follow the repair as usual: the primary adds the new leg and md copies
   onto it; when `dnvctl repair get --name <name>` shows the repair
   started, run `dnvctl repair finish --name <name>`. The finish keeps the
   old leg as a retired leg, with its data and with keep set, so the
   worker never deletes it: it stays until you clear keep with
   `dnvctl retired-leg keep --leg <leg id> --clear`, after which the worker
   deletes it once it is old enough, or delete it with
   `dnvctl retired-leg delete --leg <leg id>`.

Safe steps once the group is down, each leg's reason
`RES_REASON_IDENTITY_UNDECIDED`:

1. Decide which leg is the stale copy. A leg of another identity is from
   before an earlier forced start (rule 5 of the forced-start guide). The
   log record `forced start` of the last start names in `leg_id` the leg
   it started the array from. The cn agent of the controller node whose
   cntlr was the primary at that start wrote it, so read that node's log:
   after a failover it is not the current primary's node. The legs of that
   leg's identity hold the data the array served since; a leg of the other
   identity is the stale copy, unless the people who use the volumes say
   they need its data. If you cannot tell, stop.
2. Repair the stale leg, naming it with the flag the gateway asks for:
   `dnvctl repair create --name <name> --grp <group id> --bad-legs <stale leg id> --force-undecided`.
   Without `--force-undecided` the gateway refuses that leg while the
   primary's report names its reason; while the primary's agent is still
   starting, or its report has no row for the leg, the gateway takes a leg
   that has a health epoch without the flag, so name only the leg you
   chose in step 1. The primary then assembles the array from the other
   identity's legs, and the repair adds a new leg in place of the stale
   one. As while the array runs, the gateway refuses the repair while the
   group holds two retired legs: stop rather than delete one you cannot
   judge.
3. Follow the repair as in step 2 above. An abort of this repair leaves
   the stale leg as it is: the repair does not keep the flag.

Steps that can lose data: naming the wrong leg with `--force-undecided`
serves the older copy and loses every write made since on the other leg.
`--force` is another flag: with it, an abort of the repair copies over the
bad leg. A forced start from the leg you want wipes the other legs and
what only they hold.

When to stop: when the log does not tell which leg the last forced start
started from and the people who use the volumes cannot say which data they
need; when the gateway refuses the repair at two retired legs and you
cannot tell which retired leg holds a copy you may need; or when a repair
stays created and the primary's report,
`dnvctl cntlr inspect --id <primary's cntlr id>`, shows a `waits` or
`refusal` text you do not understand in `cntlr_info.rep_id_to_repair_info`
under the repair's `rep_id`, which `dnvctl repair get --name <name>`
shows.

Tested: the gateway's suite on fake agents, whose smoke case creates the
repair of a leg of another identity with no flag, and of an undecided one
with `--force-undecided`; unit tests of the gateway and of the cn agent,
which adds the new leg in place of a bad leg of another identity. Not run
on real agents. The cn agent suite's stage `takeback` checks a related
refusal: a repair that takes back a retired leg of another identity.

### A group whose every leg was wiped

Symptom: a forced start stopped after it wiped the other legs: its mark
was withdrawn, or the named leg was lost. The group stays down with no
member that carries a superblock.

How it comes about: a forced start wipes the superblock of every other
leg the group keeps, but never that of the leg its mark names, of the open
repair's bad leg, or of the new leg of a repair with its abort flag set.
While the mark stands and the named leg is lost, the start waits ("A
forced start that waits"). Once the mark is withdrawn, the primary
assembles the group with no mark from the legs that still carry a
superblock, as at any start: while the named leg serves, the group can
come back by itself, and md then copies onto the wiped legs; where mdadm
refuses that start, the group stays down with those legs. The group is
left with no member that carries a superblock only once the named leg,
and the open repair's bad leg where the group has one, are lost or have
left the group.

Confirm: `dnvctl group inspect --grp <group id>` shows which legs carry a
superblock. The named leg, the open repair's bad leg and an aborting
repair's new leg keep theirs while they serve; the wiped legs show none.
The group's row can name mdadm's refusal, or say that the array has run
before and no member carries a superblock, or that only some legs are
available and none carries one, at the converge that met it; the next
check round reads the row as `RES_STATUS_MISSING` with no text, so trust
the inspect.

What dnv does alone: with the mark withdrawn, the primary assembles the
group from the legs that carry a superblock. Once the worker has recorded
that the group's array ran, it makes no new array over legs that hold data
with no superblock. The scope page, section 5.2, lists the rare case where
the worker has not recorded it yet.

Safe steps: bring back the leg the mark named. A new mark can name it while
the group keeps it. A leg that has left the group, as one a repair retired,
takes the steps of "Starting from a retired leg" first.

Steps that can lose data: if that leg will never come back, the data on
the wiped legs can be reached only by hand with mdadm, by someone who
knows md's layout.

When to stop: the named leg will never come back.

Tested: the cn agent suite, repair case, stage `made`: no array over wiped
legs. The start with no mark after a withdrawn mark is run by no suite; a
unit test of the agent covers the refusal to make an array over wiped legs
once the array ran.

### An array create cut short

Symptom: a new group, of a new pool, of a grow or of a clone, has never
run. Its row reads error: mdadm's refusal to start the array, or that the
array is inactive.

Confirm: `dnvctl group inspect --grp <group id>` shows one leg with a
superblock, a full member, and the other leg with none. `dnvctl sp get`,
or for a clone's group `dnvctl clone get --name <clone>`, shows the group
with `array_made` false: the worker never saw its array run.

What happened: mdadm writes the legs' superblocks one after the other when
it creates an array, and the primary stops a command after 3 seconds. A
slow leg made the create pass that limit between the two writes. Every
later try, on this primary or on a new one, starts the array from the one
leg with a superblock, and mdadm refuses, since that superblock counts
both legs.

What dnv does alone: nothing that brings the group back. The group's
error row makes the primary unhealthy, so the worker fails the pool over,
after `primary_unhealthy` for a grow's group on a primary that has built
its stack and after `cntlr_unhealthy` for a new pool, or replaces the only
cntlr of a pool that has one; the new primary meets the same refusal, and
a grow's group that it cannot start can keep it from building that slice,
and so the pool.

Safe steps: follow "Before every forced start", then start the array from
the leg with the superblock:
`dnvctl group force-start --grp <group id> --leg <leg id>`. md then
copies that leg onto the other. Step the level back once
`dnvctl group inspect` shows the array running.

Steps that can lose data: none, as long as the group never ran: it holds
no data yet. A group whose array has run before is another case: read
"Which leg to choose".

When to stop: the group's `array_made` reads true, or `dnvctl group
inspect` shows a superblock on both legs, or one that is not a full member.

Tested: not tested.

### Keeping the old copies

Symptom: you plan a forced start, and the group has retired legs.

Confirm: `dnvctl retired-leg list` shows the group's retired legs and
their keep flags.

What dnv does alone: the write of a mark sets the keep flag on the
group's retired legs, on the legs its repairs took back, and on the bad leg
of its open repair. So the worker does not delete those legs. The flag
does not protect a leg from the start itself: the start wipes the legs of
the group's list and the new legs of its open repair, a taken-back leg
among them, and only their records stay.

Safe steps:

- `dnvctl retired-leg keep --leg <leg id>` keeps a retired leg.
- A group holds at most two retired legs. When both are kept, the worker's
  next repair of the group cannot finish until you clear a flag or delete
  a leg.

Steps that can lose data: `dnvctl retired-leg keep --leg <leg id> --clear`
lets the worker delete the leg once it is old enough. It can go sooner:
when the group holds two retired legs, the next finish of a repair of the
group gives back the oldest leg that is not kept.
`dnvctl retired-leg delete --leg <leg id>` deletes it at once. Either way
its copy is gone.

When to stop: you cannot tell which retired leg holds a copy you may need.

Tested: the gateway suite, `integtest/gateway_test.sh`, checks the flag;
the end-to-end suite's react case reads the keep flag that its first
forced start set on each retired leg, and clears it on all but one.

### Taking the group's repairs over

Symptom: you want to run repairs of a group by hand.

Confirm: `dnvctl sp get` shows the group's flag
(`operator_in_charge`) and its repair.

What dnv does alone: with the operator-in-charge flag set, five things
stop for the group: the worker creates no repair of it; the worker does not
take back a grow of it whose new group never gets ready, which it would
otherwise remove; the worker asks for no md check of it, the read of both
legs that repairs a sector one leg cannot read, and the primary stops a
check it started for the worker; a bad leg of it that a migration moves
keeps its migration, which leg repair would otherwise end; and the primary
leaves a broken array of it as it is. Failover and the other reactions go
on. The worker still drives a repair of its own that exists. A flag left
set by mistake leaves the group with no md check for good.

Safe steps:

1. Set the flag: `dnvctl group set-operator-in-charge --grp <group id>`.
2. If the worker's repair has not started, abort it with
   `dnvctl repair abort --name <name>`, then drop it with
   `dnvctl repair cancel --name <name>`. The cancel is refused until the
   primary confirms the abort, which it does only from a running array.
   At levels where reactions run, the worker may cancel its aborted repair
   itself, and your cancel then answers that the repair is gone. A repair
   that has started cannot be aborted: finish it with
   `dnvctl repair finish --name <name>`.
3. Create your own repair.
4. Clear the flag with `--operator-in-charge=false` when you are done.

Steps that can lose data: finishing a repair when the group already holds
two retired legs deletes the oldest one that is not kept, and its copy
with it. Keep the retired legs you may need first ("Keeping the old
copies").

When to stop: the group's array does not run and the worker's repair
waits for an abort it cannot get.

Tested: the worker suite, `integtest/worker_test.sh`, the gateway suite and
the end-to-end suite check that the flag holds the worker's create of a
repair. The other effects are checked only by unit tests: the broken array
left alone by the agent's, the md check not asked for, the migration not
ended and the grow not taken back by the worker's. No test covers the
primary's stop of a running check under the flag.

## A pool made before the stripe bound

Words: a pool's **stripe** is the size of the piece of a volume that each
slice holds in turn. A controller node's kernel refuses, over TCP, a
command that carries more than 4 MiB of data. dnv keeps a new pool's
stripe times its slice count at 4 MiB or less, so that its hosts never
send such a command. A pool made before dnv kept that bound keeps its
stripe, since a pool's layout never changes. On a host, a namespace's
device `nvme<X>n<Z>` tells its **best IO size**, the stripe times the
slice count, in `/sys/block/<device>/queue/optimal_io_size`. Under it,
the kernel has one hidden **path device** for each controller node's
connection, `nvme<X>c<Y>n<Z>`, listed in `/sys/block/<device>/multipath/`.
Each path device has its own **largest command** in
`queue/max_sectors_kb`, which the kernel raises to the best IO size. A
path device's largest command is the one that counts. The namespace
device's own setting does not limit what a path device sends.

Symptom: a host of the pool gets IO errors. A direct read or write over
4 MiB fails, and a buffered write fails at its flush (`fsync`), so data an
application wrote can be missing. A buffered read still succeeds, more
slowly, and its failed readahead shows in the kernel log. The host's
kernel log shows `I/O Error (sct 0x0 / sc 0xf) DNR` on a path device.

Confirm: `dnvctl sp get` shows the pool's stripe as
`sp_conf.bdev_conf.dm_raid0_conf.stripe_size` and its slices in
`sp_conf.slice_id_list`. The stripe times the slice count is over
4194304. On the host, `cat /sys/block/<device>/queue/optimal_io_size`
prints the same product, and
`cat /sys/block/<device>/multipath/*/queue/max_sectors_kb` prints values
over 4096.

What dnv does alone: nothing. The pool's stripe never changes.

Safe steps, as root on every host of the pool:

1. Set the largest command of each path device of each of the pool's
   namespaces to 4 MiB:
   `for f in /sys/block/<device>/multipath/*/queue/max_sectors_kb; do echo 4096 > $f; done`.
   A setting on the namespace's device alone changes nothing.
2. Hold the setting for path devices that come later. Path devices come
   with each new connection of the host to a controller node. The host
   makes one when it starts, when the operator or a cntlr replacement
   creates a cntlr of the pool, when the cdc lists again a cntlr it left
   out while that cntlr was unhealthy or disabled, and after the kernel
   drops a lost connection. Path devices also come when a namespace is
   created in a subsystem the host is connected to. The kernel sends no
   udev event for a path device. It sends one for the controller's
   connection, before it finds the namespaces behind it, and one for a
   new namespace's device, which can come before all of its paths are
   found. So write this script to `/usr/local/sbin/dnv-path-max4m` and
   make it executable (`chmod 755`):

   ```sh
   #!/bin/sh
   # Hold the largest command of each path device under $1 to 4 MiB. $1 is a
   # controller, as nvme3, or a namespace's device, as nvme0n2. The kernel
   # can add path devices after the event, so try for five seconds.
   case $1 in
   nvme*n*) g="/sys/block/$1/multipath/*/queue/max_sectors_kb" ;;
   *) g="/sys/class/nvme/$1/nvme*c*n*/queue/max_sectors_kb" ;;
   esac
   i=0
   while [ $i -lt 20 ]; do
       for f in $g; do
           [ -e "$f" ] && echo 4096 > "$f"
       done
       sleep 0.25
       i=$((i + 1))
   done
   ```

   Then write these two lines for each subsystem of the pool to
   `/etc/udev/rules.d/99-dnv-path-max4m.rules`, with the subsystem's NQN
   from `sp_conf.nqn_list` of `dnvctl sp get`, and run
   `udevadm control --reload`:

   ```
   ACTION=="change", SUBSYSTEM=="nvme", ENV{NVME_EVENT}=="connected", ATTR{subsysnqn}=="<subsystem NQN>", RUN+="/usr/local/sbin/dnv-path-max4m %k"
   ACTION=="add", SUBSYSTEM=="block", ENV{DEVTYPE}=="disk", KERNEL=="nvme*n*", ATTRS{subsysnqn}=="<subsystem NQN>", RUN+="/usr/local/sbin/dnv-path-max4m %k"
   ```

   A new path device can carry a large command before the script sets
   it: up to a quarter of a second once the script runs, and longer when
   udev starts the script late. That command fails. A path to a standby
   carries no IO until a failover, so this short gap matters only for a
   path to the primary. A path device the kernel finds after the
   script's five seconds keeps its large setting: the check under
   Confirm shows it, and step 1 sets it.

   The rules do not cover two cases. A subsystem created later needs its
   own two lines in the rules file of every host. A host added later to
   a subsystem of the pool needs the script and the rules file before it
   connects.
3. To leave the old pool for good, copy the data at the file level from a
   host into a volume of a new pool, and then delete the old pool from the
   inside out, in the order of step 2 of [Remove a node](#remove-a-node):
   its delete is refused while it still holds a volume, a subsystem, a
   clone, a transfer or a migration. A clone out of the old pool is
   refused when the clone's dm-clone device, on the new pool's primary,
   would tell its hosts a best IO size over 4 MiB. The refusal names both
   pools' stripe times slice count.

Steps that can lose data: steps 1 and 2 write no data. In step 3, delete
the old pool only after the copy is read back on the new volume.

When to stop: the IO errors go on while every path device of the
namespace reads 4096. Then their cause is another one: ask for help.

Tested: the lab, by hand on kernel 7.0.0-42. The test used no dnv pool,
since dnv refuses to make one now. It used a namespace of five slices at
a 1 MiB stripe, built by hand: a dm-stripe over five thin volumes,
exported by nvmet over TCP. Before step 1, an 8 MiB direct read and
write failed and a buffered write failed at its flush. The setting on
the namespace's device alone changed nothing. After step 1 as written,
all three passed. With the first rule line and the script, a second path
joined, a controller was reset, and a controller was disconnected by
hand and the host connected again: every path device read 4096, and
the three reads and writes passed after the reset and after the new
connection. With both lines and the script as written, a second
namespace created on the controller node while the host was connected
over two paths had both its path devices set, and an 8 MiB direct read
of it passed. Step 3 is not tested.

## A pool made before the block bound

Words: a pool's **data block** is the unit in which its thin pools give
out space. dnv keeps a new pool's data block at 4 MiB or less, the most
data one command over TCP may carry to a node. A pool made before dnv
kept that bound keeps its block, since a pool's layout never changes. A
**migration** of such a pool's side would make its new disk node take
commands up to one block, which that node's kernel refuses, so dnv
refuses the migration.

Symptom: `dnvctl migr create` of a side of the pool is refused with the
error code FAILED_PRECONDITION, and the message names the pool's data
block, the bound 4194304 and the way out. It matters when a side must
move while its leg still works: a slow leg (see "A slow leg"), or a disk
node to empty before its delete.

Confirm: `dnvctl sp get` shows the pool's
`sp_conf.bdev_conf.dm_pool_conf.data_block_size` above 4194304.

What dnv does alone: nothing for a leg that works. In a RAID1 pool, a leg
that fails is still replaced by leg repair, which copies the data through
md on the controller node and not through a migration, so the block does
not stop it. `dnvctl repair create` likewise replaces only a leg that
failed, or one the primary keeps out for another identity (see "A leg of
another identity that the primary does not add"), and refuses a leg that
works, naming the way out below. A pool without RAID has no repair.

Safe steps, to move every volume of the pool to a new pool:

1. Create a new pool with `dnvctl sp create` and these flags:
   - `--block-size` of 4194304 or less, such as 1048576. A cluster made
     before the bound can keep a larger block as the default of its
     pools, and then refuses a pool create that names no block.
   - `--slice-cnt` and `--stripe-size` as the old pool's, when the old
     pool's stripe times its slice count is 4 MiB or less: a clone between
     pools whose stripes do not combine within 4 MiB is refused, and the
     refusal names both stripes times slice counts
     ([the scope page](scope.md), section 10).
   - `--dn-black` with the address of the slow disk node, or of the disk
     node to empty, as `dnvctl dn get --addr` takes it, so that no leg of
     the new pool lands on it at its create. The list binds only the
     create: a later grow or leg repair of either pool may still place a
     leg there. Before the create, mark that disk node disabled with
     `dnvctl dn set-disabled --addr <disk node> --disabled`, which keeps
     every new allocation of every pool off it and leaves what it holds
     running.
   - `--cn-black` with the addresses of the controller nodes of the old
     pool's cntlrs, and `--slots` with cntlid slots, the ranges of NVMe
     controller ids a pool's exports use, that none of the old pool's
     cntlrs uses. `dnvctl sp get` of the old pool shows each cntlr's
     `addr_port` and `cntlid_slot`. The two pools export the same
     subsystem names to the same hosts: a controller node has one
     subsystem of each name, which the cntlrs of both pools would claim,
     and a host refuses a second path to a subsystem whose controller id
     another of its paths already uses. So the cluster needs controller
     nodes beyond those of the old pool. This list too binds only the
     create: a cntlr replacement of either pool may later place a cntlr on
     a controller node of the other pool. Before each move of step 2, and
     while its copy runs, compare the `addr_port` of the cntlrs that
     `dnvctl sp get` shows for both pools. If two share a controller node, stop moving and move one
     of them to another controller node, the new cntlr first, as
     [Replace a cntlr by hand](#replace-a-cntlr-by-hand) says. Create the
     new cntlr with
     `dnvctl cntlr create --slot <slot> --cn-black <the other pool's controller nodes>`,
     the addresses separated by commas, and a slot of its pool's
     `sp_conf.cntlid_slot_list` that no cntlr of either pool uses; when the
     list holds none, add one first ([Change a pool's cntlid
     slots](#change-a-pools-cntlid-slots)). A pool with fewer than four
     cntlrs always finds such a slot, since the two pools' cntlrs then use
     fewer than the eight slots. Wait until the new cntlr is built, as step
     3 of [Replace a cntlr by hand](#replace-a-cntlr-by-hand) says. Then
     disable the old one with
     `dnvctl cntlr set-enabled --id <cntlr id> --enabled=false`. If it was
     the primary, wait until `dnvctl sp get` shows another cntlr as the
     primary with `settling` false, which says the new primary reports its
     devices built, as step 4 of [Replace a cntlr by
     hand](#replace-a-cntlr-by-hand) says. The gateway refuses to delete
     the primary, and a delete while the new primary still shows
     `settling` true stops the pool's IO, since the old cntlr can then no
     longer take the role back. Then delete
     it with `dnvctl cntlr delete --id <cntlr id>`. A pool of four cntlrs,
     the most a pool can have, refuses the create: move a cntlr of the
     other pool instead when that pool has fewer. When both have four,
     disable and delete the old cntlr first, with the same wait, and then
     create the new one,
     in the old cntlr's slot when the two pools' cntlrs use every other
     slot. Delete first only a cntlr whose pool has another healthy,
     enabled cntlr: disabling a pool's only enabled cntlr stops that
     pool's IO, and no failover follows. If neither pool has one, stop and
     ask for help.
2. Move the volumes one at a time, or a few at once: a pool holds at most
   four transfers, and a fifth `dnvctl xfer create` is refused with the
   error code RESOURCE_EXHAUSTED. For each volume:
   1. Start its move: a transfer in the old pool, `dnvctl xfer create`
      with `--auto-suspend`, and a clone of it in the new pool,
      `dnvctl clone create` with `--auto-resume`, into a new, empty volume
      that a namespace of the same subsystem name and the same identity,
      its nguid and uuid, exports, as [the introduction](introduction.md#transfer-and-clone)
      explains. Create that namespace before the transfer, with
      `dnvctl ns create --suspended`, at the old namespace's `--idx`, on a
      new volume of the old one's size: a namespace that is not suspended
      serves its empty volume to hosts at once beside the old one, a
      namespace at another index is refused by the host as a second copy
      of one identity, and a smaller volume is copied short. The old pool's data block does not stop this: a transfer
      judges no block, and a clone takes a source block up to 1 GiB. The
      full steps of one move, with every command, the names and the hosts
      the two pools must share, are in [Move a volume to another
      pool](#move-a-volume-to-another-pool).
   2. Wait until the copy is complete. Hosts use the new pool's path from
      the moment the clone serves, but the clone reads every region it
      has not copied yet from the old volume, through the transfer. The
      copy is complete only when the clone has left by itself:
      `dnvctl clone get --name <clone>` in the new pool fails with the
      error code NOT_FOUND. Run it once right after the create too, and
      see that it shows the clone, so that a wrong name does not read as
      done. Meanwhile, in `dnvctl cntlr inspect --id <primary cntlr id>`
      of the new pool, the details of the clone's row in
      `cntlr_info.clone_id_to_dm_clone`, keyed by the `clone_id` that
      `dnvctl clone get` shows, carry dm-clone's status line, whose count
      of hydrated regions, the regions copied so far, out of all regions
      shows how far the copy is.
   3. Then end the transfer in the old pool, without `--force`:
      `dnvctl xfer delete --name <transfer>`. The old namespace stays
      parked, ANA inaccessible on every cntlr, until it is deleted.
   4. Then delete, in the old pool, that volume's namespace with
      `dnvctl ns delete` and the volume with `dnvctl td delete`, and the
      subsystem with `dnvctl ss delete` once it holds no namespace.

   A transfer exports a namespace, so a volume that no namespace exports,
   such as a snapshot, moves only once a namespace of the old pool exports
   it. In the new pool it is a volume of its own, which shares no block
   with its origin.
3. Once the old pool holds no volume, subsystem, clone, transfer or
   migration, delete it with `dnvctl sp delete`, which refuses a pool that
   still holds any.

Steps that can lose data:

- Creating the new pool's namespace without `--suspended`: hosts read its
  empty volume beside the old one under the same name and identity, and
  their writes to it before the clone serves are later copied over.
- Deleting a transfer while its clone still copies, that is before
  `dnvctl clone get` in the new pool answers NOT_FOUND, with or without
  `--force`, takes from the clone its only source of every region not yet
  copied. The deletes of the old namespace and volume, which the old pool
  refuses while the transfer stands, then delete the last copy of those
  regions.
- `dnvctl xfer delete --force` ends a transfer as an abort: the old
  namespace serves again, ANA optimized, under the same subsystem name and
  the same identity as the new one, so hosts that keep the new pool's path
  reach two copies that differ. Run it only to abort the move, and only
  once the new pool's namespace is deleted with `dnvctl ns delete` and its
  clone, if there is one, with `dnvctl clone delete --force`. Once the
  clone has served the hosts, first stop the volume's users on each host
  and unmount it, before that namespace's delete: the hosts' file systems
  hold in their memory what they wrote through the new pool, and would
  write it onto the old volume, as [Move a volume to another
  pool](#move-a-volume-to-another-pool) says. Even then every write made
  through the new pool is lost, since the old volume lacks it.
- A clone refused after its transfer is made leaves the old volume's
  namespace parked, with no path that serves IO, until
  `dnvctl xfer delete --force` in the old pool; so check the two pools'
  stripes, and the clone meta group as check 4 of the move says, before
  the transfer.

When to stop: the clone is refused for the two pools' stripes and no
slice count of the new pool fits; then copy the data at the file level
from a host into a volume of the new pool, as step 3 of
[a pool made before the stripe bound](#a-pool-made-before-the-stripe-bound)
says. Or the clone cannot read its source, which `dnvctl cluster
get-status` shows as a line `clone/missing/source_fault` that stays: the
When to stop of [Move a volume to another
pool](#move-a-volume-to-another-pool) says what to check. Or the clone
does not leave, and its count of hydrated regions stops growing or its
row in `cntlr_info.clone_id_to_dm_clone` reads `RES_STATUS_ERROR`: first
make the checks of step 13 of the move, as the move's When to stop says.
After a failover of the old pool, the status read can show no line for
the clone. When the count
still does not grow, delete nothing in the old pool, and ask for help.

Tested: the gateway's unit tests refuse the migration and name the way
out. The e2e suite runs a transfer and a clone within one pool: the clone
leaves by itself once its copy is complete, and the transfer is then
ended by an abort, `xfer delete --force`. The move into a new pool under
the same subsystem name and identity, its end by `xfer delete` without
`--force`, the deletes of the old volume and of the old pool, and a move
out of a pool over the bound are not tested.

## A new volume refused for a thin id that holds data

Words: a volume's **thin id** is dm-thin's number for the volume inside the
thin pool of each slice. `dnvctl td list` shows it as `dev_id`. The pool
keeps the blocks of each thin id until the id is deleted. A volume is
**recorded as created** once its primary has built it in every slice;
`dnvctl td list` shows `created`.

Symptom: a new volume, `created` false, does not come up. In
`dnvctl cntlr inspect --id <primary cntlr id>`, a row of the volume in
`td_id_to_thin_info` reads `RES_STATUS_ERROR` with the reason
`RES_REASON_THIN_ID_NOT_ADOPTED`. Its details say that the thin id exists in
the slice's pool and maps some blocks. Details that name a failed read
instead are covered under When to stop. The row reads so only in the report
of a converge that the syncup brings. Between two such converges the row
reads `RES_STATUS_MISSING`, with no details and no reason. If you see that,
run the command again a few seconds later, until the row reads
`RES_STATUS_ERROR` with the reason.

Confirm: the row's details name the pool and the block count. Two cases
give this row, and the steps differ:

1. **After an etcd restore.** The cluster's etcd was restored from a
   backup, and the volume was created after the restore. The id holds the
   data of a volume made after the backup, which the restore lost. When the
   restore ran `dnvctl cluster advance-ids`, only a pool that created more
   than 1,048,576 thin devices after the backup meets this. After a restore
   that skipped that step, any new volume can meet it
   ([the scope page](scope.md), section 7).
2. **In the volume's first moments.** No restore happened. A host wrote to
   the volume before it was recorded as created, and then a primary built
   the volume again. The id holds the volume's own data. One of these shows
   it: `dnvctl sp get` shows another cntlr as the primary than the one that
   built the volume, and the worker's log shows the failover; the primary's
   controller node booted after the volume was created (`uptime -s`); or an
   operator stepped the pool's level down to `NO_THINPOOL` or further and
   back while the volume was not yet recorded as created.

What dnv does alone: the primary builds the volume in every slice whose
thin id maps no block, and builds nothing over the id that holds data. It
does not fail over for this row. The worker sends the cntlr's syncup again
every second check round, and the primary reads the pool's records of its
thin ids again each time, until the row changes.

Safe steps for case 2, as root on the primary's controller node, for each
slice whose row reads the refusal:

1. Take the device name from the row's `res_name`, and the pool's device
   name from the slice's row in `slice_id_to_dm_pool`.
2. Read the pool device's number:
   `dmsetup info -c --noheadings -o major,minor <pool name>`.
3. Work out the size: the volume's `size` from `dnvctl td list`, divided
   by the slice count (`sp_conf.slice_id_list` of `dnvctl sp get`), and
   by 512.
4. Make the device by hand over the id:
   `dmsetup create <device name> --table "0 <size> thin <major>:<minor> <dev_id>"`.

The primary takes a device that is already there as its own, as after a
restart of its agent. The next syncup builds the rest of the volume, and
the volume is recorded as created. The data the host wrote is kept.

Safe steps for case 1: the id holds a lost volume's data. To keep a copy,
read it first through a device made by hand under a name that does not
start with `dnv-`, with `--readonly`, as in step 4 above, and copy it off;
then remove that device with `dmsetup remove`.

Steps that can lose data: in case 1, delete the id on the primary's
controller node with `dmsetup message <pool name> 0 delete <dev_id>`. Its
blocks are gone. The next syncup makes the id again, empty, and builds the
volume. Never delete the id in case 2: it holds the host's data. Deleting
the refused volume with `dnvctl td delete` and creating it again loses the
same data in case 2.

When to stop: the details name a failed read of the pool's records instead
of a block count. First wait: the primary reads the records again at each
syncup the worker sends, every second check round, and once a read
answers it builds the volume or refuses it as above. So a read that
failed once, as on a slow pool, needs nothing from you. A pool out of
metadata space takes no change: its own row in `slice_id_to_dm_pool`
reads `RES_STATUS_OK` with no reason, and its status line in the details
says `ro`. Auto-grow adds metadata to it, unless auto-grow is off for the
pool, and the volume then builds. Stop when the read keeps failing for
another cause: the node misses a tool or one of its options, which is a
node fault, or the pool's row reads `RES_REASON_POOL_NEEDS_CHECK`, a pool
that waits for a check of its records and needs its own repair first, or
auto-grow is off. Ask for help, and do not delete the id.

Tested: the cn suite plants an id with data, sends a new volume over it,
reads the refusal, deletes the id and sees the volume build. A unit test
of the cn agent makes the device by hand over the refused id and sees it
taken. Case 2 itself is not tested.

## A pool left by a Kubernetes volume whose name held ".."

Words: the CSI driver, dnv's driver for Kubernetes, makes each Kubernetes
volume a storage pool of its own, named after the volume. A **volume id**
is the name Kubernetes keeps for such a volume: the cluster name, a slash
and the pool's name.

Symptom: a cluster that ran dnv 0.1 holds a storage pool whose name holds
two dots in a row (".."), with one thin device and no subsystem, and no
Kubernetes volume uses it. Under 0.1 the driver gave a volume whose name
held ".." a pool of that name and a thin device, and then the gateway
refused the subsystem, whose name may not hold "..", so the create failed
at each try. The driver now gives such a volume a name of its own, as it
does a name too long for dnv, so the volume is made under that name. What
the failed tries made stays, and holds its space on the disk nodes.

Confirm: `dnvctl sp list --count 1024` names the pool. One reply holds at
most that many names; while a reply carries a `page_token` that is not
empty, run the command again with `--page-token <that token>` for the next
names. `dnvctl --sp '<pool name>' ss list` shows no subsystem, and
`dnvctl --sp '<pool name>' td list` one thin device. No Kubernetes volume
names it:
`kubectl get pv -o custom-columns=ID:.spec.csi.volumeHandle` lists every
volume id, and none holds "..".

What dnv does alone: nothing. The pool keeps its space until it is
deleted.

Safe steps:

1. Delete its thin device:
   `dnvctl --sp '<pool name>' td delete --name '<td name>'`.
2. Delete the pool: `dnvctl --sp '<pool name>' sp delete`. The worker then
   removes its parts in the background (its drain).

Steps that can lose data: none. No host was given a namespace of this
pool, so it holds no data.

When to stop: when the pool holds a subsystem, or a volume id names the
pool: then it is not one a failed create left.

Tested: no. The driver's unit tests make a volume whose name holds ".."
under the driver's own name for it.

## A planned restart of an agent

Words: when an agent starts, it first runs its **startup pass**: it reads
the requests it saved on its node and builds again whatever of them the
kernel does not hold. The **start hold** is how long the worker waits for
an agent's startup pass to end. A cluster stores it when it is created
(`dnvctl cluster create --start-hold-seconds`), and `dnvctl cluster get`
shows a stored one as `cluster_conf.start_hold_conf.seconds`. A cluster
that stores none waits 120 seconds, and one that stores 0 does not wait.
The **check interval** is the time between two check rounds of the
worker to one cntlr. `dnvctl cluster get` shows it as
`cluster_conf.health_check_conf.cntlr_interval`. It is 2 seconds at the
default.

When: you restart the agent of one node with the same program, for
example to change a setting of the node, and the pools on the node must go
on serving. An upgrade is different: every program stops
at once, the workers first ([Upgrade dnv](#upgrade-dnv)).

What dnv does alone:

- While the agent is stopped, what it built in the kernel goes on serving.
  Hosts keep reading and writing through the node.
- An agent stopped with SIGTERM stops answering within about a second,
  and then exits.
- The worker's check rounds to the stopped agent fail. The first failed
  round sets the health epoch of the node and of each cntlr and side on it.
- The new agent starts to listen for the worker's calls before its startup
  pass. Until the pass ends, it answers every check round that it is
  starting, and it refuses every syncup, which the worker sends again
  later.
- While the agent answers that it is starting, and for the start hold at
  most, the worker makes no failover and no cntlr replacement for a cntlr
  on the node. A side on the node does not count toward a leg repair, and
  a new leg on the node is not given up while it is zeroed. The health
  epochs stay set. The start hold counts from the agent's first start,
  also when the agent stops and starts again, during its startup pass or
  after it but before a check round has judged that cntlr or side again.
  A worker restart, or the pool's move to another worker, in that time
  counts it from the agent's latest start instead.
- The check rounds after the startup pass clear those health epochs, if
  nothing else is wrong. If a check round found the agent starting, the
  first round after the pass judges nothing yet, so the epochs clear at the
  second. If no round did, as after a short pass, they clear at the first.
- The start hold does not cover the time the agent is stopped. The worker
  learns that the agent is starting only from a check round. Two failed
  check rounds in a row can fail a primary on the node over, once its
  health epoch is as old as the pool's `primary_unhealthy`, 4 seconds at
  the default. At the defaults the two failed rounds come 2 seconds
  apart, and the health epoch, which counts in whole seconds, can be 4
  seconds old before the next round comes. So a stop a little longer than
  one check interval can already fail a primary over.
- If a startup pass takes longer than the start hold, the worker counts
  the agent's check rounds as missed from then, and the reactions follow
  at their event thresholds.

Safe steps:

1. Find how long the agent may stay stopped. On a controller node that
   holds a primary, the time from the stop to the moment the new agent
   starts to listen for the worker's calls must stay well under one check
   interval: about one and a half seconds at the defaults. The worker's
   connection to the agent waits a little before it tries again after a
   refused connect, so a whole interval leaves no margin. The agent stops
   answering within about a second of SIGTERM, so the new agent must start
   right after the old one exits. On a node that holds no primary, the
   pools' `cntlr_unhealthy` for a cntlr and `side_unhealthy` for a side
   bound it, 600 seconds each at the default. `dnvctl sp get` shows them
   in `sp_conf.event_threshold`. A pool that `dnvctl sp create` made with
   no `--thr-*` flag shows `event_threshold` as null, and a threshold that
   the create did not set shows 0. Both mean the default.
2. Have the command that starts the agent ready before you stop it, so that
   the stop lasts only as long as the restart itself. On a controller node,
   also run `dnv-agent check cn` with the agent's flags while the old agent
   still runs ([Check a node before it
   joins](#check-a-node-before-it-joins)). A `FAIL` on its
   `nvmet host cut (debugfs)` line means that the new agent will refuse to
   start, and the stop then lasts until you fix the cause: fix it first, or
   do step 3. A `WARN` on that line, which the check gives in place of the
   `FAIL` when it has `--ignore-host-disconnection`, means that the new
   agent will start and end no host's connection. If the old agent did not
   log the warning `agent host cut disabled` when it started, it ends
   connections, and the new one would not: fix the cause first.
3. If the agent of a controller node that holds a primary may stay stopped
   longer than step 1 allows, move each primary off the node first. Skip
   this step for a pool whose level is `NO_THINPOOL` or below: there the
   worker makes no failover and no cntlr replacement, neither after a
   disable nor during a stop ([Change a pool's
   level](#change-a-pools-level)). For each cntlr on the node that
   `dnvctl sp get` shows as primary, first check that `dnvctl sp get`
   shows another cntlr of the pool with
   `disabled` false and `err_epoch` 0: an enabled standby with no health
   epoch. If there is one, run
   `dnvctl cntlr set-enabled --id <cntlr id> --enabled=false`, and wait
   until `dnvctl sp get` shows another cntlr of the pool as primary with
   `settling` false: the worker sets that flag false once a new primary
   reports its devices built. A disabled cntlr serves no IO. A standby
   whose agent refuses its own syncups also shows no health epoch. The
   worker does not make such a standby primary once it has seen it refuse
   a syncup, but it can make it primary before then. That standby's agent
   then refuses the syncup that makes it primary, so the new primary
   serves no IO and keeps `settling` true. So if no other cntlr becomes
   primary within a few check intervals, or the new primary still shows
   `settling` true a minute after it became primary, enable the disabled
   cntlr again at once, with step 8's command. If another cntlr became
   primary, wait until `dnvctl sp get` shows the cntlr you enabled with
   `err_epoch` 0 and, on each host that uses the pool, `nvme list-subsys`
   shows the path to it `live`; only then disable the new primary with
   the same command as above, so that the worker moves the role back at
   once and the pool gets its IO back. That disable takes the new primary
   out of the cdc's records, so a host that follows the discovery log and
   has no other `live` path can drop its last path (Steps that can lose
   data). Then ask for help. If the pool has no enabled standby
   with no health epoch, do not disable the primary: that stops the pool's
   IO, and no other cntlr becomes primary. In such a pool a stop of any
   length fails the primary over only if an enabled standby with no health
   epoch appears meanwhile, but a stop longer than the pool's
   `cntlr_unhealthy`, 600 seconds at the default, can replace the primary
   with a new cntlr on another controller node.
4. Stop the agent with SIGTERM (`kill <pid>`, or `systemctl stop` for a
   service), not with SIGKILL, and wait until its process has exited.
   Send it once: a second SIGTERM or SIGINT ends the agent at once,
   without its clean stop, as SIGKILL would, and on a disk node a zeroing
   command it started can then go on after it.
5. Start the agent again at once with the same addresses, port id and
   `--local-store` as before, and on a controller node with
   `--ignore-host-disconnection` if the old agent ran with it.
6. In the agent's log, find the record `agent serving` and after it the
   record `agent started`. Its `startup_ms` is how long the startup pass
   took. It must be well under the start hold. In one lab measurement, of
   one pool of 32 slices, the stop, sent over ssh, took about 1.3 seconds,
   the new agent listened at once, and the startup pass took 1.8 seconds
   on the primary's controller node and 1.1 seconds on the standby's.
7. Check that nothing moved: `dnvctl sp get` shows the same primary for
   each pool, unless step 3 moved it.
8. If step 3 moved a primary, enable the cntlr it disabled again:
   `dnvctl cntlr set-enabled --id <cntlr id>`. It comes back as a standby.

Steps that can lose data: a stop longer than step 1 allows, on a
controller node that holds a primary whose pool has an enabled standby
with no health epoch, without step 3. The worker can then fail the
primary over while the old primary's kernel objects still serve, and a
write a host sent through the old primary in that time can be lost even
though the host was told it was done ([the scope page](scope.md) says
when). So can a stop longer than the pool's `cntlr_unhealthy` on a
controller node that holds the primary of a pool without such a standby:
the worker can then replace the primary while its kernel objects still
serve. Step 3 avoids the first case: the worker gives the disabled primary its
demotion first, and waits up to three seconds for it to report the
demotion done before the other cntlr serves. An agent that runs reports
it well inside that time. In step 3, a disable of the new primary before
each host shows its path to the cntlr you enabled `live` can lose data
too: the cdc stops listing the new primary, so a host that follows the
discovery log can drop its last path, and writes not yet flushed are lost.

Stop and ask for help when the agent's log shows `agent reconcile failed`,
when the agent exits on its own, when no other cntlr becomes primary after
step 3 disabled one, or the new primary still shows `settling` true a
minute after it became primary (give the pool its IO back first, as step 3
says), or when `dnvctl sp get` shows a failover or a new cntlr that step 3
did not ask for.

Tested: the e2e suite's react case restarts the primary's cn agent this
way, with its host connected, and checks that no failover and no cntlr
replacement follows and that the host reads its data. No test restarts a
dn agent this way; the worker's tests check that a dn agent's start keeps
a new leg from being given up. A unit test of the agent checks that a
second signal ends it with exit status 1; no suite sends one.

## A slow leg

Words: the primary's **leg probe** is a small test write and read of one
block at the end of each leg's metadata, which the primary makes every
few seconds through the leg. A device's **counters** are the kernel's
numbers for it: how many reads and writes it has done since it appeared,
how many sectors they moved, and how many milliseconds they took, in the
order the kernel's `stat` file of the device gives them.

Symptom: writes to a pool's volumes are slow while every row reads
`RES_STATUS_OK`. In a group of two legs, every write waits for both legs,
so one slow leg slows the whole group.

Confirm:

1. Run `dnvctl cntlr inspect --id <primary cntlr id>` twice, about ten
   seconds apart. Each run reads the counters at that moment. Under
   `io_stats.leg_id_to_io`, each leg has `probe_times`, the primary's last
   leg probe, and `dev.stat`, the counters of the leg's device on the
   controller node.
2. A slow leg shows a `probe_times.write_ms`, and a `read_ms`, well above
   those of the other legs. Between the two runs, divide the growth of its
   `write_ticks_ms` by the growth of its `write_ios`: that is the average
   time of one write, which is well above that of the other legs. The
   `round_age_ms` says how old the probe is; a probe still running is not
   shown until it ends, and one that runs longer than 15 seconds turns the
   leg's row to `RES_STATUS_ERROR` anyway.
3. Find the leg: `cntlr_info.leg_id_to_leg` gives its device name for the
   same leg id, and `dnvctl sp get` lists the slices, their groups and
   their legs, with each leg's side and the disk node that holds it.
4. On that disk node, `dnvctl dn inspect --addr <disk node address>`
   prints the disk's counters as `disk_io_stats`, read at that moment.
   Two runs show whether the disk itself is slow. The disk's own health,
   such as its SMART data, is for the node's usual tools, such as smartd
   and node_exporter.

A standby runs no leg probe, so its legs show counters and no
`probe_times`.

What dnv does alone: nothing. A slow leg reads `RES_STATUS_OK`, no rule
reads the times or the counters, and leg repair does not replace a leg
that works ([the scope page](scope.md), section 6.1).

Safe steps: move the slow leg's side to another disk node with a
migration: `dnvctl migr create` with the side's id as `--src-side`, and
`--dn-black` naming the slow disk node; watch the copy with
`dnvctl sp inspect-side`; then `dnvctl migr finish`, as
[Move a leg to another disk node](#move-a-leg-to-another-disk-node) says.
When the disk node's disk is the cause, move every side it
holds the same way, and then fix or replace the disk. A pool whose data
block is above 4 MiB refuses every migration: move that pool's volumes
instead, as [a pool made before the block bound](#a-pool-made-before-the-block-bound)
says.

When to stop: the counters do not read, and the entry shows a
`read_error` instead of `stat`; or every leg into one disk node is slow
and the disk's counters look normal, which points at the network between
the nodes rather than the disk. Ask for help before a migration in that
case: the new side may be just as slow.

Tested: a unit test of the cn agent checks that the inspect read carries the
probe times and the counters, and that no check round carries them. The cn
suite's slow leg stage, which delays one disk node's traffic with tc netem
and reads a higher write time on its legs while their rows stay ok, runs in
the suite's redund case and passes on the lab VMs.

## Prepare a node

Words: an agent's **store folder** is the folder its `--local-store` flag
names, `/var/lib/dnv` by default. The agent keeps its local files there:
the desired state it last accepted, which it builds again each time it
starts. The **kit** is what the release's `deploy/` folder holds for a
machine: the module lists of `deploy/modules-load.d/`, the udev rules of
`deploy/udev/`, the systemd units of `deploy/systemd/`, the examples of
settings in `deploy/dnv/` and the firewall example
`deploy/nftables/dnv-filter.nft`.

When: on every machine that will hold a disk node or a controller node,
before its first dnv agent starts. Run step 11 again after a kernel or
mdadm update on the machine, and step 6 when a new release changes a file
of `deploy/udev/`.

What dnv does alone: nothing. No agent loads a kernel module, installs a
udev rule, opens a port in a firewall or starts at boot, and the kit
enables no unit. The node check reads most of what these steps set, and an
agent starts whatever the check would report, save the case that step 3 of
[Check a node before it joins](#check-a-node-before-it-joins) names.

Safe steps, as root on the machine:

1. Give the machine a host name of its own before any of its nodes is
   created. A node's location is its machine's host name by default
   ([Choose a node's location](#choose-a-nodes-location)). Keep its clock
   in time with a time sync service, such as chrony or systemd-timesyncd,
   on every machine of the cluster, the control plane's too: the worker
   compares the times that the machines' clocks stamp, and a clock that is
   off or steps back can hold a failover ([the scope
   page](scope.md#8-waits-a-host-can-see)).
2. Install the tools the agents run ([the
   introduction](introduction.md#requirements)): nvme-cli, mdadm, dmsetup
   and util-linux on every node, and thin-provisioning-tools on a
   controller node.
   A disk node needs mdadm too, since the md rule of step 6 runs
   `/sbin/mdadm`. The node check's `commands` and `mdadm` lines name a tool
   that is missing.
3. Copy the program `dnv-agent` from the release folder to the folder that
   the `ExecStart=` line of its unit in `deploy/systemd/` names.
4. Load the kernel modules at every boot. Copy
   `deploy/modules-load.d/dnv-dn.conf` on a machine with disk nodes, and
   `deploy/modules-load.d/dnv-cn.conf` on a machine with a controller node,
   both on a machine with both, into `/etc/modules-load.d/`. Then load them
   now: `sudo systemctl restart systemd-modules-load.service`. A `modprobe`
   by hand, as in the tutorials, lasts only until the next boot. The node
   check's `kernel modules` line fails a module the kernel lacks, and its
   `kernel modules at boot` line warns for a module the agent needs loaded
   that no such file names.
5. Mount the kernel file systems the agents use: configfs at
   `/sys/kernel/config`, and on a controller node debugfs at
   `/sys/kernel/debug`, which the end of a host's connection needs, and
   securityfs at `/sys/kernel/security`, without which the node check and
   the cn agent cannot see a lockdown, a mode of the kernel that refuses
   that end ([Check a node before it joins](#check-a-node-before-it-joins),
   step 3). systemd mounts all three at boot on Ubuntu.
6. Install the udev rules, and set the lvm2 and md settings that go with
   them: [Install the udev rules on a node](#install-the-udev-rules-on-a-node).
7. Open the network the machine's roles need, and no more: [The
   networks](#the-networks) lists who connects to whom, and gives the
   firewall example `deploy/nftables/dnv-filter.nft`, to load with your own
   addresses and ports.
8. On a controller node, keep no NVMe-oF subsystem of your own that has no
   namespace that names a device, no host and no port: the agent removes
   it once it is 30 seconds old, and a namespace never given a device does
   not keep it. Make no NVMe-oF connection by hand with the host NQN that
   the agent uses: the agent removes it once no clone reads through it
   ([the scope
   page](scope.md#61-cases-where-dnv-does-not-come-back-alone-today), a
   controller node's sweep removes two things that may be an operator's).
9. Install the units of the machine's agents from `deploy/systemd/` into
   `/etc/systemd/system/`: `dnv-agent-dn@.service` for the disk node
   agents, one instance for each disk, such as `dnv-agent-dn@0`, and
   `dnv-agent-cn.service` for the controller node agent, at most one on a
   machine. Put each agent's settings where the first comment of its unit
   says, made from the examples in `deploy/dnv/`: among them its disk and
   its gRPC address, which differ for each agent of the machine, its
   NVMe/TCP address and port, and its nvmet port id. Name the disk by a
   link that does not move at a reboot, under `/dev/disk/by-id/`, and
   check it twice: at the node's first converge after step 13, the agent
   formats the disk unless its first 8 bytes read `DNVDISK1`, dnv's own
   header. It does not check whether the disk is mounted or used by
   anything but dnv, so a file system, an LVM volume or any other data on
   it is lost. A disk whose first 8 bytes read `DNVDISK1`, as
   `sudo head -c 8 <disk>; echo` prints, served an old node, and a new
   node on it gets nothing: first take steps 1 to 5 of [Reuse a disk from
   an old node](#reuse-a-disk-from-an-old-node). They remove the old node
   if it still exists, stop the agent that used the disk, check that no
   device of dnv maps it, zero the disk's first block, which holds that
   header, and delete that agent's disk node files. Then come back here.
   Steps 12 and 13 here take the place of its steps 6 and 7. Last, run
   `sudo systemctl daemon-reload`.
10. Give each agent a store folder of its own. Each unit of the kit names
    its agent's folder and makes it when the agent starts. An agent started
    in another way needs its folder made first, as with
    `sudo mkdir -p /var/lib/dnv`: an agent whose folder does not exist ends
    at its start. Two disk node agents never share a folder; a disk node
    agent and the controller node agent may. Keep nothing in a store folder
    but the agents' own files, and no folder of another agent inside it:
    an agent reads every name of its folder that starts with `dn-`,
    `side-`, `migr-bm-` or `probe-dn-` for a disk node agent, or with `cn-`,
    `cntlr-`, `clone-bm-`, `probe-cn-` or `torn-` for the controller node
    agent, as a file of its own. Never put an agent's local files back from
    an older copy, as from a backup or a snapshot of the system disk: a disk
    node's agent that starts on older files removes every side made since
    their time, with its data ([the scope
    page](scope.md#55-records-and-tools)). Delete them instead; the worker
    then sends the agent its desired state again.
11. Run the node check with each agent's settings ([Check a node before it
    joins](#check-a-node-before-it-joins)), and fix every `FAIL`.
12. Start the agents: `sudo systemctl start dnv-agent-dn@<name>` and
    `sudo systemctl start dnv-agent-cn`. Each agent logs the record
    `agent started` once its startup pass, its rebuild of its local files,
    has ended. `sudo systemctl enable` a unit to start it at boot too.
13. Create the nodes once their agents run:
    `dnvctl dn create --addr <ip:port> --tr-addr <ip> --tr-svc-id <port>`,
    and `dnvctl cn create` with the same flags. `--addr` is the agent's
    `--grpc-address`. `--tr-addr` and `--tr-svc-id` are where other
    machines reach the agent's NVMe-oF port: its own `--tr-addr` and
    `--tr-svc-id`, unless an address translation lies between. Leave out
    `--tr-type` and `--adr-fam` for NVMe/TCP over IPv4. A create asks the
    agent for the size of its disk or for its budget, the pool space a
    controller node may serve, and, with no `--location`, for its host
    name, so it fails while the agent does not answer. With `--disabled`,
    nothing is placed on the new node until you enable it ([Disable a
    node](#disable-a-node)). A disk node's create loses what its disk
    held: the agent formats the disk, as step 9 says. A disk that carries
    dnv's header already names an old node, and the new node gets nothing
    (step 9). Within a few check rounds of the create,
    `dnvctl dn inspect --addr <addr>` shows `meta_info` as
    `RES_STATUS_OK`.

The start order. On a machine that joins a running cluster, the order is
the order of the steps: the agent first, then its node's create. When every
program of a cluster starts, as after a power loss of the whole cluster or
an upgrade, the workers start last, once every agent has logged
`agent started`, and they stop first. To a running worker a stopped agent
is a failed node, and the worker would fail over primaries, replace cntlrs
and repair legs that only wait for their agent. The gateways and the cdc
start and stop in any order. It is the order of an upgrade ([Upgrade
dnv](#upgrade-dnv); [the scope page](scope.md), section 7, Upgrade). No
program checks the order. Units enabled at boot are safe only on one
machine. There systemd starts the worker's unit after the agents' units.
A worker that starts drives nothing for its join grace, the time it waits
after it registers in etcd before it takes a share of the work: its
`--vote-grace-time`, 60 seconds at the default. It also waits up to 120
seconds at the default, counted from the agent's start, for an agent that
answers that it is starting. Nothing orders the units of two machines.
After a power loss of the whole cluster, a worker whose join grace ends
before an agent of another machine answers sees that node as failed, as
above. To keep the order, leave the workers' units off at boot, and start
the workers by hand once every agent has logged `agent started`. A worker
left off at boot also stays stopped after a reboot of its own machine
alone: start it by hand then too. While no worker runs, no change reaches
the nodes and no reaction runs: nothing fails over, replaces a cntlr,
repairs a leg or grows a slice whose thin pool fills up. No old leg that a
repair left in its pool is deleted, and no md check, md's read of both legs
of each array, is asked for.

Steps that lose data: step 13, and, for a disk that carries dnv's header,
the steps of [Reuse a disk from an old
node](#reuse-a-disk-from-an-old-node) that step 9 sends you to. A disk
node's create makes its agent format the disk that its `--disk` names,
unless the disk carries dnv's header already, and whatever the disk held is
lost. The `dd` of that section's step 4 zeroes the disk's first block, and
then nothing of dnv reads the data of the old node's sides again. Its step
1 removes the old node, which can delete pools, each with its data ([Remove
a node](#remove-a-node)). No other step writes to a disk. Step 6's md
setting can keep an array of the machine's own from
coming up at the next boot, as that section says. A firewall rule that
refuses a connection dnv needs makes a node look dead to the worker, which
then fails its primaries over and repairs its legs. When a disk node is
cut from the controller node that holds a pool's primary, the IO to the
pool's groups on that disk node fails after about 13 seconds, as in
[Replace a dead disk node](#replace-a-dead-disk-node): in a pool without
RAID, and in a RAID1 pool for a group none of whose other legs md holds in
sync on another machine. In those groups, the IO to a leg that a
migration moves waits instead when its side on the cut disk node serves
it and the path to its other side stays up, unless another leg of the
group is in sync on that same machine, where the IO can fail too. In a
pool without RAID, and for a migration past its cutover whose copy the
worker does not see whole during the cut, it waits for as long as the cut
lasts. The copy goes on during the cut over the path between the
migration's two disk nodes, when that path stays up, so it can become
whole then. Otherwise, in a RAID1 group, it waits only until leg repair
ends the migration, after the pool's `leg_unhealthy`, 1200 seconds at the
default, or its `side_unhealthy`, 600 seconds, when the worker's checks
of that disk node fail too. That end keeps the side on the cut disk node,
and the IO then fails. md then marks the group's array broken, and the
worker fails the pool over where the pool has an enabled standby with no
health epoch whose agent does not refuse the worker's syncups; a pool
with no such standby, such as a pool of one cntlr, waits for the cntlr
replacement. In both kinds of pool, the worker can also
fail the pool over sooner, while the IO waits ([the scope page, section
8](scope.md#8-waits-a-host-can-see)).
The IO to such a leg also fails when the migration's two disk nodes are
cut from each other, from the cutover until the migration ends, for a
block the new side has not yet copied.
A thin pool can then stay read-only, or lose with no error the blocks it
mapped since its last commit, as the Steps that can lose data of [The
networks](#the-networks) say. Load new rules by the steps of that section,
on one machine at a time: on a disk node, their step 5 stops the IO of
such a pool before it starts their timer, since the timer takes a mistake
away only after minutes, and their step 7 tries each caller of each
refused port.

When to stop: the node check exits 2 as root, or a `FAIL` you cannot fix;
an agent that ends at its start; a node create that fails while its agent
runs; a disk node whose `meta_info` row still reads `RES_STATUS_ERROR` a
minute after its create. Ask for help.

Tested: the e2e suite runs the node check on every disk node and controller
node guest before it creates its pool, and the disk node, controller node,
e2e and csi suites install the udev rules of `deploy/udev/`; the e2e suite
passed in the lab with them on 2026-11-21. Every suite loads the modules
with `modprobe` and starts the programs by hand: no suite loads the modules
from the kit's files, starts an agent from the kit's units or reboots a
machine set up this way. A one-off check did, in the lab on 2026-11-22,
on one guest that held two disk nodes, a controller node and the control
plane, with the programs and the kit of commit 802231a7, but for one line
of the kit: the controller node unit's `KillMode=`, then `mixed`. It
installed the programs and the kit from an archive that `make release`
built, started every unit, made a pool and a volume, enabled the units and
rebooted the guest.
After the boot every unit ran, the node check passed for both roles with
its `kernel modules at boot` line ok, and the volume read back what was
written before the reboot. The Go tests of `agent/nodecheck/` and
`cmd/dnv-agent/` hold the kit's module lists, units and settings examples
to the code. The firewall's proof is in
[The networks](#the-networks).

## Install the udev rules on a node

Words: **udev** is the Linux service that acts when a block device
appears or changes. Its rules read the device and can, for example,
assemble an md array from it, scan it for LVM, or register it as a bcache
device. dnv ships three rule files in the folder `deploy/udev/` of the
release:

- `58-dnv-dm.rules` keeps the system's rules off every device mapper device
  of dnv, whose name starts with `dnv-`. Such a device holds what hosts
  wrote, and the node must never read that as a file system, an LVM volume,
  an md array or a cache. The rule reads the name's start alone, so the
  node must have no device mapper device of its own whose name starts with
  `dnv-`, such as a volume of an LVM volume group named `dnv` or `dnv-…`:
  such a device would lose its links by uuid and label. A device of dnv's
  keeps its name under `/dev/mapper`, which dnv uses. Some versions of
  systemd show a device of dnv's as not ready. That does no harm: dnv
  mounts nothing on it and waits for no systemd device unit.
- `63-dnv-md.rules` keeps the system's md assembly off the members of
  dnv's RAID1 arrays, so that only dnv assembles them. It reads the md
  superblock of version 1.2, the only one dnv writes, so a superblock of
  another version that a host wrote at the end of a device does not hide
  dnv's.
- `63-zz-dnv-md-array.rules` keeps the system's rules off what a host
  wrote at the start of one of dnv's RAID1 arrays: an md superblock, an
  LVM label, a bcache superblock, a file system or a partition table. It
  also takes away the links under `/dev/disk/by-uuid` and
  `/dev/disk/by-label` that the system makes from a host's file system on
  such an array, so that no such link leads there. A clone's source, which
  carries no dnv name, still gets these links (step 6).

Two settings go with the rules. They cover devices that carry no dnv name,
such as a leg's NVMe device before dnv writes its superblock there:
lvm2 must read only the node's own physical volumes, and md must assemble
by itself only the arrays that `/etc/mdadm/mdadm.conf` lists.

When: on every disk node and every controller node, before its first dnv
agent starts, and again when a new release changes or adds a file of
`deploy/udev/`, as the first release after 0.1 does on a node that ran 0.1,
which shipped none: then while the node's agents are stopped for the
upgrade, before the new ones start. The rules need systemd 253 or newer;
`systemctl --version` prints it. An older udev skips the lines of `63-zz-dnv-md-array.rules`
that take the links away, with an error in the system log, and keeps the
rest.

What dnv does alone: nothing. No agent installs or checks the rules or
the settings; the node check reads them
([Check a node before it joins](#check-a-node-before-it-joins)). It
counts the md rule only when its file is exactly the shipped one, and it
judges no rule of the node's own, even one that does the same work.

Safe steps, before the node's first dnv agent starts:

1. In the unpacked release folder, which holds `deploy/`, copy the rules
   and reload udev:

   ```
   sudo install -m 0644 deploy/udev/*.rules /etc/udev/rules.d/
   sudo udevadm control --reload
   ```

2. Check that udev takes the files, on systemd 254 or newer, the first
   whose udevadm has the `verify` command:

   ```
   sudo udevadm verify /etc/udev/rules.d/58-dnv-dm.rules /etc/udev/rules.d/63-dnv-md.rules /etc/udev/rules.d/63-zz-dnv-md-array.rules
   ```

   It must report every file as a success. On systemd 253 skip this step:
   the node check reads the files
   ([Check a node before it joins](#check-a-node-before-it-joins)).
3. lvm2. In the `devices` section of `/etc/lvm/lvm.conf`, set a global
   filter that accepts the node's own physical volumes and rejects every
   other device. For a node whose only physical volume is `/dev/vda3`:
   `global_filter = [ "a|^/dev/vda3$|", "r|.*|" ]`. For a node with none:
   `global_filter = [ "r|.*|" ]`. lvm2's devices file, which lists the
   devices LVM may use, does the same job. Then `sudo pvs` must list the
   node's own physical volumes and nothing else.
4. md. `sudo mdadm --detail --scan` prints one ARRAY line for each md
   array that runs on the node; before the node's first dnv agent starts,
   each one is the node's own. On a controller node that ran dnv before,
   as at an upgrade from 0.1, it also prints the arrays dnv runs, each
   named `/dev/md/` and 28 hex digits: leave those lines out, since only
   dnv may assemble them.
   Add the node's own lines to `/etc/mdadm/mdadm.conf`,
   and add the line `AUTO -all` after them. With it md assembles by itself
   only the arrays the file lists. An array of the node's own that the
   file does not list no longer comes up at boot. mdadm reads only the
   first AUTO line, so if the file has one already, make that line
   `AUTO -all`. Keep no POLICY line that sets `auto=`, and give every
   ARRAY line its `UUID=`. The node check does not judge a POLICY line
   that sets `auto=`, or an ARRAY line that names its array by its devices
   (`devices=`) or minor number (`super-minor=`) and not by its UUID, and
   does not count a file that has one. The lines `mdadm --detail --scan`
   prints carry the UUID.
5. Write the new settings into the initial RAM disk, which also holds a
   copy of them: `sudo update-initramfs -u` on Debian and Ubuntu.
6. On a controller node, mount the node's own file systems by device
   path, such as `/dev/vda1`, or by entries in `/etc/fstab` that name no
   label and no uuid a tenant could copy. The system makes links by label
   and uuid from a host's file system on a clone's source, which no rule of
   dnv's reaches ([the scope page](scope.md), section 6.1), and a host
   volume made from the same cloud image as the node carries the node's own
   labels and uuids. Check the file with `sudo findmnt --verify` before the
   next boot.

To remove the rules from a node that runs no dnv agent any more: delete
the three files from `/etc/udev/rules.d/` and run
`sudo udevadm control --reload`. A device mapper device made while the
rules were in place keeps their marks until it is removed, because the
device mapper's own rules carry them over from udev's record of the
device.

Steps that can lose data: none of the steps writes to a device. Step 4
can keep an array of the node's own from coming up at the next boot if
its ARRAY line is missing; add the line and run step 5 again. Step 6 can
keep the node from booting if an entry names a wrong device; that is what
`findmnt --verify` is for.

Stop and ask for help when `udevadm verify` refuses a file, or when
`sudo pvs` lists anything but the node's own physical volumes.

Tested: the disk node, controller node, e2e and csi suites install the
rules from `deploy/udev/` the same way for their runs. The controller node
suite plants a host's md superblock, LVM volume, bcache superblock and file
system at the start of a RAID1 array and an md superblock of version 1.0 at the end
of its legs, and checks that the system made nothing of them on either
controller node, through a failover. No test checks the lvm2, md and
mount settings of steps 3, 4 and 6.

## Check a node before it joins

Words: the **node check** is `dnv-agent check dn` or `dnv-agent check cn`,
a run of the agent program that reads the node and reports, one line each,
what the agent needs to work and what dnv's safety rests on. It starts no
agent and changes nothing on the node. The **md replay** is the check's
`--md-replay` mode on a controller node: it runs md's start rules on two
small loop devices with the node's mdadm and kernel.

When: before you add a disk node (`dnvctl dn create`) or a controller node
(`dnvctl cn create`), after a kernel or mdadm update on a node, and after you
change its udev rules, mdadm.conf or modules.

Safe steps:

1. Install the agent program on the node, then run, as root, with the
   flags the agent will run with (the same `--config` file serves both):
   `dnv-agent check dn --disk <disk>` on a disk node, and `dnv-agent check
   cn` on a controller node. Add `--output json` for a script.
2. Read the report. Each line is `ok`, `WARN` or `FAIL`, what it read, and
   for a `WARN` or a `FAIL` a `fix:` line under it. The last line counts
   the failures and the warnings. The exit status is 0 with no failure, 1
   with one, and 2 when the check could not run, as when not root.
3. Fix every `FAIL` before the node joins: the agent cannot work there, or a
   failover or a repair can lose data there. On a controller node the
   `nvmet host cut (debugfs)` line, on whether the node can end the
   connection of a host taken off a subsystem's list, fails where the cn
   agent would refuse to start; its text names each cause: the NVMe target
   not loaded or configfs not mounted, debugfs not mounted at
   /sys/kernel/debug, no debug folder of the NVMe target there, which needs
   a kernel of 6.11 or newer built with the kernel option
   `CONFIG_NVME_TARGET_DEBUGFS`, a lockdown in force, the mode of the kernel
   that Secure Boot switches on, or a read of one of these facts that
   failed, whose text names the fact and then `not read:`, which fails the
   line too. The check looks for the debug folder only once the NVMe target
   is loaded and debugfs is mounted, so a check that names either of those
   can name the missing folder when it runs again. Under a lockdown on a
   node where /sys/kernel/security is not mounted the line reads `ok`, since
   neither the check nor the cn agent can see that lockdown: its text then
   says that it found no lockdown file, and the kernel refuses to end a
   connection all the same ([the scope
   page](scope.md#61-cases-where-dnv-does-not-come-back-alone-today)). When
   the text says that it found no lockdown file, mount the kernel's security
   file system there, `mount -t securityfs securityfs /sys/kernel/security`,
   and run the check again: a lockdown in force then fails the line, and the
   cn agent sees it too. Keep it mounted, as with a line in /etc/fstab. A
   text that still says it found no lockdown file is a kernel with no
   lockdown. To fix a `FAIL` of this line, mend each cause its text names,
   or choose to run without the end of such connections: give both the check
   and the cn agent `--ignore-host-disconnection`. A host taken off a list
   then keeps its connection and its IO until it disconnects, so two hosts
   can write one volume ([the scope
   page](scope.md#54-hosts-and-the-kubernetes-driver)). A `WARN` is one of
   four kinds.
   - Node settings to fix. The `lvm2 scan` and `mdadm.conf AUTO -all`
     warnings are the node settings of
     [Install the udev rules on a node](#install-the-udev-rules-on-a-node)
     (steps 3 to 5). The `kernel modules at boot` warning names modules a
     reboot would not load again; list them in a file under
     `/etc/modules-load.d`, since no agent loads a module. The
     `md format (the 6.19 line)` warning says a controller node's kernel
     is older than 6.19: every controller node of the cluster must run one
     kernel series, since a RAID1 array created on 6.19 or newer is
     refused by an older kernel, so a failover to the older node cannot
     start the pool. The `nvme_core.io_timeout` warning on a disk node says
     it is at its largest value, 4294967295, so a migration's write to a
     stalled disk never times out; set it to 30. The check warns for these
     rather than fails, and they must be fixed all the same.
   - A known limit. The `disk best IO size` warning says the disk's best or
     minimum IO size is above 4 MiB, so a controller node's write to the
     disk can be refused ([the scope
     page](scope.md#10-sizes-and-performance)). Use another disk, or give it
     a smaller stripe, or accept the limit. The `nvmet host cut (debugfs)`
     warning, given only to a check with `--ignore-host-disconnection`,
     names the causes of step 3 that the check found, and says that the cn
     agent with that flag ends no connection of a host taken off a
     subsystem's list. Mend each cause its text names, or accept the limit.
   - Facts outside what dnv was tested with: a kernel outside the 7.0
     series, an mdadm other than 4.5 on a controller node, a udev rule of
     dnv's with another text, and `nvme_core.io_timeout` other than 30
     seconds on a controller node. Fix them, or accept them knowing that
     nothing tested them.
   - A fact the check could not read: the line's text starts `not read`.
     Read the fact by hand, or do first what the `fix:` line says, and run
     the check again.
4. For a new kernel or mdadm on controller nodes, run the md replay on a
   spare machine with that kernel and mdadm, never on a node in service.
   First install the udev rules on the machine
   ([Install the udev rules on a node](#install-the-udev-rules-on-a-node),
   steps 1 and 2): the replay does not start while the check finds no mask
   of the system's md assembly. Then run `dnv-agent check cn --md-replay`.
   It refuses to start on a node that holds any dnv device, array,
   subsystem or file of an agent's local store, the folder where an agent
   keeps the desired state it last accepted. An `md replay` line that reads
   `not started` names a reason on the machine, not a fault of mdadm or the
   kernel. Each of its steps is one more line of the report, named
   `md replay: <step>`. A failed step other than `build`, in a run you did
   not stop with Ctrl-C, means that mdadm or the kernel does not keep a
   rule dnv's failover rests on, and that kernel or mdadm must not run on a
   controller node. A failed `build` names a fault of the machine, such as
   no free loop device or a full `/var/tmp`, whatever its `fix:` line says.
   The replay removes what it made, also after Ctrl-C, and names anything
   it could not remove.

What dnv does alone: a cn agent without `--ignore-host-disconnection`
refuses to start where the check without that flag fails on its
`nvmet host cut (debugfs)` line, and its error names the same causes and
the flag. With the flag on such a node it starts and logs one warning,
`agent host cut disabled`, with the causes. Whatever else the check would
report, an agent starts. It logs the version of the program, the kernel
release and, on a controller node, the mdadm version once when it starts,
as the record `agent node versions`.

When to stop: the check exits 2 as root, or the md replay names something
it could not remove. Ask for help; remove what the replay names by hand
with `mdadm --stop`, `losetup -d` and `rm -rf`.

Tested: unit tests check each line of the report at each level on modelled
nodes, the exit statuses, and the md replay against a stand-in mdadm,
including one that starts a lone leg. Unit tests also check the cn agent's
refusal to start and its warning with the flag; no suite runs a cn agent
with the flag. The e2e suite runs the check, without
`--ignore-host-disconnection`, on every disk node and controller node guest
before it creates its pool, and fails the run on a failure; it passed in the
lab on 2026-11-20. The cn suite's nodecheck case runs the check, the md
replay, and the md replay with an mdadm that starts a lone leg on the lab
VMs, and it passes there.

## Choose a node's location

A node's location is the failure domain dnv keeps the legs of a group
apart by. When you create a node with no `--location`, the gateway asks
the node's agent for the host name of its machine, the name in
`/proc/sys/kernel/hostname`, and stores it as the node's location. So the
disk nodes of one machine share one location, and when every machine has
a host name of its own, a new group never has two legs on one machine. A
new pool also keeps its legs out of the location of its first primary
while another location has room, so that one machine does not hold the
primary and a leg of a group. A node keeps its location for its whole
life: no command changes it. A node that the first release, 0.1, created
with no location keeps its address as its location after an upgrade, so
it is a location of its own and not its machine's.

When: before you create the nodes of a cluster, after an upgrade from
0.1, and when a node create or a pool create is refused for want of a
location.

Symptoms:

- `dnvctl dn create` or `dnvctl cn create` with no `--location` fails with
  FAILED_PRECONDITION, and the text says the agent "reports no host name"
  or "reports a host name that is no valid location". A location is at most
  64 bytes of letters, digits and `-_/.:`. The kernel's `(none)`, the name
  of a machine never named, is not one. An agent of an older build than
  the gateway reports no host name at all.
- `dnvctl sp create` of a RAID1 pool fails with RESOURCE_EXHAUSTED, and
  the text says "disk nodes with ... free extents in distinct locations,
  need 2": the disk nodes with room are in fewer locations than a group
  has legs, for example all on one machine, or too few for every leg of
  the pool to have a disk node of its own ([the scope page](scope.md),
  section 10, A RAID1 pool needs four disk nodes per slice at create).

How to confirm: `dnvctl dn get --addr <addr>` and `dnvctl cn get --addr
<addr>` show each node's `location`. `cat /proc/sys/kernel/hostname` on
the machine shows the name its agents report; compare it across the
machines, since two machines with one name are one location. A location
equal to the node's own address is one that 0.1 gave a node created with
no location. For the second symptom, count the disk nodes that have free
extents and their locations.

What dnv does alone: it reads the host name at each create that names no
location, and nothing more. A location stored once never follows a later
rename of the machine. A repair, a migration, a grow, a failover and a
cntlr replacement do not look at the primary's location, and when too few
locations have room, a repair or a migration can put two legs of a group
in one location ([the scope page](scope.md#4-data-loss-dnv-accepts) and
[section 6.1](scope.md#61-cases-where-dnv-does-not-come-back-alone-today)).

Safe steps:

1. Give every machine a host name of its own before you create its nodes,
   with `hostnamectl set-hostname <name>`, or pass `--location` with one
   name per machine on each of its nodes. Machines made from one disk
   image often share one name: rename them before their first node
   create. Two machines with one name are one location to dnv. A new group
   still never has two legs in it, but a repair or a migration then finds
   no other location sooner and can put the new leg on the machine that
   holds the group's other leg, and a new pool can put a leg on its
   primary's machine. Set the name once: a machine renamed between two
   creates puts its nodes in two locations, and a group can then get two
   legs on that machine.
2. To keep the legs of a group in different racks, not only on different
   machines, pass `--location <rack>` on every node of every machine in
   the rack: on its disk nodes and on its controller node alike. A rack
   name on the controller node alone hides the machine, and a new pool
   then keeps its legs out of the rack of its primary, not out of its
   primary's machine.
3. Pass `--location` when the host name does not name the machine: an
   agent in a container with a host name of its own reports the
   container's name.
4. On one machine, as in [dnv on one machine](tutorial_all_in_one.md), a
   RAID1 pool needs its disk nodes in different locations: pass
   `--location dn0`, `--location dn1` and so on, one for each disk node.
   The two copies of each group are then on two disks of one machine, not
   on two machines.

Steps that lose data: none. A node delete is refused while the node holds
a side or a cntlr, and no step here deletes anything else.

When to stop: a node already holds sides or cntlrs and has the wrong
location: nodes of two machines that share one host name, nodes of one
machine created before and after a rename of it, or a node that 0.1
created with no location on a machine that runs other nodes. No command
changes a location; the node must be emptied, deleted and created again.
[Remove a node](#remove-a-node) gives the first two steps, and [Reuse a
disk from an old node](#reuse-a-disk-from-an-old-node) the use of a disk
node's disk again. Ask for help.

Tested: the gateway's Go tests check the host name as the default, the
refusals of a name that is no location, a RAID1 pool refused on one host
name, and a new pool's legs kept out of its first primary's location,
with the placement that ignores that location tried once more when it is
refused. The gateway suite's smoke case checks the legs kept out of the
primary's location over its fake nodes, and the dn and cn suites check the
host name each agent reports; those three checks passed in the lab on
2026-11-19. The e2e suite's loss of a primary's machine, a reset of its
controller node that fails the pool over and replaces the lost cntlr,
passed in the lab at one slice on 2026-11-19; it tests neither the
location default, as every node there is created with a location named,
nor a new pool keeping its legs out of its primary's location, as no
controller node there shares a location with a disk node.

## The networks

Words: the **host network** is the network on which the hosts reach dnv.
The **back network** is every other network dnv uses. It can be split in
two: the **leg network**, on which the controller nodes reach the legs on
the disk nodes and a disk node reaches another for a migration, and the
**control network**, on which `dnvctl`, the gateway, the worker, the cdc,
etcd and the agents talk. An agent's **control port** is the port of its
`--grpc-address`, where the gateway and the worker call it. nftables is
the Linux firewall, and `nft` is its command. The CSI driver is dnv's
Kubernetes driver; its **node plugin**, its **controller plugin** with its
two **copies**, and a **pod** are as in [A Kubernetes machine is
lost](#a-kubernetes-machine-is-lost).

When: before the first node of a cluster joins, and when you add a
machine, a host or a program, or change an address or a port.

The rule: hosts reach only the controller nodes' NVMe/TCP ports and the
cdc. Every other port of dnv sits on the back network, which no host can
reach or route to. dnv checks no caller of the gateway, the agents and
etcd, and no host beyond its NQN, so the network alone keeps hosts away
from them. A machine that reaches the gateway can change or delete any
pool, and one that reaches an agent's control port can make the agent
remove what its node holds. A controller node's agent takes from each call
the id of the controller node it acts as, so through its control port a
machine can also make it connect to the legs of any pool as that pool's
primary. A machine that reaches a disk node's NVMe/TCP port and claims the
NQN of the controller node that holds a pool's primary can read and write
that pool's raw legs there, the old bytes of other pools included. The NQN
of a controller node that holds a standby of the pool gets in too, but its
reads and writes fail until a failover makes that standby the primary.
From a migration's cutover until the migration ends, a machine that claims
the NQN of the destination disk node can also read and write the whole old
side at the source disk node's port.

Who connects to whom:

| Who connects | To | The port, set by these flags of the listener | Where the caller reads the address | Network |
|---|---|---|---|---|
| A host, and the CSI node plugin on a Kubernetes node | the cdc | the cdc's `--tr-addr` and `--tr-svc-id`, 8009 by default | the host's own settings, such as those of nvme-stas, the host's discovery service; the driver's `--cdc-addresses` | host |
| A host, and the CSI node plugin | a controller node's NVMe/TCP port | the cn agent's `--tr-addr` and `--tr-svc-id` | the cdc's answer, which holds the address given to `dnvctl cn create --tr-addr --tr-svc-id` | host |
| The primary of a pool that clones a volume | the source: another pool's controller node, over the transfer it exports there, or any NVMe/TCP target | that controller node's NVMe/TCP port, or the target's | the clone's source address, given to `dnvctl clone create --src-tr-addr --src-tr-svc-id` and changed by `dnvctl clone set-tr` | host, or wherever the target is |
| A controller node, for each cntlr, primary and standby | a disk node's NVMe/TCP port, for the legs | the dn agent's `--tr-addr` and `--tr-svc-id` | the address given to `dnvctl dn create --tr-addr --tr-svc-id` | leg |
| A disk node, the new side of a migration | the disk node of the old side, its NVMe/TCP port | the same | the same | leg |
| `dnvctl`, and the CSI controller plugin | the gateway | the gateway's `--grpc-address` | their `--gateway-address`; for the CSI driver, the key `gatewayAddress` of `deploy/config.yaml` | control |
| The gateway and the worker | each agent's control port | the agent's `--grpc-address` | the node's address, given to `dnvctl dn create --addr` or `dnvctl cn create --addr` | control |
| The gateway, the worker and the cdc | etcd | etcd's client address | their `--etcd-endpoints` | control |
| Each etcd server | the other etcd servers | etcd's peer address | etcd's own settings | control |

What the table means:

- The addresses that nodes and hosts dial are stored in the node records
  once, at `dnvctl dn create` and `dnvctl cn create`, and no command
  changes them. A node's `--addr` is also its name. To move a node to
  another address, empty it, delete it and create it again ([Remove a
  node](#remove-a-node)). `--tr-addr` and `--tr-svc-id` of those two
  commands default to 127.0.0.1 and 4420, which fit one machine alone:
  give both on every create. A clone's source is the one address a
  command changes.
- A record's address may differ from the agent's own flags, as when a
  program that forwards the traffic sits between. dnv does not check that
  the two agree.
- No dnv program but the cdc has a default port. The worker and `dnvctl`
  listen on nothing.
- A controller node exports to its hosts and to the clones of other pools
  from one NVMe/TCP port. So a transfer between two pools crosses the host
  network: the controller nodes of the cloning pool, any of which may hold
  its primary, must reach the NVMe/TCP ports of the source pool's
  controller nodes.
- A machine that runs the cdc is on the host network, where the hosts
  call the cdc, and on the control network too, where the cdc calls etcd,
  even when it runs nothing else. One that runs the gateway or etcd is on
  the control network, and on the host network too when it also runs the
  cdc.
- No dnv program chooses the source address of a connection it makes: the
  machine's routing table picks the network. On a machine on two networks,
  route each network through its own interface, or a leg's or a call's
  traffic can leave over the host network.

The risks a firewall does not take away:

- Anyone on the host network can pose as a host. A host is known by its
  NQN alone, and dnv checks no key. A machine that claims the NQN of a
  host, once it learns it, reaches that host's volumes on the controller
  nodes, and gets that host's answer from the cdc: the names of its
  subsystems and the addresses of their controller nodes. The same holds
  for a transfer, which a controller node exports on the same port: a
  machine that claims an NQN the transfer lists can write the source
  while a clone copies it ([the scope page](scope.md#3-the-promise-one-pool-never-reads-another-pools-bytes)).
  The promise does not cover that, so keeping tenants apart also needs
  hosts on which no tenant has root.
- All traffic is plain text. Whoever can read a network reads what
  crosses it: the volumes' data on the host and leg networks, and every
  desired state on the control network.
- A failure of the host network at the primary's controller node alone
  starts no failover: the worker reads the agents over the control
  network, never a host. See [A fault only a host
  sees](#a-fault-only-a-host-sees).
- A machine that runs a dn agent and a cn agent with the default nvmet
  port id, the configfs id of the agent's NVMe/TCP port, shares one
  NVMe/TCP port between the two at one address. The disk node's legs are
  then exported on the port the hosts reach, and no rule by port can keep
  hosts off them. Give each agent its own `--nvmet-port-id` and its own
  `--tr-svc-id` or `--tr-addr`, as [dnv on one
  machine](tutorial_all_in_one.md) does.
- A program bound to every address of its machine, as with an address of
  `0.0.0.0` or none before the port, answers on each of them, IPv6 and a
  Kubernetes node's pod network addresses among them. A pod on another
  Kubernetes node reaches those with its own pod address, which a rule on
  the hosts' addresses does not catch. Bind each program to one address,
  on the network the table names.

How to confirm: step 7 below. From a host, a refused port times out, and
the table's counter shows the drops.

What dnv does alone: it sets no firewall rule and checks no address. A
node create is refused when the gateway cannot reach the agent. While a
table drops the gateway's packets, `dnvctl dn create` and `dnvctl cn
create` fail after about 10 seconds with error code DEADLINE_EXCEEDED:
`dnvctl` waits as long as its `--timeout`, 10 seconds at the default,
and that wait ends before the gateway's own. When the agent's machine
refuses the connection, or with a `--timeout` above 10, they fail with
error code ABORTED and "get dn size from" or "get cn size from" in the
text. The gateway's call of an agent waits up to 10 seconds. A controller
node's connection to a leg's address waits 1.5 seconds, and then that try
of the leg counts as failed; the leg is tried again at a later check
round.

Safe steps, as root on every machine that runs a dnv agent, the gateway,
the cdc or etcd, and on every other machine with one interface on the host
network and one on the back network, such as a Kubernetes node that runs
a copy of the controller plugin. Take steps 5 to 8 on one machine at a
time, and end step 8 on one machine before step 5 on the next, so that a
mistake in the sets, such as a range that holds the addresses of dnv
machines, cuts one machine and not every node at once:

1. Give each listener one address on its network, from the table: the
   gateway's `--grpc-address`, each agent's `--grpc-address`, `--tr-addr`
   and `--tr-svc-id`, the cdc's `--tr-addr` and `--tr-svc-id`, and
   etcd's client and peer addresses.
2. Copy `deploy/nftables/dnv-filter.nft`, the example firewall table, and
   put the hosts' addresses in its sets `hosts4` and `hosts6`. List them
   one by one, or a range that only hosts use. A range that also holds an
   address from which another dnv machine or an etcd server connects, as
   the one subnet of a network that hosts and dnv share does, drops that
   machine's calls too. The rule by address refuses only the machines the
   sets name, so a machine left out of them passes it. Add each new host
   to them. On Kubernetes, where a pod's connection to another machine
   keeps its pod address, add the pod network's range too, but not on the
   machine that runs the gateway: the CSI controller plugin calls the
   gateway from its pod, so there the range drops its calls, and the
   network policy alone keeps the tenants' pods off that machine (see the
   part on Kubernetes below). Where a program listens on IPv6, add each
   host's IPv6 addresses, its link-local one, which starts with fe80,
   among them. A host whose `ip -6 address` shows an address marked
   `temporary` may connect from an address that changes from time to
   time, which no list of single addresses holds: put its whole prefix in
   the set instead, such as 2001:db8:100::/64, when only hosts use that
   prefix.
3. Put in the set `refused` this machine's ports that hosts must not
   reach: on a disk node, each dn agent's control port and NVMe/TCP port;
   on a controller node, the cn agent's control port; on a control-plane
   server, the gateway's port and etcd's client and peer ports. Never put
   a controller node's NVMe/TCP port or the cdc's port there. A machine
   with none of these ports, such as one that runs only the cdc, or a
   Kubernetes node that runs a copy of the controller plugin but no agent,
   gateway or etcd, keeps the set `refused` with no `elements` part. Such
   a machine is on two networks, so it still needs step 4.
4. On a machine on two networks, such as a controller node, a machine that
   runs the cdc, or a control-plane server with one interface on the host
   network, also take the `#` away from the lines at the end of the file,
   with the names of the two interfaces. Linux takes a packet sent to any
   of its addresses on any of its interfaces, so these lines refuse the
   same ports to every packet that comes in on the host network's
   interface, and forward nothing from the host network to the back
   network. Every machine on two networks needs the chain `forward` of
   those lines, even one with no port in `refused`: a machine on which
   `sysctl -n net.ipv4.ip_forward` prints 1, as on each Kubernetes node of
   the lab, otherwise carries to the back network the packets of a host
   that routes through it. A machine that must reach those ports, such as
   one that calls the gateway, then reaches them over the back network.
5. Read each pool's `cntlr_unhealthy`, `side_unhealthy` and
   `leg_unhealthy`, the waits after which the worker replaces a cntlr, or
   repairs a leg, behind a cut: 600, 600 and 1200 seconds at the default.
   `dnvctl --sp '<pool name>' sp get` shows them in
   `sp_conf.event_threshold`, where null or 0 means the default. When one
   of them is 60 seconds or less, stop here and ask for help, as When to
   stop says. On a disk node, then stop the IO of each pool that a cut
   there can fail, unless that failure is acceptable, as Steps that can
   lose data says: a pool without RAID with a side there, and a RAID1
   pool with a group whose leg there has no other leg in sync on another
   machine.
   Start that IO again only after step 8 on that machine. Then start a
   timer that takes the table away again, so that a mistake that
   cuts you off, or cuts one node from another, ends by itself before the
   worker replaces a cntlr or a leg behind the cut (Steps that can lose
   data):
   `systemd-run --collect --unit=dnv-filter-undo --on-active=5min --timer-property=AccuracySec=1s /usr/sbin/nft delete table inet dnv_filter`.
   A systemd timer runs up to its `AccuracySec` after its time, one
   minute by default, and `AccuracySec=1s` makes it run within a second
   of it. Keep its time at least 30 seconds shorter than each pool's
   `cntlr_unhealthy`, `side_unhealthy` and `leg_unhealthy`. A health
   epoch stands until a check round finds the path working again. After
   the table goes, that can take about 20 seconds. The worker connects to
   an agent again only at its next try, up to about 17 seconds later. A
   controller node connects to a leg again only at its next try, up to
   about 12 seconds later. It then finds the leg working at its next test
   write to it, which it makes every 5 seconds, and the worker reads that
   at its next check round. The timer adds up
   to a second, and the worker counts whole seconds. When the cluster's
   `cntlr_interval` or `side_interval`, which `dnvctl cluster get` shows
   under `cluster_conf.health_check_conf`, is above 2 seconds, keep the
   timer shorter by the larger of the two too. No run has measured these
   times. When the smallest threshold is under two minutes, make the tries
   of step 7 before the timer ends, and make the read of step 7 at half
   the timer's time after the load, not a minute after it. With
   `--collect`, systemd forgets the timer and the service it starts once
   they have run, even when the delete failed, so this step can be run
   again later.
6. From the machine that runs `dnvctl`, run `dnvctl cluster get-status`
   and keep what it prints ([Read what is wrong](#read-what-is-wrong)).
   Then load the table, but only while the timer still waits:
   `systemctl is-active dnv-filter-undo.timer && nft -f dnv-filter.nft`.
   It prints `active` and loads the table. If it prints `inactive`, the
   timer has run already and nothing was loaded: start again at step 5.
   A second load replaces the table, and no other table changes. Never
   run `nft flush ruleset`: it also removes the tables of other programs,
   such as Kubernetes and docker.
7. From a host, try a refused port where something listens:
   `timeout 3 bash -c '</dev/tcp/<address>/<port>'; echo $?` prints 124,
   since the table drops the packets, and the same try from a machine of
   the back network prints 0. Make that try of each refused port of this
   machine from every machine that the table above names as its caller:
   etcd's client port from each machine that runs the gateway, the worker
   or a cdc, its peer port from every other etcd server, an agent's
   control port from each machine that runs the gateway or the worker, a
   disk node's NVMe/TCP port from every controller node and every other
   disk node, and the gateway's port from the machine that runs `dnvctl`
   and from each machine that may run a copy of the controller plugin.
   Each must print 0, or 1 at once where nothing listens on the port yet,
   as on a disk node's NVMe/TCP port before a pool has a leg there: only
   124 shows that the table drops that caller. From a host, the same try
   of the cdc's port prints 0, and so does that of a controller node's
   NVMe/TCP port once a subsystem of a pool with a cntlr there exists: an
   NVMe/TCP port listens only while a subsystem or a transfer uses it.
   `nft list table inet dnv_filter` shows the counter of the drops. A
   minute after the load, run `dnvctl cluster get-status` again: it must
   list nothing that it did not list before the load. The read shows a
   cut only on a path that dnv uses at that time, and it never shows a cdc
   that cannot reach etcd, so it does not take the place of the callers'
   tries. On a machine on two networks, `nft list chain inet dnv_filter
   forward` must show the drop line with the host network's interface
   after `iifname` and the back network's after `oifname`, as `ip
   address` names them. On a machine whose set `refused` is empty, that
   is the only check of its table, as it has no refused port to try.
8. Stop the timer once the cluster works: `systemctl stop
   dnv-filter-undo.timer`. If that prints `Unit dnv-filter-undo.timer not
   loaded`, the timer has run and taken the table away: start again at
   step 5. Then `nft list table inet dnv_filter` must print the table,
   or start again at step 5. The table does not survive a reboot. To
   load it at boot, run `nft -f` on the file from a unit of your own,
   before the dnv programs start. Do not use the nftables package's own unit on a
   machine where Kubernetes or docker keep tables: on Ubuntu its stop
   empties the whole rule set, and so does the file its start and its
   reload load, as the package ships it.

On Kubernetes, the hosts are the Kubernetes nodes that run the tenants'
pods. The CSI node plugin runs on each of them, on the machine's own
network, and reaches the cdc and the controller nodes as any host does. On
k3s with its default network software, flannel, as in the lab, a pod's
connection to another machine leaves its node with the node's own address,
so a firewall on a dnv machine cannot tell a tenant's pod from the driver:
keep the tenants' pods off dnv with the example network policy, a
Kubernetes record that limits where the pods of one Kubernetes namespace
may connect ([Keep other pods off
dnv](tutorial_csi.md#keep-other-pods-off-dnv)). The CSI controller plugin
calls the gateway from the Kubernetes node its pod runs on, so the
gateway's port must be open to every node that may run one of its two
copies. Run those copies only on machines where no tenant has root. Give
each of those machines an address on the control network, and let it
reach the gateway there, as the machine that runs `dnvctl` does: on a
control-plane server on two networks, the lines of step 4 drop a call to
the gateway that comes in on the host network's interface. A call from a
copy on another machine then comes from that machine's address on the
control network, so keep that address out of the sets `hosts4` and
`hosts6`, even on a machine that is also a host. Each machine that may
run a copy is then on two networks, so take the safe steps on it too, for
the chain `forward` of step 4. Where a pod's connection to another
machine keeps its pod address, the call comes from the copy's pod address
instead: that is why step 2 keeps the pod network's range out of the sets
on the gateway's machine. To keep the copies on those
machines, give two or more of them a label, a key and a value that
Kubernetes keeps on a node: `kubectl label node <node> <key>=<value>`.
Then add a `nodeSelector`, the field of a pod's settings that names the
labels a node must carry to run the pod, with that key and value to
`deploy/controller.yaml`, under `spec.template.spec`.

Steps that can lose data: a rule that refuses a path the table needs
makes the fault of a cut network, and the table drops packets, so such a
mistake shows as a stall, not as an error. A controller node cut from the
control plane has its primaries failed over, and a host still on the old
path can lose a write it was told was done, in the cases the scope page
lists. After the pool's `cntlr_unhealthy`, 600 seconds at the default,
the worker also replaces each cntlr on that controller node. For a pool
with no standby to fail over to, such as a pool of one cntlr, it then
replaces the primary while the old primary still serves the hosts, and a
write a host was told was done can be lost. A controller node cut from a
disk node has md fail its legs there, and leg repair moves them after the
pool's `leg_unhealthy`, 1200 seconds at the default, or after its
`side_unhealthy`, 600 seconds, when the worker cannot reach that disk
node either ([the scope page](scope.md#6-the-system-comes-back-alone)).
So start the timer of step 5 first, keep it at least 30 seconds shorter
than those times, as step 5 says, and check at step 7 before it ends. In
a pool without RAID a group has one leg, so when a disk node is cut from
the controller node that holds the pool's primary, the IO to the pool's
groups on that disk node fails after about 13 seconds, as in [Replace a
dead disk node](#replace-a-dead-disk-node), long before the timer ends:
the hosts' reads and writes that reach those groups fail, and a write that
failed may or may not be on the volume. When
such a group is a data
group, a commit of the slice's thin pool under writes then leaves the thin
pool read-only, and it stays read-only after the table goes, until you
take the steps of [Repair a thin pool's
metadata](#repair-a-thin-pools-metadata). When it is the slice's meta
group, a commit under writes then fails the thin pool, and the blocks it
mapped since its last commit can be lost with no error ([the scope
page](scope.md#4-data-loss-dnv-accepts)). The pool's IO then stays failed,
even after the table goes, until the worker fails the primary over to a
standby, which it does after the pool's `primary_unhealthy`, 4 seconds at
the default, often while the table still stands. A pool with no standby
to fail over to, such as a pool of one cntlr, keeps its IO failed until
the cntlr replacement, after the pool's `cntlr_unhealthy`. The timer of
step 5 does not keep that replacement away. A RAID1 group fails the same
way when md holds in sync no leg of it on another machine, as while its
other leg is dead, while md copies onto the new leg of a repair, or while
a leg is kept out of its array. Under IO, md then marks the array broken.
The primary does not
assemble it again while the thin pool holds it open, so the group stays
down after the table goes, until that failover or cntlr replacement
([the scope page, section
6.1](scope.md#61-cases-where-dnv-does-not-come-back-alone-today)).
In a pool without RAID, and in a RAID1 group in which md holds no other
leg in sync, the IO to a leg that a migration moves waits instead when
its side on the cut disk node serves it and the path to its other side
stays up. It waits for as long as the cut lasts, unless the worker fails
the pool over meanwhile: leg repair, whose end of the migration fails
that IO in a RAID1 group, as in [Prepare a node](#prepare-a-node), comes
only after the pool's `side_unhealthy` or `leg_unhealthy`, and the timer
of step 5 takes the table away before then ([the scope page, section
8](scope.md#8-waits-a-host-can-see)).
A table on the disk node makes such a cut when its set `hosts4` or
`hosts6` holds the address of the controller node, or when the two
interface names of step 4 are swapped. It also fails the IO to a leg that
a migration moves, for a block the new side has not yet copied, when the
sets hold the address of a disk node that holds the new side of a
migration whose old side is there: from the cutover until the migration
ends, that disk node reads such a block from the old side at this one's
NVMe/TCP port. A controller node's own
table drops none of its connections to the legs, since their answers come
back to ports not in `refused`. So before step 5 on a disk node, find the
pools with a side there, by `dnvctl dn get --addr <addr>` and `dnvctl sp
get`, as the How to confirm of [Replace a dead disk
node](#replace-a-dead-disk-node) says. For each pool whose `redund_conf`
is `redund_none`, stop the users of its volumes on every host and unmount
them, or load the table there only when the failure of that IO, the
repair and the loss above are acceptable. Do the
same for a RAID1 pool with a group whose leg on that disk node has no
other leg of the group in sync on another machine.
`dnvctl --sp '<pool name>' sp get` lists each group's `grp_id` with the
`leg_id` of each of its legs in `leg_list`, save a clone's meta group,
the group that holds a clone's copy records: `sp get` names the pool's
clones in `sp_conf.clone_name_list`, and
`dnvctl --sp '<pool name>' clone get --name <clone>` shows each one's
group under `clone.meta_grp`. Then
`dnvctl --sp '<pool name>' group inspect --grp <grp_id>` shows a leg in
sync when its entry of `leg_list` has a `member.md_state` that holds the
word `in_sync` and not the word `faulty`; the entry's `addr_port` names
the leg's disk node. Start that IO again only after step 8 on that
machine.

When to stop: after a load, a host's IO stalls, a caller's try of step 7
prints 124, `dnvctl cluster get-status` fails, as it does after about 10
seconds with error code DEADLINE_EXCEEDED when the table drops its
packets, or lists something new ([Read what is
wrong](#read-what-is-wrong)), or a node create fails as above. Delete the
table at once, `nft delete table inet dnv_filter`, and stop its timer,
`systemctl stop dnv-filter-undo.timer`. Check the sets against the list
of who connects to whom above, and on a machine on two networks check the
two interface names of step 4 against `ip address`, then start again at
step 5. Also start again at step 5 when the timer ran before step 8, as
steps 6 and 8 show: the table is then gone or was never loaded, and a try
of step 7 made after that shows nothing. Do not go on past the read of
step 5 while a pool's `cntlr_unhealthy`, `side_unhealthy` or
`leg_unhealthy` is 60 seconds or less: a timer 30 seconds shorter then
leaves too little time for step 7, or none. Ask for help. A value of 0
there means the default, as step 5 says.

Tested: the firewall run in the lab on 2026-11-22. This file, with the
lab's addresses and ports in its sets, was loaded on the control-plane
guest and on the seven disk node and controller node guests, each after
its undo timer, and no other table changed. With a listener on each, the
gateway's port, etcd's client port, a cn agent's control port, a dn
agent's control port and a dn agent's NVMe/TCP port timed out from a host
and answered from a machine outside the hosts' set, and the cdc's port and
a controller node's NVMe/TCP port answered from the host. The e2e suite
at one slice then passed under the tables, on its second run: the first
stopped in its react case on a host's read that did not end while the
machine that runs the lab ran out of memory, with no packet dropped. Each
case of the Kubernetes suite except its policy case passed too, each run
alone, with
the gateway's port left open to the hosts, where one of the controller
plugin's two copies always runs in the lab; its machine case makes the
copy on a host the one that acts. After each passing run every table
stood, and its counter showed no dropped packet: neither suite needs a
refused port. In the passing run the e2e suite reset three node guests
on purpose, one of them twice, and each reset took the table away, as
any reboot does; a loop loaded it again within a minute of the guest's
return, and that table counted drops only from its new load. The sets
held the hosts one by one, their link-local IPv6 addresses among them.
The `--collect` of step 5 was checked outside the lab with systemd 255,
the version of Ubuntu 24.04, on a user's own units
(`systemd-run --user`): after a failed delete, a new timer of the same
name starts. On the same systemd, a timer of one second ran after 50
seconds at the default accuracy, and one of three seconds with
`AccuracySec=1s` after four seconds. Once a timer had run,
`systemctl is-active` printed `inactive` and `systemctl stop` printed
`Unit ... not loaded`. The lab run did not use `--collect`: it ran
`systemctl reset-failed` on the timer's units before each start, and
loaded each table only once
`systemctl is-active` printed `active`, with timers of two and a half
hours at the default accuracy. The policy case was not run under the
tables: it needs the agents' ports open from a pod on a host as its own
control. Not tested: the lines for a machine on two networks and a
route through a controller node, as every lab guest has one interface on
one network; a set `refused` with no `elements` part, which nft 1.0.9
loaded only outside the lab, in a network namespace of its own; a
refusal over IPv6, which no suite uses; a pod network whose connections
to other machines keep their pod address, with its range in the sets, as
the lab runs flannel alone; the load at boot; and
the node selector of the controller plugin, which the lab cannot run, as
its three Kubernetes nodes are the control-plane guest and the two
hosts.

## The cluster's capacity

`dnvctl cluster get-capacity` says what pools of one shape can still be
made in a cluster, and which limit the cluster reaches first. The shape is
the number of legs of a group, `--leg-cnt` (2 for RAID1, 1 for a pool with
no redundancy), the number of cntlrs of a pool, `--cntlr-cnt`, and its
number of slices, `--slice-cnt`. A flag left out takes what a pool
create takes: two legs a group, as in the RAID1 pool `dnvctl sp create`
makes with no `--redund`, and the default counts. A `--leg-cnt` of 0
takes the legs of the cluster's own redundancy, and a count of 0 the
default. The answer is under `space`. The CSI driver gives Kubernetes the
same answer for a storage class, the Kubernetes record that names the
cluster, the legs of a group and the cntlrs of each volume's pool, with
the class's legs and cntlrs and one slice.

A controller node has a budget: the most pool space its cntlrs may serve,
given in bytes to `--capacity` of its cn agent and read once, when the node
is created. Every cntlr of a pool, the primary and each standby alike,
takes the size of all the pool's groups from the budget of its controller
node. So the budgets of the controller nodes, divided by the number of
cntlrs of a pool, bound the pool space the cluster can hold, whatever its
disks hold. A cn agent started with no `--capacity`, or with 0, gives its
node the largest budget, 64 TiB. A controller node that 0.1 created with no
`--capacity` keeps the 4 TiB budget 0.1 gave it, after an upgrade too: its
`total_ext_cnt` under `addr_port_to_cn` shows it.

When: before you make pools or Kubernetes volumes in a cluster; when a pool
create is refused for want of room; when a Kubernetes claim, the request
for a volume of a size from a storage class, waits for capacity; when the
worker skips an auto-grow.

Symptoms:

- `dnvctl sp create` fails with RESOURCE_EXHAUSTED, and the text says
  "no controller node with ... free extents" or "disk nodes with ... free
  extents in distinct locations".
- A Kubernetes claim of a storage class with `volumeBindingMode:
  WaitForFirstConsumer` waits, and `kubectl get csistoragecapacities -A`
  shows its class a `maximumVolumeSize` below the claim's size: the
  scheduler, the Kubernetes program that chooses the machine where the
  claim's user runs, compares the claim with that field, which the CSI
  driver always sets. A class with `Immediate` binding, as
  `deploy/storageclass.yaml` has, never waits on capacity: a create that
  does not fit is refused instead.
- The worker's log has a `reaction skipped` line for an auto-grow with the
  reason `cn free_ext_cnt too low`: a controller node of the pool has no
  budget left for the new group.

How to confirm: run `dnvctl cluster get-capacity --leg-cnt 2 --cntlr-cnt 2
--slice-cnt 1`, with the shape of the pool you want. In `space`:

- `limit` is the limit the cluster reaches first, checked in this order:
  - `CAPACITY_LIMIT_POOL_CEILING`: the cluster holds the most pools it can,
    4096, the pools being deleted included.
  - `CAPACITY_LIMIT_CN_COUNT`: fewer controller nodes can take a new cntlr
    than the pool has cntlrs.
  - `CAPACITY_LIMIT_DN_COUNT`: fewer disk nodes can take a new leg than
    the pool has legs: the legs of a group times twice the number of
    slices, since each slice has a meta group and a data group, and every
    leg of a pool is on a disk node of its own.
  - `CAPACITY_LIMIT_DN_LOCATIONS`: there are disk nodes enough, but in too
    few locations, since a location holds at most one leg of each group.
  - `CAPACITY_LIMIT_DN_SPACE`: the free space of the disks is the limit.
  - `CAPACITY_LIMIT_CN_BUDGET`: the budgets of the controller nodes are the
    limit.
- `placeable_bytes` is the pool space, one copy of the data, that pools of
  this shape can still get. It is 0 when one of the first four limits is
  reached. A location gives each group at most one leg, so a disk node with
  far more free space than the others adds only what the others can pair
  with it.
- `largest_init_ext_cnt` is the largest first data group, in extents, that
  a new pool of this shape is sure to get when every disk node has a
  location of its own: `--init-ext-cnt` of `dnvctl sp create` above it can
  be refused. Where a location holds several disk nodes, a create below it
  can still be refused, and a second try can place it.
- `dn_raw_bytes`, `dn_charged_bytes` and `dn_free_bytes` are the whole
  space of the disk nodes, the space their legs take, and the free space of
  the disk nodes that can take a new leg; `cn_raw_bytes`,
  `cn_charged_bytes` and `cn_free_bytes` are the same for the budgets of
  the controller nodes. The free space of a disabled node, a node with a
  health epoch and a node that holds the most sides or cntlrs it can is in
  the whole space only, not in the free space: it is the whole space less
  the space taken less the free space.

Beside `space`, the reply's `addr_port_to_dn` and `addr_port_to_cn` list
each node's record: its location, whether it is disabled, its health
epoch, its extents and how many sides or cntlrs it holds.

The answer is an estimate, read from the records of the nodes. A leg that a
migration copies counts on both of its disk nodes, and a retired leg counts
until it is deleted. Every leg of a pool needs a disk node of its own, so
when a few disk nodes hold most of the free space, pools of small groups use
up the disk nodes before the space, and fewer bytes fit than
`placeable_bytes`. When no pool of the shape fits at all, because fewer
controller nodes than the pool has cntlrs, or too few disk nodes, have room
for even the smallest pool,
`placeable_bytes` and `largest_init_ext_cnt` are 0 and `limit` names the
side that has no room.

What dnv does alone: nothing adds space and nothing changes a budget.
Auto-grow adds a group to a slice while a disk node and every controller
node of the pool have room for it; when they have none, the worker skips
the grow, logs it, and goes on with its other reactions. A write that needs
a new block in a full thin pool waits for the kernel's no-space timeout and
fails ([the scope page](scope.md#4-data-loss-dnv-accepts)).

Safe steps:

1. For `CAPACITY_LIMIT_DN_SPACE` and `CAPACITY_LIMIT_DN_COUNT`, add disk
   nodes.
2. For `CAPACITY_LIMIT_DN_LOCATIONS`, add disk nodes in locations the
   cluster has few of, on other machines, or give the disk nodes of one
   machine locations of their own ([Choose a node's
   location](#choose-a-nodes-location)).
3. For `CAPACITY_LIMIT_CN_COUNT` and `CAPACITY_LIMIT_CN_BUDGET`, add
   controller nodes, their cn agents started with the `--capacity` you want
   or with none for the largest budget. No command changes the budget of a
   controller node that exists ([the scope page](scope.md)): a node that 0.1
   made with its 4 TiB default gets a larger one only once it is emptied
   and deleted, as [Remove a node](#remove-a-node) says, and created again
   with its cn agent started anew, with no local files and the
   `--capacity` you want.
4. Make the pool with fewer cntlrs or fewer slices, if the shape that
   fits is one you can use: run the command again with that shape first.

Steps that lose data: deleting a pool or a volume to make room deletes its
data. A deleted pool still counts toward the pool ceiling, and its space
stays taken, until the worker's drain of it ends.

When to stop: the answer shows free space for the shape, `limit` is
`CAPACITY_LIMIT_DN_SPACE` or `CAPACITY_LIMIT_CN_BUDGET`, and every create
of a pool of that shape is still refused. Ask for help.

Tested: the Go tests of the answer on hand-made node records, and of the
gateway's read, its refusals and its check of the cluster's stored
settings; the gateway suite's faults case checks the answer over its fake
nodes before and after its pool is made, and the csi suite's volume case
checks that the capacity Kubernetes shows for its storage class is the
gateway's answer. Both suites passed in the lab with these checks on
2026-11-19.

## A pool's space

`dnvctl sp inspect` shows how full the thin pools of a pool are, as the
pool's primary last read them, beside the space the pool's records give
it, and how far its volumes overcommit it.

When: a host's writes to a volume of the pool wait and fail; before you
make more volumes or snapshots in a pool; when the worker skips an
auto-grow of the pool.

Symptoms:

- Writes from a host to a volume of the pool wait for the kernel's
  no-space timeout and then fail, while reads still work.
- The worker's log has a `reaction skipped` line for an auto-grow of the
  pool.

How to confirm: run `dnvctl sp inspect`. Beside `space`, the reply's
`applied_revision` is the revision the primary's agent held when it made
its reading, and `obs_age` is the age of that reading, `null` while the
agent holds no reading. In `space`:

- `slice_list` has one entry for each slice. `data_bytes` is the data space
  the slice's data groups give its thin pool. When `read` is true,
  `pool_data_bytes` and `used_data_bytes` are the thin pool's data size and
  the data in use, and `pool_meta_bytes` and `used_meta_bytes` the same for
  its metadata, as the primary last read them. When `read` is false, the
  primary's report has no line it can read for that thin pool: the pool is
  not built yet, or it failed, or the primary's agent has no reading of the
  cntlr yet. In the last case every slice is unread, and the reply's
  `applied_revision` is 0 and its `obs_age` is `null`: the agent is
  starting, as right after its restart, or it does not hold the cntlr yet,
  as right after the pool's create or the replacement of a pool's one
  cntlr. Right after a failover, the new primary has not built the thin
  pools yet: its slices are unread while `obs_age` is not `null`. Run the
  command again a little later.
- `td_cnt` and `virtual_bytes` are the pool's thin devices, snapshots
  included, and their sizes added up. `overcommit_pct` is `virtual_bytes`
  over `data_bytes`, in percent: above 100, the volumes can be given more
  data than the pool holds now.
- `footprint_ext_cnt` is the extents of all the pool's groups, which each of
  its cntlrs takes from the budget of its controller node ([The cluster's
  capacity](#the-clusters-capacity)).

What dnv does alone: auto-grow adds a group to a slice whose thin pool is
filling up, while a disk node and every controller node of the pool have
room for it; when they have none, the worker skips the grow, logs it, and
tries again on its next round.

Safe steps:

1. Give the cluster room for auto-grow: [The cluster's
   capacity](#the-clusters-capacity) says which limit it reaches and what to
   add.
2. Grow a slice by hand: `dnvctl sp grow-slice --slice <slice id> --ext 1`,
   with the slice id of `slice_list`, adds a data group. Any `--ext` above
   0 asks for a data group, and the new group always takes the size of the
   slice's first data group, whatever number you give. `--meta`, with no
   `--ext`, adds a meta group instead, as large as the slice's meta groups
   together. The gateway refuses a grow while the pool's level is
   `NO_THINPOOL` or below ([Change a pool's level](#change-a-pools-level)),
   and while a group grown on that slice and of that kind earlier is still
   waiting for its disk nodes to prepare it, since a group grown over it
   would wait behind it for ever.
3. A grow that waits: the status read, `dnvctl cluster get-status`, lists
   a row "data grow waits" or "metadata grow waits" once a group grown on
   the slice has waited longer than the pool's side threshold for a disk
   node to prepare its new side, which it zeroes first. The disk node
   zeroes very slowly, or cannot, and no other grow of that kind of the
   slice runs meanwhile. When that disk node is dead or reports an error
   of its own, the worker takes the group back by itself, but only while
   the slice's thin pool is filling up. Otherwise take it back:
   `dnvctl --sp <pool> sp withdraw-grow --slice <slice id> --grp <group id>`,
   with `--meta` for a metadata group, the group the row names. The
   gateway refuses a group that is not the newest, a slice's first group,
   a group whose disk nodes have all prepared their sides, which may hold
   data, and a pool whose level is `NO_THINPOOL` or below. The next grow
   then runs again.
4. A thin pool can be read-only while its slice's `read` is true. Its
   line in `dnvctl cntlr inspect --id <primary cntlr id>`, the row under
   `slice_id_to_dm_pool`, says why. When it carries `needs_check`, follow
   [Repair a thin pool's metadata](#repair-a-thin-pools-metadata). When it
   carries `ro` and no `needs_check`, the thin pool's metadata is full: a
   meta group that auto-grow or step 2 with `--meta` adds makes it writable
   again.

Steps that lose data: deleting a thin device or a snapshot to free blocks of
the thin pool deletes its data.

When to stop: a slice stays unread, run after run, while the pool is built
and the agent has a reading of the cntlr (`obs_age` is not `null`). Or a
thin pool's line carries `ro` and no `needs_check`, and the grow of step 2
with `--meta` is refused (FAILED_PRECONDITION): the slice's meta groups
together reach dm-thin's metadata ceiling, about 16 GiB. Or the pool stays
read-only after a meta group is added. Ask for help.

Tested: the Go tests of the answer on hand-made rows, and of the gateway's
read with a fake agent; the gateway suite's smoke case checks it over a fake
agent's row, and the e2e suite's setup over real agents after a host writes
its pattern. The gateway suite passed in the lab with its check on
2026-11-19, and the e2e suite's setup read it in the lab on 2026-11-21.
Step 3: the gateway's Go tests cover the refusal of a grow over a group
that waits, `dnvctl sp withdraw-grow` and the row of a grow that waits, on
planted records; the dnvctl suite sends the command to a fake gateway. No
suite takes back a group that waits on a real disk node.

## Repair a thin pool's metadata

Words: a thin pool's **metadata** is dm-thin's record of which blocks of the
pool each volume uses. It lives on the slice's meta group. When dm-thin
fails to commit it, dm-thin writes a mark into the metadata that it **needs
a check** (`needs_check`). The mark stays in the metadata, so every
controller node then opens that thin pool read-only. The tools here,
`thin_check` and `thin_repair`, come with thin-provisioning-tools, whose
`thin_dump` the cn agent already runs.

Symptom: a write to the pool's volumes fails when it needs a new block of
the thin pool, or lands on a block that a snapshot shares; other writes and
reads still work.
`dnvctl cluster get-status` lists the object `slice <slice id> thin pool`
with `row/error/pool_needs_check`.

Confirm: in `dnvctl cntlr inspect --id <primary cntlr id>`, the slice's row
under `cntlr_info.slice_id_to_dm_pool` reads `RES_STATUS_ERROR` with the
reason `RES_REASON_POOL_NEEDS_CHECK`. Its details are the thin pool's status
line, which holds `needs_check`. Two other states are not this case: the
reason `RES_REASON_POOL_FAILED`, a thin pool that dm-thin failed, which a
failover cures; and a line with `ro` and no `needs_check`, metadata that is
full ([A pool's space](#a-pools-space)).

What dnv does alone: the worker starts no failover and no cntlr replacement
for this row, since every controller node would open the pool the same way.
It logs `reaction skipped` with the reason `shared_state` at its passes.
This lasts until you act. An error row of another kind in the same report,
such as a broken group, still brings a failover.

Safe steps:

1. Find the slice's meta group. `dnvctl sp get` lists the slices in
   `slice_list`, in the order of the slice ids in `sp_conf.slice_id_list`.
   The slice's `meta_grp_list` must hold one group: note its `grp_id`. With
   more than one, stop.
2. Step the level to `NO_THINPOOL`:
   `dnvctl sp set-level --level NO_THINPOOL`. The primary takes the thin
   pools away and keeps the group arrays. Hosts get IO errors, and no
   reaction runs.
3. Wait until `dnvctl cntlr inspect --id <primary cntlr id>` shows
   `applied_revision` at least the `sp_rev.revision` of `dnvctl sp get`,
   the slice's row under `slice_id_to_dm_pool` as `RES_STATUS_MISSING` with
   the reason `RES_REASON_SP_LEVEL`, and each row of the slice's groups
   under `grp_id_to_md_raid` as `RES_STATUS_OK`. A data group that broke
   comes back here once its legs serve
   ([A broken array that comes back by itself](#a-broken-array-that-comes-back-by-itself)).
4. Note the metadata's device: the `res_name` of the meta group's row under
   `cntlr_info.grp_id_to_md_raid`, keyed by its `grp_id`. In a RAID1 pool
   it is the path of the group's array, such as `/dev/md/<name>`. In a pool
   without RAID it is the name of a device mapper device, whose path is
   `/dev/mapper/<res_name>`. At this level that device holds the whole
   metadata of the slice's thin pool. Run the next steps as root on the
   primary's controller node, the `addr_port` of the primary in
   `dnvctl sp get`.
5. Check it: `thin_check <metadata device>`. It passes on metadata that
   only carries the mark. If it fails, stop.
6. Make a new device of the same size, a loop device over a new file on a
   local file system with room, outside the agent's `--local-store`
   directory:

   ```
   truncate -s $(blockdev --getsize64 <metadata device>) <file>
   losetup -f --show <file>
   ```

   The second command prints the loop device's path.
7. Repair onto it and check the copy:
   `thin_repair -i <metadata device> -o <loop device>`, then
   `thin_check <loop device>`.
8. Copy it back over the meta group's device:
   `dd if=<loop device> of=<metadata device> bs=1M conv=fsync status=none`.
   dnv builds the thin pool over that device by its name, so the copy back
   takes the place of a swap of devices. Then remove the loop device with
   `losetup -d <loop device>` and delete the file.
9. `thin_check --clear-needs-check-flag <metadata device>`, then
   `thin_check <metadata device>`. Both must pass.
10. Check that the mark is gone. Read the metadata's first block with
    direct IO and print its flags word, whose lowest bit is the mark:
    `dd if=<metadata device> bs=4096 count=1 iflag=direct status=none | od -A n -t u4 -j 4 -N 4`.
    It must print an even number; it printed 0 in the lab. Use GNU dd for
    it: the `dd` of uutils, which some systems ship, handles direct IO
    wrongly.
11. Step the level back: `dnvctl sp set-level --level READWRITE`, or the
    level the pool had. Wait until the slice's row under
    `slice_id_to_dm_pool` reads `RES_STATUS_OK` and its details show `rw`.
12. Write to a volume from a host, and read back data it held before.

Steps that can lose data:

- The copy of step 8 overwrites the device it is given. A wrong device,
  such as a data group's array or another pool's, loses its data. Copy only
  onto the device of step 4.
- `thin_repair` over damaged metadata can drop mappings, and the volumes
  then lose the blocks they mapped. This is not tried; step 5 stops first.
- A level stepped back before step 8 brings the pool back read-only with
  the mark. That loses nothing: do the steps again. Never step the level
  while the copy of step 8 runs: the primary would build the thin pool
  over metadata that is half copied.

When to stop: the slice has more than one meta group, since then no device
holds the whole metadata at that level; `thin_check` fails in step 5 or 7;
a group's row of the slice does not read ok at `NO_THINPOOL`; or the pool
reads `needs_check` again after step 11.

Tested: the cn agent suite, `integtest/cnagent_test.sh`, case `thinrepair`,
at the agent level. The suite plays the worker and the gateway: it sends
the level to the cn agent itself, and it finds the metadata's device with a
tool of its own, not with `dnvctl cntlr inspect`. Its pool has one cntlr
and one meta group in a RAID1 slice, and a commit over a broken data group
makes the mark; the host is disconnected during the steps. Steps 5 to 10
run as written, step 10 with GNU dd, and the host reads its data after the
level is back. It passed in the lab on 2026-11-21, every step working. Not
tested: the steps through `dnvctl` and a worker, a pool with standbys or
without RAID, metadata that `thin_check` finds damaged, and a slice of more
than one meta group.

## Change a pool's level

Words: a pool's **level** steps from `READWRITE` down to `DISABLE`. Each
step keeps what the steps before it switch off:

- `READONLY`: the primary fails every write to the pool's volumes, and
  reads work. A controller node that has not yet applied the level keeps
  serving writes until it does.
- `NO_CLONE`: the clones' copies go, and the volumes they fill give hosts
  IO errors.
- `NO_THINPOOL`: the thin pools and the volumes go: every read and write of
  the pool's volumes fails. The namespaces stay exported, so a host sees IO
  errors, not a lost device. The group arrays still run. At this level
  and every level below it the worker runs no reaction for the pool: no
  failover, cntlr replacement, leg repair or auto-grow, no delete of old
  retired legs and no md check, md's read of both legs of each array,
  every 30 days at the default
  ([The md check's mismatch count](#the-md-checks-mismatch-count)).
- `NO_REDUND`: the primary stops the group arrays. The legs stay connected
  and checked.
- `NO_MIGRATION`: the disk nodes take every migration's copy away, and no
  cutover starts.
- `NO_SIDE`: the disk nodes export no side, and the controller nodes drop
  their leg connections.
- `DISABLE`: only the sides' disk space stays. The zeroing of a new side
  goes on.

When: before a forced start
([Before every forced start](#before-every-forced-start)), before
[Repair a thin pool's metadata](#repair-a-thin-pools-metadata), or to take
a damaged pool apart in stages.

How to confirm the level: `dnvctl sp get` shows it as `sp_conf.sp_level`.
`dnvctl cluster get-status` lists a `pool` line for any level below
`READWRITE`, with the text "level SP_LEVEL_<name>: writes to its volumes
fail", and from `NO_THINPOOL` down ", and the worker runs no reaction for
the pool". From `NO_THINPOOL` down reads fail too, which the line does not
say. The primary has applied the level when
`dnvctl cntlr inspect --id <primary cntlr id>` shows `applied_revision` at
least the pool's `sp_rev.revision`.
Each row the level takes away then reads `RES_STATUS_MISSING` with the
reason `RES_REASON_SP_LEVEL`.

What dnv does alone: the worker sends the new level to every cntlr and side
of the pool. Nothing else changes, and no timer moves the level back. The
gateway refuses some calls at some levels: from `NO_THINPOOL` down,
`dnvctl sp grow-slice`; from `NO_REDUND` down, `dnvctl repair create` and
`dnvctl group force-start` (but its `--cancel`). `dnvctl group inspect`
reads the group at every level; from `NO_REDUND` down its dry run, with
`--leg`, answers that a start is refused at that level.
`dnvctl repair finish`, `repair cancel` and `repair abort` run at every
level, and so does `dnvctl migr create`, though no cutover starts from
`NO_MIGRATION` down.

Safe steps: `dnvctl sp set-level --level <level>`, with a short name such as
`NO_THINPOOL`, or the full name such as `SP_LEVEL_NO_THINPOOL`; case does
not matter. Step back the same way. Then wait for `applied_revision`, as
above.

Steps that can lose data: every level below `READWRITE` fails the hosts'
writes, and from `NO_THINPOOL` down their reads, so an application on a host
can lose what it was writing. `dnvctl sp set-level` with no `--level` sets
`READWRITE`: it brings the pool back up.

When to stop: the primary's `applied_revision` does not reach the pool's
revision, or rows stay in error after the level is back: read the pool
again from [Read what is wrong](#read-what-is-wrong).

Tested: the e2e suite's ops case, stage 05, steps the whole ladder down to
`DISABLE` and back, each step awaited on the primary's `applied_revision`
and its rows; the read-only step reads the data and keeps a write off the
disks. It passed in the lab on 2026-11-21. The cn agent suite steps
levels at the agent level: its repair case's forced start stages step
`NO_THINPOOL` and `NO_REDUND`, and that case's stage `reassembly` and the
`thinrepair` case step `NO_THINPOOL`.

## Move a leg to another disk node

Words: a migration moves the side of a leg to another disk node while the
leg stays in use ([the introduction](introduction.md#migration) shows its
flow). The **old side** is the side that moves; the **new side** is the one
the gateway makes for it. On the new disk node dm-clone copies the old side
in **regions** of one pool block. The copy is **whole** when every region is
copied.

When to use: a side must leave its disk node while its leg works: a slow
leg ([A slow leg](#a-slow-leg)), a disk node to empty before its removal
([Remove a node](#remove-a-node)), or a disk to replace. A leg that failed
is replaced by a repair instead ([Repair a leg by hand](#repair-a-leg-by-hand)).

How to confirm it can run: `dnvctl sp get` lists, under `slice_list`, each
group's `leg_list`, and each leg's `side_list` with the `side_id` and the
disk node's `addr_port`. `dnvctl dn get --addr <addr>` lists the sides on one
disk node under `dn_conf.side_ptr_list`. The gateway refuses with
FAILED_PRECONDITION, unless another code is named:

- a pool that has four migrations already (RESOURCE_EXHAUSTED), or a name in
  use (ALREADY_EXISTS);
- a pool whose data block is above 4 MiB
  ([A pool made before the block bound](#a-pool-made-before-the-block-bound));
- a side that is in no leg of the pool's slices, such as the side of a
  repair's new leg or of a retired leg (NOT_FOUND);
- a side of a clone meta group, of a repair's bad leg, or of a leg that
  already has two sides;
- a pool whose `sp_conf.cntlid_slot_list` holds no slot but the side's
  `cntlid_slot` ([Change a pool's cntlid slots](#change-a-pools-cntlid-slots));
- no disk node with room: one that holds no leg of the group, with the
  group's extents free and room in its metadata area for the copy's records
  (RESOURCE_EXHAUSTED).

What dnv does alone:

- The gateway puts the new side on a disk node that holds no leg of the
  group, and out of the group's locations when it can.
- The new disk node takes its room in its metadata area and zeroes the
  side as for any new side: its first blocks, or the whole side of a meta
  group. The worker then marks the side provisioned, and every
  cntlr connects to it. The old side serves meanwhile.
- The worker starts the cutover once the new side waits for it and the
  primary's path to it is live. It starts none while the pool's level is
  `NO_MIGRATION` or below, or while the primary is disabled.
- At the cutover the leg has no path for a moment: one to two and a half
  seconds were measured at 32 slices ([the scope page](scope.md),
  section 8). Then the worker reads the leg's list of written blocks from
  the primary and hands it to the new side, so that the copy skips the
  blocks the pool never wrote.
- The worker never ends the migration of a healthy leg. For a leg that leg
  repair would replace, it cancels a migration whose cutover is pending,
  finishes one whose copy is whole, and otherwise logs `reaction skipped`
  with the reason `migration_copy_not_whole` at each pass
  ([Ending a migration with force](#ending-a-migration-with-force)).

Safe steps:

1. First stop the hosts' discards on the pool's volumes, and make no file
   system on them, for the whole migration, as "Steps that can lose data"
   below says. Then start it:
   `dnvctl migr create --name <name> --src-side <side id>`. Add
   `--dn-black <addr>,<addr>` to keep the new side off those disk nodes,
   such as the slow one, or `--dn-white` to name the only ones it may use.
   `--hyd-threshold`, from 1 to 8, is how many regions dm-clone copies at
   once, and `--hyd-batch`, from 1 to 4, how many regions side by side it
   joins in one copy; each is 1 when left out.
2. Wait for the cutover. `dnvctl migr get --name <name>` shows the new
   side as `migr.dst_side_id` and `migr.cutover_pending`, true until the
   cutover. `dnvctl cluster get-status` lists `its cutover is pending`
   meanwhile, with the primary's path to the new side when it has one.
3. Watch the copy: `dnvctl sp inspect-side --id <new side id>` shows
   dm-clone's status line in `side_info.migr_dst_info.dm_clone_info.details`.
   Its count of hydrated regions, the regions copied so far, out of all
   regions shows how far the copy is. A copy that runs has no line in
   `dnvctl cluster get-status`; one whose row reads an error has
   `its copy reads ...`.
4. When the count reaches the total, `dnvctl cluster get-status` lists
   `its copy is whole: it waits for migr finish`. Finish it:
   `dnvctl migr finish --name <name>`. The gateway asks the new side's
   agent for the proof that the copy is whole. It refuses while the copy is
   not whole or the agent does not answer, and, with `--force` too, while
   the cutover has not started. It answers ABORTED when another migration
   took the name between its read and its write: run it again. The leg then keeps the new side alone, and
   the old disk node removes the old side and frees its extents.
5. Check: `dnvctl sp get` shows the leg with one side, the new one, and
   `dnvctl migr get --name <name>` answers NOT_FOUND.

To end it without the move before the cutover, run
`dnvctl migr cancel --name <name>`: the new side goes, and nothing is lost.
After the cutover the gateway refuses a cancel without `--force`: finish
the migration instead.

Steps that can lose data:

- `dnvctl migr finish --force` skips the proof. The regions the copy had not
  reached then read what the new side's disk held there, possibly another
  pool's bytes, with no error.
- `dnvctl migr cancel --force` after the cutover brings the old side back
  as of the cutover. The leg loses every write since, and blocks first
  mapped since read the old side's old bytes, possibly another pool's. md
  still counts the leg whole, so in a RAID1 group reads can return those
  bytes, and no call makes md copy the leg again: a repair is refused for a
  leg with no health epoch, and a member that md failed when the leg went
  back to its old side is added back from md's record of the regions
  written while it was out, which holds none of these. The call also
  returns before the primary stops using the new side, and a write until
  then can land on the new side and be lost with it ([Ending a migration
  with force](#ending-a-migration-with-force) says how to see the end of
  that).
- A host's discard, the message that a range holds no data any more, as
  `fstrim` sends, on a leg past its cutover: it marks the region as copied
  on the new side, and when the primary dies within the second before its
  thin pool records the discard, the new primary reads that block from the
  new side, which holds old bytes there, possibly another pool's ([the
  scope page](scope.md#53-transfers-clones-and-migrations)). On each host
  of the pool, stop discards for the whole migration: `sudo systemctl stop
  fstrim.timer`, and remount a file system that has the `discard` option
  without it (`sudo mount -o remount,nodiscard <mount point>`). Start them
  again once the migration has ended. `blkdiscard` sends discards too, and
  so does a file system's maker, such as `mkfs.ext4` or `mkfs.xfs`, unless
  told not to; the Kubernetes driver's node plugin makes a file system on
  a new volume this way.

Use either flag only as [Ending a migration with force](#ending-a-migration-with-force)
says.

When to stop: the cutover does not come, and the `its cutover is pending`
line names a path that does not read ok, or the new side's rows, which
`dnvctl sp inspect-side --id <new side id>` shows, wait for room in its
metadata area: cancel it, which needs no force then, and ask for help.
The copy's count stops growing, or its row reads an error: force no end,
and read [Ending a migration with force](#ending-a-migration-with-force).

Tested: the e2e suite's copy case, stage 04: a migration under a capped
copy, the cutover, the worker's read of the written blocks, the finish and
the data read back; then a second migration, whose plain cancel after the
cutover is refused and whose cancel with `--force` goes through. No host IO
runs between that one's create and its cancel, so it proves the refusal and
the steps, not the loss. Stage 04b resets the new side's disk node guest
during a copy and reads the data after the copy ends. Both passed in the lab
on 2026-11-21. The gateway's unit tests check each refusal.

### Ending a migration with force

When: a migration whose cutover has started cannot be finished: the new
side's agent does not answer the proof, or a disk node of the leg is gone
and the copy is not whole. When the old side's disk node is the one gone,
run `dnvctl migr finish --name <name>`, with no force, first. The gateway
asks only the new side's disk node for the proof, so the finish goes
through when the copy is whole. Once that disk node has lost its
connection to the old side, only some of its reports carry the copy's
count: if the finish is refused with "has not finished hydrating" after
you saw the copy whole, run it again a few seconds later. The leg
then keeps the new side alone, which holds every block, and needs no
repair: if md failed the leg meanwhile, the primary adds it back once it
serves again. While the copy is not whole the gateway refuses the finish
each time: then read the cases below.

Confirm: `dnvctl migr get --name <name>` shows `cutover_pending` false.
`dnvctl sp get` shows each side's `err_epoch` in the leg's `side_list`, and
`dnvctl dn inspect --addr <addr>` whether a disk node's agent answers.

What dnv does alone: for a leg it would repair, the worker ends a
migration only where that loses nothing, as above. With a copy that is not
whole it logs `migration_copy_not_whole` at each pass, and the group runs
one leg short, or, with no other leg in sync, its IO waits, until both
sides answer again and the copy goes on to its end.

Steps, by case:

1. The new side's agent does not answer, so `dnvctl migr finish` is refused
   with "did not report hydration", and you saw the copy whole before: the
   line `its copy is whole: it waits for migr finish`, or a count of copied
   regions equal to the total. A copy that was whole stays whole. Then
   `dnvctl migr finish --name <name> --force` loses nothing the new side
   holds.
2. A RAID1 group whose other leg md holds in sync, as
   `dnvctl group inspect --grp <group id>` shows, and a disk node of the
   leg is gone for good: its machine will not come back, or its disk
   failed. End the migration so that the leg
   keeps the side that is gone: `dnvctl migr finish --name <name> --force`
   when the new side's disk node is gone, and
   `dnvctl migr cancel --name <name> --force` when the old side's is and
   the copy is not whole, so that the finish with no force is refused
   ("When", above). The leg
   then has no path. md fails its member, and the group serves from its
   other leg, which holds every write. Then have the leg replaced at once,
   before the node can come back with its old or partial copy: once
   `dnvctl sp get` shows a health epoch (`err_epoch`) on the leg and no
   repair on the group (`rep_name` empty), create one:
   `dnvctl repair create --name <name> --grp <group id> --bad-legs <leg id>`,
   and follow [Repair a leg by hand](#repair-a-leg-by-hand). The worker may
   create its own first, once the leg's threshold has passed; the gateway
   then refuses yours and names the worker's. md copies the new leg whole
   from the other leg, and a repair's bad leg never joins the array again.
   When the repair has finished, delete the retired leg it made, which
   holds no whole copy ([Retired legs](#retired-legs)).
3. A group of one leg, in a pool without RAID, with a disk node gone and no
   finish with no force that goes through ("When", above): no end keeps the
   data. A forced end that keeps the side that is gone leaves the leg on a
   dead side, and the volumes then give IO errors over that group for good.
   A forced end that keeps the side that answers serves what that side
   lacks: the old side lacks every write since the cutover, and the new
   side the regions its copy had not reached. Stop and ask for help.
4. Both disk nodes answer, and the new side's copy row reads an error, as
   when dm-clone's own records fail: neither forced end is safe. A forced
   cancel serves the old side as of the cutover, which md counts whole; a
   forced finish serves the regions not copied from the new side's disk.
   Stop and ask for help.

Steps that can lose data: every forced end but those of cases 1 and 2. A
forced cancel also loses nothing when nothing writes to the pool from the
cutover until the primary has taken the cancel. The call returns before
that, and until then a write can still land on the new side and is lost
with it, unless another leg of the group in sync holds it too. So where
the group has no other leg in sync, keep the hosts' writes stopped after a
forced cancel until `dnvctl cntlr inspect --id <primary cntlr id>` shows
`applied_revision` at least the pool's `sp_rev.revision` that `dnvctl sp
get` shows after the cancel, and the leg's row under
`cntlr_info.leg_id_to_leg` reads `RES_STATUS_OK`: the primary has then
taken the cancel, and its converge drops its path to the new side. If that
controller node's agent log holds the record `disconnecting a dead leg
path failed` since the cancel, that path can still serve: keep the writes
stopped and ask for help. In case 2, naming the wrong side as gone keeps a live side
whose copy is old or partial, which md counts whole. A forced cancel of a
copy that is whole throws away the new side, which holds every block: in
case 2 the group then runs on one copy until the repair ends, and a second
fault in that time can lose data. Try the finish with no force first, as
"When" says.

When to stop: cases 3 and 4; you cannot tell whether a disk node is gone
for good; or the gateway refuses the repair of case 2 for a reason other
than the worker's repair.

Tested: not as a whole. The gateway suite, `integtest/gateway_test.sh`, on
fake agents, finishes a migration with `--force` while the new side's disk
node is stopped, after its plain finish is refused. The e2e suite's copy
case cancels one with `--force` after its cutover (above). The e2e suite's
react case, stage 10, runs what follows case 2, with the worker's own
finish of a whole copy in place of a forced end: the new side's disk node
goes silent, the primary cuts its path to the old side, and md fails the
leg's member while a host writes; then the worker finishes the migration,
so that the leg keeps the gone new side alone, and repairs the leg with a
fresh leg, no write failing. It passed in the lab on 2026-11-21. Not
tested: the finish with no force of a whole copy whose old side's disk
node is gone, a forced end of a copy that is not whole, the forced cancel
of case 2, and cases 3 and 4.

## Repair a leg by hand

Words: a **repair** replaces a bad leg of a RAID1 group with a new leg: a
fresh leg on another disk node, or a retired leg taken back
([Starting from a retired leg](#starting-from-a-retired-leg)). Its stages are
`REP_STAGE_CREATED`, `REP_STAGE_STARTED` once the primary has added the new
leg to the group's array, and `REP_STAGE_ABORTED` after an abort.

When to use: a leg of a RAID1 group is unhealthy, and you do not wait for
leg repair, which waits for the pool's `leg_unhealthy`, 1200 seconds at the
default, or for its side's `side_unhealthy`, 600 seconds; or you hold the
group's repairs yourself ([Taking the group's repairs over](#taking-the-groups-repairs-over));
or you want to choose where the new leg goes. A leg that works, even a slow
one, is refused: "leg <id> is healthy: use a migration to move it"
([Move a leg to another disk node](#move-a-leg-to-another-disk-node)). A leg
the primary keeps out for another identity has steps of its own
([A leg of another identity that the primary does not add](#a-leg-of-another-identity-that-the-primary-does-not-add)).

How to confirm: `dnvctl sp get` shows the leg's `err_epoch` not 0, and
`dnvctl cluster get-status` lists a line for the leg whose text holds
`health epoch set on the leg`: a `row` line, as in `row/error`, while the
primary's row for the leg reads an error, as for a dead leg, and a
`health` line otherwise.

What dnv does alone: leg repair replaces such a leg by itself once its
threshold passes, while another leg of the group is in sync and healthy.
The worker creates no repair while the group has one, an operator-in-charge
flag or a forced-start mark. It moves every repair from
`REP_STAGE_CREATED` to `REP_STAGE_STARTED` at every level, and it finishes
and cancels only its own repairs, at levels where reactions run.

Safe steps:

1. Create it:
   `dnvctl repair create --name <name> --grp <group id> --bad-legs <leg id>`,
   with one leg. `--dn-black` and `--dn-white` steer the new leg's disk
   node. The reply gives `rep_id` and `new_leg_id`. The gateway refuses: a
   name that starts with `__internal__`, which the worker's repairs use; a
   level of `NO_REDUND` or below; a pool without RAID; a name in use; a group
   with an open repair; a leg that is not in the group's list, that a
   migration moves, or that is healthy; and a fresh leg for a group that
   holds two retired legs, whose message says to delete one first
   ([Retired legs](#retired-legs)).
2. Wait until `dnvctl repair get --name <name>` shows `repair.rep_stage` as
   `REP_STAGE_STARTED`. The new disk node first zeroes the new side as for
   any new side, the worker marks it provisioned, and the primary adds the new
   leg to the array. md then copies onto it, and
   `dnvctl group inspect --grp <group id>` shows its copy position until the
   copy ends.
3. Finish it: `dnvctl repair finish --name <name>`. The new leg takes the
   bad leg's place in the group's list, and the bad leg becomes a retired
   leg, named in the reply's `retired_leg_id`. md can still be copying onto
   the new leg: wait until `dnvctl group inspect` shows every leg in sync
   before you touch another leg of the group.
4. To drop a repair that has not started, abort it with
   `dnvctl repair abort --name <name>`. Once `dnvctl repair get` shows
   `REP_STAGE_ABORTED`, which the primary confirms only from a running
   array, run `dnvctl repair cancel --name <name>`. A repair that has
   started cannot be aborted.

Steps that can lose data: `dnvctl repair create --force` lets an abort copy
over the bad leg and lets a take-back copy over a retired leg of another
identity. Your finish of
the worker's repair, whose name starts with `__internal__`, at a group that
holds two retired legs gives back the oldest one that is not kept, and its
copy with it ([Retired legs](#retired-legs)); the finish of your own repair
never does.

When to stop: the repair stays at `REP_STAGE_CREATED` and the primary's
report, `dnvctl cntlr inspect --id <primary cntlr id>`, shows a `waits` or a
`refusal` text in `cntlr_info.rep_id_to_repair_info` under the repair's
`rep_id` that you do not understand; or the group's other leg dies while md
copies onto the new leg
([A group whose in-sync leg died during a copy](#a-group-whose-in-sync-leg-died-during-a-copy)).

Tested: the e2e suite's copy case, stages 05 and 06: an operator's repair
with a fresh leg, created, started and finished while md copies onto it,
and its abort and cancel refused once it started; then a repair that takes
the retired leg back after the new leg's disk node died, and the delete of
a retired leg. Its react case, stage 09, takes a group's repairs over from
the worker. Both passed in the lab on 2026-11-21. The cn agent suite's
repair case runs the primary's part at the agent level.

## Retired legs

Words: a **retired leg** is a leg that left its group at the finish of a
repair. Its side keeps its extents on its disk node, and the data the leg
held when it left, until it is deleted. Its **keep flag** keeps the worker
from deleting it.

When: a group holds retired legs; a repair with a fresh leg is refused for
two retired legs; or a disk node to remove still holds a retired leg's side.

Confirm: `dnvctl retired-leg list` shows each retired leg with its `grp_id`,
its `side` and the side's disk node, `retired_epoch`, the time it left in
seconds since 1970, `keep`, and `partial`: md wrote onto the leg after a
repair took it back and was then cancelled, so it is no longer the copy it
was.

What dnv does alone: the worker deletes one retired leg a pass once it is
seven days old and not kept, only on a group whose array is whole, with no
repair and no health epoch on a leg or a side, and only at levels where
reactions run. So a group that does not come back whole keeps its retired
legs until you delete them. A group holds at most two. At that bound the
gateway refuses an operator's repair with a fresh leg, and the finish of
the worker's repair gives back the oldest one that is not kept; with both
kept, the worker's repair cannot finish until you clear a flag or delete
one. The write of a forced-start mark sets the keep flag on the group's
retired legs, and the finish of the repair of a leg of another identity
keeps the old leg.

When to keep one: a retired leg holds one group's blocks as of the time it
left, not a copy of a volume, and it costs its extents and one of the
1024 sides its disk node can hold. Keep it while it may be the group's only
other copy: while the group's array is not whole, or while md still copies
onto the new leg; a start from it is then the way back if the group's last
full leg dies ([Starting from a retired leg](#starting-from-a-retired-leg)).
Keep it in the two cases above, too: a forced start, and a leg of another
identity. Otherwise let the worker delete it, or delete it.

Safe steps:

- Keep it: `dnvctl retired-leg keep --leg <leg id>`.
- Clear the flag: `dnvctl retired-leg keep --leg <leg id> --clear`.
- Delete it: `dnvctl retired-leg delete --leg <leg id>`. The delete runs at
  every level, and it deletes a kept leg too.

Steps that can lose data: a delete, or a cleared flag that lets the worker
delete the leg, deletes its copy for good.

When to stop: you cannot tell whether a retired leg holds a copy you need
([Keeping the old copies](#keeping-the-old-copies)).

Tested: the e2e suite's copy case, stage 06, takes a retired leg back and
deletes a retired leg and its side; its react case reads the keep flag
that its first forced start set, and clears it on all but one retired
leg. Both passed in the lab on 2026-11-21. The gateway
suite checks the flag, and the worker's unit tests check the delete at
seven days and what holds it.

## Replace a cntlr by hand

When to use: a cntlr must move to another controller node while its own
still answers, as to empty a controller node ([Remove a node](#remove-a-node));
or its controller node is dead, and you do not wait for the cntlr
replacement, which comes after the pool's `cntlr_unhealthy`, 600 seconds at
the default.

How to confirm: `dnvctl sp get` shows the cntlr under `cntlr_list` with
its controller node's `addr_port`, and an `err_epoch` that is not 0 while
the worker's checks of it fail. `dnvctl cn inspect --addr <addr>` answers
only while that controller node's agent does.

What dnv does alone: the worker replaces a cntlr whose health epoch is as
old as `cntlr_unhealthy`, with a new cntlr in its slot on another controller
node. It never replaces a disabled cntlr. A disabled primary fails over at
the worker's next pass, with no threshold, to an enabled standby with no
health epoch whose agent does not refuse the worker's syncups, at the
levels where reactions run: `READWRITE`, `READONLY` and `NO_CLONE`
([Change a pool's level](#change-a-pools-level)). Disabling
the last enabled cntlr of a pool stops its IO, and nothing warns.

Safe steps, the new cntlr first and the delete last:

1. Read the pool: `dnvctl sp get` lists its cntlrs under `cntlr_list`, in
   the order of the ids in `sp_conf.cntlr_id_list`, the ids `--id` takes,
   each with `addr_port`, `cntlid_slot`, `primary`, `disabled`, `err_epoch`
   and `settling`, true while a new primary has not reported its devices
   built. `sp_conf.cntlid_slot_list` lists the pool's cntlid slots,
   the ranges of NVMe controller ids its exports use.
2. If the old cntlr is the primary and the pool's level is `NO_THINPOOL`
   or below, first step the level back to `NO_CLONE`, `READONLY` or
   `READWRITE` with `dnvctl sp set-level --level <level>`
   ([Change a pool's level](#change-a-pools-level)). From `NO_THINPOOL`
   down no reaction runs. So no failover follows step 4, and a disabled
   primary runs none of the pool's group arrays. Pick `READONLY` when the
   pool must go back to its old level in step 5: hosts then write nothing
   to the pool in between. Do not step the level
   back while other work holds it there, such as a forced start, the
   repair of a thin pool's metadata, or a damaged pool taken apart in
   stages. End that work first, or ask for help
   ([Before every forced start](#before-every-forced-start);
   [Repair a thin pool's metadata](#repair-a-thin-pools-metadata),
   step 8). The wait of step 3 also shows that the new cntlr has the
   level.
   Then create the new cntlr: `dnvctl cntlr create --slot <slot>`, with a
   slot of the list that no cntlr of the pool uses, the old one included. Add
   `--cn-black <addr>,<addr>` to keep it off those controller nodes; the
   gateway already keeps it off every controller node that holds a cntlr of
   the pool, and out of their locations when it can. The reply's `cntlr_id`
   is its id. The create is refused:
   - when the pool has four cntlrs, the most it can have
     (RESOURCE_EXHAUSTED): then do steps 4 and 5 for the old cntlr first,
     and steps 2 and 3 after, the pool keeping one cntlr fewer meanwhile;
   - for a slot of 8 or more (INVALID_ARGUMENT): give a slot of the list;
   - for a slot not in the list, or one another cntlr uses
     (INVALID_ARGUMENT): give a slot of the list that no cntlr uses; when
     every slot of the list is in use, add a free slot first
     ([Change a pool's cntlid slots](#change-a-pools-cntlid-slots));
   - when no controller node that is enabled, healthy and free of the pool
     has room (RESOURCE_EXHAUSTED): add a controller node first. Or, if the
     pool has another enabled cntlr, do steps 4 and 5 alone: the pool then
     has one cntlr fewer until you give it a new one, and a pool left with
     one cntlr has no standby to take over from its primary.
3. Wait until it is built: `dnvctl sp get` shows it with `err_epoch` 0, and
   `dnvctl cntlr inspect --id <new cntlr id>` shows `applied_revision` at
   least the pool's `sp_rev.revision` and each row under
   `cntlr_info.leg_id_to_leg` as `RES_STATUS_OK`, except the row of a leg
   that `dnvctl sp get` shows with an `err_epoch` that is not 0 while a
   cntlr other than the new one is the primary. The primary cannot read
   that leg either, so the fault is the leg's, and its row on the new
   cntlr can stay `RES_STATUS_ERROR`. So can the row of a leg one of
   whose sides `dnvctl sp get` shows with an `err_epoch` that is not 0,
   whichever cntlr is the primary, as when a disk node is down: the worker's
   checks of that side fail too. If that
   disk node is gone for good, replace it as [Replace a dead disk
   node](#replace-a-dead-disk-node) says. Two kinds of leg keep their
   `err_epoch`, and those of their sides, out of `dnvctl sp get`: a new
   leg of a running repair shows them in `dnvctl repair list`, under the
   repair's `new_leg_list`, and a leg of a clone's meta group, the group
   that holds a clone's copy records, in
   `dnvctl clone get --name <clone>`, under `clone.meta_grp`. At
   `NO_SIDE` and `DISABLE` the
   controller nodes drop their leg connections, and each row under
   `cntlr_info.leg_id_to_leg` reads `RES_STATUS_MISSING` with the reason
   `RES_REASON_SP_LEVEL` instead. The worker can make a new cntlr the
   primary before it is built, and the pool then serves no IO until it is
   built.
4. Disable the old cntlr:
   `dnvctl cntlr set-enabled --id <old cntlr id> --enabled=false`. If it is
   the primary, wait until `dnvctl sp get` shows another cntlr as primary
   with `settling` false, as step 3 of
   [A planned restart of an agent](#a-planned-restart-of-an-agent) says.
   A dead cntlr is disabled the same way.
5. Delete it: `dnvctl cntlr delete --id <old cntlr id>`. The gateway refuses
   the primary and an enabled cntlr (FAILED_PRECONDITION). If step 2
   stepped the pool's level back and the pool must stay at its old level,
   set that level again now: `dnvctl sp set-level --level <the old level>`.
   Between the two set-level calls the primary builds the thin pools
   again, hosts use the volumes as the raised level allows, and the worker
   runs its reactions, leg repair and auto-grow among them. The step down
   fails the hosts' reads and writes again
   ([Change a pool's level](#change-a-pools-level)).

The worker can replace the old cntlr while you do this, before step 4
disables it, whether its controller node is dead or still answers. It does
so at the levels where reactions run, once the old cntlr's health epoch is
as old as `cntlr_unhealthy`. Steps 4 and 5 then find it gone: the gateway
answers NOT_FOUND, "cntlr <id> not found". If you made the new cntlr of
step 2 first, the pool now has one cntlr more than before. To get back to
the count it had, disable one of the two new cntlrs that is not the
primary, and delete it the same way. If steps 2 and 3 were still to come,
skip them: the worker's new cntlr took the old one's place. Either way, if
step 2 stepped the pool's level back and the pool must stay at its old
level, set that level again, as step 5 says.

Steps that can lose data: the step down to the old level in step 5 fails
the hosts' reads and writes again, so an application on a host can lose
what it was writing then. No other step writes data away, but two mistakes
stop the pool's IO: disabling its last enabled cntlr, and deleting the old
cntlr while the new primary still shows `settling` true, since the old
cntlr can then no longer take the role back. A disabled cntlr whose
controller node is dead is never replaced by the worker, so do not leave
one standing in place of the new cntlr of step 2.

When to stop: the new cntlr is still not built, as step 3 reads it, a few
minutes after step 2, for any cause but a row that step 3 sets aside: as
when a row under `cntlr_info.leg_id_to_leg` stays `RES_STATUS_ERROR` for
another leg, `applied_revision` stays below the pool's
`sp_rev.revision`, or `dnvctl sp get` shows the new cntlr's own
`err_epoch` not 0. Do not go on to step 4, and ask for help. Or
no other cntlr becomes primary within a few check rounds after step 4, or
the new primary still shows `settling` true a minute after it became
primary: enable the old cntlr again,
`dnvctl cntlr set-enabled --id <old cntlr id>`, and ask for help.

Tested: the e2e suite's ops case, stage 03, gives the pool a third cntlr in
a new slot, sees its delete refused while it is enabled and its subsystem
leave the discovery answer once it is disabled, and deletes it; that half
is skipped when every controller node guest already holds a cntlr of the
pool. Its copy case, stage 07, disables the primary during a clone's copy,
sees the failover and enables it again. Both passed in the lab on
2026-11-21. No test runs the whole replacement as one sequence.

## Change a pool's cntlid slots

Words: a pool's **cntlid slots** are the ranges of NVMe controller ids its
exports use, numbered from 0 to 7. `dnvctl sp get` shows the pool's list as
`sp_conf.cntlid_slot_list`, 0 to 7 for a pool made with no `--slots`, and
the slot of each cntlr and side as `cntlid_slot`.

When: a new cntlr needs a free slot
([Replace a cntlr by hand](#replace-a-cntlr-by-hand)); a migration is refused
because the list has no slot but the old side's; or two pools joined by a
transfer and a clone need cntlrs in slots the other pool does not use
([A pool made before the block bound](#a-pool-made-before-the-block-bound)).

What dnv does alone: nothing moves. Every cntlr and side keeps its slot.
Each new side, of a grow, a repair or a clone, takes the list's first
slot, a migration's new side the first one that differs from the old
side's, and a new cntlr the slot `dnvctl cntlr create --slot` names, which
must be in the list.

Safe steps: `dnvctl sp set-cntlid-slots --slots <the whole new list>`, the
slots separated by commas, such as `0,1,2`. The new list replaces the old
one, in the order given. The gateway refuses with INVALID_ARGUMENT an empty
list, a value of 8 or more, a value given twice, and a list that drops a
slot that a cntlr or a side of a leg uses; the message names it, as in
"cntlid_slot_list drops slot 1, which cntlr 7 uses".

Steps that can lose data: none. A list of one slot refuses every migration
of the pool. A list that starts with another slot moves the sides made from
then on to that slot; no test does this.

When to stop: the gateway refuses a list for a slot that a cntlr or a side
uses, and you cannot move that cntlr or side to another slot first: keep
the slot in the list.

Tested: the e2e suite's ops case, stage 03, gives the pool a longer list,
sees lists that drop its cntlrs' slots refused, and makes a cntlr in the
new slot. It passed in the lab on 2026-11-21.

## Stripes and discards

Words: a pool's **stripe** and **data block** are as in
[A pool made before the stripe bound](#a-pool-made-before-the-stripe-bound)
and [A pool made before the block bound](#a-pool-made-before-the-block-bound).
A **discard** is a host's message that it no longer needs a range of a
volume, as `fstrim` sends for a file system's free space.

When: before you create a pool whose hosts count on discards to give space
back to the pool.

What dnv does alone: a pool's stripe and block are set when it is created
and never change. A pool that names no block takes its cluster's, 1 MiB at
the defaults. A pool that names no stripe takes its cluster's stripe, the
cluster's block up to 1 MiB at the defaults, and halves it until the stripe
times the slice count is at most 4 MiB
([the scope page](scope.md), section 10). A thin pool frees whole blocks
only, and each slice gets its own share of a discard. So a discard frees a
block on a slice only where its share there covers a whole block, aligned
to the block. With the stripe equal to the block, an aligned discard of
one block frees one block. With a smaller stripe, a discard frees a block
on a slice only when it covers nearly the slice count times the block, and
`fstrim` can report success and free nothing.

Safe steps: for a pool whose hosts count on discards, give
`dnvctl sp create` a `--stripe-size` equal to its `--block-size`, both in
bytes, with the stripe times `--slice-cnt` at most 4194304. At the defaults
this holds up to four slices; at eight slices a block and stripe of 524288
fit, and at 32 slices 131072. The gateway refuses with INVALID_ARGUMENT a
block that is not a power of two from 64 KiB to 4 MiB, a stripe that is not
a multiple of 4 KiB from 4 KiB to 1 MiB, a block that is not a whole number
of stripes, and a stripe times slice count above 4 MiB, a refusal that names
the largest stripe that fits. A smaller block gives the thin pool more
blocks to map for the same space. `dnvctl sp get` shows the pool's
`sp_conf.bdev_conf.dm_pool_conf.data_block_size` and
`sp_conf.bdev_conf.dm_raid0_conf.stripe_size`; `dnvctl sp inspect` shows
each slice's `used_data_bytes` before and after an `fstrim`.

Steps that can lose data: none.

Tested: the e2e suite's ops case, stage 10: an ext4 file system on a
volume, files written and removed, `fstrim`, and the pool's used bytes drop
by the files' whole blocks where the stripe is the block; where it is
smaller the drop is logged, not checked. The cn agent suite's discard case,
at one, two and eight slices, sees each discard free exactly the blocks
this rule gives; [the scope page](scope.md), section 10, has the numbers.
Both passed in the lab on 2026-11-21. A pool made with a smaller block to
keep its stripe equal to it is not tested.

## md's copy speed

Words: md **copies** onto a leg that joins a group's array: a repair's new
leg, a leg a forced start wiped, or a leg back from a short absence. Two
settings bound its speed, in KiB per second: a minimum and a maximum.

When: a copy slows the hosts' IO too much, or you want it to end sooner;
after a dead disk node starts a copy in every group it held, all at about
the same time.

What dnv does alone: the agents set no copy speed. md's own settings rule:
the sysctls `dev.raid.speed_limit_min` and `dev.raid.speed_limit_max` of each
controller node, 1000 and 200000 on the lab's controller nodes, and an
array's own `md/sync_speed_min` and `md/sync_speed_max`, which override them
until the array stops. A failover and a step of the level to `NO_REDUND`
and back start the array anew, with no setting of its own. The
md check runs at the same settings. A migration's copy is not md's: its
speed is set at its create ([Move a leg to another disk node](#move-a-leg-to-another-disk-node)).

Safe steps, as root on the primary's controller node:

1. Find the array: `res_name` of the group's row under
   `cntlr_info.grp_id_to_md_raid` in
   `dnvctl cntlr inspect --id <primary cntlr id>`, such as `/dev/md/<name>`.
   `readlink -f /dev/md/<name>` prints the kernel's name for it, such as
   `/dev/md126`.
2. Read it: `cat /sys/block/md126/md/sync_speed_min` and `sync_speed_max`
   in the same folder print each limit with `(system)` while the array
   follows the sysctls, or `(local)` once it has its own; `sync_speed` and
   `sync_completed` show the copy's speed and how far it is.
3. Slow or speed one array: write a number to
   `/sys/block/md126/md/sync_speed_max`, and to `sync_speed_min` when it
   must stay below the minimum, such as
   `echo 10000 > /sys/block/md126/md/sync_speed_max`. Write `system` to
   either to give it back to the sysctls.
4. For every array of a controller node:
   `sysctl -w dev.raid.speed_limit_max=<KiB per second>` on it, and on
   every controller node a failover can move the arrays to.
   It lasts until the machine restarts unless it is in a file under
   `/etc/sysctl.d/`.

Steps that can lose data: none writes data, but a low maximum keeps a group
longer on one full copy, and a second fault in that time can lose it.

Tested: the e2e suite's copy case, stages 05 and 06, slows one array's copy
by writing its own two settings on the primary, reads them back as
`(local)`, and gives them back with the word `system`; it passed in the lab
on 2026-11-21, where the sysctls read 1000 and 200000. The sysctls are not
tested.

## The md check's mismatch count

The md check is md's own read of both legs of each group's array, which the
worker asks of the primary for one group at a time, every 30 days at the
default (`dnvctl cluster create --md-check-days`, where 0 turns it off). md
counts in `/sys/block/<kernel name>/md/mismatch_cnt` the sectors where the
legs differ, with the array's kernel name, such as `md126` when
`readlink -f` prints `/dev/md126` ([md's copy speed](#mds-copy-speed),
step 1). The count means nothing, and dnv does not read it: dnv zeroes a
data leg only in its first blocks, so the legs differ wherever the pool has
not written, and the pool never reads a block it has not written. What
counts is md's bad-block list of each leg, which `dnvctl group inspect`
shows and `dnvctl cluster get-status` lists as a `group` line. dnv never
asks md for its `repair` action, and nothing here needs it.

Tested: the cn agent suite's redund case, stage `mdcheck`, runs a check
over a leg with sectors that fail and reads a count of 8 or more where a
read failed, besides the regions never written. It passed in the lab on
2026-11-21.

## One pool for each volume

When: before you give each volume a pool of its own, as the CSI driver,
dnv's driver for Kubernetes, does for every Kubernetes volume, rather than
many volumes in one pool. A pool
per volume keeps each volume's data apart, as dnv promises between pools.

What it costs, for the CSI driver's shape of one slice, RAID1 and two
cntlrs, from dnv's ceilings and code:

- Two cntlrs, on two controller nodes. A controller node holds at most
  256 cntlrs, so a cluster holds at most 128 such volumes for each
  controller node it has. A cluster holds at most 4096 pools, a deleted
  one counted until its drain ends.
- Four sides at the create, a side on each leg of the meta group and of the
  first data group; two more for each group auto-grow adds, and one more for
  each migration's new side, repair's new leg and retired leg. A disk node
  holds at most 1024 sides.
- Whole extents: one for the meta group, and on every leg at least one for
  the first data group, however small the volume. The CSI driver makes the
  first data group an eighth of the volume, rounded up to whole extents, and
  auto-grow adds groups of that size, so a full volume has 4 to 18 legs,
  besides those of a meta group auto-grow adds.
- Every cntlr connects to every leg: 8 to 36 NVMe-oF connections from the
  controller nodes to the disk nodes for each volume.
- The worker checks each cntlr and each side every check round, 2 seconds
  at the default: 6 to 20 such objects for each volume.

How to confirm: `dnvctl cluster get-capacity --leg-cnt 2 --cntlr-cnt 2 --slice-cnt 1`
says how many more such pools fit and which limit the cluster reaches
first ([The cluster's capacity](#the-clusters-capacity)).

What dnv does alone: the worker drives each pool on its own. Building many
pools at once makes failovers of pools that have built
([the introduction](introduction.md#status-and-limits) gives the times).

Safe steps: a storage class, the Kubernetes record that names the cntlrs of
each volume's pool, with one cntlr, `cntlrs: "1"`, doubles the volumes the
controller nodes can hold, at the cost of
[A one-cntlr Kubernetes volume after a cntlr replacement](#a-one-cntlr-kubernetes-volume-after-a-cntlr-replacement).
Many volumes in one pool share its cost, and their data is not kept apart
from each other.

Tested: the e2e suite's many-pools case, which runs only when it is named,
built and cut 8,
32 and 64 pools of this shape on the lab VMs; the csi suite's volume case
checks the capacity Kubernetes shows; the size step tool,
`integtest/size_step.sh`, reached 256 cntlrs on one controller node and
1024 sides on one disk node with no IO. The counts for each volume above are
read from the code, not measured.

## Change a volume's hosts

Words: a subsystem's **list** is the hosts that may connect to it, by host
NQN. A host **taken off** the list is one that the list held before a
change and does not hold after it. A host's **connection** to a controller
node is the NVMe target's record of that host's link to one subsystem
there; a host holds one for each controller node it reaches the volume
through.

When: a host must stop using a volume, or another host must start.

How to confirm the change: `dnvctl ss list` shows each subsystem by its NQN,
with its `ss_id` and its list, `allowed_hosts`. After the change, read the
pool's revision with `dnvctl sp get`, `sp_rev.revision`, and its cntlrs.
Then run `dnvctl cntlr inspect --id <cntlr id>` for each cntlr, primary and
standby. Once its `applied_revision` is that revision or a later one, the
subsystem's row, under `ss_id_to_subsystem` at the subsystem's `ss_id`,
reads `RES_STATUS_OK` when no host that is off the list is still connected
to that controller node. It reads `RES_STATUS_PENDING`, with the details
`host <nqn> still connected`, while the connection of such a host is still
there after the cn agent's wait. It reads `RES_STATUS_ERROR` when the node
cannot read the connections, or cannot end them under a lockdown the cn
agent can see (When to stop, below): its details start
`nvmet connections not read:` or `nvmet connections not ended:`, followed by
the cause. On a controller node whose cn agent logged
`agent host cut disabled` when it started, which it does when it starts with
`--ignore-host-disconnection` and finds one of the causes that the check's
`nvmet host cut (debugfs)` line names ([Check a node before it
joins](#check-a-node-before-it-joins), step 3), the row reads
`RES_STATUS_OK` all the same.

What dnv does alone: the gateway writes the new list and raises the pool's
revision, and the worker sends each cntlr its syncup. Each cn agent then
takes the hosts off the subsystem on its node, so that the kernel refuses
their next connect, and ends the connection of every host that is off the
list. In each converge it waits for those connections to go, about two
seconds at most for all of them together. It does both before it changes the
device under any namespace of the subsystem, or a transfer's device. While
the connection of a host that is off the list is still there after that
wait, or while the node cannot read or end the connections, every namespace
of the subsystem on that controller node moves to ANA inaccessible, the
primary's too, so that no host is served there, the hosts left on the list
among them. A move that fails is the exception: the cn agent logs it as
`ana transition failed`, the namespace can stay where it was, and on a
serving primary it then goes on serving every host still connected, the host
taken off among them, until a later converge ends that host's connection or
moves the namespace. While the row reads `RES_STATUS_PENDING`, the worker
sends the syncup again on every check round, which tries to end the
connections again. While it reads `RES_STATUS_ERROR`, the worker sends the
syncup every second round, and that ends nothing while the cause lasts. A
pending row starts no failover, so the hosts wait until the connection ends.
An error row on a primary makes the worker fail it over at the pool's
threshold, as for any error row, where the pool has an enabled standby with no
health epoch whose agent does not refuse the worker's syncups. A pool with
no such standby, such as a pool with one cntlr,
waits for the cntlr replacement, which comes after the pool's
`cntlr_unhealthy`, 600 seconds at the default. An error row on a standby
brings its replacement in time. So a standby that becomes the primary serves
no host that was taken off, save one that connected just as it was taken off
([the scope page](scope.md#54-hosts-and-the-kubernetes-driver)). The host
taken off sees its controller fail. It connects again after its reconnect
delay, about ten seconds by default, is refused, and its kernel then fails the
IO it held. A host left on the list keeps its connection and its IO. On a
controller node whose cn agent logged `agent host cut disabled` when it
started, the cn agent ends no connection until it is started again, even once
the kernel can: a host taken off keeps its connection and its IO until it
disconnects, and only its next connect is refused ([the scope
page](scope.md#54-hosts-and-the-kubernetes-driver)). A cn agent that logged no
such warning when it started ends them where the kernel lets it, with the flag
or without it (When to stop, below).

Safe steps:

1. On each host you take off, stop the volume's users and unmount it.
2. Run `dnvctl ss set-hosts --nqn <subsystem nqn> --hosts <nqn>,<nqn>`.
   `--hosts` is the whole new list: name every host that stays. An empty
   `--hosts=` takes every host off.
3. Confirm the change on every cntlr, as above.
4. On a host you took off, `nvme disconnect -n <subsystem nqn>` drops what
   is left of its paths. A host that uses nvme-stas drops them itself once
   the cdc no longer lists the subsystem to it.
5. Where a controller node's cn agent logged `agent host cut disabled` when
   it started, a host you took off keeps writing until step 4 runs on it,
   even if the kernel can end connections now. Run step 4 before you give
   the volume to another host.

Steps that can lose data: a host taken off while it writes has its writes
fail. A write that failed may or may not have reached the volume, so its
data is unknown. A host left off the new list by mistake loses its
connection the same way: give the list again with that host, and it can
connect again.

When to stop: the row does not read `RES_STATUS_OK` a minute after the
change. Meanwhile that controller node serves no host the subsystem's
volumes. The exception is a namespace whose move to ANA inaccessible failed
(the record `ana transition failed`): a serving primary can go on serving it
to every host still connected.

- `RES_STATUS_PENDING`, `host <nqn> still connected`: the host's IO is held
  on the controller node, and its connection ends only once that IO ends. A
  row of the cntlr in error, such as a namespace's device, often names why;
  the connection ends once that row is fixed. When the cn agent's log holds
  the record `ending a stray connection failed` for that host, the kernel
  refuses to end the connection instead: see the bullet on
  `nvmet connections not ended:`. No failover comes, so the volume's hosts
  wait. To serve the volume from another node, disable that cntlr with
  `dnvctl cntlr set-enabled --id <cntlr id> --enabled=false`, but where it
  is the primary, only once you have checked, as step 3 of [A planned
  restart of an agent](#a-planned-restart-of-an-agent) says, that the pool
  has an enabled standby with no health epoch: with none, no other cntlr
  becomes the primary, and the pool's IO stops. A disabled cntlr goes on
  ending the connection. Once its row for the subsystem reads
  `RES_STATUS_OK` in `dnvctl cntlr inspect --id <cntlr id>`, enable it again
  with `dnvctl cntlr set-enabled --id <cntlr id>`: it comes back as a
  standby. Until then the pool has one cntlr fewer to fail over to.
- `RES_STATUS_ERROR`, `nvmet connections not read:`: debugfs was unmounted,
  which `mount -t debugfs none /sys/kernel/debug` mends, or the NVMe target
  was loaded again without the option. The row of every subsystem on that
  controller node then reads so, with a host taken off or none, and the node
  serves none of its volumes. A restart of the cn agent without
  `--ignore-host-disconnection` then refuses to start and names the cause. A
  restart with the flag starts, logs `agent host cut disabled` and ends no
  connection, so the subsystem's volumes are served there again to every
  host still connected, the host taken off among them: mend the cause before
  you restart the cn agent.
- `RES_STATUS_ERROR`, `nvmet connections not ended:`: a lockdown was raised
  while the agent runs, and the kernel refuses to end a connection under it.
  The cn agent's log names each refusal in the record
  `ending a stray connection failed`. A check round between two converges
  shows the same row, since a read also reads the lockdown mode once it
  finds such a connection. A restart of the cn agent without
  `--ignore-host-disconnection` then refuses to start and names the
  lockdown; one with the flag starts, and the volumes are served there
  again, to the host taken off too, as in the bullet above. The cn agent
  reads the lockdown mode from /sys/kernel/security. Where that is not
  mounted, the agent cannot see the lockdown: the row reads
  `RES_STATUS_PENDING`, `host <nqn> still connected`, with an
  `ending a stray connection failed` record at each converge, a restart
  starts, and the kernel keeps refusing until a reboot without the lockdown.
  Run step 4 on the host taken off: its own disconnect ends the connection,
  and the row then reads `RES_STATUS_OK`. Until then no failover comes: to
  serve the volume from another node meanwhile, disable that cntlr as the
  first bullet says.

For both rows that read `RES_STATUS_ERROR` the worker fails a primary on
that node over to an enabled standby with no health epoch whose agent does
not refuse the worker's syncups, where the pool has one, and replaces the
cntlr after the pool's `cntlr_unhealthy` ([the scope
page](scope.md#61-cases-where-dnv-does-not-come-back-alone-today)).
Mend the cause before the node serves the pool again. Ask for help.

Tested: the cn suite's smoke case, on the lab's controller nodes, whose
NVMe target is built with the option, takes one of two hosts that write
off a volume's list: the converge ends that host's connection in a
fraction of a second, its next write fails about 12 seconds later, its
reconnect is refused and its controller goes, while the other host writes
on; then it takes the last host off too. Its redund case takes a host off
the lists of a primary and a standby, and both end its connections. Unit
tests check a connection that does not end, a read of the connections that
fails, the namespaces moved inaccessible meanwhile, and the start with and
without the flag. The smoke and redund cases passed in the lab on
2026-11-20. No suite runs `dnvctl ss set-hosts` on a connected host.

## Snapshots

Words: a snapshot's **origin** is the volume it is made from. A volume is
**recorded as created** once its primary has built it in every slice;
`dnvctl td list` shows `created`.

When to use: to keep a volume's blocks as they are at one moment, for
example before a risky change on a host, or to copy them off while the
volume goes on serving.

What dnv does alone: the primary makes the snapshot in every slice within
one pause of the origin's IO: writes in flight end first, and new writes of
the origin's hosts wait for that moment. The worker records the snapshot as
created once the primary reports it in every slice. A try that fails is
sent again every second check round, and each try pauses the origin's IO
for a moment again; nothing ends the tries. A primary crash between two
slices can leave the snapshot torn: its slices then hold its data from
different points in time. So does a slice that makes the snapshot only at
a later try than the other slices. The worker records the tear from the
primary's report, and the status read then shows a `torn` line for the
snapshot.

Safe steps:

1. Make the origin's data whole on the host first: stop the application's
   writes and run `sync`, or freeze a mounted file system with
   `fsfreeze -f <mount point>`. A snapshot holds what the volume held, as
   after a power cut: data that is still in a host's memory is not in it.
2. Create it: `dnvctl td create --name <snapshot> --ori <origin> --size 0`.
   A size of 0 gives the snapshot the origin's size. The gateway refuses the
   snapshot with the error code FAILED_PRECONDITION while the origin is not
   recorded as created, and while the origin is the destination of a clone
   that has not left. A snapshot counts as a volume: a pool holds at most
   1024 volumes, and the next create is refused with RESOURCE_EXHAUSTED. A
   snapshot of a snapshot is allowed.
3. Wait until `dnvctl td list` shows `name_to_td.<snapshot>.created` true.
   The primary makes the snapshot after the create has answered, so keep
   the application's writes stopped, or the file system frozen, until
   then. When it is not created within about a minute, go on to step 4 all
   the same, since the host's writes wait while the file system is frozen.
   The primary goes on trying, and a snapshot it makes after step 4 holds
   the volume as at that later moment, as after a power cut, not as at the
   freeze. When it made the snapshot in some slices at an earlier try
   than in the others, the slices hold different moments, and the
   snapshot is torn. Never take it for the copy from before your change,
   and run step 5 again once it is created. Then see When to stop.
4. Let the writes go on, or thaw the file system:
   `fsfreeze -u <mount point>`.
5. Run `dnvctl cluster get-status`. A `torn` line that names the snapshot
   means that it holds no single point in time: delete it with
   `dnvctl td delete --name <snapshot>` and make it again.

A snapshot is a volume of its own. A namespace exports it as any volume,
or shows it to the origin's hosts in the origin's place (see [Park a
namespace](#park-a-namespace)), but not both at once: the gateway lets
two namespaces export one volume, and two hosts would then write it. The
origin can be deleted once each of its snapshots is recorded as created,
and the snapshots stay.

Steps that can lose data: deleting a snapshot deletes its blocks. A torn
snapshot whose mark was lost reads as whole, with no sign ([the scope
page](scope.md), section 5.1). After an etcd restore that skipped
`dnvctl cluster advance-ids`, or in a pool that made more than 1,048,576
volumes, snapshots included, after the backup, a new snapshot can take the
thin id of a volume made after the backup and serve its data
([Restore etcd from a backup](#restore-etcd-from-a-backup)).

When to stop: the snapshot stays `created` false for minutes. When the
origin's hosts see short pauses again and again meanwhile, the primary
fails to make it in a slice. When they do not, a slice's thin pool is
not ready or takes no change, so the primary sends that slice no try and
pauses nothing for it: the thin pool's row under `slice_id_to_dm_pool`
does not read `RES_STATUS_OK` or shows `ro` in its details, or a row of
that slice under `slice_id_to_meta` or `slice_id_to_data`, its metadata
and data devices, does not read `RES_STATUS_OK` ([A pool's
space](#a-pools-space)). A thin pool whose device has only the block
layer's read-only flag set shows none of these signs. dnv never sets that
flag, but `blockdev --setro` by hand does. Then, on the controller node
of the primary, which alone builds the thin pools,
`blockdev --getro /dev/mapper/<res_name>`, with the `res_name` of the
slice's row under `slice_id_to_dm_pool`, prints 1. The snapshot waits
until the primary converges for another reason, such as the next change
to the pool, and `blockdev --setrw` on that device ends the wait. Or the
pool's level is `NO_THINPOOL` or further down ([Change a pool's
level](#change-a-pools-level)), where the primary makes no snapshot.
`dnvctl cntlr inspect --id <primary cntlr id>` shows those rows, and the
rows of the snapshot in `td_id_to_thin_info`. Ask for help. When some
slices make the snapshot at a later try than the others, the slices hold
different moments, and the snapshot is torn: once it is created, run
step 5 again. A snapshot that you no longer want can be deleted while it
is not created, which ends the tries and their pauses:
`dnvctl td delete --name <snapshot>`.

Tested: the e2e suite's ops case, stage 06, makes a snapshot with
`--size 0`, waits until it is recorded as created, and checks its size and
its origin; its soak case makes and deletes a snapshot in each round. The
cn agent suite, `integtest/cnagent_test.sh`, checks the pause (stage
`snapshot`), that the snapshot holds the origin's blocks and nothing else
(`snapio`), that a later write to the origin never reaches it
(`isolation`), and a torn snapshot and its mark (`halfsnap`, `tear`,
`recorded`). The e2e suite passed in the lab on 2026-11-21. The host's
`sync` or `fsfreeze` of step 1 and a snapshot of another size than its
origin's are not tested.

## Park a namespace

Words: to **park** a namespace is to set it suspended: every cntlr then
moves it to ANA inaccessible and puts a device that fails all IO under it,
so that no host can read or write the volume through it, while the hosts
keep the namespace and its path. A **parked** namespace serves again once
it is resumed.

When to use: before you read a volume's written blocks with
`dnvctl td get-bm`, which refuses a volume that a namespace still serves;
and around a change of the volume a namespace exports, with
`dnvctl ns set-dev`, such as showing a snapshot to the hosts in the
volume's place.

What dnv does alone: the worker sends the change to each cntlr, and the
namespace stays parked until you resume it, with these exceptions. When a clone's copy into the volume is whole, the worker sets
every namespace of that volume back to serving, and `dnvctl clone delete`
does the same. While a clone made with `--auto-resume` copies into the
volume, its namespaces serve whatever their flag says. `dnvctl xfer
delete` without `--force` parks the namespace a transfer exported, and a
transfer made with `--auto-suspend` parks it while the transfer lasts.

Safe steps:

1. On each host that uses the volume, stop its users and unmount it. While
   the namespace is parked, a host's IO to it waits and does not fail, and
   a `sync` on that host waits with it.
2. Park it: `dnvctl ns set-suspended --nqn <nqn> --idx <namespace index>`.
3. Confirm: in `dnvctl cntlr inspect --id <primary cntlr id>`, the
   namespace's row in `ns_id_to_dm_linear`, under the `ns_id` that
   `dnvctl ss list` shows for it, reads `RES_STATUS_OK` with the reason
   `RES_REASON_PARKED`.
4. Make the change. To show another volume to the hosts in this one's
   place, run `dnvctl ns set-dev --nqn <nqn> --idx <namespace index> --td
   <volume>`. The other volume must be of the same size: the gateway
   refuses another size with the error code FAILED_PRECONDITION, since the
   hosts keep the size they read. It also refuses while the namespace is a
   transfer's origin, and a volume that a transfer exports. It does not
   check that the other volume is recorded as created: wait for that in
   `dnvctl td list` first. Nor does it check that no other namespace
   exports the other volume: `dnvctl ss list` shows each namespace's
   `td_id` in `ns_list`, and `dnvctl td list` each volume's, so pick a
   volume whose `td_id` no namespace shows. With a snapshot as the other
   volume, the hosts read and write the snapshot from then on, and the old
   volume keeps its blocks as they were, unchanged, until a namespace
   points at it again or it is deleted.
5. Resume it: `dnvctl ns set-suspended --nqn <nqn> --idx <namespace index>
   --suspended=false`. Write `=false`: `--suspended false`, with a space,
   is refused.
6. On the hosts, mount the volume again.

Steps that can lose data: a change of the volume under a namespace that a
host still has mounted. The host keeps the old volume's blocks in its
memory, mixes them with the new volume's, and can write them to the new
volume later. A change onto a volume that another namespace exports, such
as a snapshot exported to another host to copy it off: two hosts then
mount and write one volume, and each host's file system overwrites what
the other's wrote. A park leaves the data as it is, but a write a host
still sends during the park waits, and lands on the volume the namespace points
at when it serves again: the new one after step 4.

When to stop: the row does not read `RES_REASON_PARKED` a minute after the
park, or reads `RES_STATUS_ERROR`: resume the namespace and ask for help
before you change the volume.

Tested: the e2e suite's ops case, stage 07, parks a namespace, sees it ANA
inaccessible on the host with the host's device kept, resumes it and checks
the data; it then points the namespace at a new volume of the same size,
which the host reads as zeros, and back, and checks the data again. The
gateway suite, `integtest/gateway_test.sh`, checks that the bitmap read
refuses a volume whose namespace is not parked. The e2e suite passed in the
lab on 2026-11-21. The stage points the namespace at the new volume while
it serves, after the resume: the change of step 4 on a parked namespace,
and a namespace pointed at a snapshot, are not tested.

## Move a volume to another pool

Words: the **source pool** holds the volume now and the **destination
pool** is to hold it. The **origin** is the volume to move, and the
**origin namespace** the namespace that exports it. This origin is not a
snapshot's origin, as in [Snapshots](#snapshots): it need have no
snapshot. A **host NQN** is the
name an NVMe host gives when it connects; a controller node has one of its
own, which it uses to read a transfer. The move is a transfer in the source
pool and a clone in the destination pool, as [the
introduction](introduction.md#transfer-and-clone) explains. A **region** is
one data block of the destination pool, the unit in which the clone
copies. dm-clone calls a region it has copied **hydrated**. Below, `--sp
<source>` and `--sp <destination>` pick the pool of each command.

When to use: a volume must leave its pool while its hosts keep using it,
for example to empty a pool before you delete it, as [A pool made before
the block bound](#a-pool-made-before-the-block-bound) does.

Before you start, check five things:

1. The two pools' layouts. `dnvctl --sp <pool> sp get` shows a pool's
   stripe, `sp_conf.bdev_conf.dm_raid0_conf.stripe_size`, its data block,
   `sp_conf.bdev_conf.dm_pool_conf.data_block_size`, and its slice count,
   the number of entries of `sp_conf.slice_id_list`. Each pool's stripe
   times its slice count gives a size, and the least common multiple of the
   two sizes must be 4 MiB or less. When that multiple is smaller than the
   destination's data block, or no whole number of it, the data block
   counts instead, and must be 4 MiB or less. Otherwise the clone is refused with the error code FAILED_PRECONDITION
   only after the transfer has parked the origin. The simple way: give the
   destination pool the source's stripe and slice count.
2. The two pools' cntlrs: no controller node may hold a cntlr of both
   pools, and their cntlid slots, the ranges of NVMe controller ids a
   pool's exports use, must differ. `dnvctl sp get` of each pool shows
   each cntlr's `addr_port` and `cntlid_slot`. Step 1 of [A pool made
   before the block bound](#a-pool-made-before-the-block-bound) says how to
   create the destination pool so, and how to move a cntlr that breaks
   this.
3. A subsystem's list of hosts and a transfer's hold at most 8 host NQNs
   each; a longer list is refused with the error code INVALID_ARGUMENT.
   The transfer's list must hold the destination pool's controller nodes.
4. A pool holds at most four transfers, and at most 64 clones. A transfer
   is refused with the error code FAILED_PRECONDITION while another
   namespace also exports the origin's volume, and while the volume is the
   destination of a clone that has not left, as for a volume moved into
   the source pool a short while before. The refusal names that clone:
   wait until its `dnvctl --sp <source> clone get` answers NOT_FOUND. A
   clone takes a clone meta group, the group of the destination pool that
   holds the copy's records: one extent on each of as many disk nodes as
   the pool's groups have legs. It is refused with
   the error code RESOURCE_EXHAUSTED when too few disk nodes have room,
   and with FAILED_PRECONDITION when the data part of one extent cannot
   hold the copy's records, 4 MiB and one byte for each region of the new
   volume; the message then names the largest new volume it holds. Both
   refusals come at step 10, after the transfer has parked the origin.
   Before the transfer, read `addr_port_to_dn` of `dnvctl cluster
   get-capacity` ([The cluster's capacity](#the-clusters-capacity)): in
   as many locations as the destination pool's groups have legs, a disk
   node must show `disabled` false, `err_epoch` 0, a `free_ext_cnt` above
   0 and a `side_cnt` below 1024, or the first refusal comes. No read
   shows the second refusal before the create.
5. The origin's size, `size` in `dnvctl --sp <source> td list`, must be a
   whole multiple of the destination pool's stripe times its slice count,
   or step 2 is refused with the error code INVALID_ARGUMENT. A
   destination volume of another size cannot take the copy: the clone's
   build fails over a larger one, since the clone reads the source as far
   as the destination reaches, and a smaller one is copied short. The
   simple way of check 1 always passes this check.

Safe steps:

1. Read the origin: `dnvctl --sp <source> ss list` shows the subsystem's
   NQN, its list of hosts, `allowed_hosts`, and in `ns_list` the origin
   namespace's `ns_idx`, `td_id`, `dev_uuid` and `dev_nguid`.
   `dnvctl --sp <source> td list` shows the volume with that `td_id`, its
   name and its `size`.
2. In the destination pool, create a new volume of the same size:
   `dnvctl --sp <destination> td create --name <new volume> --size <size>`,
   and wait until `dnvctl --sp <destination> td list` shows it recorded as
   created. It must be a new volume that no host ever wrote: the copy skips
   the regions the source never wrote, and those keep what the new volume
   holds.
3. In the destination pool, create the subsystem with no hosts, if that
   pool does not hold it yet: `dnvctl --sp <destination> ss create --nqn
   <nqn>`.
4. Create the namespace there, parked, at the origin's index and with its
   identity: `dnvctl --sp <destination> ns create --nqn <nqn> --idx
   <ns_idx> --td <new volume> --uuid <dev_uuid> --nguid <dev_nguid>
   --suspended`.
5. Only then give the destination's subsystem the origin's list of hosts:
   `dnvctl --sp <destination> ss set-hosts --nqn <nqn> --hosts
   <allowed_hosts>`, the NQNs separated by commas.
6. Work out the host NQNs of the destination pool's controller nodes. For
   each controller node that `dnvctl --sp <destination> sp get` shows in
   `cntlr_list[].addr_port`, read its id, `cn_conf.cn_id` of `dnvctl cn get
   --addr <addr_port>`, and the cluster's id, `cluster_id` of `dnvctl
   cluster get`. The NQN is
   `nqn.2024-01.io.dnv:1:<cluster id>:<controller node id>`, each id written
   in hex with 16 digits: `printf 'nqn.2024-01.io.dnv:1:%016x:%016x\n'
   <cluster_id> <cn_id>`. No command prints it.
7. On each host of the volume, stop its discards, as "Steps that can lose
   data" below says. Then, in the source pool, create the transfer with
   those NQNs alone:
   `dnvctl --sp <source> xfer create --name <transfer> --ori-nqn <nqn>
   --ori-idx <ns_idx> --hosts <the NQNs of step 6> --auto-suspend`. Its
   reply holds the transfer's id, `xfer_id`. `--auto-suspend` parks the
   origin namespace, and the hosts' IO waits from now until the clone
   serves; without the flag, which is off by default, the hosts go on
   writing the origin during the copy. Check the transfer with
   `dnvctl --sp <source> xfer get --name <transfer>`: `allowed_hosts` and
   `auto_suspend` true.
8. Work out the transfer's NQN:
   `nqn.2024-01.io.dnv:4:<cluster id>:<source pool id>:<transfer id>`, each
   in hex with 16 digits:
   `printf 'nqn.2024-01.io.dnv:4:%016x:%016x:%016x\n' <cluster_id> <sp_id>
   <xfer_id>`, with the source pool's `sp_conf.sp_id` of `dnvctl --sp
   <source> sp get`. No command prints it either.
9. Read the address of the source pool's primary: in `dnvctl --sp <source>
   sp get`, the entry of `cntlr_list` whose `primary` is true, the four
   fields of its `nvme_tr_conf`: `tr_type`, `adr_fam`, `tr_addr` and
   `tr_svc_id`.
10. In the destination pool, create the clone:
    `dnvctl --sp <destination> clone create --name <clone> --dst-td <new
    volume> --src-nqn <transfer NQN> --src-idx <ns_idx> --src-slices <the
    source's slice count> --src-stripe <the source's stripe> --src-block
    <the source's data block> --src-tr-type <tr_type> --src-adr-fam
    <adr_fam> --src-tr-addr <tr_addr> --src-tr-svc-id <tr_svc_id>
    --auto-resume`. Type the four address flags: without them `dnvctl`
    sends `tcp`, `ipv4`, 127.0.0.1 and 4420, so the destination's primary
    connects to its own machine, or by the wrong address family. Type
    `--auto-resume`, which is off by default: without it the
    destination's namespace stays parked until the copy is whole, and the
    hosts have no path that serves for that time. For a transfer of this
    cluster, the gateway reads the source pool that the transfer NQN
    names and refuses, with the error code INVALID_ARGUMENT, a slice
    count, a stripe or a data block other than that pool's; the message
    names both. Nothing checks them for a transfer of another cluster.
    The clone reads through the source's primary alone. Run
    `dnvctl --sp <destination> clone get --name <clone>` once now, and see
    that it shows the clone.
11. To shorten the copy, give the clone the list of the regions the origin
    never wrote ([Read a volume's written
    blocks](#read-a-volumes-written-blocks), below). Without it the clone
    copies every region.
12. Hosts that use nvme-stas find the destination's path through the cdc;
    a host that connects by hand connects to the destination's controller
    nodes as it did to the source's. The destination's namespace serves
    them, ANA optimized, once the clone serves, and their IO goes on there.
13. While the clone copies, read both pools' `dnvctl sp get` again from
    time to time. If the source pool shows another primary than in step
    9, point the clone at it:
    `dnvctl --sp <destination> clone set-tr --name <clone> --src-tr-type
    <tr_type> --src-adr-fam <adr_fam> --src-tr-addr <tr_addr>
    --src-tr-svc-id <tr_svc_id>`, with the four address flags as in
    step 10. If a cntlr of the destination pool moves to a controller
    node that the transfer's list lacks, give the transfer its new list:
    `dnvctl --sp <source> xfer set-hosts --name <transfer> --hosts <the
    whole new list>`. A cntlr replacement, which the worker makes once a
    cntlr has been unhealthy for its pool's `cntlr_unhealthy`, 600 seconds
    at the default, can place a cntlr of either pool on a controller node
    that holds a cntlr of the other, which check 2 forbids. So compare the
    `addr_port` of both pools' cntlrs too. When two share a controller
    node, move one of them to another controller node at once, the new
    cntlr first, as step 1 of [A pool made before the block
    bound](#a-pool-made-before-the-block-bound) says, and then check the
    two cases above again.
14. Wait until the copy is whole: `dnvctl --sp <destination> clone get
    --name <clone>` fails with the error code NOT_FOUND once the clone has
    left by itself. Meanwhile the details of the clone's row in
    `cntlr_info.clone_id_to_dm_clone` of `dnvctl --sp <destination> cntlr
    inspect --id <primary cntlr id>`, keyed by the `clone_id` that
    `dnvctl clone get` shows, carry dm-clone's status line, whose count of
    hydrated regions out of all regions shows how far the copy is. A row
    that reads `RES_STATUS_MISSING` with the reason
    `RES_REASON_SOURCE_FAULT` instead has no dm-clone under it, since the
    destination's primary cannot read the transfer: see When to stop.
15. Then end the transfer, without `--force`: `dnvctl --sp <source> xfer
    delete --name <transfer>`. The origin namespace stays parked.
16. Delete the origin in the source pool: `dnvctl --sp <source> ns delete
    --nqn <nqn> --idx <ns_idx>`, then `dnvctl --sp <source> td delete
    --name <origin>`, and the subsystem with `dnvctl --sp <source> ss delete
    --nqn <nqn>` once it holds no namespace. `dnvctl sp delete` of the
    source pool refuses a pool that still holds a volume, a subsystem, a
    clone, a transfer or a migration.

What dnv does alone: every cntlr of the source pool parks the origin
namespace and exports the origin to the transfer's list alone, and the
source's primary serves that export. The
destination's primary builds the clone once the sides of its clone meta
group are provisioned: it connects to the source, puts the new volume's namespace on
the copy and moves it ANA optimized. When the primary reports the copy
whole, the worker, which looks every 2 seconds at the default, sets the
destination's namespace to serving in its stored state and drains the
clone, and `dnvctl clone get` then answers NOT_FOUND.
A destination pool whose level is stepped down to `NO_CLONE` or further
has no clone device: the copy waits until the level is raised again, and
meanwhile every read and write of the hosts to the volume fails, as
[Change a pool's level](#change-a-pools-level) says: with `--auto-resume`
the destination's namespace stays ANA optimized over a device that fails
all IO. So do not step that level down during a move. Nothing ends the
transfer: step 15 is yours. A part of the list of step 11
that fails to reach the primary is sent again only when the worker next
sends the primary a change for another reason, and costs a fuller copy, not
data.

Steps that can lose data:

- Creating the destination's namespace without `--suspended`: the hosts
  on its subsystem's list read the new, empty volume beside the origin
  under the same name and identity.
- Creating the transfer without `--auto-suspend`: the origin namespace
  keeps serving, and with `--auto-resume` the destination's serves at
  once, under the same NQN and namespace identity, so the hosts have two
  serving copies of one volume and write to either. The copy misses each
  write to the origin in a region it has already copied.
- A host's discard during the copy, the message that a range holds no data
  any more, as `fstrim` sends: it can mark a region as copied while the
  copy of that region is still on its way, and a host write to the region
  right after is then lost under the late copy ([the scope
  page](scope.md#53-transfers-clones-and-migrations)). On each host of the
  volume, stop discards before step 7 and until the clone has left:
  `sudo systemctl stop fstrim.timer`, and remount a file system that has
  the `discard` option without it (`sudo mount -o remount,nodiscard
  <mount point>`). Start them again after step 14. A `blkdiscard` sends
  discards too.
- A host NQN on the transfer's list that is not one of the destination's
  controller nodes: that host can write the origin while the clone copies
  it, and the clone then reads zeros where it wrote a block the list of
  step 11 marked never written, and the older bytes where the copy had
  already taken the region, with no error.
- A destination volume that a host wrote before, or of another size than
  the origin's: the regions the copy skips keep the old blocks, and a
  smaller volume is copied short (check 5).
- Deleting the transfer before `dnvctl clone get` answers NOT_FOUND, with
  or without `--force`: the clone loses its only source of every region it
  has not copied, and the deletes of step 16 then delete the last copy of
  those regions.
- `dnvctl xfer delete --force` aborts the move: the origin namespace serves
  again, ANA optimized, beside the destination's under the same identity.
  To abort a move whose clone has served the hosts, first stop the
  volume's users on each host and unmount it, while the destination still
  serves: the hosts' file systems hold in their memory what they wrote
  through the destination, and would later write it onto the origin,
  which lacks those writes, and so mix the two copies, as [Park a
  namespace](#park-a-namespace) warns. An unmount after the next delete
  waits, since its flush then waits on the parked origin. Then, in every
  abort, delete the destination's namespace with `dnvctl --sp
  <destination> ns delete`, then the clone with `dnvctl --sp <destination>
  clone delete --force`, and only then the transfer with `--force`; mount
  the volume again after that. Every write made through the destination
  is lost, since the origin lacks it.
  The order matters: `dnvctl clone delete` sets every namespace of the new
  volume back to serving, so a namespace still there would serve the
  partial copy to the hosts, a second path that differs, and once the
  clone is gone, zeros where the copy never reached. `dnvctl clone delete
  --force` skips the proof that the copy is whole and returns once the
  clone is marked as deleting, before the worker has removed it: poll
  `dnvctl --sp <destination> clone get --name <clone>` until it answers
  NOT_FOUND, and only then delete the new volume, which the gateway
  refuses until then.
- A clone refused after the transfer was made leaves the origin namespace
  parked, with no path that serves IO, until `dnvctl --sp <source> xfer
  delete --force`; make checks 1 and 4 first.

When to stop: `clone create` is refused at step 10 with the error code
INVALID_ARGUMENT because a layout number differs from the source pool's:
nothing was made, so run step 10 again with the numbers the message names.
Stop when `clone create` is refused at step 10 for the layouts of
check 1, for its clone meta group as check 4 says, or for any other
reason: no clone was made, so delete the destination's namespace with
`dnvctl --sp <destination> ns delete`, then end the transfer with
`dnvctl --sp <source> xfer delete --force`, which loses nothing while no
clone served. Then fix the cause before you start again. For the clone
meta group, RESOURCE_EXHAUSTED passes once enough disk nodes have room,
and FAILED_PRECONDITION stays, in that destination pool, for every volume
over the largest size its message names. For the layouts, make the
destination pool again with the source's stripe and slice count. When the
source's own stripe times its slice count is over 4 MiB, as in [a pool
made before the stripe
bound](#a-pool-made-before-the-stripe-bound), dnv refuses to make a pool of
that stripe and slice count, and the least common multiple of the first
check is over 4 MiB for every destination. A clone then passes only when
that multiple is no whole number of the destination's data block, so that
the data block counts instead. Otherwise copy the data at the file level,
as step 3 there says. The clone cannot read its source: for a minute or
more, after step 10 or at any later time of the copy, `dnvctl cluster
get-status` shows a line whose object is `clone <clone>` and whose STATUS
is `clone/missing/source_fault`. Its text says that the clone waits for
its source, and then what the primary found of the source, such as the
states of its paths to it, a source namespace that is not there, or why
its own connect failed. When the primary holds no path to the source, as
with a wrong address or a transfer list that lacks it, the text names at
most why its last try to connect failed, and that only for a short time
after each try; at other times it says no more than that the clone waits.
In `dnvctl --sp <destination> cntlr inspect --id <primary cntlr id>`, the
clone's row in `cntlr_info.clone_id_to_target` or in
`cntlr_info.clone_id_to_dm_clone` reads `RES_STATUS_MISSING` with the
reason `RES_REASON_SOURCE_FAULT`. When the row in
`cntlr_info.clone_id_to_dm_clone` reads so, the primary has built no
dm-clone: no count of hydrated regions grows, and the hosts' IO waits,
from the park of step 7 on when no clone served before. When that row
reads `RES_STATUS_OK` and only the row in
`cntlr_info.clone_id_to_target` carries the reason, as after a failover
of the source pool whose old primary's controller node is down, the
dm-clone serves the hosts but cannot read the source: its count of
hydrated regions stops growing, and the hosts' reads of the regions not
yet copied, and their writes to part of such a region, fail after a short
wait. The worker does not take either for a fault, so no failover or cntlr
replacement comes, and nothing ends it by itself. Check the four address
flags of step 10 against the source pool's primary as step 9 reads it
now, and point the clone at that address with `clone set-tr`, as step 13
says. Check that the transfer's list holds the host NQN of the controller
node of the destination's primary, worked out as in step 6; when it lacks
it, give the transfer its whole list with `xfer set-hosts`, as step 13
says. When the line stays, ask for help. While no clone has served the
hosts, you may instead abort the move as Steps that can lose data says:
the abort then loses nothing and needs no unmount. Once the clone has
served them, the abort needs the unmount that Steps that can lose data
names, and that unmount can wait or fail on the regions the clone cannot
read: ask for help. The clone does not
leave, and its count of hydrated regions stops growing or its row in
`cntlr_info.clone_id_to_dm_clone` reads `RES_STATUS_ERROR`: first make the
checks of step 13. After a failover of
the source pool whose old primary's controller node stays up, the status
read shows no line for the clone, both of the clone's rows read
`RES_STATUS_OK`, and the hosts' reads of the regions not yet copied, and
their writes to part of such a region, wait until `clone set-tr` points
the clone at the new primary.
When the count still does not grow, delete nothing in the source pool,
and ask for help.
A cntlr of one pool shares a controller node with a cntlr of the other,
and neither pool can move one as step 13 says: ask for help.

Tested: the e2e suite's copy case runs a transfer made with
`--auto-suspend` and read by a host, `xfer set-hosts` with every controller
node's host NQN, the bitmap read of the parked origin for each slice that
one region of the destination spans, which at one slice is that slice, a
clone made with the four address flags and `--auto-resume`, one part of
the list for each of those slices, `clone set-tr` with the same address
in a run whose copy is not whole by then, the worker's end of the clone,
and then the end of the transfer with `--force`, all within
one pool, into a second namespace of another identity. Its stage 07 fails
the destination's primary over while the clone lives. It passed in the lab
on 2026-11-21 at one slice. The move into another pool under the same
subsystem name and identity, the order of steps 3 to 5, the end of the
transfer without `--force`, the deletes of step 16, `clone set-tr` to a new
address after a failover of the source pool, and the abort are not tested.
For a clone that cannot read its source, the gateway's unit tests plant
its row and read the line the gateway makes of it, the cn agent suite
reads the reason on the rows of a clone whose source namespace is gone
while its dm-clone serves (case `clone_xfer`), and the worker suite,
with a fake agent that reports such a row, checks that the worker takes
the primary as built, with `settling` false and no health epoch. No
suite reads the line of a real clone whose source is refused or gone.

### Read a volume's written blocks

Words: a volume's **written-block list** says, for each block of the pool's
data block size in each slice, whether the volume has written it. A
**part** of a clone's list is up to 1 MiB of it, 8,388,608 blocks, which
`--bm-idx` numbers from 0. A clone takes at most 16 parts of each slice's
list, 16 MiB: the gateway refuses `--bm-idx` 16 or more with the error
code INVALID_ARGUMENT, and the clone copies the regions past the 16th
part whole.

When to use: to give a clone the regions it may skip, in step 11 above.

Safe steps:

1. The volume must not take writes: the gateway refuses the read with the
   error code FAILED_PRECONDITION while a namespace on the volume is not
   parked in the stored state, or while the primary does not report it
   parked. A transfer made with `--auto-suspend` counts as a park. A volume
   no namespace exports, such as a snapshot, is read at once.
2. For each slice of the source pool, from 0 to its slice count less one:
   `dnvctl --sp <source> td get-bm --name <origin> --slice-idx <slice>`. It
   prints `{"bitmap_hex": "<hex>", "byte_cnt": <bytes>, "slice_idx":
   <slice>, "start_block": "<block>"}`: the list, and the slice and the
   first block it covers. Bit k of the list, the low bit of each byte
   first, is 1 when block start_block + k of that slice holds no data: it
   was never written, or it was discarded.
3. Give each slice's list to the clone, unchanged, at the place the read
   printed: `dnvctl --sp <destination> clone append-bm --name <clone>
   --src-slice-idx <slice_idx> --src-start-block <start_block> --bm-hex
   <hex>`; the gateway works out the list's part and its place in it.
   Give every slice's:
   the clone skips a region only where every slice it spans says
   never written. A part applies whenever it comes, before the copy is
   whole. Once the hosts' IO goes through the clone, each part pauses that
   IO for a moment while it applies.
4. A slice's list of 64 KiB or more, 524,288 blocks or more, does not fit
   one command line: in hex it takes two characters a byte, and one
   argument holds less than 128 KiB on Linux, its closing zero byte
   counted. Read it in windows, `--start <block> --cnt <blocks>` on `td
   get-bm`, and give each window in order, with the `--src-start-block`
   the read printed for it. Windows of 262,144 blocks fit: 32 of them fill
   one part, of 8,388,608 blocks, so none crosses the end of a part, which
   the gateway refuses with the error code INVALID_ARGUMENT; the last
   window of a slice can be shorter. The older form, `--bm-idx <start /
   8388608>` with `--offset <(start / 8) % 1048576>`, still works, but
   nothing checks that its numbers match the window. A window starts at a multiple of 8 blocks. The gateway
   refuses an offset that is not the end of what the part holds, unless the
   same bytes stand there already, which it takes as done.

What dnv does alone: nothing for the read; the worker sends each part to
the destination's primary, which keeps it and skips those regions.

Steps that can lose data: a list read while a host could still write the
volume, which the gateway refuses; a list given to a clone of another
volume; and a slice or a first block typed wrong in step 3 or 4, or, in
the older form, a part and an offset that do not match the window. Nothing
checks these: the clone then skips regions the source wrote and reads
zeros there.

When to stop: the refusal says a namespace is on the volume and not
suspended: park it first ([Park a namespace](#park-a-namespace)). A
refusal that says a namespace is not parked on the primary, or that the
namespaces are not proven parked, comes while the primary's report still
lags the park, or while the primary's controller node does not answer:
read again a few seconds later, and ask for help when it lasts a minute.

Tested: the e2e suite's ops case reads a snapshot's list at once and is
refused for the origin while its namespace serves; its copy case reads the
list of a parked origin, and gives one part of it, for each slice that one
region of the destination spans. The gateway
suite gives parts in windows with `--offset` through its own test driver,
over fake agents. Windows through `dnvctl` are not tested.

## Clone from another cluster

Words: a **source outside this cluster** is a volume a transfer exports in
another dnv cluster, under the same etcd or another, or a namespace of an
NVMe-oF target that is not dnv.

When to use: to copy a volume into this cluster from one of these.

Safe steps for a dnv transfer of another cluster: follow [Move a volume to
another pool](#move-a-volume-to-another-pool), with the source pool's
commands run against the other cluster, with its `--cluster` and, for
another etcd, the `--gateway-address` of its gateways. In step 6 the host
NQNs carry this cluster's id, since this cluster's controller nodes read the
transfer; in step 8 the transfer's NQN carries the other cluster's id and
its pool's. Hosts of the other etcd's cluster find this cluster's paths only
when they also list this cluster's cdc ([Give hosts the cdc
addresses](#give-hosts-the-cdc-addresses)), or connect by address.

Safe steps for a target that is not dnv:

1. On that target, let this cluster's controller nodes connect: their host
   NQNs as step 6 of the move works them out, for every controller node
   that holds a cntlr of the destination pool.
2. Stop every writer of the source namespace for the whole copy, and
   unmount it on each host: nothing parks it, and the copy misses each
   write to a region it has already copied. The unmount also flushes the
   host's writes before step 4 drops its only path to the target, a drop
   that loses every write not yet flushed. Step 4 cuts the hosts off every
   namespace of the target's subsystem, not only this one: stop the users
   of each other namespace there too, unmount it, and copy it as well,
   with a volume, a namespace and a clone of its own as steps 3, 5 and 6
   say.
3. Read the source namespace's size in bytes on a host:
   `blockdev --getsize64 <its device>`. The new volume must have exactly
   that size, and the destination pool must be able to make it: a whole
   multiple of the pool's stripe times its slice count, as check 5 of the
   move says. A volume of another size cannot take the copy. If the new
   namespace is to keep the source's identity, read it there too:
   `nvme ns-descs <its device>` prints its `uuid`, and `nvme id-ns <its
   device>` its `nguid`, each in the form the gateway takes, the uuid with
   dashes and the nguid as 32 hex digits with none.
4. Take the hosts off the target for good. First, on the target, let in
   only the controller nodes of step 1: take every host off the
   subsystem's list of allowed hosts, and turn off any setting that lets
   in every host, so that the target refuses the hosts' next connect.
   Then disconnect every host from the target: as root on each host,
   `nvme disconnect -n <the target's subsystem NQN>`. Without the refusal,
   a host that runs nvme-stas, the host program that follows the cdc
   ([Restart nvme-stas](#restart-nvme-stas)), can connect to the target
   again by itself when the `[Controllers]` part of its
   `/etc/stas/stacd.conf` names the target, or a discovery service that
   its `/etc/stas/stafd.conf` names lists it; take the target out of that
   configuration later, as that section says. A host that holds a path to
   the target and one to this cluster under one subsystem NQN, one
   namespace id and one identity joins them into one device. It then
   sends some of its writes to the
   target, which this cluster never holds, with no sign. When only one or
   two of the three match, the host can refuse this cluster's namespace
   while it holds the target's.
5. Create the new volume, of the size of step 3, and its namespace as
   steps 2 to 5 of the move say, under the target's subsystem NQN or
   another one, at the target's namespace id or another (a namespace's
   `--idx` is its NVMe namespace id), and with the source's identity of
   step 3 or a new one: leave out `--uuid` and `--nguid`, and the gateway
   makes a new one.
6. Create the clone as in step 10, with `--src-nqn <the target's subsystem
   NQN> --src-idx <its namespace id>` and the target's address in the four
   address flags. Give the layout flags values that pass the gateway's
   checks, such as the
   destination pool's own stripe and data block with `--src-slices 1`, when
   that block is a multiple of that stripe: a
   stripe a multiple of 4 KiB up to 1 MiB, a data block a multiple of the
   stripe and of 64 KiB up to 1 GiB, a slice count from 1 to 32, and the
   layout rule of the move's first check. There is no written-block list
   for such a source, so the clone copies every region.
7. Give the hosts this cluster's paths. A host that runs nvme-stas finds
   them through the cdc once it lists the cdc ([Give hosts the cdc
   addresses](#give-hosts-the-cdc-addresses)). A host that connects by
   hand connects to each cntlr that `dnvctl --sp <destination> sp get`
   shows, at the address of its `nvme_tr_conf`. Once the clone serves,
   its row of step 14 of the move reading `RES_STATUS_OK`, the
   namespace moves to ANA optimized, and the hosts may mount this
   cluster's device of the volume and start their writers again. It is a
   new device, not the target's.
8. Wait as step 14 of the move says. While the clone copies, read
   `dnvctl --sp <destination> sp get` from time to time. When a cntlr of
   the destination pool moves to a controller node that the target does
   not let in, let that node's host NQN, worked out as in step 6 of the
   move, in on the target too, as step 13 of the move does for a
   transfer's list: the target refuses that node's connect, so a primary
   there cannot read the source. `dnvctl clone set-tr` points the clone at
   another address of its source, with the four address flags as in step
   10.

What dnv does alone: as for the move: the worker ends the clone once its
copy is whole. Nothing points the clone at another address of its source.

Steps that can lose data: a source that takes writes during the copy; for
a target that is not dnv, a mount of the target's device left at the
disconnect of step 4, which loses the writes not yet flushed (step 2), and
a host still connected to it once this cluster gives the host the
subsystem (step 4); a source lost before the copy is
whole, which takes every region not yet
copied with it ([the scope page](scope.md), section 4, case 3); a destination
volume of another size than the source, which the gateway cannot compare.

When to stop: the clone cannot read its source, as the When to stop of
the move says: its line `clone/missing/source_fault` in `dnvctl cluster
get-status` stays, and nothing ends that wait. Check the four address
flags of step 10 of the move and point the clone at the right address
with `dnvctl clone set-tr`. For a dnv transfer, check its list of hosts
as the move says; for a target that is not dnv, check that the target
lets in the host NQN of the controller node of the destination's
primary, as step 1 says. When the line stays, ask for help. The clone
does not leave, and its count of hydrated regions stops growing or its
row in `cntlr_info.clone_id_to_dm_clone` reads `RES_STATUS_ERROR`: for a
dnv transfer, first make the checks of step 13 of the move, as the move's
When to stop says. After a failover of the source pool of a dnv transfer,
the status read can show no line for the clone. For a target that is
not dnv, first make the checks of step 8. When the count still does not
grow, ask for help, and keep the source.

Tested: not tested from another cluster or from a target that is not dnv.
The nearest run is the e2e suite's copy case, stage 07, which on every run
clones a plain namespace of a second pool of the same cluster, as the steps
for a target that is not dnv do: its subsystem's NQN is none of dnv's
transfer NQNs, its list of hosts holds every controller node's host NQN,
and the clone takes that pool's slice count, stripe and data block, with
no transfer and no written-block list. It passed in the lab on 2026-11-21
at one slice.

## Remove a node

Words: an agent's **local files** are the files where it keeps the last
desired state it accepted, under its `--local-store` directory. A disk
node's **header block** is the first block of its disk, where dnv records
which node the disk belongs to.

When: a disk node or a controller node leaves the cluster, for example
because its machine is retired, or because the node must be created again
with another location.

How to confirm the node is empty: `dnvctl dn get --addr <addr>` shows
`dn_conf.side_ptr_list` as `[]`, and `dnvctl cn get --addr <addr>` shows
`cn_conf.cntlr_ptr_list` as `[]`.

What dnv does alone:

- A node delete is refused while a side or a cntlr is on the node. Nothing
  moves them off by itself.
- When the last side or cntlr leaves the node's desired state, the worker
  sends the node a syncup, and its agent removes the devices, exports and
  connections that are no longer wanted. While a sweep leaves something
  behind, the agent reports a leftover and the worker sends the syncup
  again.
- On a disk node, the agent can keep a side's space in use after the side
  left, while an NVMe-oF export of its pool on the machine is younger than
  30 seconds, and longer in the cases [the scope page](scope.md) gives
  (Space of a dropped side can stay in use for a while).
- A delete without `--force` asks the node's agent for its latest report
  of the node. It is refused unless the agent has applied the node's
  latest desired state and reports no leftover, and, on a disk node, unless
  the disk keeps no side and no part of its metadata area in use. The
  refusal says what the agent reports. The first report that can pass
  comes a check round or two after the node's last sweep, and about 30
  seconds later when a side's space waited for a young export, as the item
  above says. A node
  check round comes every 2 seconds at the default, which `dnvctl cluster
  get` shows as `cluster_conf.health_check_conf.dn_interval` and
  `cn_interval`. These times are not measured.
- The delete removes the node from etcd, and the worker sends the node
  nothing more. The agent keeps the node's desired state in its local files
  and applies it again each time it starts: after a delete without
  `--force`, that is a node with nothing on it but its NVMe-oF port and, on
  a disk node, the disk's header block.
- No agent removes an NVMe-oF host entry, the folder under
  `/sys/kernel/config/nvmet/hosts/` that the machine makes for each host
  NQN one of its subsystems lets in. So a removed node leaves the entries of
  the hosts and nodes its exports let in on its own machine, and an entry of
  its own host NQN on each machine that exported to it: a controller node's
  on each disk node machine of its pools' legs, and on a controller node
  machine whose transfer listed it; a disk node's on each disk node machine
  that was the source of a migration to it. An entry that no subsystem
  lists lets no host in.

Safe steps:

1. Disable the node, so that nothing new is placed on it:
   `dnvctl dn set-disabled --addr <addr> --disabled`, or
   `dnvctl cn set-disabled --addr <addr> --disabled`.
2. Empty a disk node: delete the storage pools that have a leg on it, or
   move each such leg to another disk node with a migration
   ([Move a leg to another disk node](#move-a-leg-to-another-disk-node)).
   `dnvctl dn get --addr <addr>` lists each side on
   the node under `dn_conf.side_ptr_list`, with the `sp_id` of its pool,
   its `leg_id` and its `side_id`. `dnvctl sp find-names --ids <sp_id>`
   prints the name of the pool, the name that `--sp` takes. No migration
   moves a leg of a clone meta group, the group that holds a clone's copy
   records: wait until the clone's copy is complete and the clone leaves by
   itself, or, in a RAID1 pool, replace that leg with a repair.
   `dnvctl retired-leg list` shows the pool's retired legs; delete each one
   on the node with `dnvctl retired-leg delete --leg <leg id>` once you no
   longer want its copy.

   A pool is deleted from the inside out. Its delete is refused while the
   pool still holds a thin device, a subsystem, a clone, a transfer or a
   migration, and the refusal names which. `dnvctl --sp '<pool name>' sp get`
   lists them under `sp_conf`: `td_name_list`, `nqn_list`,
   `clone_name_list`, `xfer_name_list` and `migr_name_list`.
   `dnvctl --sp '<pool name>' ss list` shows each subsystem under its NQN
   in `nqn_to_subsystem`, with its namespaces under `ns_list`, each with
   its `ns_idx`. Delete them in this order:
   1. Each transfer: `dnvctl --sp '<pool name>' xfer delete --name <transfer>`.
      A namespace that a transfer reads from cannot be deleted while the
      transfer lives, so the transfers go first. A transfer is the source of
      a clone in another pool: its delete takes from that clone the only
      source of each region it has not copied yet. If that clone must stay
      whole, first wait until
      `dnvctl --sp '<other pool>' clone get --name <clone>` answers
      NOT_FOUND: the worker removes a clone by itself once its copy is
      whole. The clone is the one whose `src_nqn` in that reply is the
      transfer's NQN, worked out as [Move a volume to another
      pool](#move-a-volume-to-another-pool), step 8, says. `dnvctl sp list`
      lists the pools, and `sp_conf.clone_name_list` of
      `dnvctl --sp '<other pool>' sp get` names a pool's clones. For a
      clone in another cluster, run these reads there, with that cluster's
      `--cluster` and, for another etcd, the `--gateway-address` of its
      gateways ([Clone from another cluster](#clone-from-another-cluster)).
   2. Each namespace: `dnvctl --sp '<pool name>' ns delete --nqn <nqn> --idx <ns_idx>`.
   3. Each subsystem, once it holds no namespace:
      `dnvctl --sp '<pool name>' ss delete --nqn <nqn>`.
   4. Each clone: `dnvctl --sp '<pool name>' clone delete --name <clone>`.
      A clone's delete sets each namespace of its volume back to serving,
      so a namespace still there would serve the copy to the hosts, a
      second path beside the source under the same identity, and once the
      clone is gone, zeros where the copy never reached: this is why the
      namespaces go first.
      The delete is refused unless the pool's primary shows the clone's
      copy complete; with `--force` it is deleted without that proof. Then
      wait until `dnvctl --sp '<pool name>' clone get --name <clone>`
      answers NOT_FOUND: the worker removes the clone in the background,
      and the deletes of its volume and of the pool are refused until then.
   5. Each migration: `dnvctl --sp '<pool name>' migr cancel --name <migration>`.
      Once a migration's cutover has started, the cancel is refused without
      `--force`, and its refusal says so. Then cancel it with `--force`: the
      pool's namespaces are gone and its thin devices are deleted next, so
      the forced cancel loses nothing more.
   6. Each thin device: `dnvctl --sp '<pool name>' td delete --name <td name>`.
      A thin device with a snapshot that is not made yet is refused, and
      the refusal names the snapshot: delete the snapshot first.
   7. The pool: `dnvctl --sp '<pool name>' sp delete`. The worker then
      removes its parts in the background (its drain).
3. Empty a controller node: delete the storage pools that have a cntlr on
   it, as step 2 says, or move each cntlr on it to another controller node,
   one at a time.
   `dnvctl cn get --addr <addr>` lists each cntlr on the node under
   `cn_conf.cntlr_ptr_list`, with the `sp_id` of its pool and its
   `cntlr_id`, the id that `--id` takes below.
   `dnvctl sp find-names --ids <sp_id>` prints the name of the pool.
   `dnvctl sp get` lists the pool's cntlrs under `cntlr_list`, which shows
   no ids: its entries come in the order of the ids in
   `sp_conf.cntlr_id_list`. To move a cntlr:
   1. If the old cntlr is the primary and the pool's level is
      `NO_THINPOOL` or below, first step the level back, as step 2 of
      [Replace a cntlr by hand](#replace-a-cntlr-by-hand) says: at those
      levels no failover follows sub-step 3.
      Give the pool a new cntlr with `dnvctl cntlr create --slot <slot>`.
      The slot is a cntlid slot, a range of NVMe controller ids: one of
      the pool's `sp_conf.cntlid_slot_list` in `dnvctl sp get` that no
      cntlr of the pool uses as its `cntlid_slot`. The `cntlr_id` of the
      reply is the new cntlr's id. In this move the create can be refused
      in three cases:
      - The pool has four cntlrs, the most a pool can have: the refusal
        says that the cntlr count has reached the maximum. Then do
        sub-steps 3 and 4 for the old cntlr first, and sub-steps 1 and 2
        after them. The pool keeps three cntlrs meanwhile.
      - No controller node can take the new cntlr: the refusal says that
        no controller node has the free extents. A new cntlr goes only on
        a controller node that is enabled, healthy and has room, and that
        holds no cntlr of the pool yet. A cluster of two controller nodes
        whose pools have two cntlrs each is such a case. Then add a
        controller node first. Or, if the pool has another enabled cntlr,
        do sub-steps 3 and 4 alone: the pool then has one cntlr fewer
        until you give it a new one, and a pool left with one cntlr has
        no standby to take over from its primary.
      - Every slot of the list is in use: the refusal says that the slot
        is already used or is not in the list. Then first add a slot from
        0 to 7 that the list lacks, with
        `dnvctl sp set-cntlid-slots --slots <the whole new list>`, the
        slots separated by commas.
   2. Wait until the new cntlr is built. `dnvctl sp get` shows it with
      `err_epoch` 0, and `dnvctl cntlr inspect --id <new cntlr id>` shows
      `applied_revision`, the revision its agent has applied, at least the
      pool's `sp_rev.revision` in `dnvctl sp get`, and each row under
      `cntlr_info.leg_id_to_leg` as `RES_STATUS_OK`, or, at `NO_SIDE` and
      `DISABLE`, as `RES_STATUS_MISSING` with the reason
      `RES_REASON_SP_LEVEL`. The row of a leg that `dnvctl sp get` shows
      with an `err_epoch` that is not 0 while a cntlr other than the new
      one is the primary can stay `RES_STATUS_ERROR`: the primary cannot
      read that leg either. A new leg of a running repair shows its
      `err_epoch` in `dnvctl repair list`, and a leg of a clone's meta
      group, the group that holds a clone's copy records, in
      `dnvctl clone get --name <clone>`. The worker can make a new cntlr
      the primary before it is built, and the pool then serves no IO until
      it is built. If the new cntlr is still not built a few minutes after
      sub-step 1, for any cause but a row this sub-step sets aside, as
      when a row under `cntlr_info.leg_id_to_leg` stays
      `RES_STATUS_ERROR` for another leg, `applied_revision` stays below
      the pool's `sp_rev.revision`, or `dnvctl sp get` shows the new
      cntlr's own `err_epoch` not 0, do not go on to sub-step 3: ask for
      help.
   3. Disable the old cntlr with
      `dnvctl cntlr set-enabled --id <cntlr id> --enabled=false`. If it is
      the primary, the worker moves the role to an enabled standby with no
      health epoch whose agent does not refuse the worker's syncups. Then
      wait, as [A planned restart of an
      agent](#a-planned-restart-of-an-agent), step 3, says, until
      `dnvctl sp get` shows another cntlr as the primary with `settling`
      false, which says the new primary reports its devices built. If no
      other cntlr becomes primary within a few check rounds, or the new
      primary still shows `settling` true a minute after it became
      primary, give the pool its IO back as that step says, starting with
      `dnvctl cntlr set-enabled --id <cntlr id>` on the old cntlr, and ask
      for help. Do not delete the old cntlr then: it is the one that can
      take the role back.
   4. Then delete the old cntlr with `dnvctl cntlr delete --id <cntlr id>`.
      A primary and an enabled cntlr are never deleted.
   5. If sub-step 1 stepped the pool's level back and the pool must stay at
      its old level, set that level again once the move is done:
      `dnvctl sp set-level --level <the old level>`. At the raised level the
      primary builds the thin pools again and the worker runs its
      reactions, leg repair and auto-grow among them.
4. Check that the node is empty, as above, and note its id,
   `dn_conf.dn_id` or `cn_conf.cn_id`, and the cluster's `cluster_id`, which
   `dnvctl cluster get` prints: steps 6 and 7 name the node's files and
   entries by them.
5. Delete the node, and run the delete again until it passes:

   ```
   for i in $(seq 90); do
     dnvctl dn delete --addr <addr> && break
     sleep 1
   done
   ```

   Use `dnvctl cn delete` for a controller node. Read each refusal: it
   names what the agent still reports. After 90 refusals the loop ends
   with no error of its own: `dnvctl dn get --addr <addr>` still shows the
   node, and the last refusal says why ([Leftovers on a
   node](#leftovers-on-a-node)).
6. Stop the node's agent if its machine leaves, or before an agent serves
   another node with the same store folder, its `--local-store`. The node's
   local files still hold its desired state, and an agent started on them
   builds it again. Delete them, and only them: another agent may share the
   folder, and a controller node agent can also serve nodes of other
   clusters. With the ids of step 4, list the files of a disk node, then run
   the same command with `-delete` after `-print`:

   ```
   cid=$(printf '%016x' <cluster id>); nid=$(printf '%016x' <node id>)
   sudo find <store folder> -maxdepth 1 -type f \( -name "dn-$cid-$nid*" -o -name "probe-dn-$cid-$nid*" -o -name "side-$cid-$nid-*" -o -name "migr-bm-$cid-$nid-*" \) -print
   ```

   For a controller node, the names are these:

   ```
   cid=$(printf '%016x' <cluster id>); nid=$(printf '%016x' <node id>)
   sudo find <store folder> -maxdepth 1 -type f \( -name "cn-$cid-$nid*" -o -name "probe-cn-$cid-$nid*" -o -name "cntlr-$cid-$nid-*" -o -name "torn-$cid-$nid-*" -o -name "clone-bm-$cid-$nid-*" \) -print
   ```

   To use a disk node's disk again: [Reuse a disk from an old
   node](#reuse-a-disk-from-an-old-node); for a disk that leaves dnv for
   good: [A disk leaves dnv](#a-disk-leaves-dnv).
7. Remove by hand what no agent removes. On the node's machine, once the
   node's agent is stopped and its files are gone: its NVMe-oF port, the
   folder of its agent's `--nvmet-port-id`, when
   `ls /sys/kernel/config/nvmet/ports/<port id>/subsystems/` lists nothing
   and no other agent of the machine uses that port id. Its two ANA groups
   go first:
   `sudo rmdir /sys/kernel/config/nvmet/ports/<port id>/ana_groups/3 /sys/kernel/config/nvmet/ports/<port id>/ana_groups/2 /sys/kernel/config/nvmet/ports/<port id>`.
   After the delete, whether the node's agent still runs or not, on the
   node's own machine and on each machine that exported to it, as What dnv
   does alone says above: the host entries that no subsystem lists any
   more, which
   `ls /sys/kernel/config/nvmet/subsystems/*/allowed_hosts/` shows:
   `sudo rmdir /sys/kernel/config/nvmet/hosts/<nqn>`. The node's own host
   NQN is `nqn.2024-01.io.dnv:0:<cluster id>:<node id>` for a disk node and
   `nqn.2024-01.io.dnv:1:<cluster id>:<node id>` for a controller node,
   with both ids as in step 6. On a controller node's machine, the folder
   `/run/dnv-thin-dump` stays until the next boot.

Steps that can lose data or leave state behind: the deletes of a pool's
thin devices in steps 2 and 3, each with its data, and of its transfers,
whose clones in other pools lose their source; a clone's delete with
`--force`, whose volume holds only the regions copied so far, and which
serves them to the hosts beside the source while a namespace of that volume
is still there; the retired leg deletes of step 2, which delete the copy
each leg held; and `--force` on the node's delete.
`dnvctl dn delete --force` and `dnvctl cn delete --force` delete the node
without asking its agent. Use them only for a node whose agent is gone for
good, or whose disk is dead or names another node, since such a node never
passes the check. A node with a side or a cntlr on it is refused with
`--force` too. The node keeps every device, export and connection its last
sweep had not removed, and its agent builds again, each time it starts,
what its local files still name: NVMe-oF exports included, open to the
hosts and controller nodes they named, over disk space that the cluster
counts as free. So after a forced delete, stop that agent and delete its
local files, remove by hand what it built ([Leftovers on a
node](#leftovers-on-a-node)), and on a disk node zero the header block
before the disk is used again ([Reuse a disk from an old
node](#reuse-a-disk-from-an-old-node)). [Replace a dead disk
node](#replace-a-dead-disk-node) and [Replace a dead controller
node](#replace-a-dead-controller-node) give the steps for a node whose
machine is gone.

When to stop and ask for help: a new cntlr of step 3 that is still not
built a few minutes after its create, as sub-step 2 says; a refusal that
names the same leftover for several minutes, while the agent's log keeps
showing `sweep leftover` records for it; a refusal that says the agent
holds a newer desired state than etcd, which happens after an etcd
restore (do not force the delete); a disk node whose agent reports no
counts of its disk, whose disk failed or names another node, and which
can only leave with `--force`.

Tested: the gateway's Go tests check each refusal, a clean delete, `--force`
on a node whose agent does not answer and on a node that still holds a side,
and a node whose desired state changed, or that was deleted and created
again, while its agent was asked. The gateway suite's faults case checks the
refusals and `--force` over fake agents, and its smoke case deletes every
node of its cluster over fake agents that answer clean. The tutorial suite
runs both tutorials' teardowns on real agents: their node deletes, without
`--force`, run again until they pass, as in the loop of step 5, and their
last steps remove the NVMe-oF ports and host entries by hand. It then
checks that no dnv device, md array, NVMe-oF port, subsystem or host entry
is left, and passed in the lab on 2026-11-21. There the nodes' pools were
deleted first, all on one machine: no test removes a node emptied by
moves, and none removes host entries from another machine.

## Disable a node

Words: **disabled** is a word of the core glossary. A **placement** is the
choice of a disk node for a new leg, or of a controller node for a new
cntlr.

When:

- before you empty a node to remove it ([Remove a node](#remove-a-node),
  step 1);
- while a disk node reads healthy but its new legs fail, so that the
  worker's next repair of a group does not go to it again ([the scope
  page](scope.md#61-cases-where-dnv-does-not-come-back-alone-today), a disk
  node that stays healthy but cannot serve a new leg);
- while a node that died is replaced, and while a node fails and comes back
  by turns: its health epoch keeps new placements off it only while the
  epoch is set, and the worker clears the epoch at the first check round
  that finds nothing wrong;
- to add a node that gets nothing until you are ready: `dnvctl dn create`
  or `dnvctl cn create` with `--disabled` ([Prepare a node](#prepare-a-node),
  step 13).

What a disable does:

- The node gets no new placement: no leg of a new pool, of a grow or of a
  clone, no new leg of a repair, the worker's or yours, no new side of a
  migration, and no new cntlr, by `dnvctl cntlr create` or by the worker's
  cntlr replacement. A white list, the node addresses that `--dn-white` or
  `--cn-white` gives a command as the only nodes it may use, that names
  only disabled nodes is refused with RESOURCE_EXHAUSTED, as for nodes with
  no room.
- It moves nothing and stops nothing. The node's sides and cntlrs keep
  serving, and nothing moves them off the node. A disable starts no
  failover, no repair and no migration, and it never reaches the node's
  agent.
- A standby on a disabled controller node can still become the primary:
  the failover chooses the enabled cntlr of the pool with no health epoch
  and the smallest id, whatever its node. To keep primaries off a
  controller node, also disable its cntlrs, with
  `dnvctl --sp '<pool name>' cntlr set-enabled --id <cntlr id> --enabled=false`.
  A disabled cntlr serves no IO, is never made the primary and is never
  replaced by the worker, and the disable of a primary that has an enabled
  standby with no health epoch fails it over at once, at the levels where
  reactions run, `READWRITE`, `READONLY` and `NO_CLONE`. At the other
  levels it stays the primary and runs none of the pool's group arrays.
  No failover follows either when the agent of each such standby refuses
  the worker's syncups. [A planned
  restart of an agent](#a-planned-restart-of-an-agent), step 3, gives the
  checks to make first and how to give the pool its IO back.
- A leg that fails on a disabled disk node is repaired onto another disk
  node, as on any node. The disable alone repairs nothing.

How to confirm: `dnvctl dn get --addr <addr>` shows `dn_conf.disabled` as
true, and `dnvctl cn get --addr <addr>` shows `cn_conf.disabled`.
`dnvctl cluster get-capacity` shows `disabled` for each node under
`addr_port_to_dn` and `addr_port_to_cn`, and counts a disabled node's free
space in the whole space only ([The cluster's
capacity](#the-clusters-capacity)). The status read names no disabled node.

What dnv does alone: nothing. Only these commands change the flag.

Safe steps:

1. Disable the node: `dnvctl dn set-disabled --addr <addr> --disabled`, or
   `dnvctl cn set-disabled --addr <addr> --disabled`.
2. Enable it again: `dnvctl dn set-disabled --addr <addr> --disabled=false`,
   or the same with `cn`. The command stores the value it is given, and
   with no `--disabled` it stores false: a command that leaves the flag out
   enables the node.
3. These commands, as a node's delete, take as `--rev` the node's own
   revision, which `dnvctl dn get` prints as `dn_rev.revision` and
   `dnvctl cn get` as `cn_rev.revision`, not the pool's. A disable or an
   enable does not move it.

Steps that lose data: none. A disable can leave a group without its repair,
though. Leg repair needs an enabled disk node with room that holds no leg
of the group. When there is none, the worker logs `reaction skipped` with
the reason `no candidate`, and the group runs on its other legs until a
disk node can take the new leg. Auto-grow needs enabled disk nodes with
room for the new group too: with none, the worker skips the grow and logs
it, as [A pool's space](#a-pools-space) says, and a thin pool that fills
up gets no new group; once it is full, the hosts' writes that need a new
block wait and then fail. A pool's create and a grow by hand are refused
the same way, with RESOURCE_EXHAUSTED.

When to stop: a group that waits for its repair while every disk node with
room is disabled, and you cannot enable one. Ask for help.

Tested: the e2e suite's ops case disables a disk node and checks that a
grow white-listed to it is refused, disables a controller node that holds
no cntlr of the pool and checks that a cntlr create white-listed to it is
refused, and enables both again; its react case disables every disk node
that holds a side to steer a repair's new leg. Both passed in the lab on
2026-11-21. The cn half of the ops case runs only when a controller node
holds no cntlr of the pool. No test makes a standby on a disabled
controller node the primary.

## Read a node's state

`dnvctl dn inspect --addr <addr>` and `dnvctl cn inspect --addr <addr>`
show a node's live state: its agent's latest observation of the node, made
at the agent's latest converge or read of it, not etcd's record.
`dnvctl dn get` and `dnvctl cn get` show the record.

When: a `health` or `report/unknown` line of the status read names a node
([Read what is wrong](#read-what-is-wrong)); before a node delete; to compare
a disk node's own free count with its record.

How to read it:

1. Run `dnvctl dn inspect --addr <addr>`, or `dnvctl cn inspect --addr <addr>`.
   In the reply:
   - `applied_revision` is the revision of the node's desired state that the
     agent held when the reading started. `dnvctl dn get --addr <addr>`
     prints the node's latest one as `dn_rev.revision`, and `dnvctl cn get`
     as `cn_rev.revision`. A lower one is a change that has not reached the
     node yet, or whose converge still runs.
   - `dn_info` holds a disk node's rows: `disk_info`, its disk; `meta_info`,
     the disk's header block and volume table, where dnv records the disk
     and its sides; `port_info`, its NVMe-oF port; and `store_info`, its
     store folder. `cn_info` holds a controller node's `port_info` and
     `store_info`. A row has its `status`, such as `RES_STATUS_OK` or
     `RES_STATUS_ERROR`, its text in `details`, and in `epoch` the time its
     status last changed.
   - `dn_info.disk_counts` holds the disk node's own counts of its volume
     table (below).
   - `obs_stamp.obs_time` is when the reading ended, and `obs_age` the
     reading's age when the agent answered. `obs_stamp.converge_start` is
     set while a converge runs, and `obs_stamp.agent_start_time` is when the
     agent started.
   - `disk_io_stats`, on a disk node, is the disk's counters, read at the
     call ([A slow leg](#a-slow-leg)).
2. An agent that is still starting answers with `applied_revision` 0, no
   `dn_info` or `cn_info`, no `obs_time` and `obs_age` as `null`: run the
   command again a little later. An agent that does not answer makes the
   command fail with ABORTED, with the text `get dn info from` or
   `get cn info from` and the node's address.
3. The cntlrs and the sides on a node have rows of their own:
   `dnvctl --sp '<pool name>' cntlr inspect --id <cntlr id>` and
   `dnvctl --sp '<pool name>' sp inspect-side --id <side id>`.
4. `dnvctl cluster get-capacity` lists every node's record under
   `addr_port_to_dn` and `addr_port_to_cn`: its location, whether it is
   disabled, its health epoch, its extents and how many sides or cntlrs it
   holds ([The cluster's capacity](#the-clusters-capacity)).

The reply holds no code of the agent, so a leftover on the node does not
show in it ([Leftovers on a node](#leftovers-on-a-node)).

The free count of a disk node. The agent counts the free extents of its
disk from the disk's volume table; etcd's record counts them from what the
cluster placed there. The two counts are compared, never reconciled:

- The agent's counts are under `dn_info.disk_counts` of `dnvctl dn inspect`:
  `total_ext_cnt`, `free_ext_cnt`, `side_rec_cnt`, the side records of the
  table, `free_meta_units`, and `side_recs_match`, whether those records
  name exactly the sides of the desired state the agent holds. The agent
  sends them only beside a `meta_info` row that reads `RES_STATUS_OK`, from
  a table it has confirmed as this node's and a read of the disk's size
  that answered; otherwise `disk_counts` is `null`.
- The record's counts are `dn_conf.total_ext_cnt` and `dn_conf.free_ext_cnt`
  of `dnvctl dn get`.
- Compare the two free counts only when `applied_revision` equals
  `dn_rev.revision` and `side_recs_match` is true. At other times they count
  two different states. The totals also differ when the disk's size changed
  after the node was created: the record keeps the total of the create, and
  the agent counts from its latest read of the size.
- What dnv does alone: the worker makes the same compare at the check
  rounds that bring a new observation. When two compares in a row disagree,
  it logs `dn free count disagrees` at the error level, once until a compare
  agrees, with both counts: `free_ext_cnt` and `agent_free_ext_cnt`,
  `total_ext_cnt` and `agent_total_ext_cnt`. Nothing corrects either count,
  and no health epoch is set. A disk node whose side records never come to
  match its desired state is never compared, and no record says so.
- What a disagreement costs: a record that counts more free extents than
  the disk has can place a side that the disk refuses, whose row then reads
  `RES_STATUS_ERROR`. A record that counts fewer leaves room unused.

Safe steps on a `dn free count disagrees` record: disable the node, so that
no new side is placed on it by a count that may be wrong ([Disable a
node](#disable-a-node)), and ask for help. The way to make the counts agree
again is to empty the node, remove it and add its disk again ([Remove a
node](#remove-a-node), [Reuse a disk from an old
node](#reuse-a-disk-from-an-old-node)).

Steps that lose data: none. The reads change nothing.

When to stop: a `dn free count disagrees` record; a row that stays
`RES_STATUS_ERROR`. Ask for help.

Tested: the e2e suite's ops case runs `dnvctl dn inspect` and
`dnvctl cn inspect` on the lab's nodes. At the end of every case but the
upgrade case, the e2e suite waits until each disk node's agent, at its
record's revision, counts its whole data area free, as its record does,
and checks that the worker logged no `dn free count disagrees`. The
tutorial suite runs the all-in-one tutorial, which counts the
`RES_STATUS_OK` rows of `dnvctl dn inspect` and `dnvctl cn inspect`. Both
suites passed in the lab on 2026-11-21. No suite makes the counts
disagree; the worker's unit tests check the compare and its record. The way
back of the safe steps is not tested.

## Leftovers on a node

Words: a **leftover** is a word of the core glossary. An agent's answer
carries the **leftover code**, code 4, while its latest sweep or read of the
node found something that the desired state no longer wants.

When:

- a node delete without `--force` is refused for several minutes, and its
  text says that the agent's latest observation of the node "answers code
  4";
- an agent's log keeps showing `sweep leftover` records for the same
  objects;
- on the machine of a node deleted with `--force`;
- after any node delete, on the node's machine and on the machines that
  exported to it ([Remove a node](#remove-a-node), step 7).

How to confirm: the agent's `sweep leftover` record names what is left. Its
`leftover_cnt` counts the objects, and `leftovers` names each one as
`<kind>:<name>`: `dm` a device mapper device, `md` an md array, `nvmet` an
NVMe-oF subsystem the machine exports, `nvmet_ns` one of its namespaces,
`nvme` an NVMe-oF connection the machine made, and `record` a side's record
in a disk's volume table. `failures` names each step that failed, with its
cause. The worker logs `syncup leftover` for the answer to each syncup it
sends, with at most eight names in its `details`; a check round's answer
gets no such record. Both records are at the info level, so a
program run with `--log-level warn` logs neither ([The logs](#the-logs)).
For a cntlr, the cn agent's `sweep leftover` record of a read of the
cntlr, not of a converge, can also name `md_member`, `md_broken` and
`md_absent`: a member, a broken array or a missing array of a group the
primary still wants. These are no objects to remove by hand: the converge
that the worker sends on that code fails the member early, puts it back,
adds it or removes it from the array, assembles the array again, or
starts it. The worker's `syncup leftover` record of that converge does not
name them.
`dnvctl dn inspect` and `dnvctl cn inspect` show no code, and the status
read shows no leftover.

What dnv does alone: the worker sends the node's desired state again at
each check round whose answer carries the leftover code, every 2 seconds at
the default, and each converge's sweep tries again, from the top down. It
stops below a layer that left something, since that layer may still use
what is under it. A leftover goes at the first sweep that can remove it, as
once a host lets go of a device; nothing keeps a memory of failures. A
sweep never removes:

- what a node deleted with `--force` still holds: no worker sends that node
  anything any more, and its agent builds it again each time it starts,
  for as long as the node's local files stay;
- after any node delete, the node's NVMe-oF port with its two ANA groups,
  and the NVMe-oF host entries ([Remove a node](#remove-a-node));
- an NVMe-oF subsystem or connection whose name has dnv's prefix but names
  no dnv object ([the scope
  page](scope.md#61-cases-where-dnv-does-not-come-back-alone-today));
- a leg's device left suspended under an md array of a pool that has left a
  controller node (the scope page, section 6.1);
- on a disk node, a device of another cluster or of another node that maps
  the disk, which keeps a blank disk from being formatted ([Reuse a disk
  from an old node](#reuse-a-disk-from-an-old-node)).

Safe steps:

1. For a node that still exists, read `failures` in the agent's latest
   `sweep leftover` record. A removal fails most often because something
   still uses the object: a host still connected to a subsystem, a process
   that holds a device open, or a device above it. End that use, and the
   next round's sweep removes the object. Do not remove by hand an object of
   a node that still exists while its agent runs: ask for help instead.
2. A leg's device of a pool that has left a controller node, named
   `dnv-<cluster id>-<node id>-c9-<pool id>-<leg id>` with each id in 16 hex
   digits, where no entry of `cn_conf.cntlr_ptr_list` of `dnvctl cn get`
   names that pool: `sudo dmsetup info <name>` shows `State: SUSPENDED`, and
   `sudo dmsetup resume <name>` ends it. Resume no other device by hand.
3. For a node deleted with `--force`, or a machine whose nodes are all
   removed:
   1. Stop the node's agent, so that it builds nothing again, and delete the
      node's local files ([Remove a node](#remove-a-node), step 6). That
      step and the next name the node's files and objects by the cluster's
      id and the node's. Once the node is deleted, no record holds the
      node's id: if you did not note it before, read both ids from the name
      of the node's file in the store folder before you delete the files.
      It is `dn-<cluster id>-<node id>` for a disk node and
      `cn-<cluster id>-<node id>` for a controller node, with both ids in
      16 hex digits already; take the one whose cluster id is the
      `cluster_id` of `dnvctl cluster get` in 16 hex digits, and set `cid`
      and `nid` of that step to the two ids in place of the `printf`.
   2. Reboot the machine, if no other node on it serves a pool. No device
      mapper device, md array or NVMe-oF object survives a reboot, and an
      agent with no files of the node builds none of them again. On a
      machine whose other nodes serve pools, a reboot is the loss of those
      nodes until they are back ([the scope page](scope.md), section 6).
      There, remove the node's objects by hand instead, each one only after
      what is above it, with `<cluster id>` and `<node id>` in 16 hex
      digits: `printf '%016x' <id>` of an id noted in decimal, or the ids of
      the file's name of sub-step 1 as they are, with no `printf`, which
      reads a number with a leading 0 as octal. The objects are these:
      - The NVMe-oF subsystems that export a device of the node.
        `grep -l 'dnv-<cluster id>-<node id>-' /sys/kernel/config/nvmet/subsystems/*/namespaces/*/device_path`
        lists their namespaces. For each subsystem, remove its link from
        each port, `sudo rm /sys/kernel/config/nvmet/ports/<port id>/subsystems/<nqn>`,
        which ends every connection to it. Disable each namespace,
        `echo 0 | sudo tee /sys/kernel/config/nvmet/subsystems/<nqn>/namespaces/<nsid>/enable`,
        and remove it with `sudo rmdir`. Remove the links under the
        subsystem's `allowed_hosts`, then the subsystem itself with
        `sudo rmdir`.
      - The device mapper devices whose names start
        `dnv-<cluster id>-<node id>-`, from the top down:
        `sudo dmsetup remove <name>` for each one that no other device
        holds, as `lsblk` shows; first `sudo dmsetup resume <name>` for one
        that `sudo dmsetup info <name>` shows suspended. On a controller
        node, stop each md array left over the node's leg devices, once no
        device above it is left, and before the leg devices: `lsblk` shows
        each array, of type `raid1`, as `md<N>` under the leg devices it
        holds, or as `md_<name>` where mdadm.conf says `CREATE names=yes`,
        and `sudo mdadm --stop /dev/<that name>` stops it.
      - The node's own NVMe-oF connections, whose host NQN is the node's:
        on a controller node, `nqn.2024-01.io.dnv:1:<cluster id>:<node id>`,
        to the disk nodes and to a clone's source; on a disk node,
        `nqn.2024-01.io.dnv:0:<cluster id>:<node id>`, to the source of a
        migration whose destination it was.
        `grep -l '<host nqn>' /sys/class/nvme/nvme*/hostnqn` names them, and
        `sudo nvme disconnect -d nvme<N>` ends each one.
   3. Then remove the node's port and host entries, as [Remove a
      node](#remove-a-node), step 7, says.
4. A subsystem or a connection whose name has dnv's prefix,
   `nqn.2024-01.io.dnv:`, but names no dnv object: remove it by hand in the
   same way, once you know whose it is.

Steps that lose data: the removal of a subsystem or a device that a pool
still uses cuts its hosts or its legs. Remove only objects whose names
carry the removed node's ids, and only after its agent has stopped. Never
remove a port that another agent of the machine uses: a disk node agent
and the controller node agent can share a port id.

When to stop: a removal that fails; an object whose owner its name does not
tell; a leftover that a node still in the cluster names for minutes while
nothing uses it. Ask for help.

Tested: the cn suite's smoke case suspends a striped device by hand and
checks that a check round's read names it with the leftover code and that
the desired state sent again resumes it. The e2e suite's react case checks
that a dead controller node agent, started again, removes what it left.
The tutorial suite checks that the tutorials' teardowns, whose last steps
remove the NVMe-oF ports and host entries by hand, leave no dnv device, md
array, NVMe-oF port, subsystem or host entry. The three suites passed in
the lab on 2026-11-21. The removals by hand of step 3 and a reboot to clear
a node are not tested.

## Reuse a disk from an old node

Words: a disk node's **header block** is the first block of its disk, 4 KiB,
where dnv records which node the disk belongs to. A disk node's **volume
table** is the record on the disk of which extents each side holds.

When: a disk that served a disk node of dnv must serve a new disk node: its
node was removed and is created again, as for another location, or the disk
goes to another cluster or another machine. Also when a disk node's agent
gets a new disk: its old local files must go first (step 5).

How to confirm:

- `sudo head -c 8 <disk>; echo` prints `DNVDISK1` on a disk whose header
  block dnv wrote.
- A node created over such a disk before its header block was zeroed gets
  nothing: `dnvctl dn inspect --addr <addr>` shows its `meta_info` row as
  `RES_STATUS_ERROR` with the text
  `foreign disk: cluster/dn/extent is <a>/<b>/<c>, want <d>/<e>/<f>`, its
  health epoch stays set, and a delete without `--force` is refused, with
  "answers code 4" and "disk identity is not confirmed for this node". It
  holds no side, so delete it with
  `dnvctl dn delete --addr <addr> --force`.

What dnv does alone: nothing zeroes a header block. A disk node's agent
reads a header block that does not start with `DNVDISK1` as blank, a
zeroed one or one that holds anything else, such as a file system. It
formats a disk whose header block is blank at the first converge of its
node, and then only when no side device or clone metadata device of dnv,
of any node, still maps the disk, its local files hold no provisioned side
of a node that still lists the side or of a node it holds no desired state
for, and the disk reads 512-byte blocks. Otherwise its `meta_info` row reads
`RES_STATUS_ERROR` with a text that starts
`blank disk header; refusing to format`, and the disk stays blank. An agent
started with the old node's local files formats a blank disk under the old
node's id, so that the new node refuses it again as foreign.

Safe steps, as root on the machine of the disk:

1. If the old node still exists in a cluster, remove it first ([Remove a
   node](#remove-a-node)). If it was deleted with `--force`, its machine can
   still hold what it built ([Leftovers on a node](#leftovers-on-a-node)).
2. Stop the disk node agent that uses the disk, and wait until its process
   has exited.
3. Check that no device of dnv maps the disk: `lsblk <disk>` lists no device
   whose name starts `dnv-` above it. If one does, remove it as [Leftovers on
   a node](#leftovers-on-a-node), step 3, says, or reboot the machine when no
   other node on it serves a pool.
4. Zero the header block. This step loses data:

   ```
   sudo dd if=/dev/zero of=<disk> bs=4096 count=1 conv=fsync
   ```

   Check `<disk>` twice first: on any other device the command wipes the
   first 4 KiB.
5. Delete the agent's disk node files from its store folder. They are all
   this agent's, since two disk node agents never share a folder; the files
   of a controller node agent in the same folder stay:

   ```
   sudo find <store folder> -maxdepth 1 -type f \( -name 'dn-*' -o -name 'probe-dn-*' -o -name 'side-*' -o -name 'migr-bm-*' \) -print -delete
   ```

   When the disk comes from another machine, delete the files of the agent
   that used it there too, before that agent starts again.
6. Start the agent with the disk as its `--disk`.
7. Create the node ([Prepare a node](#prepare-a-node), step 13). Within a few
   check rounds, `dnvctl dn inspect --addr <addr>` shows `meta_info` as
   `RES_STATUS_OK` and `dn_info.disk_counts.free_ext_cnt` equal to
   `total_ext_cnt`.

A new disk needs no step 4, but the agent's old files must still go
(step 5) before it serves the new disk.

Steps that lose data: step 4. With the header block zeroed, nothing of dnv
reads the old volume table again, so the data of the old node's sides is
gone for dnv. Only the header block is zeroed: the old bytes stay on the
disk, readable by root on the machine, until new sides write over them
([the scope page](scope.md), section 3). To erase a disk that leaves dnv
for good, follow [A disk leaves dnv](#a-disk-leaves-dnv) instead.

When to stop: `meta_info` still reads `blank disk header; refusing to
format` a minute after the create; its text names the first device that
still maps the disk, or the side its files hold. Or `meta_info` reads
`foreign disk` after step 7. Ask for help.

Tested: not tested as a whole. Unit tests of the disk node agent check that
it never writes a disk whose header block names another node, and that it
does not format a blank disk that a side device of this node or of another
maps, or whose local files hold such a side. The disk node, controller
node, e2e and csi suites zero each test disk's header block with the `dd`
command of step 4 at their cleanup, and then delete the disk: no suite
creates a node again on a used disk. The e2e suite's upgrade case deletes
every agent's local files and starts the agents again, but keeps each disk's
header block and its node.

## A disk leaves dnv

When: a disk that served a disk node leaves dnv for good: it goes to
another use, back to its owner, or out of the machine.

Why: a freed extent keeps its old bytes, and [Reuse a disk from an old
node](#reuse-a-disk-from-an-old-node) zeroes only the disk's header block.
Root on the next machine, or whoever holds the disk, can read the old bytes
of every pool that had a side on it.

What dnv does alone: nothing. No agent erases a disk.

Safe steps, as root on the machine of the disk:

1. Remove the disk's node ([Remove a node](#remove-a-node)), or, for a dead
   node, [replace it](#replace-a-dead-disk-node) first: the disk must hold
   no side.
2. Stop the disk node agent that uses the disk, and keep it from starting
   again at the next boot: `sudo systemctl disable --now
   dnv-agent-dn@<name>`, for an agent started from the kit's unit. Wait
   until its process has exited. Check that no device of dnv maps the disk:
   `lsblk <disk>` lists no device whose name starts `dnv-` above it. If one
   does, remove it as [Leftovers on a node](#leftovers-on-a-node), step 3,
   says, or reboot the machine when no other node on it serves a pool.
3. Erase the whole disk with a step that writes zeros or erases for
   certain. A plain discard, such as `blkdiscard` with no option, is not an
   erase on every disk: some disks keep the old data, and some return it.
   Use one of:
   - `sudo blkdiscard --zeroout <disk>`, which writes zeros over the whole
     disk, or has the disk write them; it takes at most as long as writing
     the disk once. `blkdiscard` may ask for `-f` when it finds a known
     signature on the disk; give it only once step 2's checks pass, since
     `-f` also lets it run while another program holds the disk.
   - The disk's own erase: on an NVMe disk, `sudo nvme format <disk>
     --ses=1`, a user data erase, or `--ses=2`, a cryptographic erase,
     where the disk has one. Use it only when `<disk>` is a whole
     namespace, a name such as `/dev/nvme0n1` with no partition number
     after it, and no other namespace of the drive holds data you keep: a
     drive can apply a format to every one of its namespaces, as
     `sudo nvme id-ctrl <disk> -H` shows in its "Format NVM Attributes".
     Otherwise use the write of zeros.
   A write of zeros erases what the disk returns to reads. A flash disk
   can still hold old data in its spare cells, and any disk in blocks it
   set aside as bad, which someone who reads the disk's chips can recover.
   For a disk that leaves your hands, use the disk's own erase where it is
   safe, or destroy the disk.
4. Read the whole disk back. After a write of zeros,
   `sudo cmp <disk> /dev/zero` must stop only at the end of the disk, with
   `cmp: EOF on <disk> after byte <N>`, where `<N>` is the disk's size,
   `sudo blockdev --getsize64 <disk>`. After `nvme format` a disk can read
   back as zeros, as ones, which
   `tr ' ' 'ÿ' < /dev/zero | sudo cmp <disk> -` checks the same way,
   or, after a cryptographic erase, as random bytes, which no read back can
   tell from old data: then run the write of zeros of step 3 after it, and
   check that.
5. Delete the agent's disk node files from its store folder, as step 5 of
   [Reuse a disk from an old node](#reuse-a-disk-from-an-old-node) says.

Steps that lose data: step 3 erases everything on the disk, and an
`nvme format` can erase every namespace of the drive. Check `<disk>` twice
first.

When to stop: the read back of step 4 reports a difference before the end
of the disk after a write of zeros: the write did not reach that place.
Keep the disk where nobody else can read it, and ask for help.

Tested: not tested.

## Replace a dead disk node

Words: a **repair** and a **retired leg** are as in [Words of this
guide](#words-of-this-guide): a repair replaces a bad leg of a group with a
new leg, and the bad leg then stays in the pool as a retired leg, with its
side and that side's extents, until it is deleted.

When: a disk node's machine or disk is gone for good. If the machine runs
and only its agent stopped, start the agent again ([A planned restart of an
agent](#a-planned-restart-of-an-agent)). A node that comes back needs
nothing.

Symptom: `dnvctl cluster get-status` shows a `health` line for
`dn <addr>`, "health epoch set: the worker's checks of the disk node fail",
whose age grows, and `row` or `health` lines for its legs and sides in each
pool that has one there. While the machine is gone,
`dnvctl dn inspect --addr <addr>` fails with ABORTED and
`get dn info from`; a node whose disk died reads its rows as
`RES_STATUS_ERROR` instead. A RAID1 pool goes on serving over its
other legs, but a group none of whose other legs md holds in sync on
another machine, as while md copies onto the new leg of a repair, fails its
IO as a pool without RAID does. In a pool without RAID, IO to the groups
on the node waits until the controller node drops its connections to the
node, 6 to 10 seconds after the node went silent in the lab, and then for
the pool's fast IO fail timeout, the kernel's wait before it fails the IO
queued on a lost path, 5 seconds at the default. Then it fails: a write
failed after 13.2 seconds in the lab ([the
introduction](introduction.md#status-and-limits)). In both cases, the IO
to a leg that a migration moves waits instead when its side on the node
serves it while the node's machine is gone, and the path to its other
side stays up, unless another leg of the group is in sync on that same
machine, where the IO can fail too. In a pool without RAID, and for a
migration past its cutover whose copy is not whole, it waits until the
node comes back or step 3 ends the migration. Otherwise, in a RAID1
group, it waits only until the migration ends, by step 3 or by leg
repair after the pool's `side_unhealthy`, 600 seconds at the default.
Either end, step 3's or leg repair's, keeps the side on the dead node
when that side serves, and the IO then fails. md then marks the group's
array broken, the worker fails the pool over where the pool has an
enabled standby with no health epoch whose agent does not refuse the
worker's syncups (a pool with no such standby, such as a pool of one
cntlr, waits for the cntlr replacement), and the group waits for the
node or a forced start, since no other leg of it is in sync. In both
kinds of pool, the worker can also fail the
pool over sooner, while the IO waits ([the scope page, section
8](scope.md#8-waits-a-host-can-see)).

How to confirm: find the pools. `dnvctl dn get --addr <addr>` lists each side
on the node under `dn_conf.side_ptr_list`, with the `sp_id` of its pool, its
`leg_id` and its `side_id`, and `dnvctl sp find-names --ids <sp_id>,<sp_id>`
prints the pools' names. `dnvctl --sp '<pool name>' sp get` shows under
`sp_conf.bdev_conf.redund_conf` whether a pool is RAID1, `redund_md_raid1`,
or has no redundancy, `redund_none`.

What dnv does alone:

- In a RAID1 pool, leg repair replaces each leg on the node, once the leg's
  side has had its health epoch for the pool's `side_unhealthy`, 600 seconds
  at the default, while the leg has one too, or once the leg has had its own
  for the pool's `leg_unhealthy`, 1200 seconds. The worker repairs one leg of
  a group at a time. It puts the new leg on an enabled, healthy disk node
  with room that holds no leg of the group, in another location when it
  can. Once the new leg is ready, the primary adds it to the group's array
  and md starts to copy the group's data onto it. The worker then finishes
  the repair, which puts the new leg in the dead leg's place in the group:
  the dead leg is then a retired leg, and its side stays on the dead node,
  in its `side_ptr_list`. md's copy goes on until the group is whole.
- A group none of whose other legs is in sync and free of a health epoch
  gets no repair: the worker logs `reaction skipped` with the reason
  `no_healthy_leg` ([the forced-start guide](#the-forced-start-guide)). With
  no disk node to take the new leg, the reason is `no candidate`, and the
  repair waits until one can. A pool without RAID gets no repair: its data on
  the node is gone, save that of a leg whose migration step 3 ends with no
  loss.
- Leg repair ends the migration of a leg it repairs only where that loses
  nothing: it cancels one whose cutover is pending, and finishes one whose
  copy is whole. A leg that still serves through its side on another disk
  node gets no repair, and its migration waits for you (step 3), as does
  one whose copy is not whole.
- The worker deletes a retired leg by itself once it is 7 days old and not
  kept, when its group has no repair, its row reads `RES_STATUS_OK`, md
  holds its array whole, and no leg or side of the group has a health
  epoch. It deletes one at a time. It never deletes a kept one, and a forced
  start keeps the group's retired legs ([Keeping the old
  copies](#keeping-the-old-copies)).
- At the pool's level `NO_THINPOOL` or below the worker makes no leg repair
  and deletes no retired leg ([Change a pool's
  level](#change-a-pools-level)).
- Nothing deletes the node.

Safe steps:

1. Disable the node, so that it gets no new leg if it comes back for a
   while: `dnvctl dn set-disabled --addr <addr> --disabled` ([Disable a
   node](#disable-a-node)).
2. If no other disk node has room for the new legs, add one ([Prepare a
   node](#prepare-a-node)).
3. For each pool with a side on the node, RAID1 or without RAID, first end
   each migration that has a side on the node. In a RAID1 pool, a leg that
   still serves through its other side gets no repair, so its side would
   stay on the node. In a pool without RAID, the worker ends no migration.
   `dnvctl --sp '<pool name>' sp get` names the pool's migrations under
   `sp_conf.migr_name_list`, and
   `dnvctl --sp '<pool name>' migr get --name <migration>` shows the old
   side as `migr.src_side_id`, the new side as `migr.dst_side_id`, and
   `migr.cutover_pending`. A migration with a `side_id` of the node's
   `side_ptr_list` has a side on the node.
   - While its cutover is pending, cancel it:
     `dnvctl --sp '<pool name>' migr cancel --name <migration>`. The old
     side holds every write, so nothing is lost.
   - Past the cutover, finish it:
     `dnvctl --sp '<pool name>' migr finish --name <migration>`. The
     gateway asks the new side's agent for the proof that the copy is
     whole, and refuses while the copy is not whole or the agent does not
     answer, so a finish that passes loses nothing.
   - If the finish is refused, end the migration as [Ending a migration
     with force](#ending-a-migration-with-force) says. The worker's
     `reaction skipped` record with the reason `migration_copy_not_whole`
     names such a migration in its `migr_name`.

   In a pool without RAID, two migrations end with no loss, and their leg
   keeps its data: one whose cutover is pending and whose new side is on
   the node, and one past its cutover whose old side is on the node and
   whose copy is whole. If a finish there is still refused once you have
   run it again as [Ending a migration with
   force](#ending-a-migration-with-force) says, that is its case 3: stop
   and ask for help.
4. For each RAID1 pool with a leg on the node, wait for its leg repairs, or
   start them yourself ([Repair a leg by hand](#repair-a-leg-by-hand)). A
   leg's repair has finished when
   `dnvctl --sp '<pool name>' retired-leg list` lists the leg under
   `leg_id_to_retired_leg`, with the dead node's address as `side.addr_port`.
   Then wait until `dnvctl --sp '<pool name>' group inspect --grp <grp_id>`,
   with the retired leg's `grp_id`, shows every leg in sync: each entry of
   `leg_list` has a `member.md_state` that holds the word `in_sync` and not
   the word `faulty`. md has then copied the group's data onto the new leg.
   The status read does not prove it: it shows no `group` line that says
   the array is not whole while the new leg has a line of its own, or while
   the primary sends no report.
5. Match the node's sides with the retired legs. Each `leg_id` of the node's
   `side_ptr_list` that is a key of its pool's `leg_id_to_retired_leg` is a
   retired leg with its side on the node. `dnvctl` prints both ids as decimal
   numbers in quotes, so they compare as text. A `leg_id` that is no retired
   leg is a leg still in its group: its repair has not finished, or a
   migration of step 3 still moves it.
6. Delete each such retired leg once its group is whole. Read the pool's
   revision, `sp_rev.revision` of `dnvctl --sp '<pool name>' sp get`, then
   run `dnvctl --sp '<pool name>' retired-leg delete --leg <leg id> --rev <revision>`.
   Each delete moves the revision, so read it again before the next one. Or
   wait, and the worker deletes them by itself after seven days, as above.
7. For each pool without RAID that still has a side on the node after
   step 3, delete the pool, from the inside out, as [Remove a
   node](#remove-a-node), step 2, says: its transfers, its namespaces, its
   subsystems, its clones, its migrations, its thin devices, and then the
   pool itself. The pool's delete is refused while it holds any of them.
   After each clone's delete, wait until
   `dnvctl --sp '<pool name>' clone get --name <clone>` answers NOT_FOUND:
   until then the deletes of its volume and of the pool are refused. This
   step loses data: the thin device deletes delete each volume with what it
   still holds on the other disk nodes. Its volumes lost their data on the
   dead node, and a node with a side on it cannot be deleted. A clone whose
   copy the primary does not show complete is deleted only with `--force`,
   and its volume holds only the regions copied so far: delete its
   namespaces first, since a clone's delete sets them back to serving. With
   `--rev`, read the pool's revision again before each command, as in step
   6. The removal of a clone or of the pool in the background needs no
   answer from the dead node.
8. Once `dnvctl dn get --addr <addr>` shows `dn_conf.side_ptr_list` as `[]`,
   note the node's id, its `dn_conf.dn_id`, and the cluster's
   `cluster_id`, which `dnvctl cluster get` prints: once the node is
   deleted, no record holds its id, and step 9 names the node's files,
   devices and host NQN by the two ids. Then delete the node with force:
   `dnvctl dn delete --addr <addr> --force`. Its reply prints the node's id
   as `dn_id` too.
   Without `--force` the delete is refused, since the agent does not answer,
   or, on a machine whose disk alone died, cannot prove the disk clean: it
   reports no counts of a disk it cannot read. With a side left on the node
   it is refused with `--force` too.
9. If the node's agent still runs, as on a machine whose disk alone died,
   or if the machine comes back, stop that agent at once, or before it
   starts, delete the node's local files, and remove what it built, as
   [Leftovers on a node](#leftovers-on-a-node), step 3, says: the agent
   builds again, each time it starts, what its files name, NVMe-oF exports
   included, over disk space that the cluster counts as free, and what it
   built stays after it stops. To use its disk again, or to give its agent
   a new disk: [Reuse a disk from an old
   node](#reuse-a-disk-from-an-old-node); for a disk that leaves dnv for
   good: [A disk leaves dnv](#a-disk-leaves-dnv).

Steps that lose data: step 7, with its thin device deletes and its forced
clone deletes, whose volumes serve a partial copy to the hosts while a
namespace of them is still there; and the delete of a retired leg, which
drops its copy, the last one while its group is not whole: delete one only
once its group is whole. A forced end of a migration that step 3 sends you
to loses data as [Ending a migration with
force](#ending-a-migration-with-force) says.

When to stop: a group with no healthy leg ([the forced-start
guide](#the-forced-start-guide)); a repair that does not start a few minutes
after the thresholds above, or does not finish; a `no candidate` record that
goes on once a disk node has room; a migration of step 3 that falls in case
3 or 4 of [Ending a migration with force](#ending-a-migration-with-force).
Ask for help.

Tested: the e2e suite's react case kills a disk node's agent and removes its
NVMe-oF port, and checks that the worker repairs the dead leg with a repair
of its own, finishes it and keeps the dead leg as a retired leg; it also
kills the disk nodes of both legs of one group, and checks that the group
gets no repair until one of them comes back. It runs with short thresholds,
not the defaults, and passed in the lab on 2026-11-21. The e2e suite's copy
case, in its stage 06, kills the disk node of a repair's new leg while md
copies onto it, retires that leg with a second repair, and deletes it with
`dnvctl retired-leg delete` while its disk node is still dead, on real
agents. It checks that the side leaves the node's `side_ptr_list` and that
the node's record counts every extent free again; that node's agent, back,
then removes the side. It passed in the lab on 2026-11-21, in the e2e run
at one slice. The worker suite checks the delete of a retired leg and the
worker's own delete at seven days, over fake agents; the gateway's tests
check `--force` on a node whose agent does not answer, and its refusal
while a side is on the node. The e2e suite's react case, stage 10, runs
the worker's own finish of a migration whose new side's disk node went
silent; no test ends a migration of step 3 by hand while a disk node is
dead. The procedure as a whole, and the forced delete of a dead disk node
with real agents, are not tested.

## Replace a dead controller node

Words: the **start hold** and a primary's `settling` flag are as in [A
planned restart of an agent](#a-planned-restart-of-an-agent): the start
hold is how long the worker waits for a starting agent to end its startup
pass, and `settling` stays true until a new primary reports its devices
built. A **cntlid slot** is a range of NVMe controller ids, as in [Remove a
node](#remove-a-node), step 3: each cntlr of a pool uses one slot of the
pool's list.

When: a controller node's machine is gone for good. If the machine runs and
only its agent stopped, start the agent again ([A planned restart of an
agent](#a-planned-restart-of-an-agent)): the cntlrs that were failed over
come back as standbys.

Symptom: `dnvctl cluster get-status` shows a `health` line for
`cn <addr>`, "health epoch set: the worker's checks of the controller node
fail", whose age grows, and one for each cntlr on the node.
`dnvctl cn inspect --addr <addr>` fails with ABORTED and `get cn info from`.
Each pool whose primary was on the node, and that had an enabled standby
with no health epoch whose agent does not refuse the worker's syncups,
has failed over to it about 6 to 10 seconds after the
node died, at the defaults, unless its level is `NO_THINPOOL` or below; its
hosts saw a pause. A pool with no such standby, as a pool of one cntlr,
serves no IO.

How to confirm: `dnvctl cn get --addr <addr>` lists each cntlr on the node
under `cn_conf.cntlr_ptr_list`, with the `sp_id` of its pool and its
`cntlr_id`, and `dnvctl sp find-names --ids <sp_id>,<sp_id>` prints the
pools' names. `dnvctl --sp '<pool name>' sp get` lists the pool's cntlrs
under `cntlr_list`, in the order of the ids of `sp_conf.cntlr_id_list`, and
shows which one is the primary.

What dnv does alone:

- Failover: a primary on the node that two check rounds in a row find
  unhealthy, with its health epoch as old as the pool's
  `primary_unhealthy`, 4 seconds at the default, is failed over to the
  pool's enabled cntlr with no health epoch whose agent does not refuse
  the worker's syncups, the one with the smallest id. When the agent of
  each enabled standby with no health epoch refuses the worker's syncups,
  no failover comes, and the worker logs `reaction skipped` with the
  reason `candidate_refuses`. The worker holds an agent as refusing from
  its refusal of a syncup until it accepts a syncup, reports the revision
  it refused as applied, or misses a check round. It is failed over only
  at the levels where reactions run, `READWRITE`, `READONLY` and
  `NO_CLONE` ([Change a pool's level](#change-a-pools-level)).
- Cntlr replacement: the worker replaces each cntlr on the node that is not
  disabled and has had its health epoch for the pool's `cntlr_unhealthy`,
  600 seconds at the default. It deletes the cntlr and makes a new one with
  the same cntlid slot on a controller node that is enabled, healthy, has
  room and holds no cntlr of the pool, in another location when it can. It
  replaces a primary only when the pool has no cntlr to fail it over to, and
  the new cntlr is then the primary. Each replacement takes the old cntlr off
  the dead node's `cntlr_ptr_list`, so with room elsewhere the node empties
  itself about 600 seconds after it died. With no controller node to take a
  cntlr, the worker logs `reaction skipped` with the reason `no candidate`,
  and tries again at its next passes. The replacement in a pool of one cntlr
  can cost a Kubernetes volume its path ([A one-cntlr Kubernetes volume after
  a cntlr replacement](#a-one-cntlr-kubernetes-volume-after-a-cntlr-replacement)).
  The worker replaces a cntlr only at the levels where reactions run, as
  for a failover: a pool held at `NO_THINPOOL` or below keeps every cntlr
  it has on the dead node, primary or standby, and the worker logs no
  `no candidate` record for them.
- The worker never replaces a disabled cntlr.
- A node that comes back before its cntlrs are replaced: its agent answers
  that it is starting while it builds its stored cntlrs again, and the
  worker waits for it for up to the start hold, 120 seconds at the default.
  Its cntlrs then serve again, those that were failed over as standbys. A
  node that comes back after: its agent removes the cntlrs it no longer
  holds.
- Nothing points a clone at the new primary of its source pool, and
  nothing adds a new controller node of a clone's pool to its transfer's
  list (step 3).
- Nothing deletes the node.

Safe steps:

1. Disable the node, so that no cntlr is placed on it if it comes back for a
   while: `dnvctl cn set-disabled --addr <addr> --disabled` ([Disable a
   node](#disable-a-node)).
2. Wait for the replacements, or make them now. At `NO_THINPOOL` or below
   none comes: make each one now. For each cntlr on the node,
   in its pool, with `--rev` the pool's `sp_rev.revision` from
   `dnvctl --sp '<pool name>' sp get`, read again before each command:
   1. If the old cntlr is the primary and the pool's level is
      `NO_THINPOOL` or below, first step the level back, as step 2 of
      [Replace a cntlr by hand](#replace-a-cntlr-by-hand) says: at those
      levels the worker makes no failover and no cntlr replacement.
      Give the pool a new cntlr:
      `dnvctl --sp '<pool name>' cntlr create --slot <slot> --rev <revision>`,
      with a slot of `sp_conf.cntlid_slot_list` that no cntlr of the pool
      uses as its `cntlid_slot`. [Remove a node](#remove-a-node), step 3,
      sub-step 1, gives the create's refusals and their ways out. The
      `cntlr_id` of the reply is the new cntlr's id.
   2. Wait until the new cntlr is built: `dnvctl --sp '<pool name>' sp get`
      shows it with `err_epoch` 0, and
      `dnvctl --sp '<pool name>' cntlr inspect --id <new cntlr id>` shows
      `applied_revision` at least the pool's `sp_rev.revision` and each row
      under `cntlr_info.leg_id_to_leg` as `RES_STATUS_OK`, or, at `NO_SIDE`
      and `DISABLE`, as `RES_STATUS_MISSING` with the reason
      `RES_REASON_SP_LEVEL`. The row of a leg that `sp get` shows with an
      `err_epoch` that is not 0 while a cntlr other than the new one is the
      primary can stay `RES_STATUS_ERROR`: the primary cannot read that leg
      either. So can the row of a leg one of whose sides `sp get` shows
      with an `err_epoch` that is not 0, whichever cntlr is the primary, as
      when a disk node went down with the controller node: the worker's
      checks of that side fail too. If that disk node is gone for good,
      replace it as [Replace a dead disk node](#replace-a-dead-disk-node)
      says. A new leg of a running repair shows its `err_epoch`, and those
      of its sides, in `dnvctl --sp '<pool name>' repair list`, and a leg
      of a clone's meta group, the group that holds a clone's copy
      records, in `dnvctl --sp '<pool name>' clone get --name <clone>`. If
      the old cntlr is the primary, the worker fails it over at its next
      passes to an enabled cntlr with no health epoch whose agent does not
      refuse the worker's syncups, the new one or another, before or after
      the new one is built.
      The pool serves again once the new primary is built and `sp get`
      shows it with `settling` false.
   3. Disable the old cntlr:
      `dnvctl --sp '<pool name>' cntlr set-enabled --id <old cntlr id> --enabled=false --rev <revision>`.
      Do it only once another cntlr is the primary: a disabled cntlr is
      never replaced, so a dead primary disabled while its pool has no other
      cntlr to fail over to is never replaced either, and its pool stays
      without IO until you enable it again and the worker replaces it.
   4. Delete the old cntlr:
      `dnvctl --sp '<pool name>' cntlr delete --id <old cntlr id> --rev <revision>`.
      The gateway never deletes a primary or an enabled cntlr.
   5. If sub-step 1 stepped the pool's level back and the pool must stay at
      its old level, set that level again:
      `dnvctl --sp '<pool name>' sp set-level --level <the old level> --rev <revision>`.

   If the worker replaces the old cntlr while you do this, sub-steps 3 and
   4 find it gone, and the pool has one cntlr more than before. To get back
   to the count it had, disable one of the two new cntlrs that is not the
   primary, and delete it the same way.
3. Once each pool of the node serves again, check its copies between
   pools. A clone reads its source through the source pool's primary
   alone, at the address the clone holds, and only a controller node on a
   transfer's list may read the transfer. Neither changes by itself. Until
   you change it, the copy stops, and a host's read of a block not yet
   copied waits or fails. For each pool of the node:
   - If it holds a transfer, under `sp_conf.xfer_name_list` of
     `dnvctl --sp '<pool name>' sp get`, and its primary moved, point the
     clone that reads the transfer at the new primary:
     `dnvctl --sp '<clone pool>' clone set-tr --name <clone> --src-tr-type <tr_type> --src-adr-fam <adr_fam> --src-tr-addr <tr_addr> --src-tr-svc-id <tr_svc_id>`,
     with the four fields of `nvme_tr_conf` of the entry of `cntlr_list`
     whose `primary` is true in that `sp get`. Type the four address
     flags: without them `dnvctl` sends `tcp`, `ipv4`, 127.0.0.1 and 4420.
     For a clone in another cluster, run the command there, with that
     cluster's `--cluster` and, for another etcd, the `--gateway-address`
     of its gateways ([Clone from another
     cluster](#clone-from-another-cluster)).
   - If it holds a clone, under `sp_conf.clone_name_list`, and a cntlr of
     the pool is now on a controller node that the transfer's list lacks,
     give the transfer its new list:
     `dnvctl --sp '<transfer pool>' xfer set-hosts --name <transfer> --hosts <the whole new list>`,
     the host NQNs separated by commas.

   [Move a volume to another pool](#move-a-volume-to-another-pool), step 13,
   gives both commands. The clone that reads a transfer shows the
   transfer's NQN, which step 8 there works out, as `src_nqn` in
   `dnvctl --sp '<clone pool>' clone get --name <clone>`, and step 6 there
   works out a controller node's host NQN. For a source outside this
   cluster, let the new controller node in there ([Clone from another
   cluster](#clone-from-another-cluster)).
4. Once `dnvctl cn get --addr <addr>` shows `cn_conf.cntlr_ptr_list` as
   `[]`, note the node's id, its `cn_conf.cn_id`, and the cluster's
   `cluster_id`, which `dnvctl cluster get` prints: once the node is
   deleted, no record holds its id, and step 5 names the node's files,
   objects and host NQN by the two ids. Then delete the node with force:
   `dnvctl cn delete --addr <addr> --force`. Its reply prints the node's id
   as `cn_id` too.
   Without `--force` the delete is refused, since the agent does not answer.
5. If the machine comes back, stop its controller node agent at once, or
   before it starts, delete the node's local files, and remove what it
   built, as [Leftovers on a node](#leftovers-on-a-node), step 3, says: the
   agent builds again, each time it starts, what its files name, NVMe-oF
   exports to hosts included, and what it built stays after it stops.

Steps that lose data: none deletes volume data, since a controller node
holds none. The disable of step 2.3 on a cntlr that is still the primary
keeps its pool without IO.

When to stop: a new cntlr that is still not built, as step 2.2 reads it, a
few minutes after its create, for any cause but a row that step 2.2 sets
aside: as when a row under `cntlr_info.leg_id_to_leg` stays
`RES_STATUS_ERROR` for another leg, `applied_revision` stays below the
pool's `sp_rev.revision`, or `dnvctl --sp '<pool name>' sp get` shows the
new cntlr's own `err_epoch` not 0 (do not go on to step 2.3); a pool
whose IO does not come back, with no new primary whose `settling` is
false a minute after it became primary; a `no candidate` record that goes
on while a controller node has room. Ask for help.

Tested: the e2e suite's react case kills the primary's controller node
agent, and checks the failover, the replacement of the dead cntlr on a node
that held none, with its slot and its role, and that the dead agent,
started again, removes what it left. It also resets the primary's machine
twice, which fails the pool over and replaces the lost cntlr, and the
machine comes back with no cntlr of the pool. It runs with short thresholds,
not the defaults, and passed in the lab on 2026-11-21. A node back before
its cntlrs are replaced, the replacement by hand of step 2 on a dead node,
and a forced `dnvctl cn delete` with real agents are not tested. Neither
are the commands of step 3 after a failover or a replacement: the e2e
suite's copy case runs `xfer set-hosts`, and `clone set-tr` with the same
address in a run whose copy is not whole by then ([Move a volume to another
pool](#move-a-volume-to-another-pool)).

## Set up the control plane

Words: an **etcd server** is one of the etcd processes that keep the same
store, which etcd's own tools call a member. A **control-plane machine** is
a machine that runs part of the control plane: an etcd server, a gateway, a
worker or a cdc. The kit's units for them, in `deploy/systemd/`, are
`dnv-etcd.service`, an example for one etcd server, and
`dnv-gateway.service`, `dnv-worker.service` and `dnv-cdc.service`; the
examples of their settings are in `deploy/dnv/`.

When: once, before the first cluster of a new etcd is created; and when a
gateway, a worker or a cdc is added on a machine. To add an etcd server to
a running etcd, follow etcd's own steps in place of step 1: such a server
is first added with `etcdctl member add`, and it starts with
`--initial-cluster-state existing`, not `new` as in the example.

What dnv does alone: nothing. No dnv program installs or starts another,
none checks how etcd runs, and the kit enables no unit. Each dnv program
starts whether etcd answers or not: until etcd answers, the gateway fails
each call that needs it, and the cdc answers no host. A worker that has
started drives nothing for its join grace, the worker's
`--vote-grace-time`, 60 seconds at the default.

Safe steps, as root on each control-plane machine:

1. On each machine of an etcd server, install a release of etcd 3.5.13 or
   later as `/usr/local/bin/etcd`, and copy
   `deploy/systemd/dnv-etcd.service` into `/etc/systemd/system/`. In it,
   give the server its own `--name` and addresses, and list every server in
   `--initial-cluster`. Keep `--max-txn-ops` and `--max-request-bytes` at
   the unit's values or higher, since etcd's defaults are far under what
   dnv writes in one request, and keep `--auto-compaction-retention`, since
   no dnv program compacts etcd. etcd keeps its data in
   `/var/lib/dnv-etcd`, which the unit makes: keep it on a file system
   apart from the logs, or cap the journal (`SystemMaxUse=` in
   `journald.conf`), since etcd refuses a write it cannot save. Then run
   `systemctl daemon-reload` and `systemctl start dnv-etcd` on each server.
   On a new etcd each start waits for the other servers too.
2. Open the network the control plane needs, and no more: [The
   networks](#the-networks) lists who connects to whom and gives the
   firewall example. Keep the machine's clock in time with a time sync
   service, as step 1 of [Prepare a node](#prepare-a-node) says: the
   workers' clocks stamp the health epochs.
3. Copy from the release folder into `/usr/local/bin/` each of
   `dnv-gateway`, `dnv-worker` and `dnv-cdc` that the machine runs, and
   `dnvctl`, and copy their units from `deploy/systemd/` into
   `/etc/systemd/system/`. Make each program's settings file in
   `/etc/dnv/`, mode 0644, from its example in `deploy/dnv/`, under the
   same name, with your own values: `etcd-endpoints`, the client address of
   every etcd server in one string, separated by commas; the gateway's
   `grpc-address`, where `dnvctl` and the Kubernetes driver call it; and
   the cdc's `tr-addr` and `tr-svc-id`, an address of the machine and the
   port hosts reach the cdc on. Settings for `dnvctl`, made from
   `deploy/dnv/dnvctl.yaml`, go in a file that its `--config` flag or the
   environment variable `DNVCTL_CONFIG` names. Then run
   `systemctl daemon-reload`.
4. Start every gateway and every cdc, in any order:
   `systemctl start dnv-gateway dnv-cdc`. Check that each gateway answers:
   `dnvctl --gateway-address <its address> cluster list` prints the
   clusters, an empty list on a new etcd. Check that each cdc has read
   etcd: `journalctl -u dnv-cdc` holds the record `cdc scan complete`.
5. Start the workers: `systemctl start dnv-worker`. On a new etcd, which
   holds no node yet, start them now. Once nodes exist, the workers start
   last, once every agent has logged `agent started`, and stop first (the
   start order in [Prepare a node](#prepare-a-node)).
6. Create the cluster, once:
   `dnvctl --gateway-address <address> cluster create --name <name>`. Its
   `--extent-size`, `--md-check-days` and `--start-hold-seconds`, left out
   for their defaults, stay as they are for the cluster's whole life: no
   call changes them. Then prepare its nodes and create them ([Prepare a
   node](#prepare-a-node)).
7. `systemctl enable` a unit to start it at boot too. Before you enable a
   worker's unit, read the start order in [Prepare a node](#prepare-a-node):
   at boot, nothing starts a worker after the agents of other machines.
   Take backups from then on ([Back up etcd](#back-up-etcd)).

Steps that lose data: none; no step writes to a node's disk. An etcd with
lower limits than the example's refuses the larger writes of dnv, such as
the create of a pool of many slices, and an etcd without automatic
compaction fills its store and then refuses writes ([etcd refuses writes:
its store is full](#etcd-refuses-writes-its-store-is-full)).

When to stop: an etcd server whose start does not end once every server has
started; a gateway that does not answer `cluster list` while etcd runs; a
cdc that does not log `cdc scan complete`. Read the program's log,
`journalctl -u <unit>`, where a program that could not listen says so, and
ask for help.

Tested: a check by hand on one lab guest, on 2026-11-22, set the guest up
from an archive that `make release` built from the repository, whose
`deploy/` holds the kit: etcd 3.6.14 as one etcd server on the guest's own
loopback address, from the example unit with its
addresses changed, and the gateway, the worker and the cdc from their
units, with settings made from the examples, on the same machine as the
agents. Every unit started; a cluster, its nodes, a pool and a volume were
made with `dnvctl`; and, enabled, every unit came back after a reboot. [dnv
on one machine](tutorial_all_in_one.md) starts the same programs by hand,
with the example's etcd flags. An etcd of several servers started from the
example unit, and a control plane on machines of its own, are not tested.

## etcd refuses writes: its store is full

Words: etcd runs as one **etcd server** or several that keep the same store;
etcd's own tools and output call each of them a member, which is not an md
member (above). etcd keeps every old value of every key, its **history**,
and counts every change to the whole store in its **store revision**, which
is etcd's own counter, not a pool's revision. A **compaction** drops the
history older than a store revision. The store's file on disk may grow to
the **quota**, set by etcd's `--quota-backend-bytes` flag, 2 GiB when the
flag is not given; the quota counts the file's size, not the part of it in
use. A **defragmentation** gives the space a compaction freed back to the
disk. When the file reaches the quota, etcd raises its **NOSPACE alarm** and
refuses every write but a delete until the alarm is disarmed; reads still
work. etcd's `--auto-compaction-retention` flag compacts on its own; without
it, or with a long retention, the history grows until the store is full.

Symptom: every worker stops driving its pools, and its log shows `worker
fenced` again about every three vote intervals (the worker's
`--vote-interval`, 10 seconds at the default, so about every 30 seconds).
The worker's `etcd put` records carry the error `database space exceeded`. A
change through `dnvctl` that writes a value fails with the same error. Reads
still work, and the hosts' IO goes on.

How to confirm:

1. Run `etcdctl --endpoints <every etcd server> alarm list`. It prints
   `alarm:NOSPACE` for one etcd server or more.
2. Run `etcdctl --endpoints <every etcd server> endpoint status -w table`.
   The DB SIZE column is at the QUOTA column, close under it or above it;
   the alarm of step 1 is the sign. While any alarm stands, `etcdctl
   endpoint health` reports every etcd server unhealthy and names the alarm:
   that is the alarm, not a broken etcd server, so do not restart an etcd
   server for it. A restart does not clear the alarm.

What dnv does alone: nothing frees the store, since no dnv program compacts
etcd. While etcd refuses writes, no pool is driven: no failover, no repair,
no grow and no other reaction runs, as while etcd is down ([the scope
page](scope.md), section 6). The agents keep serving what they last built.
As soon as etcd takes writes again, every worker registers again with no
step from you, and drives its pools again one grace time later (the worker's
`--vote-grace-time`, 60 seconds at the default).

Safe steps:

1. Read the store revision: `etcdctl --endpoints <every etcd server>
   endpoint status -w fields`. Use the largest `"Revision"` it prints.
2. Compact the history up to it, and wait until etcd has removed the old
   values: `etcdctl --endpoints <every etcd server> --command-timeout 10m
   compact --physical <revision>`. It returns once the etcd server that
   answers the call has removed them, which can take minutes on a large
   store; the other etcd servers remove theirs at about the same time, and
   step 4 shows one that had not finished. The current value of every key
   stays.
3. Only after step 2 has returned, defragment every etcd server: `etcdctl
   --endpoints <every etcd server> --command-timeout 10m defrag --cluster`.
   With `--cluster`, `etcdctl` defragments one etcd server after another,
   and an etcd server answers nothing while its own defragmentation runs. If
   the command reports an etcd server failed with a deadline exceeded, that
   etcd server is still at work: wait until `etcdctl --endpoints <that etcd
   server> endpoint status` answers before you go on.
4. Read `etcdctl --endpoints <every etcd server> endpoint status -w table`
   again. If DB SIZE is now well under QUOTA, go on. If DB SIZE is still
   high but IN USE is small, the removal was still running when the
   defragmentation ran: defragment again, as in step 3, and do not raise the
   quota. If IN USE itself stays near QUOTA, the keys fill the quota, not
   their history: see when to stop, below.
5. Disarm the alarm: `etcdctl --endpoints <every etcd server> alarm disarm`.
6. Check: `alarm list` prints nothing, and each worker's log shows `worker
   registered` for each of its roles. A worker can log one more `worker
   fenced` right after the disarm, followed by `worker registered`; what
   counts is that no `worker fenced` comes after its last `worker
   registered`.
7. So that it does not come back, run every etcd server with
   `--auto-compaction-retention`, such as `1h`, as the tutorial does.

Steps that can lose data: deleting keys to free space. A delete still lands
while the alarm stands. Never delete a key that starts with `dnv `: every
such key is part of the desired state, and the nodes act on what the store
no longer holds, as after a restore ([Restore etcd from a
backup](#restore-etcd-from-a-backup)).

When to stop: IN USE stays near QUOTA in step 4, or the alarm comes back
right after the disarm. Then the keys themselves fill the quota, not their
history, and no compaction helps. Raise the quota: restart one etcd server
at a time with a larger `--quota-backend-bytes`, each on its own data
directory. Before you restart the next etcd server, run `etcdctl --endpoints
<every etcd server> endpoint status -w table`: the restarted etcd server
must answer, show the same RAFT TERM as the others (etcd's count of its
leader elections), and show a RAFT APPLIED INDEX (how far the etcd server
has applied the store's changes) equal to theirs or close to it. `endpoint
health` reports every etcd server unhealthy until the alarm is disarmed, so
it is no sign here. Then disarm the alarm again. Ask for help before you
delete anything.

Tested: the worker suite's three-member mode (`--etcd-members 3`, three etcd
servers) runs these steps in its case etcd_nospace on the lab. It fills
their store at a small quota with the history of one key, and checks that
etcd refuses the filler's write, that a read and a delete still land, and
that every worker stops driving its pools again and again, each stop logging
`worker fenced`. It then compacts with `--physical`, defragments every etcd
server and disarms the alarm, and checks that every worker comes back with
no other step and none stops again. Its store is a few tens of MB, so it
does not show a removal that outlasts the steps: step 3's wait on a large
store, step 4's second defragmentation and the raise of the quota are not
tested.

## Back up etcd

Words: a **backup** is etcd's own snapshot of its whole store in one file,
made with `etcdctl snapshot save`. It holds the desired state of every
cluster of that etcd. etcd runs as one **etcd server** or several, as [etcd
refuses writes: its store is full](#etcd-refuses-writes-its-store-is-full)
says.

When to use: on a schedule, always; before an upgrade; and right after a
restore, as step 7 of [Restore etcd from a backup](#restore-etcd-from-a-backup)
says.

What dnv does alone: nothing. No dnv program backs etcd up, and nothing
warns when no backup is taken.

Safe steps:

1. Take the backup from one etcd server, as root or as etcd's user:
   `etcdctl --endpoints <one etcd server> snapshot save <file>`. With
   several etcd servers, one is enough, since each holds the whole store,
   but take it from etcd's leader, the server that orders every write:
   `etcdctl --endpoints <every etcd server> endpoint status -w table` shows
   it with `IS LEADER` true and the highest `RAFT TERM`, etcd's count of
   its leader elections. A server cut off from the others can hold an older
   store.
2. Check it: `etcdutl snapshot status <file> -w json` prints its store
   revision, its count of keys and its size. A file that this command
   cannot read is no backup.
3. Copy the file off the machines of etcd, and keep several, the newest
   ones and some older ones, as a restore of one record may need a backup
   from just before a loss ([Restore one record from a
   backup](#restore-one-record-from-a-backup)).
4. Run steps 1 to 3 on a schedule, for example from a systemd timer or a
   cron job, every hour or more often. The time between two backups is what
   a restore loses: every pool, volume and snapshot made after the backup
   is lost with its data, and every volume made after the backup loses its
   thin id, dm-thin's device number of the volume inside its pool, which
   the primary then deletes with the blocks it holds; none of this can be
   undone. A pool grown, or whose legs moved, after the backup can break
   ([Restore etcd from a backup](#restore-etcd-from-a-backup)).

Steps that can lose data: none in the backup itself. Keep the file where
only root can read it: it names every host, subsystem and address of each
cluster, though it holds no volume data, and dnv keeps no secret in etcd.

When to stop: `etcdctl snapshot save` fails, or `etcdutl snapshot status`
cannot read the file: the etcd server is in trouble; read [etcd refuses
writes: its store is full](#etcd-refuses-writes-its-store-is-full) and ask
for help.

Tested: the e2e suite's upgrade case (`integtest/e2e_test.sh`, run with
`--only upgrade`) takes a backup with `etcdctl snapshot save` from one etcd server
and reads it with `etcdutl snapshot status`; it passed in the lab on
2026-11-21. The rehearsal of [Restore one record from a
backup](#restore-one-record-from-a-backup) took one too, on 2026-11-22. A schedule, and a
backup taken from one of several etcd servers, are not tested.

## Restore etcd from a backup

Words: a **backup** is as [Back up etcd](#back-up-etcd) says. A **restore**
is etcd's restore tool run on a backup: `etcdutl snapshot restore` on etcd
3.6. etcd's **quorum** is more than half of its etcd servers: etcd writes and
reads only while a quorum of its servers answer one another. A **thin id** is
dm-thin's device number of a volume inside its pool. The **restore check**,
`dnvctl cn check-restore`, compares what one controller node's agent holds
with the restored etcd, pool by pool. An agent's local files are as [Remove
a node](#remove-a-node) says.

When: etcd lost its data, or holds data that cannot be used, or lost its
quorum for good, and a backup is the copy of the desired state left.

How to confirm: etcd does not start on its data, or `dnvctl cluster list`
lists none of the clusters it held. A lost quorum: half of the etcd
servers or fewer answer `etcdctl --endpoints <every etcd server> endpoint
status`, and the others will not come back. While they may come back, etcd is
only down: wait for them ([While the control plane is
down](#while-the-control-plane-is-down)), since a restore loses every change
since the backup.

What dnv does alone:

- Nothing restores etcd by itself. While etcd is down the data plane goes
  on: each node keeps what it built, hosts keep their IO, and the workers
  change nothing. Every cdc stays stopped from step 1 to step 7: on a host
  that uses nvme-stas, an outage of more than a few seconds is not tested,
  and nvme-stas may drop the host's paths ([While the control plane is
  down](#while-the-control-plane-is-down)).
- A restore brings the desired state back to the backup's time. What was made
  after the backup is lost: disk nodes free the sides the restored state does
  not name, data included, and primaries delete the volumes it does not name,
  data included. A pool that grew, or whose legs moved, since the backup can
  break. What was deleted since comes back. A node added since is unknown to
  the restored state. A host added to a subsystem's list since the backup is
  off the restored list: once the workers start, each controller node whose cn
  agent ends connections ends that host's connection, and its IO fails
  ([Change a volume's hosts](#change-a-volumes-hosts)). A Kubernetes machine
  that started to use a volume after the backup loses it this way. A host
  taken off a list since the backup is on the restored list again, so it may
  connect again. The restore check does not name such hosts.
- Each agent's local files hold what its node was told before the restore.
  An agent that keeps them refuses a syncup of the restored state until the
  pool's or the node's revision climbs back to the one it holds, and then
  applies it, so keeping the files only delays the losses above. Deleting
  them lets the restored state reach the node at once.

Safe steps:

1. Stop every worker, every cdc and every gateway, and the controller
   plugin of every CSI driver that calls the gateways, the part of the
   driver that calls the gateway, for example with
   `kubectl -n dnv-csi scale deployment dnv-csi-controller --replicas=0`.
   Keep every agent running. A worker would hand out ids from the restored
   counters, and the controller plugin would retry a volume create as soon
   as a gateway answers.
2. Stop every etcd server and move its data directory aside. When the data
   directory is the mount point of a file system of its own, `mv` cannot
   move it: move the `member` folder in it aside instead, to
   `<data directory>/member.old`. After a lost
   quorum, and only then, first choose what to restore: a server that
   survived with its data directory can hold a newer store than any backup,
   but one cut off before the loss can hold an older one. Compare the store
   revision that `etcdutl snapshot status <file> -w json` prints for the file
   `member/snap/db` in each data directory you moved aside, or
   `<data directory>/member.old/snap/db` where you moved the `member`
   folder alone, with that of the newest backup, and use the file whose
   revision is the highest as the backup file below. A server's file
   needs `--skip-hash-check` added to each restore, etcd's own way for a
   file copied from a data directory.
   When etcd lost its data or holds data that cannot be used, restore no
   server's own file: restore the newest backup whose data can be used.
   Restore the backup into the data directory that etcd's own command line
   names, `--data-dir`, with the name and the
   peer address of that command line:
   `etcdutl snapshot restore <backup file> --data-dir <data directory> --name <name> --initial-cluster <name>=<peer URL> --initial-advertise-peer-urls <peer URL>`.
   For the etcd command of [dnv on one machine](tutorial_all_in_one.md),
   that is `--data-dir ./etcd-data --name dnv --initial-cluster
   dnv=http://127.0.0.1:2380 --initial-advertise-peer-urls
   http://127.0.0.1:2380`, run in the tutorial's folder; for the kit's
   example unit, `dnv-etcd.service`, the data directory is
   `/var/lib/dnv-etcd`. The restored `member` folder must end up in that
   path and no other. etcd started on a data directory that is missing or
   holds no `member` folder makes a new, empty store there. It does so
   under `--initial-cluster-state new`, which is the kit unit's setting and
   etcd's default. systemd makes the
   unit's directory, empty, when it is missing. `etcdutl` refuses a data
   directory that is not empty, such as a mount point that holds
   `member.old`: there, restore into a new folder inside it,
   `--data-dir <data directory>/restored`, then move
   `<data directory>/restored/member` to `<data directory>/member` and
   remove the empty `restored` folder. Start etcd with its usual command
   line, with the request caps and the automatic compaction of the
   tutorial's etcd command, as [the
   requirements](introduction.md#requirements) say. With several etcd
   servers, restore the same backup on each of them, each with its own
   `--name` and `--initial-advertise-peer-urls` and the same
   `--initial-cluster`, which names every server you restore:
   `<name 1>=<peer URL 1>,<name 2>=<peer URL 2>,<name 3>=<peer URL 3>`.
   Name there only the servers you restore and start now: after a lost
   quorum, the servers that are left and any new machines in place of the
   lost ones, each restored the same way. A server that `--initial-cluster`
   names but that never starts counts against the quorum: when half of the
   named servers or fewer start, etcd elects no leader and answers nothing,
   and `systemctl start` of the kit's example unit, `dnv-etcd.service`,
   waits without end. Add a server later by etcd's own steps, as the When
   part of [Set up the control plane](#set-up-the-control-plane) says.
   Start every etcd server on its restored directory. Then read each
   server alone, since a server started on an empty store can join the
   restored ones, and then answers from its empty store:
   `etcdctl --endpoints <client address of that server> --consistency=s get 'dnv cluster_conf ' --prefix --keys-only`.
   `--consistency=s` makes the server answer from its own store at once,
   without asking the leader first. Every server must print the same keys,
   at least one: the key `dnv cluster_conf <name>` of each cluster of the
   backup. Also read each server alone with `etcdctl --endpoints <client
   address of that server> endpoint status -w fields`: every server must
   print the same `"ClusterID"`, the id etcd gives the servers that elect
   one leader together. A server restored with an `--initial-cluster`
   unlike the others' gets another id and starts as an etcd of its own
   beside them, with the same keys. The gateways and the workers would then
   send their calls to both, and two workers could drive the same pools.
   When the servers print different keys, or none, or different ids, a
   server did not start on the restored backup, or started as an etcd of
   its own beside the others: stop every etcd server,
   move every data directory aside again to a name not yet taken, or, where
   you moved the `member` folder alone, that folder to a name not yet
   taken, such as `member.old2`, since `mv` onto a folder that exists moves
   into it, and restore every server again, each with the same
   `--initial-cluster`, which names every server you restore. Only then
   start the gateways,
   and check that `dnvctl cluster list` names the clusters whose keys the
   first read printed. When it does not, stop the gateways and check that
   the `etcd-endpoints` of their settings name the restored servers.
3. For every cluster, before any other call but `dnvctl cluster list`,
   which names the clusters, run `dnvctl cluster advance-ids --name
   <cluster>`. Without this step a new object can take the ids, and with
   them the data on the nodes, of an object made after the backup. Even
   with it, a pool that created more than 1,048,576 volumes, snapshots
   included, after the backup can hand one of their thin ids, with its data,
   to a new volume ([a new volume refused for a thin id that holds
   data](#a-new-volume-refused-for-a-thin-id-that-holds-data)).
4. Run the restore check for every controller node: each address
   `dnvctl cn list` prints, and each controller node added after the
   backup, which the list does not hold:
   `dnvctl --timeout 120 cn check-restore --addr <addr>`. A node of many
   pools needs a longer `--timeout`. Each row is one cntlr of a pool, and
   its `verdict` says what the restore does to what that cntlr was told. A
   standby can hold an older state than its primary, and its row then
   lists as lost a volume that was deleted later. So read each pool from
   its row with the highest `held_revision`, on any controller node: the
   cntlr the worker told last. `held_cn_list` lists the controller nodes
   the agent holds; when it is empty, the agent lost its local files, and
   every cntlr its node had in the backup reads not held, or record lost
   where a record of its pool in the backup is lost (both below).
   - `RESTORE_VERDICT_SAME`: this cntlr was told nothing since the
     backup.
   - `RESTORE_VERDICT_OLDER`: the pool loses every change since the
     backup, and its layout stays as it is. `lost_td_list` lists its
     volumes that are lost, by `td_id` and thin id. `returning_td_list`
     lists the volumes deleted since the backup, by `td_id`: their records
     come back. A volume whose thin id the primary deleted comes back as
     an error with no data. A volume whose thin id the pool still holds,
     because its delete on the primary failed or had not yet run, comes
     back working, with its old data. `dnvctl td list` shows each volume
     with its name and `td_id`. After an upgrade from 0.1, a backup taken
     before the upgrade reads older, not the same, for a RAID1 pool no
     call touched since: the new release marks each such pool's arrays as
     made after the upgrade, as its primary's reports first read them
     running. Such a pool loses only that mark, which the worker sets
     again, and lists no lost volume. A pool without RAID1 has no arrays
     to mark and reads the same.
   - `RESTORE_VERDICT_MAY_BREAK`: as older, and the pool's layout changed
     since the backup, so the pool may not come back working: a part of it
     was made since (a grow, a repair, a migration, a clone), or removed
     since (a migration finished or cancelled, a retired leg deleted, a
     clone drained). A side made since counts even when a later repair
     made its leg a retired leg. A repair that only finished since moves
     its new leg into the group, and a repair that took back a retired leg
     moves that leg back; both read older. `held_side_cnt` and
     `restored_side_cnt` count the sides of the two layouts. It can be a
     false alarm: a migration made after the backup and not yet at its
     cutover gives it too, while its old side still holds every write. A
     row whose `held_side_ids_incomplete` is true was told by an older
     version of the worker, which did not send the side of each retired
     leg. The check then cannot see every change, so the row reads may
     break whenever the cntlr holds a newer state than the backup. Three
     more changes read may break, though the layout is the same: a
     migration whose cutover the backup holds as not done, which may have
     run since; a cntlr whose role differs between what the agent holds
     and the backup, after a failover or a cntlr replacement since,
     including a cntlr made since that the backup does not name; and a
     namespace that serves in the backup but was parked or deleted since,
     such as the namespace of a volume moved to a pool of another etcd,
     before or after the move's last step deleted it. `break_reason_list`
     gives one line for each cause, which names the migrations and, by
     their `ns_id` (`dnvctl ss list` shows each subsystem's namespaces
     with it), the namespaces. On every pool with a row read may break,
     stop the hosts' IO before step 7 starts the workers: unmount its
     volumes on each host, or stop what writes them. Step 5's copy of a
     lost volume may read first. Once a worker starts, an old side of a
     migration can serve again with old bytes, or another pool's, the old
     primary can serve again beside the new one, and a volume can have two
     serving copies. The check's rows are the only record of this: step 6
     deletes the agents' local files, so no agent can tell a new worker
     which node served. Start the hosts' IO on such a pool again only
     after step 7, once these hold: `dnvctl migr get` shows each migration
     the line names with its cutover no longer pending, and the status
     read, `dnvctl cluster get-status`, lists no row of the pool; and each
     namespace the line names is parked again with `dnvctl ns
     set-suspended`, or deleted with `dnvctl ns delete`, as step 8 says
     for a namespace that came back.
   - `RESTORE_VERDICT_NEW`: the pool is not in the backup and is lost
     whole, with each volume `lost_td_list` lists. `nqn_list` names its
     subsystems; the NQN of a Kubernetes volume's subsystem carries the
     volume's name.
   - `RESTORE_VERDICT_BEHIND`: the agent holds an older state than the
     backup, because this cntlr was behind its pool at the backup and was
     not told since, or because its local files were put back from an
     older copy; nothing can be compared. An agent that lost its local
     files holds nothing, and its rows read not held, or record lost where
     a record of the pool in the backup is lost, until the worker tells it
     the current state again.
   - `RESTORE_VERDICT_NOT_HELD`: a cntlr of the backup that the node no
     longer holds, because the pool was deleted or the cntlr moved to
     another node after the backup, or because the agent lost its local
     files (`held_cn_list` is empty); it comes back.
   - `RESTORE_VERDICT_NOT_LOADED`: the agent names the cntlr but holds
     none of its state: it could not load the cntlr's file, or the cntlr
     was made just before the failure and its state never came, which is
     often a cntlr of a pool made after the backup. Nothing can be
     compared.
   - `RESTORE_VERDICT_RECORD_LOST`: a record of the pool in the backup is
     missing or broken, or the backup's node names a cntlr of a pool the
     backup does not hold.
   A lost volume whose `id_may_return` is true has a thin id that a new
   volume of its pool can take after the restore.
5. To keep a lost volume, copy its data off now, through a host that still
   has a path to it. No worker runs, so each node still serves what it held
   before the restore. The upgrade case of the e2e suite shows it on the
   lab: with no worker running and before any agent's files are reset, a
   host reads back every block its writer wrote to a volume of a pool made
   after the backup, and another host reads the data of a pool made before
   it.
6. For each agent: stop it, copy its local files aside, delete them, and
   start it again. The copy is the last record of what the node was told
   before the restore. Never put the copy back, nor local files from any
   other older copy: a disk node's agent that starts on older files removes
   every side made since their time, with its data ([the scope
   page](scope.md#55-records-and-tools)).
7. First set each subsystem's list to exactly the hosts that use it now.
   `dnvctl ss list` shows each list. On each host, `nvme list-subsys` shows
   the subsystems it is connected to, and
   `cat /sys/class/nvme/nvme*/hostnqn 2>/dev/null | sort -u` prints the
   host NQN of its connections. For a volume of the Kubernetes driver,
   `kubectl get volumeattachment` names the one
   Kubernetes node the volume is attached to, and the list must hold that
   node's host NQN alone, or nothing when the volume is attached to no
   node: the driver attaches a volume to a node only when the list is empty
   or names that node alone.
   Where a list misses a host that uses the volume, or names a host that
   stopped using it after the backup, give the whole list again, as step 2
   of [Change a volume's hosts](#change-a-volumes-hosts) says. Then start
   the workers and the cdc, let clients call the gateways again, start the
   CSI controller plugin again with its former count of copies, and take a
   new backup: a later restore of a backup from before step 3 would hand
   out again the ids handed out since.
8. Kubernetes still holds its record of each volume whose pool step 4 found
   new: delete the record, or make the volume again. Delete again each
   volume step 4 listed in `returning_td_list`: it was deleted after the
   backup. A namespace that exported the volume at the backup comes back
   too, and the delete is then refused with the number of that namespace
   and the NQN of its subsystem. First delete the namespace,
   `dnvctl ns delete --nqn <nqn> --idx <number>`, or point it at the
   volume it used before the restore,
   `dnvctl ns set-dev --nqn <nqn> --idx <number> --td <volume name>`. A
   pool deleted after the backup comes back: it is one whose rows of its
   `sp_id`, on every controller node, all read
   `RESTORE_VERDICT_NOT_HELD`, from checks whose `held_cn_list` each lists
   their node with a `revision` at or above that check's
   `restored_revision`. Delete it again. A node held at a lower revision
   was never told of a cntlr the backup names, so its rows do not show a
   delete. A pool that has any other row lives on another node, and one
   seen only by a node whose agent lost its local files, or only by nodes
   held at a lower revision, is not known to be deleted; delete neither.

Steps that can lose data: step 6 makes every loss of step 4 happen, with no
undo: every volume made after the backup, snapshots included, loses its
thin id, and the primary deletes it with the blocks it holds; an agent
whose files are kept meets the same losses later, once the revisions climb
back. Step 7 ends the connection of each host that a restored
list leaves out, and that host's writes fail, unless step 7 gave it back
first. A host taken off a list after the backup that step 7 leaves on the
restored list may connect to the volume again, as a host that uses nvme-stas
does by itself once the cdc lists the subsystem to it, and what writes on it
then writes beside the host that uses the volume now. Step 6 before step 3, or
a worker started before step 3, can give a new object the remains of one made
after the backup, data included. Step 1 keeps every cdc stopped until
step 7, and a host that uses nvme-stas may then drop its paths (What dnv
does alone): their IO fails, and writes not yet flushed are lost. Step 8
deletes again the volumes and the pools that came back, with any data they
hold: a volume that came back working
holds its old data. A volume made in a cluster created after the backup is
lost unseen: no call names that cluster.

When to stop and ask for help, before step 6: a node whose agent does not
answer the check, since a pool none of whose controller nodes answers is
not seen; a node whose check lists no held node while its node in the
backup names cntlrs, since its agent lost what the node was told; a pool
with a `RESTORE_VERDICT_BEHIND` or `RESTORE_VERDICT_NOT_LOADED` row and no
row read `RESTORE_VERDICT_SAME`, `RESTORE_VERDICT_OLDER`,
`RESTORE_VERDICT_MAY_BREAK` or `RESTORE_VERDICT_NEW`, whose losses are
then not known; a row read `RESTORE_VERDICT_MAY_BREAK` or
`RESTORE_VERDICT_RECORD_LOST`; a lost volume that must be kept and that no
host can reach. A pool whose every row is not held, from nodes whose
agents hold their nodes at or above the restored revision, was deleted
after the backup: step 8 deletes it again. The check lists no disk node's
sides: the sides a disk node frees at step 6 belong to the pools the check
finds new, older or breaking, and to the pools it does not see.

Tested: the gateway's Go tests check every verdict against planted records
and a fake agent, and the cn agent's tests check what it answers; the
gateway suite's smoke case runs the check over a fake controller node after
step 3's move of every id counter, which it makes through its own test
client, not through `dnvctl cluster advance-ids`. The upgrade case of the e2e suite
runs steps 1 to 6, step 2 without its reads of each etcd server alone and
its check of the cluster names, and, of step 7, the start of the workers
and the cdc and the new backup, on the lab's real agents, from a backup
taken under 0.1 and with the programs of a later build: a real `etcdutl`
restore into etcd's data directory, the old one
moved aside, the id step of step 3
(`dnvctl cluster advance-ids`), the check on every controller node, the reads
of step 5 and every agent's files reset. It then checks that a pool made
before the backup keeps its data, that a new pool's id lies above that of
the pool made after the backup and a new volume's above every id the grow
after the backup took, that a pool grown after
the backup breaks, and that the disk nodes free the sides made after the
backup. It does not read each etcd server alone, since with its one etcd
member that read is the gateway's own and has no other `"ClusterID"` to
compare, and it reads the restored pools through `dnvctl sp list`, not the
names `dnvctl cluster list` prints. It runs one etcd member and no
Kubernetes, adds or takes away no node after the backup, and adds no host
to and takes none off the list of a pool kept from before the backup (the
one subsystem made after it is lost whole), so step 7's list change has
nothing to do and step 8 does not run.
The restore of several etcd servers, after a lost quorum or otherwise, a
restore from a server's own data directory, and a restore into a data
directory that is the mount point of a file system of its own are not
tested. A check by hand with three etcd 3.6.14 servers on one machine, on
2026-11-22, restored two of them and started the third on an empty data
directory: the third joined them and became their leader, and read alone
it printed no key, while `endpoint status` showed the three alike. A
second check by hand, the same day, restored one backup on three
servers, one of them with an `--initial-cluster` that named it alone: read
alone, each printed the backup's key, and `endpoint status -w fields`
printed one `"ClusterID"` for that server and another for the two others.

### Restore one record from a backup

Words: a **lost record** is a record that a pool's lists name and that etcd
no longer holds, after a hand edit or a bug. Its key names its kind, its
cluster, its pool and its name, the ids in hex with 16 digits, such as
`dnv thin_device <cluster id> <pool id> <volume name>` for a volume. A
**scratch etcd** is a second etcd you start on a copy of a backup, on ports
of its own, for reading alone.

Symptom: `dnvctl cluster get-status` shows a `lost-record` line whose
object is the key and whose text says that the pool's records name this key
and etcd does not hold it, that the worker sends the pool nothing, and never
to remove the name from its list. For a volume's record,
`dnvctl td list` of the pool fails with the error code ABORTED, naming the
key as missing; `dnvctl td delete` of the volume answers NOT_FOUND, and a
create of its name ALREADY_EXISTS. The worker's log shows the record
`invalid stored conf` at `error` with `listed record missing:` and the
key.

Confirm: `etcdctl --endpoints <every etcd server> get '<key>' --keys-only`
prints nothing. Quote the key: it holds spaces.

What dnv does alone: the worker sends the pool's nodes nothing and runs no
reaction for the pool, a failover among them, and tries again every 2
seconds at the default. What the nodes built keeps serving. A change of the
pool made meanwhile is stored but reaches no node: a volume created
meanwhile is not built. Once the record is back, the worker drives the pool
again at its next try, with no step from you.

Safe steps, for a volume's record:

1. Change nothing in the pool until step 10.
2. Pick the newest backup that holds the record with `created` true: one
   taken after the volume was recorded as created.
3. Restore that backup into a scratch folder, with two free ports of your
   choice for the scratch etcd:
   `etcdutl snapshot restore <backup file> --data-dir <scratch folder>/etcd --name scratch --initial-cluster scratch=http://127.0.0.1:<peer port> --initial-advertise-peer-urls http://127.0.0.1:<peer port>`.
4. Start the scratch etcd on it:
   `etcd --name scratch --data-dir <scratch folder>/etcd --listen-client-urls http://127.0.0.1:<client port> --advertise-client-urls http://127.0.0.1:<client port> --listen-peer-urls http://127.0.0.1:<peer port> --initial-advertise-peer-urls http://127.0.0.1:<peer port> --initial-cluster scratch=http://127.0.0.1:<peer port> --max-txn-ops=1024 --max-request-bytes=33554432`.
   Never start a worker or a cdc on it: a worker there would drive the
   nodes back to the backup's state.
5. Read the record as the backup holds it, through a gateway of its own on
   the scratch etcd:
   `dnv-gateway --grpc-address 127.0.0.1:<gateway port> --etcd-endpoints 127.0.0.1:<client port>`,
   then
   `dnvctl --gateway-address 127.0.0.1:<gateway port> --cluster <cluster> --sp <pool> td list`.
   Check that `name_to_td.<volume>` shows `created` true, and note its
   `td_id`. Send no write through this gateway.
6. Check that the record is the one of the live volume, through the live
   gateway. While the record is lost the worker sends the primary nothing,
   so the primary keeps the volumes it last built, each under its `td_id`.
   `dnvctl cntlr inspect --id <primary cntlr id>` must show a row of the
   backup's `td_id` in `cntlr_info.td_id_to_thin_info`; the primary is the
   cntlr whose `primary` is true under `cntlr_list` of `dnvctl sp get`, and
   its id stands at the same place in `sp_conf.cntlr_id_list`. A volume
   deleted and made again under the same name after the backup has another
   `td_id`, and the primary's rows show that one, not the backup's: the
   backup's record is then of the old volume, so stop, as When to stop
   says.
7. Check again that the live etcd lacks the key, as Confirm says, and that
   the scratch etcd holds it:
   `etcdctl --endpoints http://127.0.0.1:<client port> get '<key>' --keys-only`
   prints the key.
8. Copy the record's value into a file. etcd prints a value in base64 in
   its JSON output:
   `etcdctl --endpoints http://127.0.0.1:<client port> get '<key>' -w json | jq -r '.kvs[0].value' | base64 -d > <scratch folder>/record.bin`.
9. Put it back on the live etcd:
   `etcdctl --endpoints <every etcd server> put '<key>' < <scratch folder>/record.bin`.
   It prints `OK`.
10. Stop the scratch gateway and the scratch etcd, and delete the scratch
    folder.
11. Check: within a few seconds the `lost-record` line is gone from
    every page of `dnvctl cluster get-status`, `dnvctl td list` answers
    again and shows the volume as the backup held it, and a change made
    meanwhile reaches the nodes, such as a volume created meanwhile that is
    now recorded as created.

Steps that can lose data:

- Removing the volume's name from the pool's lists, by a hand edit of the
  pool's record, instead of restoring the record: the primary then deletes
  that volume's data for good.
- The record of an older volume of the same name, one deleted and made
  again after the backup, put back without the check of step 6: the
  primary takes the live volume for one the pool no longer holds, and
  deletes its blocks for good.
- A record from a backup taken before the volume was recorded as created:
  the primary takes it for a volume to build, and a primary that does not
  hold the volume's device yet, such as after a failover, refuses to build
  it over a thin id that holds data ([A new volume refused for a thin id
  that holds data](#a-new-volume-refused-for-a-thin-id-that-holds-data)).
- A torn mark, the worker's record that a snapshot's slices hold its data
  from different points in time ([Snapshots](#snapshots)), is lost with the
  restore when the worker recorded it after the backup: such a snapshot
  then reads as whole.
- A worker started on the scratch etcd, or a put of another key or value.

When to stop: no backup holds the record with `created` true; or the
primary shows no row of the backup's `td_id`, as when the volume was made
again after that backup; or the line names another kind of record: a
slice, a cntlr, a subsystem, a clone, a transfer, a migration, a repair or
a retired leg, whose record changes with the pool's legs, its health and
its hosts, so that a backup's copy is older than the pool; or the line
names the pool's revision key, the pool's id-to-name record, or a disk
node's or a controller node's record. Ask for help.

Tested: rehearsed in the lab on 2026-11-22, on the control-plane machine
alone, with the test suites' fake agents in place of the nodes' agents:
etcd, a gateway and a worker at its shipped settings, four fake disk nodes
and two fake controller nodes, a RAID1 pool with one volume recorded as
created, and a backup. The volume's record was deleted by hand: the status
read showed its `lost-record` line, `dnvctl td list` failed with ABORTED,
the worker logged `invalid stored conf`, and a volume created meanwhile was
not built, its primary still at the older revision. Steps 3 to 10 ran as
written, with the read of the primary's rows of step 6 made before step 3:
the primary showed a row of the backup's `td_id`. The value put back was
byte for byte the one before the loss, and within two seconds the line was
gone and the volume created meanwhile was recorded as created. No run on
real agents, no volume made again after the backup, and no record of
another kind, is tested.

## Upgrade dnv

Words: an upgrade is **in lockstep**: every dnv program of one etcd stops,
every program file is replaced, and only then does any program start again,
since no two builds may run at once. That is every gateway, worker and cdc,
the agents of every cluster the etcd holds, and every CSI driver that calls
its gateways: its controller plugin, the part that calls the gateway, and
its node plugins, the part on each Kubernetes node. A **build** is one
version of the dnv programs. The **install kit**'s units of the dnv
programs, in `deploy/systemd/`, are `dnv-agent-dn@.service`, one instance
for each disk, `dnv-agent-cn.service`, `dnv-gateway.service`,
`dnv-worker.service` and `dnv-cdc.service`; the commands below use those
names, and a machine set up another way stops and starts its programs its
own way. The folder also holds `dnv-etcd.service`, an example for etcd,
which an upgrade of dnv does not touch.

When to use: a new release is out and its notes allow an upgrade from the
build you run. There is no rolling upgrade and no downgrade: no build may
replace a newer one ([the scope page](scope.md), section 7, Steps known
today, for the rolling upgrade, and section 5.5 for the downgrade).

What dnv does alone: nothing during the upgrade. A stopped node plugin, and
a stopped agent, leave the devices, the connections and the mounts of their
node in place, so the hosts' IO goes on while every program is stopped,
with no failover and no repair. No run stops every program while a host
uses nvme-stas: the cdc suite stops every cdc for a few seconds only, and
a longer outage may drop the host's paths ([While the control plane is
down](#while-the-control-plane-is-down)). Once the workers start, each
drives nothing for its join grace, the worker's `--vote-grace-time`, 60
seconds at the default. After an upgrade from 0.1, the new worker marks
the arrays of each RAID1 pool once, so a backup taken before the upgrade
then reads older, `RESTORE_VERDICT_OLDER`, to the restore check ([Restore etcd from a
backup](#restore-etcd-from-a-backup), step 4).

Safe steps, in this order, the order the e2e suite's upgrade case ran
(Tested says which steps it leaves out):

1. Before the upgrade: take a backup ([Back up etcd](#back-up-etcd)). Note
   what `dnvctl cluster get-status --name <cluster>` shows for each
   cluster, every page of it ([Read what is wrong](#read-what-is-wrong)).
   The build 0.1 has no status read, so before an upgrade from 0.1 there is
   nothing to note. Run the new build's node check on every node, with the
   flags its agent will run with ([Check a node before it
   joins](#check-a-node-before-it-joins)). On a node that ran 0.1 the
   lines `udev rule 58-dnv-dm.rules`, `udev rule 63-dnv-md.rules`,
   `udev rule 63-zz-dnv-md-array.rules` and `md assembly mask` fail or warn
   until step 6 installs the new rules: there those lines are expected,
   and the run of step 6 decides.
   On each host, compare `nvme list-subsys` with `dnvctl ss list`: a host
   that is off a subsystem's list but still connected loses its connection
   at the new agent's first converge, where the controller node's cn agent
   ends connections; stop the volume's users on it and run
   `nvme disconnect -n <subsystem nqn>` first.
2. Stop every worker first: `systemctl stop dnv-worker.service` on each
   control-plane machine. It returns once the worker has ended. Check with
   `systemctl is-active dnv-worker.service` that none still runs.
3. Stop every gateway and every cdc, in any order:
   `systemctl stop dnv-gateway.service dnv-cdc.service`. Stop the CSI
   driver: `kubectl -n dnv-csi scale deployment dnv-csi-controller
   --replicas=0` and `kubectl -n dnv-csi delete daemonset dnv-csi-node`.
4. Stop every agent: `systemctl stop dnv-agent-cn.service` on each
   controller node, then `systemctl stop 'dnv-agent-dn@*'` on each disk
   node. The upgrade case stopped the controller nodes' agents one machine
   at a time, the one with the most primaries first, 20 seconds apart, then
   every disk node's, and held all of them stopped 30 seconds: those gaps
   are the test's, which would show a worker left running by mistake; no
   rule asks for them.
5. Replace every program file of the new release on every machine,
   `dnvctl` included. On a machine set up from the install kit, also copy
   the new release's units of the dnv programs, the five that Words names,
   over the installed ones, each with your own changes of it made again,
   and run `systemctl daemon-reload`: until then systemd keeps the old
   units. A command line of the gateway, the worker or the cdc that still
   passes `--etcd-dial-timeout`, in a unit of your own or a script, must
   drop it: the new build refuses that flag, which bounded nothing. A key
   of that name in a config file, or the environment variables
   `DNV_GATEWAY_ETCD_DIAL_TIMEOUT`, `DNV_WORKER_ETCD_DIAL_TIMEOUT` and
   `DNV_CDC_ETCD_DIAL_TIMEOUT`, are ignored.
6. On every disk node and every controller node, install the udev rules of
   the new release's `deploy/udev/`, with the two settings that go with
   them ([Install the udev rules on a node](#install-the-udev-rules-on-a-node)).
   0.1 shipped none, so an upgrade from 0.1 installs them all. Then run the
   node check of step 1 again on the node: this run decides. No line may
   read `FAIL`; a `WARN` is handled as step 3 of [Check a node before it
   joins](#check-a-node-before-it-joins) says.
7. Start the agents: `systemctl start dnv-agent-dn@<instance>.service` for
   the instance of each disk on each disk node, then
   `systemctl start dnv-agent-cn.service` on each controller node. Wait until each agent's log holds `agent started`, the
   record that it has rebuilt its node from its local files. The worker
   waits for an agent that is still rebuilding for the cluster's start hold
   at most, 120 seconds by default ([A planned restart of an
   agent](#a-planned-restart-of-an-agent)), before it judges it.
8. Check that every program runs the new build. `--version` on each
   program file prints its build, such as `dnv-agent --version`, and the
   first record each agent logs, `agent node versions`, carries the
   `version` the running agent was built as. A program that was not stopped
   still runs the old build from a file the new one replaced. The programs
   of 0.1 have no `--version` ([the introduction](introduction.md#get-dnv)).
9. Start every gateway and every cdc, `systemctl start dnv-gateway.service
   dnv-cdc.service`, and check the `version` of their start records,
   `gateway starting` and `cdc starting`.
10. Build the driver's image from the new release's folder under the name
    its `deploy/controller.yaml` gives it, `dnv-csi:` and the release's
    version, and import it into every Kubernetes node's image store, as the
    CSI tutorial's "The image" does: the new manifests name the new tag,
    which no node holds yet. Then start the CSI driver from the new
    release's files, as the upgrade from 0.1 below names them, with `kubectl apply -f <file>` for each, which
    starts the node plugins again, and give the controller plugin back its
    copies: `kubectl -n dnv-csi scale deployment dnv-csi-controller
    --replicas=2`. Their start records carry `version` too.
11. Start the workers last: `systemctl start dnv-worker.service`, and check
    the `version` of the `worker starting` record.
12. Check: once the join grace has passed, `dnvctl cluster get-status
    --name <cluster>`, every page of it, shows for each cluster no line
    that step 1 did not show, and after an upgrade from 0.1 no line at
    all, and the worker's log holds no `reaction applied` record. Take a
    new backup.

For an upgrade from 0.1, also:

- The CSI driver: apply these files of the new release's `deploy/` folder
  by name: `namespace.yaml`, `csidriver.yaml`,
  `controller-serviceaccount.yaml`, `node-serviceaccount.yaml`,
  `controller-clusterrole.yaml`, `controller-clusterrolebinding.yaml`,
  `controller-role.yaml`, `controller-rolebinding.yaml`, `controller.yaml`
  and `node.yaml`, each with your own changes of it made again, such as
  another driver image name or another directory of the kubelet, the
  Kubernetes program on each machine. Do not apply `config.yaml`,
  `storageclass.yaml` or `networkpolicy.yaml`: they are examples that hold
  placeholders, and you keep your own copies. Applied, the example
  `config.yaml` would point both plugins at the placeholder addresses, so
  that every create of a volume and every attach, the step that lets a
  Kubernetes node into a volume's subsystem, fails; the example storage
  class, the Kubernetes record that names the cluster, the redundancy and
  the cntlr count of each new volume, would name the placeholder cluster,
  or be refused where your class of its name names another, since
  Kubernetes lets no one change a storage class's settings; and the example
  policy, which names no Kubernetes namespace, would replace your own copy
  in the current Kubernetes namespace. The rights of the controller plugin
  within its Kubernetes namespace are a file of their own,
  `controller-role.yaml`, and the new `controller-clusterrole.yaml` applied
  without it takes them away, after which the controller plugin's
  provisioner, the helper program that asks it for volumes, stops at its
  start.
- Every Kubernetes node: the node plugin of a later release starts only
  with native NVMe multipath on and the module nvme-fabrics loaded, which
  nvme-tcp loads, and one that does not start unmounts nothing ([A
  Kubernetes node plugin that does not start](#a-kubernetes-node-plugin-that-does-not-start)).
- A cn agent that finds, when it starts, that the kernel cannot end a
  host's connection to the controller node, or that cannot read whether it
  can, starts only with `--ignore-host-disconnection`: a `FAIL` on the node
  check's `nvmet host cut (debugfs)` line in step 1 means that it would
  refuse to start there.
- A node that 0.1 created with no location keeps its address as its
  location ([Choose a node's location](#choose-a-nodes-location)), and a
  controller node that 0.1 created with no budget keeps the 4 TiB 0.1 gave
  it ([The cluster's capacity](#the-clusters-capacity)); only emptying,
  deleting and creating such a node again changes them.

A disk node's disk keeps the format it was made with across every upgrade,
and each new build reads it as it is, unless the new release's notes say
that the disks must be made again.

Steps that can lose data:

- A worker left running while the agents stop: it reads each stopped agent
  as a failed node, fails primaries over, replaces cntlrs and repairs legs.
  In a planted run of the upgrade case that stopped the worker after the
  agents, the 0.1 worker failed three primaries over.
- An older build started after a newer one: it ignores what the newer one
  added and can undo it. A cn agent of 0.1, for one, can make a new array
  over legs whose md superblocks a forced start wiped, though they hold
  data.
- Making a disk again, when the notes ask for it: every side the disk holds
  is lost.
- A host still connected but off a subsystem's list, when the new cn agent
  ends its connection: its writes fail.
- Every cdc stopped from step 3 to step 9, on a host that uses nvme-stas:
  an outage of more than a few seconds is not tested, and nvme-stas may
  drop the host's paths, so that the volumes' IO fails and writes not yet
  flushed are lost.
- A node whose agents start before the new udev rules are on it: the
  system's own rules can assemble and probe what the agents build.

When to stop: a program prints or logs another version than the new one;
a line of the node check of step 6 reads `FAIL`, or a line of the one of
step 1 does, other than those it names for a node that ran 0.1; an agent does not log `agent started`; or at step 12 a
line that step 1 did not show, any line after an upgrade from 0.1, or a
`reaction applied` record, shows: stop, start no more programs, and ask for
help. Never go back to the old build by starting its files again.

Tested: the e2e suite's upgrade case (`integtest/e2e_test.sh`, run with
`--only upgrade`) upgrades a cluster of 0.1 to the build under test in the
order of steps 2 to 11, under a host's writes, after a backup taken with
`etcdctl snapshot save`: it stops the worker, the gateway and the cdc,
then the agents as step 4 says, replaces every program
file, installs the udev rules, runs the node check of step 6 on each
controller node, starts the disk nodes' agents and then the controller nodes', checks every
`--version` and every agent's `agent node versions` record, starts the
gateway, the cdc and the worker last, and checks their start records. It
then checks that no reaction ran, that each pool kept its cntlrs, its
primary and its sides, that the host's writer had no failed write and reads
its whole range back, and that neither host's kernel log shows a failed IO.
It passed in the lab on 2026-11-21, in 17 minutes. It runs one slice, two
cntlrs per pool and no migration, repair, clone or forced start open; it
runs the worker at the suite's shorter vote times, not the shipped ones,
and its programs from a work folder, not from the kit's units. Of step 1,
it takes the backup; of step 12, it checks that no `reaction applied`
record shows. The node check of step 1, the node check of step 6 on the
disk nodes, the status read and the hosts' comparison of step 1, the status read
and the new backup of step 12, the units of step 5, and the CSI driver's
part, steps 3 and 10, are not tested.

## While the control plane is down

Words: the control plane is etcd, the gateways, the workers and the cdcs,
as the [core glossary](core_glossary.md) says. A worker **stops itself**
when it stops driving its pools because it cannot prove that it still
holds them; its log then shows the record `worker fenced`. This is not the
fence of a failover, which a disk node makes.

When: etcd, every gateway, every worker or every cdc is down, by a fault or
for an upgrade.

What dnv does alone: the data plane serves on. Each agent keeps what it
built and goes on serving; hosts keep their paths and their IO. What stops,
and how long the workers wait, by what is down, at the shipped settings:

- etcd down, or without its quorum: no `dnvctl` call works, reads included:
  a gateway waits for etcd up to 10 seconds and then fails the call with the
  error code ABORTED, and `dnvctl`'s own `--timeout`, 10 seconds by
  default, can end it first. A write that failed so may have been made:
  read the state before you send it again. The calls of the CSI driver's
  controller plugin, the part of the driver that calls the gateway, fail
  the same way, and Kubernetes tries them again. Its node plugin, the part
  on each Kubernetes node, calls no gateway. A pod whose volume is already
  attached to its node, with the node on the list of the volume's
  subsystem, still mounts it while a cdc serves what it last read.
  Each worker stops itself about 30 seconds after its last write to etcd
  that went through: two vote intervals, the worker's `--vote-interval`, 10
  seconds at the default, plus one more; later when its writes hang, each
  up to 10 seconds. Its log shows `worker fenced` with the reason
  `heartbeat_stalled`. Once etcd is back, each worker joins again with no
  step from you, and drives its pools again one join grace later, the
  worker's `--vote-grace-time`, 60 seconds at the default; it can stop
  itself once more first. A cdc that runs keeps serving what it last
  read; a cdc started during the outage answers no host until it has read
  etcd once.
- Every gateway down: no `dnvctl` call works, and no call of the CSI
  driver's controller plugin: no volume is created, deleted, attached or
  detached. The node plugin calls no gateway, so a pod whose volume is
  already attached to its node still mounts it. The workers go on.
- Every worker down: nothing reacts. No failover, no cntlr replacement, no
  leg repair, no auto-grow, no end of a clone whose copy is whole, and no
  change reaches the nodes. A worker that starts again drives nothing for
  its join grace, 60 seconds, even when it is the only one. When one of
  several workers is down, the others take its pools about 45 seconds after
  its last write: twice the vote interval, plus the worker's
  `--vote-leave-grace-time`, 25 seconds at the default.
- Every cdc down: hosts keep their paths but learn of no change: a new
  host finds no path, and after a failover a host stays on the path that
  the failover withdrew. On a host that uses nvme-stas, this is tested
  only for an outage of a few seconds. How long nvme-stas keeps its paths
  after that is not known: it may drop them once the kernel gives up the
  host's connection to a stopped cdc, after that connection's
  `ctrl_loss_tmo`, 600 seconds at the kernel's default. A node of dnv's
  Kubernetes driver keeps its paths while a cdc of its list cannot be
  reached.

Safe steps: bring the program back; nothing else is needed, except on a
host where nvme-stas dropped every path of a volume. There the volume's
device went, and once a cdc that the host lists and that serves the
volume's pool answers, nvme-stas connects the paths again, under a new
device. That can take a minute or more: stafd, the part of nvme-stas that
holds the connections to the cdcs, tries about once a minute to connect
again a cdc whose connection the kernel ended, as the source of nvme-stas
2.4.1 reads. A file system mounted on the old device must be unmounted and
mounted again on the new device, which replays its journal. When the mount
fails, or the kernel log then reports an error, unmount it if it is
mounted, check it with its own check tool, and mount it again once the
check finds nothing more to repair. Do not let the check tool throw the
journal away, as `xfs_repair -L` does: that loses the changes the journal
holds. Send no write through `dnvctl` until the state reads again, and
read the state of each call that failed before you send it again.

Steps that can lose data: no run has shown a loss from the outage itself.
But an outage of every cdc longer than a few seconds is not tested:
nvme-stas may then drop a host's paths, as Every cdc down says, and then
the volumes' IO fails and writes not yet flushed are lost. While no worker
drives, a primary that dies is not failed over, so its volumes have no IO
until a worker drives again, and a thin pool that fills gets no auto-grow,
so a write that needs a new block waits and then fails.

When to stop: etcd does not come back: see [Restore etcd from a
backup](#restore-etcd-from-a-backup). A worker that logs `worker fenced`
again and again while etcd answers: see [etcd refuses writes: its store is
full](#etcd-refuses-writes-its-store-is-full). A host that dropped its
paths during the outage and still finds no path two minutes after a cdc
that it lists and that serves the volume's pool answers again: read [Read
what is wrong](#read-what-is-wrong). When that read lists nothing wrong in
the volume's pool, restart nvme-stas on that host ([Restart
nvme-stas](#restart-nvme-stas)). A file system whose check still finds
damage, whose check tool asks to throw its journal away, or that still
does not mount: ask for help.

Tested: the e2e suite's cutoff case, stage 07, stops etcd under a host's
write loop: a read through `dnvctl` fails with ABORTED, the worker stops
itself and logs `worker fenced` with the reason `heartbeat_stalled`, the
host's writes go on, the cdc serves what it read, and after etcd is back
the worker drives again with the pools unchanged. It ran at the suite's
shorter vote times, a vote interval of 2 seconds, a join grace of 6 and a
leave grace of 13, and passed in the lab on 2026-11-21. The upgrade case
keeps every agent stopped for 30 seconds, and the worker for about two
minutes, under a host's writes with no failed write (2026-11-21).
The cdc suite, `integtest/cdc_test.sh`, kills every cdc for a few seconds
under hosts that keep their connections, and starts them again; a longer
outage is not tested. The worker suite's three-member mode stops one etcd
server at a time. The times at the shipped settings are read from the code,
not measured. No suite runs the CSI driver while etcd or the gateways are
down: that the node plugin still mounts an attached volume then is read
from the code. That a node of the Kubernetes driver keeps its paths while
a cdc of its list cannot be reached is shown by the driver's Go tests,
with a fake host, not by a suite. No suite runs a cdc outage long enough
for nvme-stas to drop a path, so its connect afterwards is not tested.

## Give hosts the cdc addresses

Words: a host's **discovery** is how it learns where its subsystems are:
it holds a connection to each cdc it lists, and the cdc answers with the
addresses of the controller nodes that serve each subsystem the host is on
the list of. A **range** is the part of the pools one cdc serves, set by
its `--range` flag, all of them by default; two cdcs of one range are
**twins**, which serve the same pools. To **front** the gateways is to put
them behind one address that clients call.

When: a host is set up; a cdc moves, is added or is removed, or the pools
are split among cdcs in another way; or several gateways should serve one
client.

What dnv does alone: each cdc follows etcd by itself and tells each host
connected to it when its answer changes. A cdc never checks that every
range has one, and nothing says so when a range has none. A host whose
listed cdcs all answer and leave out a range drops its paths to that
range's pools. When every cdc of a range is stopped, hosts keep their
paths to its pools but learn of no change to them, as [While the control
plane is down](#while-the-control-plane-is-down) says of every cdc down.
On a host that uses nvme-stas, this is tested only for an outage of a few
seconds, and a longer one may drop those paths, as that section says.
The gateways keep no state: any one of them answers any call.

Safe steps for the cdcs:

1. Run one cdc on each control-plane machine, each with `--tr-addr <its own
   address>`, an IP address, and `--tr-svc-id`, 8009 by default, and leave
   out `--range`, so that each serves every pool and they are twins. Where
   `--range` splits the pools among cdcs, give each range two cdcs on
   different machines.
2. On each host, copy `deploy/modules-load.d/dnv-host.conf` into
   `/etc/modules-load.d/`, so that nvme-tcp, which a host needs to reach its
   volumes, loads at every boot, and load it now:
   `sudo systemctl restart systemd-modules-load.service`. On each host that
   uses nvme-stas, list every cdc in `/etc/stas/stafd.conf`, section
   `[Controllers]`, one line for each:
   `controller = transport=tcp;traddr=<cdc address>;trsvcid=<port>`, with
   `persistent-connections=true` and `zeroconf=disabled` as in [dnv on one
   machine](tutorial_all_in_one.md). The host's NQN, which the subsystems'
   lists name, is the `nqn` of the `[Host]` section of `/etc/stas/sys.conf`,
   as the tutorial sets it, or, where that names none, the content of
   `/etc/nvme/hostnqn`.
3. Restart nvme-stas in its safe order ([Restart
   nvme-stas](#restart-nvme-stas)) so that it reads the list.
4. Check on the host: `stafctl adlp -d` shows each cdc's address with its
   `connected` field `True`, as step 4 of [Restart
   nvme-stas](#restart-nvme-stas) reads it.
5. When a cdc moves, is removed or is to serve a narrower `--range`, keep
   it serving what it serves now until every host's list names another
   running cdc of each range it gives up. First start the cdc at its new
   address, or another cdc of each range it gives up, or check that a twin
   serves that range, as step 1 says. Then give every host its new list,
   which names such a cdc: on a host that uses nvme-stas, write the list as
   the nvme-stas part of step 2 says and do steps 3 and 4; for the
   Kubernetes driver, do steps 1 to 4 of [Change the cdc addresses of the
   Kubernetes
   driver](#change-the-cdc-addresses-of-the-kubernetes-driver). Only then
   stop the old cdc, or restart it with its narrower `--range`. A host
   whose list names no other cdc of the range learns of no change once the
   old cdc stops: after a failover it stays on the path that the failover
   withdrew, and it finds no path to a new subsystem. Once a cdc restarted
   with a narrower `--range` answers, such a host drops its paths to the
   pools of each range the cdc gave up (Steps that can lose data).
6. For the Kubernetes driver, the list is its config map, and [Change the
   cdc addresses of the Kubernetes
   driver](#change-the-cdc-addresses-of-the-kubernetes-driver) gives its
   whole order; after the old cdc stops, its step 6 drops each node's
   connection to the old address.

Safe steps for the gateways:

1. Run several gateways on the same etcd, each with its own
   `--grpc-address`; they need nothing else to share the work.
2. `dnvctl` and the CSI driver's controller plugin each call one address:
   `dnvctl --gateway-address`, or `DNVCTL_GATEWAY_ADDRESS`, and the
   `gatewayAddress` of the driver's config map, `deploy/config.yaml`. To
   give them several gateways, put the gateways behind one address, such as
   a TCP load balancer that passes each connection to a gateway that
   answers. A call that fails while its gateway goes away can be sent again
   to another; read the state first, as a write that failed may have been
   made.
3. The gateway checks no caller: keep its port reachable only from the
   machines of the operators and of the controller plugin ([The
   networks](#the-networks)).

Steps that can lose data: two changes make stacd, the part of nvme-stas
that connects the host to the cntlrs, drop a host's paths to the volumes
of a range. One is a cdc restarted with a narrower `--range` while no
other cdc the host lists serves a range it gives up: once that cdc answers
again, none of the cdcs the host lists names those paths. The other is a
host's new list that leaves out every cdc of a range whose volumes the host
uses: once stacd starts on that host, none of the cdcs it lists names those
paths. stacd then drops them: the volumes' IO fails, and a write not yet
flushed is lost. [Restart nvme-stas](#restart-nvme-stas) stops at its step
4, before stacd starts, when the records of a new list are missing. A host
that lists no cdc of a range gets no path to that range's volumes. On a
Kubernetes node the same two changes drop the volumes' paths, as [Change
the cdc addresses of the Kubernetes
driver](#change-the-cdc-addresses-of-the-kubernetes-driver) says.

When to stop: a host that lists every cdc still finds no path to a volume
whose subsystem lists it: read [Read what is wrong](#read-what-is-wrong).

Tested: the cdc suite, `integtest/cdc_test.sh`, runs four cdcs, two twins
of each half of the ranges, with hosts that list several cdcs through
nvme-stas: one cdc killed, its twin serving alone, its restart, and every
cdc killed for a few seconds under hosts that keep their connections. The
gateway suite, `integtest/gateway_test.sh`, runs three gateways on one etcd
and sends calls to each in turn. The move, the removal or the narrower
range of a cdc of step 5, with its change of each host's list, a longer
outage of every cdc of a range, and a load balancer in front of the
gateways are not tested, and no suite loads the host's module
file of step 2.

## The logs

Words: each dnv program writes its **log** to stderr as JSON, one record
per line; every record names its time, its log level, its message and the
trace id of the call it belongs to. The **log level** is the least serious
kind of record a program writes: `debug`, `info` or `warn`; `error`
records are always written. At `info`, the default of every program but
`dnvctl`, a program writes each change, each failure and each event, and
leaves out the reads it repeats: a check round that finds nothing to do,
and the reads of the node behind it, write nothing. Two repeated reads are
written at `info` all the same. One is the capacity read the CSI driver's
controller plugin makes for Kubernetes, once a minute per storage class.
Both the plugin and the gateway write its request and its reply, and the
reply names every disk node and controller node of the cluster, about 100
bytes each, so at 1,000 nodes each poll writes about 100 KB to each of the
two logs, about 140 MB a day per storage class. The other is the
controller plugin's wait in a detach, the step that takes a Kubernetes
node off a volume's list of hosts, and in an attach that puts a node on an
empty list: once a second while the wait lasts, it reads the volume's pool
and the report of the pool's primary through the gateway. The plugin and
the gateway write each request and its reply, the gateway's read of the
report from the primary's cn agent among them, and that cn agent writes
each read of its report, with the whole report, at `info` too. A wait
lasts a second or two in the common case, and longer while the primary
does not answer or its connection to the old host does not end. At `debug`
a program also writes each read, each message of a check round and each
leg probe. The records at `warn` and `error` are the ones an operator must
look at, with one exception: `log level changed`, which a signal writes at
`warn` so that every log level shows it.

When: a log grows fast or a disk fills; a fault needs more detail than the
default log level writes; or you are about to reproduce a fault for a bug
report.

Confirm:

1. Look at the size of each log file, and at what fills it:
   `jq -R -r 'fromjson? | .level' <log file> | sort | uniq -c` counts the
   records of each log level, and
   `jq -R -r 'fromjson? | .msg' <log file> | sort | uniq -c | sort -rn | head`
   the records of each message. `-R` with `fromjson?` skips a line that is
   not JSON, such as the `nohup: ignoring input` line `nohup` puts first
   when started from a terminal, or the trace of a program that crashed;
   without it, `jq` stops at that line.
2. The log level a running program writes at is the `log_level` of its
   last `log level changed` record; with none, it is the log level it
   started with: its `--log-level` or the environment variable of safe
   step 1, or `info` when neither was given.

What dnv does alone: nothing. No program rotates, cuts or compresses its
log, and a log level a signal set stays until the next signal or until the
program ends.

Safe steps:

1. Start every program at `--log-level info`, the default, or set it with
   the environment variable of the program: `DNV_AGENT_LOG_LEVEL`,
   `DNV_WORKER_LOG_LEVEL`, `DNV_GATEWAY_LOG_LEVEL`, `DNV_CDC_LOG_LEVEL` or
   `DNV_CSI_LOG_LEVEL`. `dnvctl` writes `warn` by default; with
   `--log-level info` it also prints each call it makes to stderr.
2. Keep the logs on a filesystem apart from the agents' local stores and
   from etcd's data. Rotate them yourself: logrotate with `copytruncate`,
   since a program keeps its log file open, or journald with a size cap.
3. For more detail, send SIGUSR1 to the program's main process:
   `kill -USR1 <pid>`. It writes a `log level changed` record at `warn`,
   and from then on writes `debug` records. `kill -USR2 <pid>` takes it
   back to the log level it started with, with one more such record. Under
   systemd, signal the main process alone:
   `kill -USR1 $(systemctl show -p MainPID --value <unit>)`. In Kubernetes,
   the CSI driver runs in pods, the Kubernetes units of one or more
   containers: its controller plugin in two copies, two pods, and its node
   plugin, the part that runs on each Kubernetes node, in one pod per node.
   Each pod is signalled on its own
   (`kubectl -n dnv-csi get pods -o wide` lists them and their nodes).
   Send the signal on the pod's node, as root, to the pid the container
   runtime reports for the pod's `dnv-csi` container; on k3s:
   `cid=$(k3s crictl ps -q --label io.kubernetes.pod.name=<pod> --label io.kubernetes.container.name=dnv-csi)`,
   then `kill -USR1 $(k3s crictl inspect --output go-template --template '{{.info.pid}}' $cid)`.
   A `kubectl exec <pod> -c dnv-csi -- kill -USR1 1` does not work for a
   controller copy: the container runtime's AppArmor profile refuses the
   signal under the copy's least rights. For a node pod, which runs
   privileged, that way has not been checked.
   `dnvctl`, `dnv-agent check` and `dnv-agent collect` take no signal; give
   `dnvctl` its `--log-level` instead.
4. Before you reproduce a fault for a bug report, send SIGUSR1 to the
   agent, reproduce the fault, then
   [gather the node's state](#gather-a-nodes-state-for-a-bug-report); the
   archive then holds the agent's reads of the node. Send SIGUSR2 after.
5. Alert on the records at `warn` and `error` but `log level changed`, for
   example with
   `jq -R -c 'fromjson? | select((.level == "WARN" or .level == "ERROR") and .msg != "log level changed")'`.
   A failure a program clears alone, such as a path connect a controller
   node refuses before it has built the volume, is an `info` record, so it
   sets off no alert; a cdc a node plugin cannot reach or read is a `warn`
   record, as it holds until the cdc comes back or the list is fixed.

Steps that can stop the data path:

- A log on the filesystem of an agent's local store: once the log fills
  that filesystem, the agent cannot save a syncup and refuses it, a
  failover's among them, so a failover that needs that node waits until
  space is free. The worker logs each refusal as `syncup rejected` at
  `error`.
- A signal to the program's whole process group, or to every process of
  its systemd unit, which is what `systemctl kill` does by default, also
  reaches the commands an agent is running at that moment, such as
  `blkdiscard` or `nvme disconnect`, and ends them, since they do not
  catch it. The agent then sees a failed command, and its work waits for
  the next try.
- At `debug`, each agent writes several records per object on every check
  round, so a long time at `debug` fills a disk fast. Go back with SIGUSR2.

When to stop: the program writes no `log level changed` record after a
signal. Check that the signal went to the main process of the right
program, not to a shell or a wrapper, before you send another.

Tested: unit tests check the log level of the records every program shares
(the reads and the writes of the node, the messages of a check round, the
records of etcd and of the leg probe), of the worker's skipped reactions,
its stops (`worker fenced`) and its late rounds, of the CSI driver's
calls and of its records for a cdc it cannot reach; that every program
but the agent's node check and gather parses `--log-level` and refuses a
word other than the three;
and that SIGUSR1 and SIGUSR2 change the log level of a running program. On the lab VMs, the dn suite's loglevel
case restarts an agent at the default log level, checks that a check round
writes no record of its reads or of its messages and that a request still
writes its call and its save, and sends SIGUSR1 and SIGUSR2; the e2e suite's
smoke case checks, by the log level of each record, that an idle primary cn
agent and the worker write no record of their check rounds at `info`; and
the Kubernetes suite sends SIGUSR1 and SIGUSR2 to one CSI controller copy's
plugin from that copy's node, as above.

### Read the logs

Words: the **trace id** is as the [core glossary](core_glossary.md) says;
each record carries it as `trace_id`.

When to use: to follow one call through the programs it passed, or to find
why something did or did not happen.

What dnv does alone: nothing; reading changes nothing.

Safe steps:

1. Follow one call. A `dnvctl` call that fails prints `(trace_id <id>)` at
   the end of its message, and `dnvctl --trace-id <id>` gives a call an id
   of your choice. The gateway's records of that call, and those of each
   agent it called, carry the same id:
   `jq -R -c 'fromjson? | select(.trace_id == "<id>")' <log file>`. The
   work the worker does after a call runs under ids of its own.
2. Look for the records that say what dnv did or did not do. The worker
   writes `reaction applied` and `reaction skipped`, each with the reaction
   and its reason; `worker fenced`, when it stops driving its pools, and
   `membership committed`, when the workers share the pools anew;
   `syncup rejected`, at `error` when an agent refused a syncup for its
   revision or because it could not save it, and at `info` for any other
   refusal; `syncup leftover`, for what a node still holds that it should
   not; `invalid stored conf`, at `error`, for a pool it refuses to drive
   and why; and `clone latched` and `clone drain failed` for the end of a
   clone. An agent writes `agent started` once it has rebuilt its node,
   and `sweep leftover` for what its removal left. The worker writes an
   `agent started` record of its own when it sees a node's agent start, so
   look in the log of the program you mean.
3. Know what five kinds of record do not say:
   - A call that reads the state first and writes after, such as a pool
     create, a volume delete or `dnvctl ns set-dev`, reads again when the
     state moved in between, and after a fixed count of rounds gives up
     with the error code ABORTED and the message `candidate allocation did
     not settle in <N> rounds`. It names no node and no change: other
     calls kept changing what it read. Send it again.
   - An `etcd put` or `etcd delete` record inside a transaction is written
     when the write is prepared, before etcd takes or refuses the whole: it
     does not say that the write was made. Read the state.
   - The worker's `syncup leftover` record names eight objects at most,
     then `[+<N> more]`; the agent's own `sweep leftover` record names them
     all.
   - The cdc's `host disconnected` record gives the reason `closed` both
     for a host that went away and for a write to a host that timed out.
   - A listing of a controller node's devices that `dmsetup` ended with an
     error, as when a device goes away during it, still gives the devices
     it read, and the cn agent's read can judge the node from that part.
     The next read lists the node again.

Steps that can lose data: none.

When to stop: a record at `error` that none of the sections of this
document names: keep the log, gather the node's state ([Gather a node's
state for a bug report](#gather-a-nodes-state-for-a-bug-report)) and ask
for help.

Tested: not tested as a procedure. The suites read several of these
records to judge their runs: the e2e suite counts `reaction applied` and
reads `worker fenced` with its reason and each agent's `agent started`, and
the dn agent suite's loglevel case checks which records a check round and a
request write. The five kinds of record of step 3 are read from the code.

## Gather a node's state for a bug report

Words: the **gather** is `dnv-agent collect`, a run of the agent program
that writes one archive of the node's state for a bug report. Some reads
can wait in the kernel without end on a node with a stalled device, such
as a read of /proc/mdstat or of a dm device's status; the gather runs each
of those apart, under one time limit, and names in the archive each one
that did not finish.

When: a node or a pool on it behaves wrongly and you want to report it,
or to keep the node's state before you change anything. An agent at the
default log level leaves its reads of the node out of its log; to have
them in the archive, send it SIGUSR1 before you reproduce the fault
([the logs](#the-logs)).

Safe steps:

1. On the node, as root:
   `dnv-agent collect --log-file <the agent's log file>`, or
   `--journal-unit <unit>` when systemd keeps the agent's log. Add
   `--local-store` with the agent's own value when the agent does not use
   the default, /var/lib/dnv; otherwise the archive lists the default
   folder, not the agent's local store. `--deadline` sets the time limit of
   the reads that can wait (20 seconds when not given), and `--out` the
   archive's path; by default it is `dnv-collect-<host>-<time>.tar.gz` in
   the current directory; an `--out` that names a directory, or a link to
   one, is refused before anything is read. The archive takes the end of the
   agent's log: the last 16 MiB of the file, or the last 100000 records of
   the unit, newest first. A log read that runs out of its time limit, which
   is as long as `--deadline`, keeps what it read and ends with the line
   `[cut at the bound ...]`; for the unit that is the newest records. Keep
   an older part by hand if the trouble began earlier.
2. It prints the archive's path, then each read that did not finish. A
   read that did not finish is a fact for the report: it shows what the
   kernel waits on. The reads stay stopped in the kernel until what they
   wait on ends; the gather does not wait for them.
3. Read the archive before you share it: it names the cluster's hosts,
   NQNs and addresses, and it holds the kernel log, which on most
   distributions only root may read. The gather writes it so that only its
   owner can read it (mode 0600). It holds no keys and no contents of the
   agent's local store, only the store's file names, sizes and times.

What dnv does alone: nothing. The gather reads; it writes only the
archive. It reads no device's data and never flushes a thin pool's or a
dm-clone's metadata.

When to stop: the gather exits 2, which means it could not run or could
not write the archive; it says why on stderr.

Tested: unit tests check that a dm status read that never returns ends the
gather by its time limit and is named in the archive, that every status
read is the one that flushes nothing, and that the gather never reads
/proc/mdstat, nvmet's configfs tree or a dm status in its own process. The
cn suite's nodecheck case stalls an md array on a lab VM and checks that
the gather returns by its time limit and names the /proc/mdstat read as
not finished, and it passes there.

## A Kubernetes volume the node plugin will not format

Words: the **node plugin** is the part of dnv's Kubernetes driver that runs
on each Kubernetes node. A **pod** is the Kubernetes unit of one or more
containers. A **filesystem volume** is one the plugin formats and mounts
for a pod; a **raw block volume** is one a pod gets as a bare block device.
A **claim** is a pod's request for a volume, and a **persistent volume** is
Kubernetes' record of the volume that serves a claim; its handle names the
dnv volume. A **workload** is the Kubernetes record that keeps pods
running, such as a Deployment or a StatefulSet, and makes a new pod at once
when one of its pods is deleted. A **Kubernetes namespace** is a named
group of Kubernetes records, such as claims and pods; it is not an NVMe
namespace. A persistent volume's **reclaim policy** says what deleting it
does to the volume under it: `Retain` keeps the volume. Before a pod can
use a volume, the kubelet, the Kubernetes program on each node, asks the
node plugin to make the volume ready there: the plugin connects the node to
the volume and, for a filesystem volume, formats it when it is new and
mounts it. The plugin formats a volume only when blkid finds nothing on it
and its first MiB, read straight from the device, is all zeros. A new
volume reads as zeros, since a thin device reads zeros where nothing was
written.

Symptom: a pod that uses a filesystem volume stays in `ContainerCreating`.
`kubectl -n <Kubernetes namespace> describe pod <pod>` shows events with
the plugin's refusal. This section covers the two refusals of the check
that the volume is blank:

1. **The read failed**: "reading the start of the device <device> ... the
   driver formats only a device whose start it has read".
2. **Data at the start**: "the device <device> of the volume holds data at
   byte N of its first M bytes and no filesystem or partition table blkid
   knows; the driver never formats over it".

The text tells the two apart. `<device>` is
`/dev/disk/by-id/nvme-uuid.<uuid>` on the node that runs the pod. A
refusal that names a partition table ("carries a ... partition table and
no filesystem"), or a filesystem other than the one the claim asks for
("carries ..., not ...; the driver never reformats"), means that the
volume holds someone's content, which the plugin never formats over; it
disconnects the node from the volume after each such try, as after data at
the start. Reach the volume and keep a copy as under "Data at the start",
steps 1 to 6, and ask for help.

What dnv does alone: the kubelet asks again and again, waiting longer each
time, up to about two minutes between tries. After a failed read the node
keeps its paths to the volume, and each try reads again, so the volume is
formatted and mounted once a read works. After data at the start the
plugin disconnects the node from the volume, since Kubernetes never asks it
to undo a refused try, and every try connects, reads, refuses and
disconnects again: nothing changes by itself.

### The read failed

Confirm, as root on the node: `nvme list-subsys` shows the volume's paths
and their state, and the kernel log (`dmesg`) shows any IO errors of the
device.

Safe steps: check that the volume's pool serves. `dnvctl sp get` lists the
pool's cntlrs; `dnvctl cntlr inspect --id <primary cntlr id>` shows the
primary's rows. The node reconnects its paths by itself; once a path
serves, the next try formats the volume.

When to stop: the read keeps failing while the pool's cntlrs serve, which
is a fault only the host sees ([A fault only a host
sees](#a-fault-only-a-host-sees)). Ask for help.

Tested: a unit test of the node plugin checks that a failed read formats
nothing and keeps the volume's paths, and that the next try formats the
volume once the read works. No suite case fails a read under a Kubernetes
volume.

### Data at the start

Between tries the node holds no path to the volume, so `<device>` does not
exist there. Do not run blkid, hexdump or dd on it on the node: blkid
answers a missing path as it answers an empty device, and `dd of=` a
missing path makes a plain file and leaves the volume as it was. Reach the
volume through a pod that gets it as a raw block volume, as below. Two
causes are known:

1. **A first format cut off part way.** The plugin started `mkfs` on the
   new volume and the try ended before `mkfs` did: the kubelet's time for
   one try ran out, or the plugin restarted. The bytes `mkfs` had written
   are no filesystem blkid takes. Such a volume never held a pod's data.
   The plugin logs one `os command` record when a command ends, at the
   `info` log level for a command that writes, such as `mkfs`, or that
   fails, and at `debug` only for a read that succeeds
   ([the logs](#the-logs)). For this cause its log shows the device's
   `mkfs` ending with an error, or the
   plugin restarted during the `mkfs`: `kubectl -n dnv-csi get pods` counts
   a restart of the node's plugin pod.
2. **The volume held data already.** Someone wrote to the volume before:
   through a raw block volume, or by hand, and then gave it to Kubernetes
   as a filesystem volume. That data is someone's.

Safe steps:

1. Stop the tries: scale the workload that owns the pod to zero; delete
   the pod only when no workload owns it, since a workload makes a new pod
   with the same claim at once. Wait until `kubectl get volumeattachment`
   lists no attachment of the claim's persistent volume
   (`kubectl -n <Kubernetes namespace> get pvc <claim>` names it). A try
   still running would disconnect the node from the volume under the steps
   below.
2. Read the volume's handle:
   `kubectl get pv <pv> -o jsonpath='{.spec.csi.volumeHandle}'`.
3. Give the same volume to a pod as a raw block volume, through a second
   persistent volume over that handle. `Retain` is what keeps the second
   one's delete from deleting the dnv volume:

   ```
   kubectl apply -f - <<'EOF'
   apiVersion: v1
   kind: PersistentVolume
   metadata:
     name: inspect-<pv>
   spec:
     capacity:
       storage: 1Gi
     accessModes: [ReadWriteOnce]
     volumeMode: Block
     persistentVolumeReclaimPolicy: Retain
     storageClassName: ""
     csi:
       driver: csi.dnv.io
       volumeHandle: <the handle from step 2>
   ---
   apiVersion: v1
   kind: PersistentVolumeClaim
   metadata:
     name: inspect-<pv>
     namespace: <the claim's Kubernetes namespace>
   spec:
     accessModes: [ReadWriteOnce]
     volumeMode: Block
     storageClassName: ""
     volumeName: inspect-<pv>
     resources:
       requests:
         storage: 1Gi
   ---
   apiVersion: v1
   kind: Pod
   metadata:
     name: inspect-<pv>
     namespace: <the claim's Kubernetes namespace>
   spec:
     containers:
       - name: inspect
         image: busybox:1.37.0
         command: ["sleep", "86400"]
         volumeDevices:
           - name: vol
             devicePath: /dev/vol
     volumes:
       - name: vol
         persistentVolumeClaim:
           claimName: inspect-<pv>
   EOF
   ```

   The plugin connects the node and gives the pod the device; it formats
   nothing for a raw block volume.
4. Look at the start:
   `kubectl exec -n <Kubernetes namespace> inspect-<pv> -- hexdump -C -n 1048576 /dev/vol | head -n 20`.
5. Keep a copy before you change anything:
   `kubectl exec -n <Kubernetes namespace> inspect-<pv> -- head -c 1048576 /dev/vol > <volume>.start`
   copies the first MiB to the machine you run kubectl on. To keep the
   whole volume, use `cat /dev/vol` in place of `head -c 1048576 /dev/vol`.
6. Delete the pod, then the claim, then the persistent volume of step 3.
   Each delete leaves the dnv volume as it is.
7. Start the workload again. If the start still holds data, the next try
   refuses it again.

Steps that can lose data: zeroing the start, between steps 5 and 6:
`kubectl exec -n <Kubernetes namespace> inspect-<pv> -- dd if=/dev/zero of=/dev/vol bs=1M count=1 conv=fsync`.
It erases what the first MiB holds; the try after step 7 then formats the
volume, which erases the rest of any filesystem on it. Do it only for a
volume that never held data, as after a first format cut off part way.
`wipefs -a` is no help here: it erases only what blkid knows, and blkid
knows nothing on this device. A `mkfs` by hand loses the data the same way
as the zeroing. Deleting the second persistent volume with any reclaim
policy but `Retain` deletes the dnv volume and all its data.

When to stop: you cannot tell who wrote the data. Keep the workload
stopped, keep the copy, and ask for help; do not zero the volume.

Tested: unit tests of the node plugin check that a failed read formats
nothing and keeps the volume's paths, that a byte that is not zero in the
first MiB formats nothing, that an all-zero start is formatted, and that a
volume smaller than a MiB is read whole. No suite case fails a read under
a Kubernetes volume or puts data at the start of one, and the steps of
"Data at the start", the second persistent volume over the same handle
above all, have not been run. The Kubernetes suite's volume case shows that
a new volume is still formatted and that a raw block volume reaches its pod.

## Change the cdc addresses of the Kubernetes driver

Words: the **node plugin**, a **pod** and a **Kubernetes namespace** are as
in [A Kubernetes volume the node plugin will not format](#a-kubernetes-volume-the-node-plugin-will-not-format).
The **controller plugin** is the part of dnv's Kubernetes driver that calls
the gateway; it runs as two copies, two pods, and each of its two helper
programs, which watch Kubernetes for it, acts in one copy at a time ([A
Kubernetes machine is lost](#a-kubernetes-machine-is-lost)). To **attach** a volume to a Kubernetes
node, the controller plugin puts the node's host NQN in the allowed hosts of
the volume's subsystem; to **detach** it, it empties that list, and the cdcs
then list no path of the volume to any host. The **config map**
`dnv-csi-config`, in the Kubernetes namespace `dnv-csi`, holds the cdc
addresses both plugins start with: its key `cdcAddresses` is a list of
`ip:port` joined by commas. A **range** is the part of the pools one cdc
serves, set by its `--range` flag; two cdcs of one range are **twins**,
which serve the same pools.

When: a cdc moves to another address, a cdc is added or removed, or the
pools are split among cdcs in another way.

What dnv does alone: each node plugin follows the cdc addresses it read
from the config map when it started, and no other. A change of the config
map reaches a plugin only when the plugin starts again. A listed cdc that
cannot be reached, or whose answer cannot be read, keeps the node plugin
from dropping any path on its node, so a node keeps its paths while a cdc
is away; the plugin logs `csi follower sync error` naming that cdc on each
try. But when every listed cdc answers and none lists a path of a volume,
as when the only listed cdc of the volume's pool no longer serves that
pool, the node plugin takes it as a detach and drops the volume's paths. A node plugin that restarts
leaves the paths and the mounts of its node in place.

Safe steps, in this order:

1. Start the new cdcs, each with its range, while the old cdcs still serve
   every pool they served. Do not yet stop an old cdc or restart it with a
   narrower range.
2. Write the new list: the address of every cdc that still serves after
   step 5, the new cdcs and the old ones that stay, so that a cdc of the
   list serves each pool. Leave out each address that step 5 stops: a
   listed cdc that cannot be reached keeps the node plugin from dropping
   any path, a failed-over primary's path included. Edit the config map,
   `kubectl -n dnv-csi edit configmap dnv-csi-config`, or apply the edited
   `deploy/config.yaml`.
3. Start both plugins again:
   `kubectl -n dnv-csi rollout restart deployment/dnv-csi-controller daemonset/dnv-csi-node`,
   and wait for `kubectl -n dnv-csi rollout status daemonset/dnv-csi-node`.
4. Check each node plugin: `kubectl -n dnv-csi logs <node plugin pod> -c dnv-csi`
   shows `csi node starting` with the new list, and on a node that holds a
   dnv volume, `csi discovery connection opened` for each new address, and
   no `csi follower sync error` after it.
5. Only now stop an old cdc, or restart it with its narrower range.
6. On each node, drop the connection to an address the new list leaves out,
   since the node plugin leaves a connection to an address outside its list
   alone. As root on the node, `grep -H . /sys/class/nvme/nvme*/address`
   shows the address of each NVMe connection; for the one at the old cdc
   address whose `subsysnqn` file in the same folder reads
   `nqn.2014-08.org.nvmexpress.discovery`, run `nvme disconnect -d nvmeN`
   with its folder's name. It carries no volume's IO.

Steps that can lose a mount: a cdc that a node's list names, restarted with
a narrower range while no other cdc of that list serves the pools it gives
up, as before step 4 ends on that node; and a list that leaves out the only
cdcs of a range that serves volumes on a node. Once the listed cdcs answer,
none of them lists a path for those volumes, which the node plugin takes as
a detach: it drops their paths, their mounts fail, and their pods must be
stopped and started again, as [A Kubernetes mount that lost its last
path](#a-kubernetes-mount-that-lost-its-last-path) says ([the scope
page](scope.md), section 6.1). No
data on the disks is lost, but a write a pod had not yet flushed is. A
listed cdc that is stopped, by contrast, only holds the drops while it
cannot be reached. Keep the order of the steps, and check the new list
against every range before step 3.

When to stop: after step 3 or step 5, a node logs
`csi follower sync error` naming a cdc of the list on every try: the
address is wrong or the cdc is down. Fix the list or the cdc before you go
on and before any volume fails over.

Tested: unit tests of the node plugin check that it follows its own list
alone, and not an address the controller plugin passes at attach; that it
reads its volumes back at a restart; and that it leaves a connection to an
address outside its list alone. The Kubernetes suite's failover case changes
the list in the config map and restarts the controller plugin alone, so that
every attach passes on a dead cdc address, and checks that the node still
drops its path to the old primary. No suite case restarts the node plugins
with a new list or runs steps 1, 4, 5 and 6.

## A Kubernetes node plugin that does not start

Words: the **node plugin**, a **pod** and a **Kubernetes namespace** are as
in [A Kubernetes volume the node plugin will not format](#a-kubernetes-volume-the-node-plugin-will-not-format),
and **attach** and **detach** as in [Change the cdc addresses of the
Kubernetes driver](#change-the-cdc-addresses-of-the-kubernetes-driver). Native NVMe
**multipath** is the kernel's joining of a host's paths to one volume into
one device, which the setting `nvme_core.multipath` switches on. The **NVMe
over Fabrics device**, `/dev/nvme-fabrics`, is the file through which every
NVMe-oF connection of the host is made; the kernel module nvme-fabrics makes
it, and the module nvme-tcp loads nvme-fabrics with it.

Symptom: `kubectl -n dnv-csi get pods -o wide` shows the node plugin pod of
a node in `CrashLoopBackOff`, or its `dnv-csi` container restarting. Pods
on that node that use a dnv volume stay in `ContainerCreating`; pods that
already run keep their volumes.

Confirm: `kubectl -n dnv-csi logs <pod> -c dnv-csi --previous` ends with the
plugin's refusal, which names each fact it read wrong and its fix:

1. "the node's identity file ...": the host NQN or host id file is missing,
   empty or holds no host NQN. The refusal names both fixes of step 1.
2. "native NVMe multipath is off" or "native NVMe multipath is not built
   into this kernel": without multipath, the loss of one path of a volume
   takes the device its mount uses.
3. "the NVMe over Fabrics device is not there": nvme-fabrics is not loaded.
4. "... cannot be read: ...": a read of the kernel's facts failed.

The plugin reads the host NQN file first, then the host id file, then the
kernel's facts, and it stops at the first identity file it finds wrong. So
a refusal of an identity file names that file alone, and only once both
files are right does a refusal name every fact of the kernel it read
wrong. The plugin does not ask for nvme-tcp itself: the kernel loads it at
the first connect.

What dnv does alone: nothing; the worker reads no Kubernetes node.
Kubernetes starts the container again and again, waiting longer each time,
up to five minutes between tries.

Safe steps:

1. Fix what the refusal names, as root on the node.
   - The identity files, on a node with any NVMe-oF connection, a dnv
     volume's or another tool's, as `nvme list-subsys` shows: write back
     the host NQN and host id its connections carry, which the kernel
     shows for each connection:
     `cat /sys/class/nvme/nvme*/hostnqn 2>/dev/null | sort -u` prints one
     host NQN, to put in `/etc/nvme/hostnqn`, and
     `cat /sys/class/nvme/nvme*/hostid 2>/dev/null | sort -u` one host id,
     to put in `/etc/nvme/hostid`. Stop if either prints more than one
     line.
   - The identity files, on a node with no NVMe-oF connection:
     `nvme gen-hostnqn > /etc/nvme/hostnqn` and
     `uuidgen > /etc/nvme/hostid`, only for a file the refusal names as
     missing, empty or holding no host NQN.
   - Multipath: add `nvme_core.multipath=Y` to the kernel's command line and
     reboot the node; a kernel built without native NVMe multipath must be
     replaced.
   - The fabrics device: `modprobe nvme-tcp`, and copy the release's
     `deploy/modules-load.d/dnv-host.conf`, which names nvme-tcp, into
     `/etc/modules-load.d/`, so that it loads at boot.
2. Delete the node plugin pod, `kubectl -n dnv-csi delete pod <pod>`, so
   that Kubernetes starts it at once rather than after its wait. Its log
   then shows `csi node serving`, or a refusal of another fact, as of the
   host id file once the host NQN file is fixed: go back to step 1 for it.

Before an upgrade from 0.1, whose node plugin started without these checks,
check every Kubernetes node: `cat /sys/module/nvme_core/parameters/multipath`
prints `Y`, `/sys/class/misc/nvme-fabrics` exists, and nvme-tcp is named under
`/etc/modules-load.d`. A node plugin that refuses to start leaves the paths
and the mounts of its node in place, but it unmounts nothing: a pod on that
node that stops stays `Terminating`, and a `kubectl drain` of the node, which
moves its pods to other nodes, waits, until the plugin starts.

Steps that can lose data: new identity files on a node that holds dnv
volumes. The host NQN is the node's name to dnv: an attach lets that name
alone into the volume's subsystem, and the node's connections carry it. A
node plugin that starts under a new host NQN hears from the cdcs no path of
the volumes attached under the old one, takes that as a detach of each, and
drops all their paths: their mounts fail, and writes their pods had not
flushed are lost. Make new identity files only on a node with no NVMe
connection, after a `kubectl drain` of the node has moved its pods away.

When to stop: the log names none of these facts, the plugin still refuses
for a fact you fixed, or the node's connections carry more than one host
NQN. Ask for help.

Tested: unit tests of the node plugin check each refusal, and that the
plugin starts on a node where nvme-tcp is not loaded. No suite case starts
the plugin on a node that lacks one of these.

## A Kubernetes machine is lost

Words: the **node plugin**, a **pod**, a **workload** and a **Kubernetes
namespace** are as in [A Kubernetes volume the node plugin will not
format](#a-kubernetes-volume-the-node-plugin-will-not-format), and the
**controller plugin**, **attach** and **detach** as in [Change the cdc
addresses of the Kubernetes driver](#change-the-cdc-addresses-of-the-kubernetes-driver).
A **node**, in Kubernetes' words, is a machine of the Kubernetes cluster, and
its **Node object** is Kubernetes' record of it. The controller plugin runs as
two **copies**, two pods on two machines where the cluster has two. Each of
its two helper programs, the provisioner and the attacher, acts in one copy
at a time: the copy that holds that program's **lease**, a Kubernetes record
that the copy renews every few seconds and that the other copy takes once it
runs out.

Symptom: `kubectl get nodes` shows a node `NotReady`, and the pods on it
show `Terminating` or `Unknown`.

Confirm: `kubectl get nodes`; `kubectl -n dnv-csi get pods -o wide` shows
where the controller plugin's copies run; `kubectl -n dnv-csi get lease`
shows which copy holds the two leases, `csi-dnv-io` and
`external-attacher-leader-csi-dnv-io`.

What dnv does alone:
- If the lost machine ran the copy that held one lease or both, the other copy
  takes them within about 20 seconds and goes on creating, deleting, attaching
  and detaching volumes. Kubernetes gives up on the lost machine's copy after
  about 70 seconds and starts a new one on another machine, which needs its
  images there: the first time, a pull of about 15 seconds.
- The pods of other workloads on the lost machine stay `Terminating`, and
  their volumes stay attached to the lost machine, for as long as Kubernetes
  keeps the Node object: Kubernetes does not detach a volume while it cannot
  tell whether the machine still writes to it. Their workloads' new pods on
  other machines wait for those volumes.
- If the machine comes back, its kubelet stops the pods Kubernetes gave up
  on, and the volumes detach and move by themselves.

Safe steps, for a machine that does not come back. Check "When to stop"
below before step 1.
1. Make sure the machine cannot write to a volume any more: power it off, or
   cut it from the network that reaches the controller nodes. This step is
   needed where a controller node's cn agent does not end a host's
   connection: one started with `--ignore-host-disconnection` that found one
   of the causes that the check's `nvmet host cut (debugfs)` line names
   ([Check a node before it joins](#check-a-node-before-it-joins), step 3).
   That agent logs the warning "agent host cut disabled" at its start, and
   it ends no connection until it is started again, even once the kernel
   can. Where the cn agents end a host's connection, the detach of step 2
   waits until the volume's primary has ended the machine's connection, and
   an attach to another machine waits for the same end, so the other machine
   gets the volume only after it, save a connection that shows only after
   the primary's converge has looked. The converge that the attach's write
   brings lets the other machine in and ends that connection right after;
   with no attach, a later check round finds it and ends it ([the scope
   page](scope.md#54-hosts-and-the-kubernetes-driver)). A standby ends its
   own too, and in any case before it serves. There this step closes that
   short window and adds a second guard.
2. Mark it out of service:
   `kubectl taint nodes <node> node.kubernetes.io/out-of-service=nodeshutdown:NoExecute`.
   Kubernetes deletes its pods and detaches their volumes; the workloads'
   new pods attach them on other machines. Each detach takes the machine off
   the list of hosts that may connect to the volume, and then waits until
   the cn agent of the volume's primary has ended the machine's connection
   to the volume at its next converge, which takes a second or two, save a
   connection that shows only after that converge has looked (step 1). Once
   a detach call has failed, Kubernetes may already ask to attach the volume
   to another machine; that attach waits for the same end, and it is this
   wait that keeps the other machine out. A primary whose cn agent does not
   answer holds the detach and the attach, and so does a connection that
   does not end: Kubernetes gives each call 15 seconds and then tries it
   again, with a pause between tries that grows from one second up to five
   minutes, so the volume moves only once that cn agent answers, once the
   connection ends, or once another controller node becomes the primary, and
   then up to one pause later. A controller node that cannot read its
   connections, or that a lockdown, the kernel mode that Secure Boot
   switches on, keeps from ending them on a node where /sys/kernel/security
   is mounted, reports an error. Where the pool has an enabled standby with
   no health epoch whose agent does not refuse the worker's syncups, the
   worker then fails the primary over to it at the pool's threshold. A
   pool with no such standby, such as a pool with one cntlr, waits for the
   cntlr replacement, which comes after the pool's `cntlr_unhealthy`, 600
   seconds at the default, and the volume waits with it. A connection
   that does not end for any other reason starts no failover: the cn agent
   reports it as pending, as when the machine's IO is held on the controller
   node, or under a lockdown on a node where /sys/kernel/security is not
   mounted, which the cn agent cannot see. To serve the volume from another node
   meanwhile, first check, as step 3 of [A planned restart of an
   agent](#a-planned-restart-of-an-agent) says, that `dnvctl sp get` shows
   another cntlr of the pool with `disabled` false and `err_epoch` 0; if there
   is none, do not disable the primary: no other cntlr becomes the primary, and
   the pool's IO stops. If there is one, disable the primary's cntlr with
   `dnvctl cntlr set-enabled --id <cntlr id> --enabled=false`: a disabled
   cntlr is never the primary, so the worker makes another cntlr the primary
   without its usual wait, and that cntlr ends the machine's connection
   there before it serves. Enable the old cntlr again once its row for the
   volume's subsystem reads `RES_STATUS_OK` in
   `dnvctl cntlr inspect --id <cntlr id>`, with
   `dnvctl cntlr set-enabled --id <cntlr id>`.
3. Once the machine is back and clean, or replaced, remove the mark:
   `kubectl taint nodes <node> node.kubernetes.io/out-of-service-`.

Steps that can lose data: the out-of-service mark, a forced delete of the lost
machine's pods (`kubectl delete pod --force --grace-period=0`), or a delete of
its Node object, while the machine still runs and reaches a controller node
whose cn agent ends no connection (step 1). A detach takes the machine off the
volume's list and waits for the primary's cn agent to end the machine's
connection, and the attach to another machine waits for the same end, where
that cn agent ends connections; where it logged "agent host cut disabled" at
its start (step 1), nothing ends the connection and neither waits for it ([the
scope page](scope.md#54-hosts-and-the-kubernetes-driver)): the machine keeps
its connections to the volumes there and can write beside the machine the
volumes move to. Without step 1, a machine whose connection shows only after
the primary's converge has looked can also write for a moment beside the
machine the volume moves to, until the converge that lets that machine in ends
that connection right after (step 1).

When to stop: before step 1, the machine answers, by ssh or on its console,
while Kubernetes shows it `NotReady`: that is a fault of its network or of
its kubelet, not a lost machine. Fix that and do not mark it. A machine that
you cut from the network yourself in step 1 still answers on its console:
that is no reason to stop, and step 2 marks it.

Tested: the Kubernetes suite's machine case cuts the host of the copy that
holds the leases off the network and checks the controller plugin's part: the
other copy takes both leases, serves a new volume on the other host, and a new
copy runs elsewhere while the host stays cut. Its detach case cuts a host that
writes to a volume off the machine of the Kubernetes server alone, so that the
host still reaches the controller nodes. It stops the worker, so that no cn
agent converges, and marks the host out of service. For 20 seconds the volume
stays attached to the host, and the host stays connected to the primary.
Kubernetes asks to attach the volume to the other host meanwhile, and that
attach waits too. Once the worker runs again, the primary's cn agent ends the
host's connection, and only then do the detach and the attach end. The host's
writes then fail, and the volume's new pod on the other host reads what the
host wrote and writes. The detach case, and the whole Kubernetes suite with it,
passed in the lab on 2026-11-20. Both cases run on controller nodes that can
end a host's connection; a machine lost while a cn agent runs with
`--ignore-host-disconnection` is not tested, and neither is a connection that
does not end.

## A one-cntlr Kubernetes volume after a cntlr replacement

Words: as in [A Kubernetes machine is lost](#a-kubernetes-machine-is-lost);
a **filesystem volume** and a **raw block volume** are as in [A Kubernetes
volume the node plugin will not
format](#a-kubernetes-volume-the-node-plugin-will-not-format), and the
**node's mount** of a volume as in [A Kubernetes mount that lost its last
path](#a-kubernetes-mount-that-lost-its-last-path).
A **controller id** is the number the target gives each connection of a
subsystem; one host's kernel refuses two connections of one subsystem with
the same number.

Symptom: a pod's IO to a volume of a storage class with one cntlr waits
with no end, after the worker replaced the volume's cntlr, the reaction that
follows the loss of the cntlr's controller node by the pool's cntlr
threshold, 600 seconds at the default.

Confirm:
- `dnvctl sp get` for the volume's pool, whose name is the persistent
  volume's (`kubectl -n <Kubernetes namespace> get pvc <claim>` names it),
  lists one cntlr, on another controller node than before.
- On the node of the pod, as root, `nvme list-subsys` shows the volume's
  subsystem with one path, to the old controller node's address, in the
  state `connecting`, and `dmesg` shows "Duplicate cntlid" for the
  subsystem.
- The node plugin's log, `kubectl -n dnv-csi logs <node plugin pod> -c dnv-csi`,
  shows `csi follower sync error` for the connect to the new controller
  node's address on each try.

What dnv does alone: nothing ends it. The new cntlr took the old one's
controller id, which the old path holds; the node plugin keeps the old path
until a path to the new cntlr shows the volume, and its connect to the new
cntlr is refused while the old path holds the id.

Safe steps:
1. On the node, as root, find the old path:
   `grep -l <subsystem NQN> /sys/class/nvme/nvme*/subsysnqn`, and among those
   folders the one whose `address` file names the old controller node.
2. Drop it: `nvme disconnect -d nvmeN`, with that folder's name.
3. Within a minute the node plugin connects the new cntlr: `nvme
   list-subsys` shows the new path `live`.
4. The pod's mount does not follow the new device. For a filesystem
   volume, deleting the pod is not enough: its workload's new pod can start
   on the same node while the node's mount of the volume, the old one,
   stands, and get it. Stop the volume's pods and start them again once the
   node holds nothing of the volume, with steps 2, 4 and 5 of [A Kubernetes
   mount that lost its last
   path](#a-kubernetes-mount-that-lost-its-last-path). For a raw block
   volume, delete the pod: its workload makes a new one, whose start takes
   the new device.

Steps that can lose data: step 2 fails the IO that waits on the old path,
so writes the pod had not yet flushed to the volume are lost.

When to stop: the pool lists more than one cntlr, or the kernel log shows no
"Duplicate cntlid": that is another fault. Ask for help.

Tested: not tested. The suites give every volume two cntlrs, where the
failover's new primary serves while the old path goes.

## Restart nvme-stas

Words: **nvme-stas** is the host program that [dnv on one
machine](tutorial_all_in_one.md) uses to follow the cdc. It has two
daemons. **stafd** holds a connection to each cdc of its configuration and
reads the cdc's answer, the **discovery log**, which lists the address of
each cntlr of each subsystem the host may use. **stacd** connects the host
to the cntlrs the discovery logs list. With that tutorial's setting
`disconnect-scope=only-stas-connections` in `/etc/stas/stacd.conf`, it also
disconnects a connection it made once no log lists it. `stafctl` and
`stacctl` are the commands of the two daemons. A host's **path** to a
subsystem is one of its connections to one cntlr of the pool; the kernel
joins the paths to one volume into one device.

When: stafd must start again on a host that holds paths to dnv volumes, for
example after a change of `/etc/stas/stafd.conf`, such as a new cdc
address. Or stacd alone must start again.

What dnv does alone: nothing. dnv runs no program on such a host and does
not see what nvme-stas does. stacd reads a stafd it cannot reach as an
empty discovery log. The next time it compares its connections with the
logs, it then disconnects every path it made. It does so at its start, at a
reload, and at a pass that a removed controller, a device audit or one of
its timers starts. This was read in the source of nvme-stas 2.3.1; the lab
runs nvme-stas 2.4.1. A stop of stacd keeps the paths it made, and a stop
of stafd keeps its connections to the cdcs under the tutorial's setting
`persistent-connections=true` in `/etc/stas/stafd.conf`: both were seen in
the lab with nvme-stas 2.4.1. While stacd is stopped, nothing on the host
follows the discovery log: a failover meanwhile leaves the host on the old
primary's path ([A host that keeps a withdrawn
path](#a-host-that-keeps-a-withdrawn-path)), and the host gets no path to a
new cntlr.

Safe steps, as root on the host:

1. Note what the host uses: `stafctl adlp -d` lists the records stafd
   holds now, read as step 4 says, and `nvme list-subsys` lists each
   subsystem with its paths, their addresses and their states.
2. Stop stacd: `systemctl stop stacd`. Keep the time until step 5 short, for
   the reason What dnv does alone gives.
3. Restart stafd: `systemctl restart stafd`. To restart stacd alone, leave
   this step out.
4. Wait until stafd holds its connection to every cdc again.
   `stafctl adlp -d` prints one entry for each cdc address of stafd's
   configuration. In each entry, the `discovery-controller` part names the
   address, as `traddr` and `trsvcid`, and its `connected` field must read
   `True`. The `log-pages` part lists the records the cdc gave this host:
   the cdc's record of itself, and for each subsystem the host may use its
   NQN, `subnqn`, with the address of each of its cntlrs that the cdc
   lists. Read it again until every cdc address reads connected and the
   records name every subsystem that they named at step 1. stacd keeps a
   path while some cdc lists the path's subsystem at the path's address,
   and drops it once none does. So if a cdc address still does not read
   connected after a minute, go on to step 5 only when the records of the
   connected cdcs together list each path of step 1: its subsystem's
   `subnqn` with its `traddr` and `trsvcid`. They may leave out a path
   whose cntlr the cdc no longer lists, by step 1 of Confirm of [A host
   that keeps a withdrawn path](#a-host-that-keeps-a-withdrawn-path):
   `dnvctl sp get` no longer shows that cntlr, or shows it with `disabled`
   true, or with `primary` false and `err_epoch` set. stacd then drops
   that path, as it should. If they leave out any other path, stop (When
   to stop). After step 6, repair the cdc whose address does not read
   connected, or its address in stafd's configuration; a change of that
   configuration needs this procedure again. Then read `stafctl adlp -d`
   again until that address reads `connected` `True`. If it still does
   not a minute after the repair, do this procedure again: no run shows
   that a stafd started while a cdc did not answer connects to that cdc
   once it answers.
5. Start stacd: `systemctl start stacd`.
6. Check that `nvme list-subsys` still shows the paths of step 1, `live`,
   save a path to a cntlr that the cdcs no longer list.

Steps that can lose data: these can make stacd disconnect every path it
made: a restart or a stop of stafd while stacd runs; a start of stacd while
stafd is down or before step 4 shows its records, which
`systemctl restart stafd stacd` can do on a host that holds paths; and a
reload of stacd while stafd is away. A start of stacd while a cdc address
does not read connected, and the connected cdcs leave out a path of step 1
whose cntlr the cdc still lists, makes stacd drop that path; step 4 forbids
it. When a connected cdc serves old records, that path can be the one to a
new primary after a failover, and the host is left on the fenced old
primary, whose IO fails. Once a subsystem has no path left, its
device goes: IO to it fails, and writes not yet flushed to it are lost. A
later connect makes a new device, which a mount of the old one does not
follow: unmount it and mount the new device. Also never disconnect by hand,
while stafd runs, a connection that stafd made to a cdc, one whose
`subsysnqn` file reads `nqn.2014-08.org.nvmexpress.discovery`: the host
then hears of none of that cdc's changes until stafd connects it again.
The source of nvme-stas 2.4.1 reads that stafd tries again at its next
retry, most often a minute later; no run shows that it does. The cdc suite
never touches these connections while stafd runs, for this reason.

When to stop: at step 4, after a minute, the records of all the connected
cdcs together still leave out a subsystem that they named at step 1 and
whose paths the host must keep; or, while a cdc address does not read
connected, they leave out a path of step 1 whose cntlr the cdc still lists.
Do not start stacd then: it would drop the paths that only the missing
records name. A connected cdc that leaves out such a path may serve old
records ([A host that keeps a withdrawn
path](#a-host-that-keeps-a-withdrawn-path), step 3 of Confirm). Fix stafd's
configuration, the network to the cdc, or the cdc first, and keep the time
short, as What dnv does alone says. Ask for help if you cannot.

Tested: not tested. No suite restarts stafd while stacd is stopped, or
starts stacd after such a restart: the suites start both daemons together
before the host holds any path, and stop them after. The read of step 4 is
the one the cdc suite, `integtest/cdc_test.sh`, makes in its manyhosts case
on the lab's nvme-stas 2.4.1, where each cdc address must read connected
and carry its records. That a stop of stacd or of stafd keeps their
connections was seen in the lab; no suite checks it. The same suite's ha
case kills every cdc for a few seconds while nvme-stas runs on the hosts
and starts the cdcs again: stafd, which held a connection to each of them,
connects to each again, and the hosts keep the device of every subsystem
they use throughout. Not tested: the start of stacd at step 4 while a cdc
address does not read connected, and the later connect of a stafd that was
started while a cdc did not answer.

## A host that keeps a withdrawn path

Words: **nvme-stas**, **stafd**, **stacd**, the **discovery log** and a
**path** are as in [Restart nvme-stas](#restart-nvme-stas), and a **range**
and **twins** as in [Change the cdc addresses of the Kubernetes
driver](#change-the-cdc-addresses-of-the-kubernetes-driver). The cdc lists a
cntlr of a subsystem while the cntlr is enabled and is either the primary
or has no health epoch. A **withdrawn path** is a host's path to a cntlr
that the cdc no longer lists: a cntlr that was disabled, deleted or
replaced, or one that is not the primary while its health epoch is set,
such as an old primary after a failover, until that health epoch clears.
A host that follows the discovery log drops a withdrawn path by
itself: nvme-stas once no cdc it reads lists the cntlr, and the node plugin
of dnv's Kubernetes driver once it also holds a path to a listed cntlr that
shows the volume, or once every cdc of its list answers and none lists a
cntlr of the volume.

Symptom: a host holds a withdrawn path, and one of these follows:

- After a failover, the host's IO to the volume fails on that path from
  the moment the disk nodes fence the old primary. A write the host was
  told was done can then be lost, in the cases [the scope page](scope.md),
  section 5.1, lists.
- After a cntlr replacement, the host gets no path to the new cntlr, and
  `dmesg` shows "Duplicate cntlid" for the subsystem. A **controller id** is
  the number the target gives each connection of a subsystem, and one
  host's kernel takes no two connections of one subsystem with the same
  number: the new cntlr gave the host's connect the number that the old
  path holds.

The causes:

1. **A path nothing manages**: one connected by hand with `nvme connect`,
   or a controller named in stacd's own configuration, the `[Controllers]`
   part of `/etc/stas/stacd.conf`, rather than learned from the cdc.
   Nothing ever drops it.
2. **A stale cdc**: a cdc that cannot reach etcd serves the records it last
   read, and so does a cdc whose stream of changes from etcd stopped while
   etcd still answers. nvme-stas keeps a path while any cdc it reads lists
   it, so one stale twin is enough.
3. **stacd is stopped**: nothing drops a path until it runs again ([Restart
   nvme-stas](#restart-nvme-stas)).

Confirm:

1. `dnvctl sp get` lists the pool's cntlrs under `cntlr_list`, each with its
   address as `nvme_tr_conf.tr_addr` and `nvme_tr_conf.tr_svc_id`. The cdc
   lists those whose `disabled` is false and whose `primary` is true or
   whose `err_epoch` is 0.
2. As root on the host, `grep -H . /sys/class/nvme/nvme*/address` shows the
   address of each connection, and the `subsysnqn` file in the same folder
   names its subsystem; `nvme list-subsys` shows the state of each path. A
   path of the subsystem whose address is not that of a cntlr the cdc lists,
   by step 1, is withdrawn.
3. What the cdcs tell the host: on an nvme-stas host, `stafctl adlp -d`
   lists the records stafd read from each cdc ([Restart
   nvme-stas](#restart-nvme-stas), step 4). A cdc whose records still name
   the address of a cntlr that the cdc should not list, by step 1, is stale.
   On a host with no follower of the discovery log, the path is one of
   cause 1.

What dnv does alone: in a failover, the worker gives the old primary its
demotion first, which moves the old primary's namespaces to ANA
inaccessible, so a host that also holds a path to the new primary sends its
IO there once that path serves. The worker waits up to three seconds for the
demotion to be reported done, and then the disk nodes fence the old primary.
The three seconds are the time a host that follows the discovery log needs
to drop the path. When the old primary's agent reports its demotion before
the host drops the path, the fence can come first, but the path is then ANA
inaccessible and carries no IO. An old primary whose agent the worker
cannot reach gets no demotion, and keeps serving its path until the fence.
A stale cdc that reaches etcd again catches up by itself and tells its
hosts of the change, and nvme-stas then drops the path. Nothing drops a
path nothing manages.

Safe steps, as root on the host:

1. For a path nothing manages, on a pool whose cntlr was failed over or
   disabled: first connect the host to the primary, if it holds no path to
   it:
   `nvme connect -t tcp -a <tr_addr> -s <tr_svc_id> -n <subsystem NQN> --hostnqn <host NQN> --hostid <host id>`,
   with the primary's address from step 1 of Confirm, and the host NQN and
   host id that the host's other connections carry, which
   `/sys/class/nvme/nvme*/hostnqn` and `/sys/class/nvme/nvme*/hostid` show.
   Wait until `nvme list-subsys` shows that path `live`. Then drop the
   withdrawn path by its folder's name from step 2 of Confirm:
   `nvme disconnect -d nvmeN`. With a live path left, the device stays.
2. For a path nothing manages whose cntlr was replaced: the new cntlr's path
   cannot join while the old path holds its controller id, so drop the old
   path first, `nvme disconnect -d nvmeN`, and then connect the new cntlr as
   in step 1. If the old path is the host's only path to the subsystem, this
   drop can lose data (Steps that can lose data).
3. For a controller named in `/etc/stas/stacd.conf`: take it out of that
   file, restart stacd as [Restart nvme-stas](#restart-nvme-stas) says, and
   drop the path as in step 1 if it stays.
4. For a stale cdc: if its machine cannot reach etcd, repair that link, and
   the cdc catches up by itself. If it reaches etcd and still serves the old
   records, restart it with the same flags: when it starts, it reads etcd
   whole before it accepts a host. Never restart a cdc that cannot reach
   etcd: it then accepts no host until it can, and logs
   `cdc waiting for first scan` once a minute. Do not stop the stale cdc to
   be rid of its records. In the cdc suite's ha case, hosts that run
   nvme-stas keep the device of every subsystem they use while every cdc
   is down for a few seconds and is started again, so a stop does not make
   nvme-stas drop a path at once; how long it keeps the records of a cdc it
   cannot reach is not known. And a node of dnv's Kubernetes driver drops
   no path at all while a cdc of its list cannot be reached.
5. For a stopped stacd: finish [Restart nvme-stas](#restart-nvme-stas).
6. On a node of dnv's Kubernetes driver, drop no path by hand that a cdc
   still lists: the node plugin connects every path a cdc lists. A pool of
   one cntlr after a cntlr replacement has steps of its own: [A one-cntlr
   Kubernetes volume after a cntlr
   replacement](#a-one-cntlr-kubernetes-volume-after-a-cntlr-replacement).

Steps that can lose data: `nvme disconnect -n <subsystem NQN>` drops every
path of the subsystem, the live ones too: never use it to drop one path. A
drop of the host's only path to a subsystem, as step 2 can be, fails the IO
that waits on it: the device goes, writes not yet flushed to it are lost, and
a mount of the old device must be unmounted and mounted again on the new
one. A path dropped by hand while a stale cdc still lists it may be
connected again by stacd; whether it is, is not known.

When to stop: the path you would drop is the primary's, since `dnvctl sp get`
shows its address on the cntlr whose `primary` is true. That path is not
withdrawn: see [A fault only a host sees](#a-fault-only-a-host-sees). Stop
too when the cdcs still disagree after step 4 while each reaches etcd, or
when a node of the Kubernetes driver keeps a withdrawn path while it holds a
`live` path to the primary and every cdc of its list answers. Ask for help.

Tested: the e2e suite's cutoff case connects a host by hand to a pool's
primary alone and stops the primary's cn agent: the host keeps that path
past the failover, and its writes fail from the fence. The host then drops
the old path, with `nvme disconnect -n` since it holds no other, connects to
the new primary by hand, and reads back what it wrote before the failed
write. The same case checks that nvme-stas on another host drops its path
to the old primary before the fence, with no failed write, and connects it
again once the old cntlr is a healthy standby, and that the cdc serves the
same records while etcd is down. It passed in the lab on 2026-11-21. The cdc
suite's stas case checks that nvme-stas drops a connection whose record
goes, and keeps the device when a record moves to a new address, the new
address first. Its ha case, with nvme-stas 2.4.1 on the hosts, kills one
cdc and starts it again: the cdc reads etcd and then serves the same
records as its twin. It then kills every cdc for a few seconds and starts
them again, and checks at each of its reads meanwhile that the hosts keep
the device of every subsystem they use. The Kubernetes suite's
failover case checks that a node drops its path to the old primary after a
failover. Not tested: the connect before the drop of step 1, a drop by
device of one path among several, the lost cntlr's path of step 2, a stale
cdc during a failover and its recovery, the restart at step 4 of a cdc that
serves old records, a controller named in stacd's configuration, and the
start of a stopped stacd at step 5.

## A Kubernetes mount that lost its last path

Words: the **node plugin**, a **pod**, a **filesystem volume**, a **raw
block volume**, a **claim**, a **persistent volume**, a **workload**, a
**Kubernetes namespace** and the **kubelet** are as in [A Kubernetes volume
the node plugin will not
format](#a-kubernetes-volume-the-node-plugin-will-not-format), **attach**
and **detach** as in [Change the cdc addresses of the Kubernetes
driver](#change-the-cdc-addresses-of-the-kubernetes-driver), and a **path**
as in [Restart nvme-stas](#restart-nvme-stas). The node plugin mounts a
filesystem volume once on each node that uses it, in a folder of the
kubelet, and gives each pod of that node that uses the volume this one
mount: it is the **node's mount** of the volume. When the last pod of the
volume on a node is gone, the kubelet asks the node plugin to **let go** of
the volume there: the plugin unmounts the node's mount and drops every path
of the volume.

Symptom: a pod's IO to its volume fails or waits with no end, in one of two
ways:

1. **The mount waits.** No cntlr of the volume serves the node, and the IO
   waits with no error, since the node plugin's connects never give up. On
   the node, `nvme list-subsys` shows each path of the volume's subsystem in
   a state other than `live`, such as `connecting`. A pod whose IO waits
   like this does not stop when it is deleted.
2. **The mount is dead.** The volume's last path on the node ended once:
   by a reconnect the controller node refused with "do not retry", by
   someone's `nvme disconnect`, or by the node plugin, which drops every
   path of the volume once every cdc of its list answers and none lists a
   cntlr of the volume. The volume's device went with it, and the
   node's mount kept that device. A later connect made a new device, which
   the mount does not follow: the pod's IO fails while `nvme list-subsys` on
   the node shows a `live` path of the subsystem.

Confirm:

1. Find the volume. `kubectl -n <Kubernetes namespace> get pvc <claim>`
   names its persistent volume, and the `Used By` line of
   `kubectl -n <Kubernetes namespace> describe pvc <claim>` names the pods
   that use the claim.
   `kubectl get pv <pv> -o jsonpath='{.spec.csi.volumeHandle}'` prints the
   volume's id: the cluster's name, a slash and the name of the volume's
   storage pool, which `--cluster` and `--sp` take below. The subsystem's
   NQN is
   `kubectl get pv <pv> -o jsonpath='{.spec.csi.volumeAttributes.nqn}'`,
   and the volume's device on a node is `/dev/disk/by-id/nvme-uuid.<uuid>`,
   with `<uuid>` read the same way from `{.spec.csi.volumeAttributes.uuid}`.
2. `dnvctl sp get` for that pool. For the mount that waits, every cntlr
   shows `err_epoch` set, and `dnvctl cluster get-status` shows a `health`
   line for each. For the dead mount, a cntlr is primary with `err_epoch` 0.
   A pool of one cntlr, now on another controller node than before, with
   "Duplicate cntlid" for the subsystem in the node's `dmesg`, is the case
   of [A one-cntlr Kubernetes volume after a cntlr
   replacement](#a-one-cntlr-kubernetes-volume-after-a-cntlr-replacement):
   follow it first.
3. On the node of the pod, as root, `nvme list-subsys` shows the
   subsystem's paths and their states, as the symptom says.

What dnv does alone:

- The node plugin's connects never give up: while a path of the volume is
  left, the kernel holds the IO and reconnects onto the same device. So the
  mount that waits goes on by itself, with no loss, once a cntlr of the
  volume serves again. The worker makes an enabled standby with no health
  epoch the primary once the primary has been unhealthy for the pool's
  `primary_unhealthy`, 4 seconds at the default, and replaces a cntlr that
  has been unhealthy for the pool's `cntlr_unhealthy`, 600 seconds at the
  default.
- Nothing reports a dead mount, and nothing ends it: the node plugin tells
  Kubernetes nothing of a volume's health.
- While a volume is in use on a node, the node plugin runs a pass, in which
  it matches the node's paths to what the cdcs list, at each change notice
  of an NVMe connection of the node and every 30 seconds, its
  `--sync-period`, which `deploy/node.yaml` does not set. Each pass connects
  the node to every cntlr the cdcs list for the volume that it holds no path
  to. After a connect that
  failed, it tries again one second later, then at waits that double up to
  30 seconds. So once the last path is gone, the next pass after a listed
  cntlr answers makes a new device.
- When a pod of a filesystem volume starts on a node where the node's mount
  of the volume stands, the node plugin takes that mount as it is: it does
  not check the device under it. So a pod that starts there while a dead
  mount stands gets the dead mount. A raw block volume has no node's mount:
  each pod's start takes the device that `/dev/disk/by-id/nvme-uuid.<uuid>`
  names at that moment, so a new pod gets the new device.
- Kubernetes detaches the volume from a node only once the node plugin has
  let go of it there; this order is Kubernetes' own, and no dnv test checks
  it. The detach waits until the pool's primary reports, and an attach to a
  node waits the same way, as [A Kubernetes machine is
  lost](#a-kubernetes-machine-is-lost) says. So while no cntlr serves, the
  volume stays attached, and no pod can start with it on any node.

Safe steps, for a filesystem volume:

1. If a cntlr of the volume can serve again, bring it back: start its
   controller node, or repair the network to it. The mount that waits then
   goes on by itself: once its IO goes on, stop here. Steps 2 to 5 are for
   the dead mount. Do them for a mount that waits only when its pods must
   stop before a cntlr can serve again: step 3 then loses the writes they
   had not flushed, which waiting keeps.
2. Stop the volume's pods. Find the workload of each pod of step 1 of
   Confirm:
   `kubectl -n <Kubernetes namespace> get pod <pod> -o jsonpath='{.metadata.ownerReferences[0].kind}/{.metadata.ownerReferences[0].name}'`
   prints its owner, such as `StatefulSet/<name>`, and `/` alone for a pod
   with no owner. A pod of a Deployment prints `ReplicaSet/<name>`: a
   ReplicaSet is the record through which a Deployment keeps its pods. The
   same read of `replicaset <name>` in place of `pod <pod>` then prints
   `Deployment/<name>`, or `/` alone for a ReplicaSet with no owner, which
   is then itself the workload. Note the workload's pod count,
   `kubectl -n <Kubernetes namespace> get <kind>/<name> -o jsonpath='{.spec.replicas}'`,
   where `<kind>` is `deployment`, `statefulset` or `replicaset`, and scale
   the workload to zero:
   `kubectl -n <Kubernetes namespace> scale <kind>/<name> --replicas=0`.
   A StatefulSet's scale to zero stops all of its pods, each with a claim of
   its own, so their IO stops too until step 5. Delete the pod only when the
   read prints `/` alone: a workload makes a new pod with the same claim at
   once, which can start on the same node and get the old mount. For any
   other owner, such as a DaemonSet, which keeps one pod on each node, or a
   Job, which runs a pod until its work ends, this step has no command:
   stop that owner by its own means, or ask for help.
3. If a pod of the volume does not stop, because its IO waits: on its node,
   as root, run `nvme disconnect -n <subsystem NQN>`. This fails the IO that
   waits, and the pod stops. Until a cntlr serves again, the node plugin's
   connects to the volume fail.
4. Wait until the node holds nothing of the volume:
   `kubectl get volumeattachment` lists no attachment of the persistent
   volume to the node, and, as root on the node, `nvme list-subsys` shows no
   path of the subsystem. While no cntlr serves, the attachment stays, as
   What dnv does alone says: do step 1 meanwhile.
5. Scale the workload back to its count:
   `kubectl -n <Kubernetes namespace> scale <kind>/<name> --replicas=<count>`,
   or make the pod again. Its start connects the node again and mounts the
   volume's new device.

Safe steps, for a raw block volume: for the mount that waits, do step 1,
and the pod goes on by itself. Otherwise delete the pod, and if it does not
stop because its IO waits, do step 3. Its workload's new pod takes the
device that the volume's link names at its start, so it starts once a cntlr
serves and a path of the volume is `live` again.

Steps that can lose data: step 3 fails every write that waits on the volume
on that node, so writes the pod had not flushed are lost. A pod stopped while
its IO fails, as for a dead mount, loses what it had not flushed as well.
What was written and flushed stays on the pool.

When to stop: `dnvctl sp get` shows a primary with `err_epoch` 0 while the
node's paths to it are not `live`: that is [A fault only a host
sees](#a-fault-only-a-host-sees). Stop too when step 4 does not end once a
cntlr serves, or when the new pod's IO still fails. Ask for help.

Tested: not tested. No suite case loses a volume's last path, ends a pod's
waiting IO with `nvme disconnect -n` or starts a pod over a new device; the
Kubernetes suite's failover, restart and detach cases keep a path, or detach,
by design. A unit test of the node plugin checks that a second request to
make a filesystem volume ready, over a mount that stands, mounts nothing
again. The CSI tutorial, which the tutorial suite runs, deletes a pod and
waits until the node shows no path of the volume, the reading of step 4.

## A fault only a host sees

Words: a **path** is as in [Restart nvme-stas](#restart-nvme-stas), the
**status read** as in [Read what is wrong](#read-what-is-wrong), and the
**check interval** as in [A planned restart of an
agent](#a-planned-restart-of-an-agent). A **fault only a host sees** keeps
hosts from a pool's primary while the
worker still reaches the primary's agent: for example, the network between
the hosts and the primary's controller node is cut, and the network between
that node and the control plane is not.

Symptom: the IO of hosts to the volumes of one pool stops or waits, while
dnv sees nothing wrong.

Confirm:

1. `dnvctl cluster get-status` lists no line for the pool, and
   `dnvctl sp get` shows the cntlr whose `primary` is true with `err_epoch` 0.
   `dnvctl sp get` lists the cntlrs under `cntlr_list` in the order of the
   ids in `sp_conf.cntlr_id_list`, so the primary's id stands at the same
   place in that list. `dnvctl cntlr inspect --id <primary cntlr id>` shows
   no row of the primary that reads `RES_STATUS_ERROR`.
2. As root on a host, `nvme list-subsys` shows the volume's subsystem with
   its paths. The path to the primary's address, its `nvme_tr_conf` in
   `dnvctl sp get`, is not `live`, for example `connecting`, while the path
   to a standby is `live`.

What dnv does alone: nothing, for as long as the fault lasts. The worker
judges the agents' answers and never reads a host, so the primary stays the
primary, and the cdc keeps listing it. A host's IO waits while the host holds
a path to a standby; on a node of dnv's Kubernetes driver it waits with no
end. Once the operator disables the primary, the worker makes a standby the
primary at its next pass over the pool, with no event threshold to wait
out; it makes such a pass every check interval, 2 seconds at the default.
It takes, of the other cntlrs that are enabled and have no health epoch,
the one with the smallest id, and passes over one whose agent refuses its
syncups. It does not ask whether the hosts reach that cntlr. It
first gives the old primary its demotion and waits up to three seconds for
it to report the demotion done; then the disk nodes fence the old primary,
and the new primary builds its devices. Until it reports them built,
`dnvctl sp get` shows the new primary with `settling` true. A disabled
cntlr serves no IO, is never made the primary and is never replaced, and the
gateway takes it out of the cdc's records in the same write that disables it.
The status read lists a disabled primary until its failover, and never says
that a cntlr which is not the primary is disabled.

Safe steps:

1. Find the cntlrs the worker can make the primary: those that
   `dnvctl sp get` shows with `primary` false, `disabled` false and
   `err_epoch` 0. Each entry of `cntlr_list` has its id at the same place in
   `sp_conf.cntlr_id_list`. The worker takes the one with the smallest id,
   so the hosts must reach it: on each host that holds a path to it,
   `nvme list-subsys` must show that path `live`. Disable first each of
   these cntlrs whose path is not `live`, with the command of step 2 and its
   own id. It is not the primary, so no failover follows, and the worker
   then passes it over. If none is left whose path is `live`, do not
   disable the primary (Steps that can lose data): enable again each cntlr
   you disabled here, and repair the network instead.
2. Disable the primary:
   `dnvctl cntlr set-enabled --id <primary cntlr id> --enabled=false`.
3. Wait until `dnvctl sp get` shows another cntlr as primary with `settling`
   false. If no other cntlr becomes primary within a few check intervals,
   enable the old cntlr again at once, with the command of step 6: it is
   still the primary, and a disabled primary serves no IO. If another cntlr
   became primary but still shows `settling` true a minute later, as when
   its agent refuses the syncup that makes it primary, the hosts' IO waits
   on its path. In that case, do not enable the old cntlr while the hosts
   cannot reach it: the worker can give it the role back, at once if the new
   primary is disabled, or else once the new primary's health epoch is as
   old as the longer of the pool's `primary_unhealthy` and
   `cntlr_unhealthy`, 600 seconds at the defaults. The cdc
   then stops listing the new primary, so a host that follows the
   discovery log drops its last path (Steps that can lose data). If step 1
   left another cntlr that `dnvctl sp get` shows with `primary` false,
   `disabled` false and `err_epoch` 0, disable the new primary too, with
   the command of step 2 and its id, so that the worker gives the role to
   such a cntlr, whose path the hosts reach. If none is left, repair the
   network first, as step 5 says, check that the hosts reach the old
   cntlr, as step 6 says, and enable it. Then wait until `dnvctl sp get`
   shows it with `err_epoch` 0 and, on each host that uses the pool,
   `nvme list-subsys` shows the path to it `live`, as step 1 says; only
   then disable the new primary. Ask for help in each of
   these cases: a standby whose agent refuses its syncups can take the role
   and then serve nothing.
4. Check the hosts: `nvme list-subsys` shows the path to the new primary
   `live`, and their IO goes on. A host that runs nvme-stas, and a node of
   dnv's Kubernetes driver, drop the old primary's path by themselves; a path
   connected by hand stays ([A host that keeps a withdrawn
   path](#a-host-that-keeps-a-withdrawn-path)).
5. Repair the network between the hosts and the old primary's controller
   node, and the controller nodes of the cntlrs step 1 disabled.
6. For the old cntlr, and for each cntlr step 1 disabled, check that the
   hosts reach its controller node again. `nvme list-subsys` cannot show
   this: the cdc does not list a disabled cntlr, so a host that follows the
   discovery log has dropped its path to it. Instead, on each host that
   uses the pool, `timeout 3 bash -c '</dev/tcp/<tr_addr>/<tr_svc_id>' && echo open`
   must print `open`, with the cntlr's `nvme_tr_conf.tr_addr` and
   `nvme_tr_conf.tr_svc_id` from `dnvctl sp get`: a disabled cntlr still
   exports its subsystems there, as a standby does. Every host must print
   it: the fault can cut some hosts and not others, so a host that was
   never cut tells nothing of the rest. Then enable the cntlr:
   `dnvctl cntlr set-enabled --id <cntlr id>`. It comes back as a standby,
   and the cdc lists it again once its `err_epoch` is 0. While it stays
   disabled it is no standby: in a pool of two cntlrs, no cntlr can take over
   from the new primary. Do not enable it while the hosts cannot reach it: an
   enabled cntlr is a candidate for the next failover, and a failover to it
   brings the fault back. Leave a new primary that step 3 disabled as it
   is, and ask for help about it.
7. Check that `dnvctl sp get` shows each cntlr you disabled in steps 1 and
   2 with `disabled` false. Nothing else reminds you of it: the status read
   never says that a
   cntlr which is not the primary is disabled.

Steps that can lose data: a disable on a pool with no enabled standby whose
`err_epoch` is 0, such as a pool of one cntlr. No failover follows, the
pool's IO stops at once, and the cdc lists no cntlr of the pool, so a host
that follows the discovery log drops its last path: the device goes, writes
not yet flushed are lost, and a Kubernetes mount does not survive it ([A
Kubernetes mount that lost its last
path](#a-kubernetes-mount-that-lost-its-last-path)). The worker's log then
shows `reaction skipped` with the reason `no candidate` at each pass. Enable
the cntlr again at once if this happened. The same loss follows an enable
of the old cntlr while the hosts cannot reach it, once another cntlr has
taken the role, as step 3 says: the worker can give the role back to it,
and the cdc then stops listing the new primary.

When to stop: `dnvctl sp get` shows the primary with `err_epoch` set, or
`dnvctl cluster get-status` lists lines for the pool: dnv sees a fault, and
[Read what is wrong](#read-what-is-wrong) leads on. Stop too when the hosts
reach no cntlr of the pool, a fault of the whole host network, or when step
3 or step 4 does not end as it says. Ask for help.

Tested: the worker suite, `integtest/worker_test.sh`, reaction case, on
fake agents: a disable of a primary with no health epoch, and no other
change, makes the enabled standby the primary within five seconds at a
check interval of one second. Neither cntlr has a health epoch then, so no
threshold was waited out. The suite writes that disable straight to etcd,
not through the gateway. The same case checks that a disabled standby is
not replaced. The e2e suite's copy case disables the primary of a
pool on the lab's real agents while a host writes, waits for the standby to take
the role, enables the old cntlr again, and checks that every write of the
host passed, that its data reads back, and that the disable made the one
failover and the enable none. It passed in the lab on 2026-11-21. Its host
holds the paths to both cntlrs, which the suite made, and runs no nvme-stas.
The e2e suite also disables a standby on the lab's agents: its ops case
disables a third cntlr it has just made and waits for the cdc to stop
listing it, and skips the third cntlr's whole part when every controller
node guest already holds a cntlr of the pool; and each cycle of its soak
case, a long run, disables a standby and enables it again with no
reaction of the worker. A unit test of the worker checks that a disabled cntlr is never
made the primary, and that a failover passes over a disabled cntlr with a
smaller id and takes the next one. Not tested: the fault itself, a host cut
off from a primary that the worker reaches, a host that follows the
discovery log, nvme-stas or the node plugin, through these steps, a
failover past a disabled standby in any suite, and the check of step 6.
