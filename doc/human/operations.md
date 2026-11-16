# Operations

This document holds the procedures an operator follows by hand. The words
are those of the [core glossary](core_glossary.md). [The scope
page](scope.md) says what dnv promises and which limits each procedure
works around. The first procedures are the forced-start guide: how to bring
back a group whose legs all failed.

Every procedure has the same parts: the symptom, how to confirm it, what
dnv does alone and how long it waits, the safe steps, the steps that can
lose data, and when to stop and ask for help. Each one says which test
checked it, or that no test did.

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
- The **force flag** of a mark lets the start go on from a leg whose
  change count is below that of a leg it wipes.
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
the start would wipe, and whether it would stop the array first. Or it
says why the start would refuse, in the same words as the real start. Add
`--force` to see what a start with the force flag would do. The dry run
runs nothing.

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
4. A leg of another identity is from before an earlier forced start.
   Nothing compares it with the others: a mark naming it wipes the legs of
   the current identity, with no refusal and no warning in the dry run.
   Compare it by hand.
5. The named leg may be dead when you write the mark. The primary then
   waits for it and starts from it when it comes back.
6. The start waits for every other leg it would wipe. A leg that will
   never come back blocks it: see "A forced start that waits".

Safe steps:

1. Do the steps "Before every forced start".
2. Run the dry run from the leg you chose.
3. Write the mark:
   `dnvctl group force-start --grp <group id> --leg <leg id>`.
   The gateway refuses it while the primary reads the group's array
   running. It also refuses it while the primary's report has no row for
   the group, or the primary does not answer. Then try again after a few
   seconds.
4. Wait until the group's row reads ok and the mark is gone. The worker
   clears the mark once the primary reports that it acted on it, and that
   the array runs with a member in sync on a leg whose row reads ok.
5. Read the group again. md is copying onto the other legs. Wait until
   every leg is in sync. Check that no leg shows a bad-block list. If the
   named leg's list is still there, the start's second run, which drops
   it, did not happen, and hosts get IO errors over the list's regions.
   No procedure for this is tested: stop. No such case was seen in the
   lab.
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
- If two legs have the same change count and you name the wrong one,
  hosts read the regions md never copied as whatever that leg's disk held,
  with no error.
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
bad-block list dropped). The end-to-end suite, `integtest/e2e_test.sh`,
react case: its first forced start.

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
sync, a mark naming it is not refused. Hosts then read the regions md never
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
   `dnvctl group force-start --grp <group id> --leg <retired leg id>`.
   If md once wrote onto that leg, as a repair that was cancelled can leave
   it, the gateway refuses the mark unless you add `--force`. Such a leg is
   no longer the copy it was: stop instead.
6. Once the array runs and `dnvctl repair get --name <name>` shows the
   repair started, finish it: `dnvctl repair finish --name <name>`. The
   new leg then takes the bad leg's place in the list.
7. Wait until every leg is in sync, then step the level back and write
   from a host, as in "Which leg to choose".

Steps that can lose data: this is a start from an older copy. Everything
written to the group since the leg retired is lost, while the pool's other
groups keep their newer state. Blocks first written since can read another
pool's bytes.

When to stop: as in steps 2, 4 and 5.

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
refusal in the primary's report for the group;
`dnvctl group inspect --grp <group id>` shows the two identities.

What dnv does alone: the primary leaves that leg out, since it may hold
data the array lacks.

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
