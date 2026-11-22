# tutorial_integtest.md — the tutorial suite (`integtest/tutorial_test.sh`)

This document owns the tutorial suite: what it proves, the markers it reads
in the two tutorials, the guests it runs on, how it puts them back and what
it does not prove. The tutorials themselves are human docs,
[`tutorial_all_in_one.md`](../human/tutorial_all_in_one.md) and
[`tutorial_csi.md`](../human/tutorial_csi.md); the release archive it
unpacks is the `Makefile` row of `layout.md`, Directory tree; the suite
builds it into a folder of its own on the suite's machine, which it
removes at its end, never into the tree's `_out/`.

## Purpose

The suite proves that a release archive, unpacked on a lab guest, runs the
tutorials as written: every shell block of a tutorial, in the order the
tutorial gives them, from the commands a reader copies, with the output the
tutorial's text says each check prints. It runs before each tag, so that a
change to a program, a flag, a default or a manifest that a tutorial's
command no longer matches fails here and not in a reader's terminal; a
tutorial that changes is checked by the same run. It proves nothing about
dnv beyond what the tutorials show: the other suites own the behaviour.

A block runs as its own file under `bash -e`, as the plain user, in the
unpacked release folder, with no input, so that any command of it that
fails fails the block, where a reader would see the error. A block whose
command a reader repeats until a stated output ("repeat until", "run it
again") runs again until its checks hold, within the time its marker gives.

A crash fails the run: after each block the suite reads every log under its
work folder on the guest and, in the CSI part, the log of every container of
the driver's pods, with the log of its previous run when it restarted, and a
line that starts as Go's report of a crash starts, `panic: ` or
`fatal error: `, fails the run with the log and the line, while a record that
holds those words inside a field never matches. The read follows each block,
not the run's end, as the tutorial's teardown removes its programs' logs and
pods; each read of the containers logs how many logs it read, and a CSI part
in which no read found one fails. The preflight proves the read on a sample
log it plants on the guest, and a failed run prints every crash line first. A
crash while the suite's own cleanup stops a program is not read, as the read
has ended by then.

## The markers

A tutorial carries, beside each block that needs one, an HTML comment that
the suite reads and a reader's markdown viewer hides: it starts with
`<!-- test`, ends with `-->` on the line just before the block's opening
fence, and holds one directive per line. A marker travels with its block, so
a block added or moved keeps its checks; a block index would bind silently
to the wrong block. A marker that binds to no block, a directive the suite
does not know, or a fence left open fails the parse before anything runs.

The directives: `wait SECS` runs the block again, a second apart, until it
exits zero and its checks hold, for at most that long; `timeout SECS` bounds
one run of the block; `out TEXT`, given once per line, is the block's whole
output, line for line; `line TEXT` is one line of the output, whole;
`has TEXT` and `lacks TEXT` are text the output holds or does not hold;
`word TEXT` is text the output holds as a whole word, so that `Ready` is not
read in `NotReady`; `count N TEXT` says that exactly N lines of the output
hold the text, as two pods each `Running`; `same-first-word` says that the
output has two lines or more and that every one starts with the same word,
as two checksums of one data do; `save NAME` keeps the first word of the
output's last line, and `same NAME` in a later block wants it again, as a
sum read before and after a step; `skip REASON` keeps the block from
running; `omit TEXT` runs the block without its line that starts with the
text and the lines that line continues to; `replaces TEXT` marks a block
that one tutorial runs in place of a command of another (Topology, The CSI
part); `run` is a marker with no check, for a block of the CSI tutorial
that needs none (below). Every check reads the block's standard output; its
error output goes to the transcript only. A directive is a check the
tutorial's own text states; a marker never adds a check the text does not
make, so that the tutorial stays the source of what a reader sees; where the
text states a check for every line, as "`closed` for every address", the
block's marker reads every line, so that one wrong line fails it.

**The plan.** Before any block runs, the suite checks every block the run
will run, and refuses the run on the first fault: an `omit` that takes no
line of its block, or more than one; a CSI tutorial with no block marked
`replaces`, or more than one, and a `replaces` that takes no line of the
prelude, or more than one; a last section of the prelude the tutorial does
not have; a block of the CSI tutorial with no marker, since in the CSI part
a block runs only when its marker lets it; and a block whose text, its omits
applied, still names k3s or rancher, in any case, once every `sudo k3s
kubectl ` and `sudo k3s ctr ` is taken out of it, whatever its marker says.
Those two are the uses the tutorials have of the lab's k3s, so the guard
holds by the shape of a use and fails closed: a k3s install, uninstall or
stop in any form, a removal of rancher's folders, or a `k3s server` is
refused without being named in a list. Each block is checked once more for
the last of these just before it runs. Reason: on the lab the CSI part runs on
the lab's k3s, which `csi.md` CSI1 says the csi suite installs only on a
guest without it and never removes, so a change of a tutorial's text that a
marker no longer matches must stop the run, not run the command.

## Topology

**The all-in-one part** runs on one lab guest that holds no dnv state: dn2,
whose suites leave nothing behind when they pass. "Fresh" is a cleaned lab
guest, not a new image: the spare VM is never touched, and a new image per
run would cost a VM and its disk for what preflight and cleanup give (the
guest checked empty of dnv first, and put back after).

**The CSI part** runs on the lab's k3s, on its server node, the control-plane
guest, since the lab's k3s stays installed between runs (`csi.md` CSI1) and
a second k3s cannot run beside it on one guest. The CSI tutorial is written
for one machine that is both the k3s node and the dnv machine; the lab's k3s
has three nodes. So for the run the suite puts a taint of its own on the other
nodes, which none of the tutorial's pods tolerates, and lifts the server's own
control-plane taint, so that every pod of the tutorial lands on the server
node, where the image was imported and the cdc listens on 127.0.0.1; it puts
both taints back at cleanup. The CSI part first runs the prelude its tutorial
asks for: the all-in-one tutorial from its start through "Cluster and nodes",
with the CSI tutorial's gateway start, the block marked `replaces`, in place
of the all-in-one's, which the transcript says where that block stands. Its
k3s install block is skipped, and the line of its teardown that uninstalls k3s
is omitted, since the k3s is the lab's; the plan (The markers) refuses the run
if either guard no longer holds.

## Preflight

On the driver: the tools the run calls, and the parse of both tutorials. On
the guest: passwordless sudo; no dnv device-mapper device, md array, nvmet
object, NVMe-oF controller, dnv or etcd process or loop device of the
suite's folder; the stock nvmet, since the tutorials are written for a
stock kernel, whose nvmet cannot end one nvmet connection and leave the
others; every port the tutorial listens on free, the CSI driver's node
plugin's health port among them in the CSI part; and every
package a tutorial installs either absent or at its candidate version, so
that the tutorial's install never upgrades a tool the other suites run with;
the CSI part lets it upgrade one on the k3s server node, where no other
suite runs a node's tools, and says so. A read of the guest that fails, a
failed listing of its devices, arrays, loop devices or processes and a failed
read of a package or its candidate included, is a failed preflight, never a
guest that holds nothing. nvme-stas may be installed and running: a lab guest
runs its two daemons idle, since the package starts them at install, and the
suite saves their state and puts it back (Cleanup), where a refusal would need
them stopped by hand before every run. In the CSI part, the lab's k3s answers
on the guest and holds none of what the tutorial makes or a run leaves: an
object of the suite's list (Cleanup), a volume or an attachment of the
driver's, a node with the suite's taint, the server without its own, the
driver's image in the server's store.

## Cleanup

The suite saves, before the first block, what a tutorial changes and its
teardown may not put back: the installed packages, the udev rule files, the
mask of nvme-cli's connector units, nvme-stas's files and the state of its
two daemons, which a lab guest may run idle since the package starts them
at install, the policy of iptables' forward chain and the network links,
since docker's install changes the one and adds a bridge to the other, and
neither goes with its package, which of docker's and containerd's state
folders and the docker group exist, and the images of k3s's store.

**After the teardown.** When the last block has run, the suite reads the
guest before its own cleanup touches it, and fails the run on anything the
tutorial's teardown left that the tutorial says it removes: a dnv device or
md array, an nvmet port, subsystem or host, an NVMe-oF controller, a dnv or
etcd program, a loop device of the folder, a udev rule file or a connector
unit's mask that differs from before the run, and, for the all-in-one
tutorial, whose teardown puts nvme-stas's files back and leaves its daemons
stopped, a file of `/etc/stas` that differs from before the run or a daemon
that runs, and on a listing or a daemon's state it could not read. Reason: the
cleanup below removes all of these, so a read after it cannot tell a teardown
that works from one that does not.

**Cleanup.** At the end, passed or failed, the suite takes down what the run
left in the order a holder goes first: the worker, then the agents, then
the gateway, the cdc and the run's etcd, so that nothing rebuilds what
follows or connects a leg again; nvme-stas, when the save is complete; the
nvmet objects, whose enabled namespaces hold the disk nodes' sides and the
controller node's namespace devices open, so that no device below them can go
before them; the host's NVMe-oF controllers; the dnv devices and the md arrays
in rounds, each round taking what the one before freed, until none is left;
the loop devices; and then a read of what stays. It then restores what a
complete save kept, purges every package the run added, deletes docker's
bridge, the state folders and the group the run's packages left, and the
images the run added to k3s's store. In the CSI part it first deletes from the
lab's k3s every object the tutorial makes, by kind and name from the suite's
own list and never through the release's manifests, since a run with
`--cleanup-only` has no release folder and the lab's k3s is permanent, so
that nothing outside the list goes; setup checks, before the first block,
that the release's manifests hold the list's objects that name a manifest,
and no other. It waits
for the driver's volumes and attachments to go before the driver does, since
without the driver a volume or an attachment keeps its finalizer, deletes
the image it imported, puts the taints back, and reads every object of the
list and the lab's k3s back as preflight reads it. The save
fails, and so the run, when any of its readings fails, and a reading it could
not keep is never written; a file is recorded as there before the run only
once its copy reads back the same as the original, and the udev rules only as
a whole. The restore acts only on what the save recorded: it deletes nothing,
changes no daemon and deletes no group by a reading it could not make,
never removes an original it has no saved copy of, and reports the step;
it then reads back every reading the save kept, nvme-stas's files and the
udev rules, and reports each difference, but for the network links, where
it reports only a docker link the run added that is still there, and
nothing when their read fails. The restore runs only after a
complete save, which the save records last, once every other record is
written, read back and on disk, and puts on disk before any block runs: a save
that did not complete runs no block, so it leaves nothing to put back, and its
records go with the work folder; a read of what the save kept that fails is a
failed step, and the restore does not run. Any step that fails and anything
left, on the guest or in the lab's k3s, fails the run, and a restore that
failed or did not run for a read that failed leaves the saved state on the
guest, which the next run's preflight refuses until a run with
`--cleanup-only` has put the guest back. A failed run, and every run with
`--cleanup-only`, which runs no block, keeps its programs' logs under the
work folder for the runner to copy, and the next run's setup removes them,
once its preflight has passed and before its first block, so that its crash
scan reads its own logs alone; a passed run leaves nothing. A run that
SIGTERM, SIGINT or SIGHUP stops before its exit handler exits non-zero and
logs no PASS; the handler's removals are best effort, as a signal while they
run, a dead terminal or tee or a full disk can end them early; after a stopped
or failed run, run the suite's `--cleanup-only`, as every lab chain does
(`e2e_integtest.md` E2E6). That cleanup is what lets the suite run on a guest
that other suites use: a run's preflight proved the guest held none of what
cleanup removes, and a run with `--cleanup-only`, which runs no preflight,
acts only on a guest that holds the suite's work folder, which every run of
the suite makes before it changes the guest, and changes nothing on any other.

## Known limits

- The guest is a cleaned lab guest, not a fresh machine (Topology): a
  package, a module or a setting that the lab image carries and a fresh
  Ubuntu lacks is not caught. The kernel modules the tutorial loads stay
  loaded, as the tutorial says.
- The CSI part runs on a three-node k3s made to schedule on one node, with
  the k3s install skipped, so the install command and the one-node cluster
  it makes are not checked.
- The checks read what a tutorial's text states, no more: a check the text
  makes in words alone ("a line with the machine's hostname") is read only
  as far as a fixed text can say it.
- A release built in a git worktree carries the commit of the checkout
  whose folder holds the worktree, or no version stamp when none does (the
  `Makefile` row of `layout.md`, Directory tree), and the tutorials read no
  version, so the suite does not catch a wrong or missing stamp.
- docker's purge on the k3s server node leaves the chains docker added to
  iptables and its rules in them until the next boot; the suite puts back
  the forward chain's policy alone, since a restore of every rule would
  also undo the rules k3s keeps there.
