# Operations

This document holds the procedures an operator follows by hand. The words
are those of the [core glossary](core_glossary.md). [The scope
page](scope.md) says what dnv promises and which limits each procedure
works around. The first procedures are the forced-start guide: how to bring
back a group whose legs all failed.

A procedure has these parts, each where it applies: the symptom, or when to
use the procedure; how to confirm it; what dnv does alone and how long it
waits; the safe steps; the steps that can lose data; and when to stop and
ask for help. Each one says which test checked it, or that no test did.

The commands are `dnvctl` commands. Each one needs `--gateway-address`,
`--cluster` and `--sp`, which are left out below. A command that changes
the pool can also take `--rev`, the pool's revision. `dnvctl sp get`
prints it as `sp_rev.revision`. With `--rev`, the gateway refuses the
change if the pool changed since you read it. Most writes move the
revision, so read it again before the next write.

`dnvctl sp get` also lists the pool's cntlrs. The one marked primary is the
group's primary. `dnvctl cntlr inspect --id <cntlr id>` shows its rows.

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
- A forced start gives the array a new **identity**. A leg from before it
  keeps the old identity. Change counts can be compared only between legs
  of the same identity.
- The **force flag** of a mark lets the start go on past three refusals:
  a named leg whose change count is below that of a leg it wipes, a named
  leg that carries a bad-block list, which the start drops, and a named
  leg that a repair took back after md wrote onto it. One flag passes all
  three, so a flag given for one passes the others too.
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
bad-block list. Or it says why the start would refuse, in the same words as
the real start. Add `--force` to see what a start with the force flag would
do. The dry run stops at the first refusal it meets, so it shows one refusal
at most. Of the three refusals the force flag passes, it meets first a leg a
repair took back after md wrote onto it, then a bad-block list, then a named
leg whose change count is below that of a leg it wipes; with `--force` it
shows none of them. So before you add `--force` for one refusal, compare by
hand in `dnvctl group inspect` the change counts of the named leg and of
every leg it wipes (rule 2 below). The dry run runs nothing.

Tested: the cn agent suite, `integtest/cnagent_test.sh`, repair case,
stage `forced`. It runs the primary's read and dry run before a real
start, and checks that the real start does what the dry run said. The
dnvctl suite, `integtest/dnvctl_test.sh`, checks the command against a
stand-in gateway. No suite runs `dnvctl group inspect` against a real
gateway.

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

For a forced start, the suites step the level down before a leg dies.
Stepping it down after, as here, is tested only for "A broken array that
comes back by itself".

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
4. A named leg with a bad-block list is refused unless the mark carries
   the force flag. The start drops the list, and md then serves the listed
   regions from what the leg holds there. That is right for the leg md
   copied from, whose list the array's stop wrote, and wrong for the leg it
   copied onto. `dnvctl group inspect` shows each leg's list length.
5. A leg of another identity is from before an earlier forced start.
   Nothing compares it with the others: a mark naming it wipes the legs of
   the current identity, with no refusal and no warning in the dry run.
   Compare it by hand.
6. The named leg may be dead when you write the mark. The primary then
   waits for it and starts from it when it comes back.
7. The start waits for every other leg it would wipe. A leg that will
   never come back blocks it: see "A forced start that waits".

Safe steps:

1. Do the steps "Before every forced start".
2. Run the dry run from the leg you chose, without `--force`. When it
   refuses the named leg for its bad-block list, or as older than a leg it
   wipes, and you choose to go on (rules 2 to 4), first compare by hand the
   change counts of the named leg and of every leg it would wipe, since the
   dry run shows only the first refusal it meets. Then run it again with
   `--force` and read what it would wipe and drop.
3. Write the mark:
   `dnvctl group force-start --grp <group id> --leg <leg id>`, with
   `--force` only when step 2 needed it.
   The gateway refuses it while the primary reads the group's array
   running. It also refuses it while the primary's report has no row for
   the group, or the primary does not answer. Then try again after a few
   seconds.
4. Wait until the group's row reads ok and the mark is gone. The worker
   clears the mark once the primary reports that it acted on it, and that
   the array runs with a member in sync on a leg whose row reads ok.
5. Read the group again. md is copying onto the other legs. Wait until
   every leg is in sync. Check that no leg shows a bad-block list. If the
   named leg's list is still there after a start with the force flag, the
   start's second run, which drops it, did not happen, and hosts get IO
   errors over the list's regions. No procedure for this is tested: stop.
   No such case was seen in the lab.
6. Step the level back:
   `dnvctl sp set-level --level READWRITE`, or the level the pool had.
7. Write to a volume from a host. A thin pool that comes back read-only
   has no repair procedure yet (the scope page, section 6.1): stop.

Steps that can lose data:

- Every forced start wipes the other legs the group keeps, the new legs
  of its open repair included. Whatever only they held is lost.
- A mark naming a leg of another identity wipes the legs of the current
  identity with no refusal: their newer data is lost.
- The force flag (`--force`) starts the array from a leg older than a leg
  it wipes. That leg serves and holds newer data, and you choose to give
  that data up. Use it only when the newer data is known to be bad.
- The force flag also drops the named leg's bad-block list. If two legs have
  the same change count and you force the wrong one, hosts read the regions
  md never copied as whatever that leg's disk held, with no error. A flag
  given for the list also passes an older named leg in the same start, and
  the dry run without the flag shows only the list's refusal then. So
  compare the change counts by hand first (rule 2).
- A start from an older copy restores this group alone. The pool's other
  groups keep their newer state, so the pool's volumes can mix old and new
  blocks. A block first written since that copy can read another pool's
  bytes. Check the volumes from the hosts.

When to stop: no leg of the array's identity can come back; the dry run
refuses every leg; or two legs have the same change count and you cannot
tell which one md copied from.

Tested: the cn agent suite, repair case, stages `refuse` (a half-copied
leg refused, nothing wiped), `forced` (a start from the leg that kept the
data), `older` and `olderstart` (an older leg refused, then started with
the force flag), and `badblocks` and `badstart` (the named leg's
bad-block list refused with no force flag, nothing wiped, then dropped
with it). The end-to-end suite, `integtest/e2e_test.sh`, react case: its
first forced start, which reads the named leg's list length first and,
when the leg has a list, checks the refusal and then forces.

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
   still dead. The primary stops the broken array and waits for that leg.
3. Bring the leg back. The primary starts the array from it and wipes the
   half-copied leg. md copies onto it again.
4. Continue with steps 4 to 7 of "Which leg to choose".

Why this order: if the leg comes back before the primary stops the array,
md can end its copy by marking the half-copied leg in sync. md then writes
bad-block lists on both legs, and you can no longer tell the two legs
apart.

Steps that can lose data: if md has already marked the half-copied leg in
sync, a mark naming it is refused for its bad-block list only while it
carries no force flag. With the flag, hosts read the regions md never
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
   it, the gateway refuses the mark unless you add `--force`. Such a leg is
   no longer the copy it was: stop instead. If the dry run refuses the leg
   for its bad-block list, read rule 4 of "Which leg to choose". A retired
   leg can carry such a list from its time in the array, for example when
   md recorded bad blocks on it before a repair replaced it. The force flag
   drops the list, and md then serves those regions from what the leg holds
   there. Add `--force` to the dry run and to the mark only if you accept
   that; otherwise stop.
6. Once the array runs and `dnvctl repair get --name <name>` shows the
   repair started, finish it: `dnvctl repair finish --name <name>`. The
   new leg then takes the bad leg's place in the list.
7. Wait until every leg is in sync, then step the level back and write
   from a host, as in "Which leg to choose".

Steps that can lose data: this is a start from an older copy. Everything
written to the group since the leg retired is lost, while the pool's other
groups keep their newer state. Blocks first written since can read another
pool's bytes.

When to stop: as in steps 2, 4 and 5, the refusal for a bad-block list
included when you do not accept what the force flag does.

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

Steps that can lose data: the assembly loses nothing; mdadm starts the
array from the newest legs, and md copies the rest. But the failure itself
can: a data group that broke while the slice's metadata group took writes
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
  then stops the array and acts on the mark. Not tested: the suites write
  every mark at that level.
- The array runs. The mark was written over a running array, or the
  primary's agent restarted after the start and forgot that it acted on
  the mark. Cancel the mark at once:
  `dnvctl group force-start --grp <group id> --cancel`. Left standing, the
  mark acts at the array's next stop, from a leg that may be stale by
  then. The worker also makes no leg repair of the group while it stands.
  Not tested.
- A leg the start would wipe does not serve. The start waits for it. Bring the
  leg back. If it will never come back, replace it with a repair:
  `dnvctl repair create --name <name> --grp <group id> --bad-legs <leg
  id>`. The start does not wait for an open repair's bad leg, but it waits
  until the repair's new leg serves. The group must have no other open
  repair. The repair is also refused while the group holds two retired
  legs, and every mark keeps them; deleting one may delete the copy you
  need, so stop instead. Not tested.
- A start from a repair's new leg waits the same way for the leg of the
  list that is not the repair's bad leg.

Steps that can lose data: a mark left standing over a running array (see
above).

When to stop: the mark stays after a cancel, or the array runs and the
row still reads error.

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

Symptom: after a forced start from one leg, a leg that kept the old
identity, often a repair's bad leg, is never added back. After the next
failover or restart the group can stay down, its row naming two
identities.

Confirm: `dnvctl cntlr inspect --id <primary's cntlr id>` shows the
refusal in the primary's report for the group, and the leg's row there
carries the reason `RES_REASON_OTHER_IDENTITY` while the array runs and
the row reads `RES_STATUS_OK`; once
the group is down with two identities, each leg of either identity carries
`RES_REASON_IDENTITY_UNDECIDED`, since dnv cannot tell which identity is
the current one. `dnvctl group inspect --grp <group id>` shows the two
identities.

What dnv does alone: the primary leaves that leg out, since it may hold
data the array lacks, and names it by that reason. A repair that replaces
such a leg, with no health epoch on it, is decided and not yet built:
while the array runs, the running array tells which leg is the old one;
once the group is down, that repair will ask for the force flag, since a
wrong choice serves older data and loses the writes made since.

Safe steps: none is tested. While the array runs, the gateway refuses a
mark, and a repair whose bad leg is that leg is refused while the leg
reads healthy, which it does. Stop and ask for help.

Steps that can lose data: a forced start from the leg you want wipes the
other leg and what only it holds.

When to stop: always, for now.

Tested: no. The cn agent suite's stage `takeback` checks a related
refusal: a repair that takes back a retired leg of another identity.

### A group whose every leg was wiped

Symptom: a forced start stopped after it wiped the other legs: its mark
was withdrawn, or the named leg was lost. The group stays down. Its row
says that the array has run before and no member carries a superblock,
when every leg serves; or that only some legs are available and none
carries a superblock, when the named leg is lost.

Confirm: `dnvctl group inspect --grp <group id>` shows no superblock on any
leg that serves.

What dnv does alone: once the worker has recorded that the group's array
ran, the primary makes no new array over legs that hold data with no
superblock. The scope page, section 5.2, lists the rare case where the
worker has not recorded it yet.

Safe steps: bring back the leg the mark named. A new mark can name it.

Steps that can lose data: if that leg will never come back, the data on
the wiped legs can be reached only by hand with mdadm, by someone who
knows md's layout.

When to stop: the named leg will never come back.

Tested: the cn agent suite, repair case, stage `made`: no array over wiped
legs.

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
the end-to-end suite, react case, keeps and clears it around its two
forced starts.

### Taking the group's repairs over

Symptom: you want to run repairs of a group by hand.

Confirm: `dnvctl sp get` shows the group's flag
(`operator_in_charge`) and its repair.

What dnv does alone: with the operator-in-charge flag set, the worker
creates no repair of the group.
The primary leaves a broken array of the group as it is. Failover and the
other reactions go on. The worker still drives a repair of its own that
exists.

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
the end-to-end suite check the flag. That the primary leaves a broken array
alone under the flag is checked only by the agent's unit tests.

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
   host into a volume of a new pool, and then delete the old pool. A
   clone out of the old pool is refused when the clone's dm-clone device,
   on the new pool's primary, would tell its hosts a best IO size over
   4 MiB. The refusal names both pools' stripe times slice count.

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
at once, the workers first ([the scope page](scope.md), Upgrade).

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
2. Have the command that starts the agent ready before you stop it, so
   that the stop lasts only as long as the restart itself.
3. If the agent of a controller node that holds a primary may stay stopped
   longer than step 1 allows, move each primary off the node first. For
   each cntlr on the node that `dnvctl sp get` shows as primary, first
   check that `dnvctl sp get` shows another cntlr of the pool with
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
   `err_epoch` 0, and then disable the new primary with the same command
   as above, so that the worker moves the role back at once and the pool
   gets its IO back. Then ask for help. If the pool has no enabled standby
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
   `--local-store` as before.
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
it well inside that time.

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
`dnvctl sp inspect-side`; then `dnvctl migr finish`. The scope page lists
the whole migration flow as a procedure still to write; the introduction
explains it. When the disk node's disk is the cause, move every side it
holds the same way, and then fix or replace the disk.

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

## Install the udev rules on a node

Words: **udev** is the Linux service that acts when a block device
appears or changes. Its rules read the device and can, for example,
assemble an md array from it, scan it for LVM, or register it as a bcache
device. dnv ships three rule files in the folder `deploy/udev/` of the
release:

- `58-dnv-dm.rules` keeps the system's rules off every device mapper device
  of dnv, whose name starts with `dnv-`. Such a device holds what hosts
  wrote, and the node must never read that as a file system, an LVM volume,
  an md array or a cache. The device keeps its name under `/dev/mapper`,
  which dnv uses. Some versions of systemd show such a device as not ready.
  That does no harm: dnv mounts nothing on it and waits for no systemd
  device unit.
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
agent starts, and again when a new release changes a file of
`deploy/udev/`. The rules need systemd 253 or newer; `systemctl --version`
prints it. An older udev skips the lines of `63-zz-dnv-md-array.rules`
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

2. Check that udev takes the files:

   ```
   sudo udevadm verify /etc/udev/rules.d/58-dnv-dm.rules /etc/udev/rules.d/63-dnv-md.rules /etc/udev/rules.d/63-zz-dnv-md-array.rules
   ```

   It must report every file as a success.
3. lvm2. In the `devices` section of `/etc/lvm/lvm.conf`, set a global
   filter that accepts the node's own physical volumes and rejects every
   other device. For a node whose only physical volume is `/dev/vda3`:
   `global_filter = [ "a|^/dev/vda3$|", "r|.*|" ]`. For a node with none:
   `global_filter = [ "r|.*|" ]`. lvm2's devices file, which lists the
   devices LVM may use, does the same job. Then `sudo pvs` must list the
   node's own physical volumes and nothing else.
4. md. `sudo mdadm --detail --scan` prints one ARRAY line for each md
   array that runs on the node; before the node's first dnv agent starts,
   each one is the node's own. Add those lines to `/etc/mdadm/mdadm.conf`,
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
3. Fix every `FAIL` before the node joins: the agent cannot work there, or
   a failover or a repair can lose data there. A `WARN` is one of five
   kinds.
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
   - A known limit. The `disk best IO size` warning says the disk's best
     or minimum IO size is above 4 MiB, so a controller node's write to
     the disk can be refused
     ([the scope page](scope.md#10-sizes-and-performance)).
     Use another disk, or give it a smaller stripe, or accept the limit.
   - A feature nothing uses yet. The `nvmet host cut (debugfs)` warning
     says the node could not cut a removed host's connection; nothing in
     dnv cuts one today, so it can stay.
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

What dnv does alone: nothing. An agent starts whatever the check would
report. It logs the kernel release and, on a controller node, the mdadm
version once when it starts, as the record `agent node versions`.

When to stop: the check exits 2 as root, or the md replay names something
it could not remove. Ask for help; remove what the replay names by hand
with `mdadm --stop`, `losetup -d` and `rm -rf`.

Tested: unit tests check each line of the report at each level on modelled
nodes, the exit statuses, and the md replay against a stand-in mdadm,
including one that starts a lone leg. The e2e suite runs the check on
every disk node and controller node guest before it creates its pool, and
fails the run on a failure; that stage is written and has not yet run in
the lab. The cn suite's nodecheck case runs the check, the md replay, and
the md replay with an mdadm that starts a lone leg on the lab VMs, and it
passes there.

## Gather a node's state for a bug report

Words: the **gather** is `dnv-agent collect`, a run of the agent program
that writes one archive of the node's state for a bug report. Some reads
can wait in the kernel without end on a node with a stalled device, such
as a read of /proc/mdstat or of a dm device's status; the gather runs each
of those apart, under one time limit, and names in the archive each one
that did not finish.

When: a node or a pool on it behaves wrongly and you want to report it,
or to keep the node's state before you change anything.

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
