# Glossary

The vocabulary of dnv in alphabetical order, one entry per term, with synonyms sharing an entry and a code identifier named in backticks where it differs from the prose term.

**activation sweep** — The pass that lists the thin ids a slice's pool holds through a thin-metadata snapshot and deletes every id no thin device of the cntlr's request owns: creating the pool device arms it, and the first converge whose request came through the revision-gated syncup runs it (see `cnagent.md` CN14). It heals the ids a delete message never reached, as when a demotion, a level change or a failover raced the delete.

**AEN, discovery-log-change AEN** — The asynchronous event the cdc completes on a host's armed asynchronous event request when that host's rendered discovery log changes. A change that moves a host's view marks it dirty and sets a pending bit on each of the host's connections; several impacts before delivery coalesce into one event.

**age gate** — The dn agent's rule that a namespace-less side export linked to no port but its own, which no held side claims, is removed only once its configfs directory's modification time is older than the orphan grace (`DnExportOrphanGrace`). A sweep never looks up the export's attributes, which on some kernels refreshes that time, so a younger one, possibly a sibling agent's build in flight, is left alone (see `dnagent.md` DN6).

**agent** — Either role of `dnv-agent`: the process on a disk node or a controller node that converges the local device-mapper, md, nvmet and nvme-host state to the desired state it is sent, persists the last accepted request per object, and reports live state. An agent never talks to etcd.

**allowed hosts** — The host NQNs a subsystem admits (`allowed_hosts`), its only admission gate, by one rule for host-facing and dnv-internal subsystems alike: a subsystem admits exactly the hosts its list names, and a host-facing subsystem's cdc entry is shown to exactly those hosts. An empty list therefore admits no host and shows the entry to no host, which is how a subsystem is staged before its hosts are granted, or closed to new connections by revoking every host (see `architecture.md`, Primary cntlr, and `cdc.md` DS4).

**ANA groups, fixed ANA groups** — The groups every agent's nvmet port carries with fixed ids and states, optimized, non-optimized and inaccessible, written once at port setup (`AnaGrpIdOptimized`, `AnaGrpIdNonOptimized`, `AnaGrpIdInaccessible`). Every ANA transition moves a namespace by rewriting its "ana_grpid"; a group keeps its fixed state, which the port converge only restores.

**anti-affinity, tier 1, tier 2** — The allocator's rule that one scan keeps at most one candidate per location, so the legs one allocation round places never share a failure domain. An operation that adds to existing placements excludes their locations at tier 1 and rescans without the exclusion at tier 2 when tier 1 yields fewer candidates than it must place, so the new placement may share a domain already held.

**applied set** — The bitmap chunks an agent reports as held for a migration or a clone, derived from the chunk files present in its local store (`bm_idx_list` for a migration, `chunk_id_list` for a clone). The worker pushes the chunks etcd holds that the applied set lacks.

**attribution** — The sweep's test of which agent, cluster, node and storage pool a kernel object belongs to, by its parsed dm name or NQN, or for a nameless object by what it is built from. An object that is another agent's is foreign and is never touched; one nobody claims is unowned.

**auto-grow, thin-pool auto-grow** — The sp worker's reaction that appends a data or meta group to a slice whose pool usage has crossed its low-water mark, a data group by the slice's original allocation and a meta group by the next rung of the meta ladder, the fixed sequence of sizes a slice's meta groups take. One grow of a kind stays pending until the reported pool total reflects it.

**auto_resume** — A clone attribute that makes the destination namespace serve while the clone runs although it is stored suspended. In cross-SP live migration the destination namespace is created suspended and the clone overrides that until it is deleted.

**auto_suspend** — A transfer attribute that parks the origin namespace on every cntlr for as long as the transfer exists, so the transfer is the only reader and writer of the bytes; an auto-resume clone on the same thin device overrides it.

**base state** — The once-per-node state a node syncup ensures before any object and no sweep removes: on a disk node the dnv disk format and the agent's nvmet port, on a controller node the tmpfs, the sparse arena file, its single loop device and the port (see `cnagent.md` CN5). A check round that reads a piece of the controller node's base state absent names it in its verdict, so the worker re-sends the node syncup.

**behavior file** — The JSON file a fake agent or fake gateway re-reads whenever it changes to decide what to report or reply (`behavior.json`): statuses, rows, hangs, dropped streams and forced reply codes. A malformed file leaves the behavior in force unchanged.

**bins, bin ladder** — The free-extent bands disk-node capacity keys are indexed under, defined by the cluster's shift ladder (`DnBinConf`). A scan walks the bins from the smallest that can hold the request, largest free first.

**bitmap chunk** — One stored piece of a skip bitmap. A migration chunk is an immutable append in sequence (`MigrBitmap`, `bm_idx`); a clone chunk is addressed by the pair of source slice and chunk index, self-positioned at a fixed quantum, and may grow in place (`CloneBitmap`, `src_slice_idx`, `bm_idx`).

**bitmap push** — The unary delivery of one chunk to the agent that runs the dm-clone: `PushMigrBitmap` to the destination side's disk node, `PushCloneBitmap` to the primary cntlr's controller node. A push carries no revision and passes no revision gate, refused only for a migration or clone the agent does not know or a clone chunk addressed outside the clone's geometry; the worker keeps one in flight per migration or clone, in ascending address order.

**build phase, sweep phase** — The two halves of a cntlr converge: the sweep phase removes, top-down, what the node holds of the pool that the wanted set does not name, and the build phase then ensures, bottom-up, legs, groups, pools, thin volumes, raid0s, clones, transfers and namespaces, moving namespaces to optimized last.

**candidate** — A disk node or controller node a capacity scan offers for an allocation (`Cand`); the caller picks randomly among the candidates and re-reads each pick's capacity key inside its deciding transaction.

**candidate unit** — One scan-outside-plus-transaction round of an allocating RPC. A pick whose capacity key moved, or a plan the stored state has outgrown since the scan, fails the unit as "candidate changed" and the handler runs it again.

**capacity key** — The index key a node holds exactly while it is allocatable, ordered by free extents (`DnCapacity`, `CnCapacity`). Every transaction that changes an input of the presence rule rewrites it, and the allocator scans nothing else.

**case** — One scenario of an integration suite, run in a fixed order with the run stopping at the first failure. Each case is isolated: in the agent suites by pool ids of its own and a teardown, elsewhere by a wiped store and restarted processes before it starts.

**cdc, central discovery controller** — `dnv-cdc`, the control-plane process that serves the well-known NVMe-oF discovery subsystem over NVMe/TCP from the `CdcEntry` keys it watches, keeps one filtered view per connected host, sends AENs to the hosts a change impacts, and closes a connection that sends no command within its keep-alive timeout. It registers nothing and never writes etcd.

**CdcEntry** — The etcd record per subsystem that the cdc serves: the subsystem NQN, the transport of every enabled cntlr's controller node, and the allowed hosts. The gateway writes it and puts a missing one back when it rewrites it, while the worker's cntlr replacement rewrites the transports only of an entry that exists.

**chain** — The cn sweep's ordered removal list for one sp: the layers, each with P0 ahead of it, built from the pass's enumerations against the sp's wanted set and walked top-down under the stop rule. A node-level pass runs a chain per sp that has left the pointer list and a cntlr-level pass one for its own sp; an enumeration that did not answer stops the pass before any chain runs (see `cnagent.md` CN21).

**check round** — One request-and-reply exchange on an object's check stream, sent by the worker at the interval the cluster's `health_check_conf` sets per object kind and answered with the revision of the last request the agent accepted for the object (see `dnagent.md` SH25), the verdict and, when it changed, the info. The worker re-syncs the object when the reply's revision or code says so.

**check stream** — The long-lived bidirectional stream the owning worker keeps open per disk node, side, controller node and cntlr (`CheckDn`, `CheckSide`, `CheckCn`, `CheckCntlr`). It carries no desired state; it lets the worker read live state each round cheaply instead of polling `Get*Info`.

**chunk store, bitmap-chunk store** — The agent mechanism that persists each received push as its own file before applying it and derives the applied set from the files present (`LocalMigrBmPath`, `LocalCloneBmPath`). A chunk whose file the startup reload did not load stays out of the applied set until a push rewrites it or a later restart loads it.

**claim rule** — The sweep's test for an object whose name carries no side, such as a migration device or export: it goes unless a side the agent holds state for still wants it, read from that side's stored request under the same gate that keeps the object in the wanted set; the claim on a migration's clone-metadata slot carries no level gate (see `dnagent.md` DN6).

**cleanup, debris** — A suite's scrub of what its runs leave behind, run at the start of every run and at the end only on success, each step tolerating absence; the e2e suite also scrubs between cases and stops a run whose scrub did not finish. A failed run leaves its debris, its processes, devices, connections and logs, in place and prints diagnostics instead.

**clone** — The destination side of a copy: a pull of an external NVMe-oF namespace into a local thin device through a dm-clone on the primary cntlr (`Clone`, `CnCloneFinalName`). The destination thin device must be empty when the clone is created.

**clone crash recovery** — The primary's rebuild of a clone whose dm-clone metadata is missing or unusable, whose dm-clone device is absent, or whose dm-clone does not show hydration enabled: park the destination's ns-devs, rebuild the dm-clone with hydration disabled, mark every region the destination pools already map as hydrated from their bitmaps, then enable hydration.

**clone drain** — The sp worker's asynchronous teardown of a clone that `DeleteClone` latched: the chunk keys go in bounded batches, then the clone record together with its name-list entry. The clone's exclusion from every cntlr's request is what tears the devices down.

**clone-metadata arena** — The controller node's sparse tmpfs-backed file on one loop device, carved into units from which each clone's dm-clone metadata slot is allocated first-fit (`CnCloneMetaAreaSize`, `CnCloneMetaUnit`). The kind-`cb` wrapper tables are its allocation registry, and it is volatile together with the kernel's dm state.

**clone-metadata slot, clone-metadata wrapper** — A contiguous run of units holding one dm-clone's metadata, in the disk node's clone-metadata area for a migration destination and in the controller node's arena for a clone, presented as a device by a wrapper dm-linear because dm-clone reads its superblock from sector zero (`DnMigrMetaDmName`, `CnCloneMetaDmName`).

**cluster** — The namespace for everything else, identified by its name and by an id derived from the name and the creation epoch stamped when it is created (`ClusterConf`, `cluster_id`, `creation_epoch`). A cluster deleted and recreated under the same name is a different cluster: every key but its name-keyed conf, and every node-local name but the md superblock name, is new.

**cntlid slot** — One of the fixed partitions of the NVMe controller-id space a subsystem is given through its minimum and maximum controller id, so that the controllers a host or a controller node aggregates never share an id (`cntlid_slot`, `cntlid_slot_list`).

**cntlr** — One instance of a storage pool on one controller node (`Cntlr`); exactly one cntlr of a pool is primary and the others are standby. A cntlr carries its cntlid slot, its disabled flag, its health epoch and its settling flag.

**cntlr replacement** — The sp worker's reaction that deletes a cntlr unhealthy past its threshold and creates one on a fresh controller node with the same cntlid slot, primary only when the old one was primary and had no failover candidate.

**command timeouts, soft timeout, hard timeout** — The two bounds on every OS command an agent runs (`CmdSoftTimeout`, `CmdHardTimeout`): SIGTERM at the soft timeout and SIGKILL at the hard one, after which the command did not answer. Neither bounds a child in an uninterruptible kernel wait, which returns only when the kernel does (see `architecture.md`, Common validation).

**concat** — The multi-target dm-linear that joins a slice's group devices end to end in list order, one over its meta groups (`CnPoolMetaName`) and one over its data groups (`CnPoolDataName`), on which the slice's thin pool sits: a grow appends a target and reloads the pool, and the provisioning deferral truncates a concat to its list's longest leading run of effective groups, never dropping a group out of the middle (see `cnagent.md` CN9, CN13).

**conf gate** — An agent's refusal of a request whose conf carries a value the control plane cannot have written, such as a zero geometry member (`ReplyCodeInvalidConf`). It runs after the revision gate and before the request becomes desired state, so nothing converges and nothing is persisted.

**connect retry** — An agent's background re-converge of an object whose last pass left a reason to try again: on the dn, a migration destination whose build stopped short, at the connect to its source or at another step (see `dnagent.md` DN13); on the cn, a leg or clone source that failed to converge, a failed clone step, a group member that is not yet available, or an ns-dev the build held off its raid0 (see `cnagent.md` CN10). Each attempt is a whole converge that decides afresh whether a reason to retry remains.

**control plane, data plane** — The control plane is etcd plus the stateless processes on the control-plane servers, gateway, worker and cdc; the data plane is the disk nodes, the controller nodes and the NVMe-oF paths between them and the hosts.

**controller node, CN** — A machine that runs the volume logic of the cntlrs it hosts: it contributes no persistent storage, only a tmpfs for clone metadata, and offers a capacity budget in extents from which every cntlr's footprint is debited (`--capacity`). It runs one cn agent, never two on one kernel, and exports its namespaces over that agent's nvmet port.

**converge** — An agent's one pass that brings its node's state for an object to the request it holds: probe-first, bottom-up build, top-down removal by sweep, with every failure captured in a row rather than aborting the pass. A converge is idempotent and crash-restartable.

**coordinator** — The sp role's revision worker for one storage pool: it loads the pool, builds every side and cntlr request, keeps one child goroutine per side and per cntlr, runs the flips and the reaction pass, and holds each fan-out's cntlr requests until the sides report it applied or one cntlr interval has passed.

**created, materialization** — The thin-device flag the sp worker sets once the primary has reported the device's thin volume OK in every slice, and never clears (`ThinDevice.created`). Only a created thin device can be snapshotted, an origin is not deleted while a snapshot of it is uncreated, and no converge sends a created thin device a create or snapshot message again, though one that leaves `td_list` is still sent the delete message (see `cnagent.md` CN14).

**cross-SP live migration** — Moving a volume between storage pools, even across clusters: the destination pool serves a subsystem and namespace identical to the source's, so hosts aggregate both pools' exports into one device and follow the ANA flip, while a transfer exports the source volume over a subsystem of its own and a clone in the destination pool pulls it.

**cutover window, cutover grace window** — The bounded suspension a migration source holds each of its per-CN dm-linears in before reloading it onto its dm-error (`SuspendSeconds`), so IO the old primary still had in flight is absorbed and then failed rather than replayed onto the side's data. The first converge at or after its deadline ends it and tearing down the side or its exports ends it early; it is a floor, and a bound only as far as the reloads succeed (see `dnagent.md` DN12).

**dead threshold** — Twice the vote interval: a registration whose put an observer has not seen for that long is observed dead (see `dnv-worker.md` VW3).

**deciding STM, deciding transaction** — The transaction that commits an RPC's mutation; in a two-phase RPC it is the second one, which re-runs the full resolution and the token check and trusts what the first phase found only as a hint (see `gateway.md` GW8, AG4).

**deferred, provisioning deferral, provisioning gate** — The cn agent's exclusion of a resource from its effective desired state while a side under it is still being zeroed: a provisioning leg and the group over it, which also takes every later group of its list out of the pool's concat, a slice left with no effective group in either list, and the thin devices over such a slice with their namespaces, transfers and clones (see `cnagent.md` CN9). A deferred row reports provisioning, never error, unless the level also suppresses it.

**demote, demotion** — The move of a cntlr from primary to standby, which a failover decides and the cntlr's next converge carries out: its namespaces move inaccessible and are parked, its arrays stop, and its legs stay connected (see `architecture.md`, Standby cntlr). Also a cn converge's pre-step that reloads a transfer device this cntlr no longer serves onto an error table of its own size, so that it lets go of the origin's raid0 (see `cnagent.md` CN9).

**desired state** — What etcd records a node or a pool should look like, carried whole in every syncup request. Agents converge to it, and removal is derived from the difference between it and what the node holds.

**did not answer, unanswered** — The outcome of a probe that told the caller nothing: a command killed at a command timeout, never started, cancelled or refused an OS client slot, or a sysfs or configfs read that failed with any error but ENOENT. It is never "absent": an unanswered probe reads as unknown and counts as still present (`Reported`; see `osclient.md`, RunCommand).

**disabled** — A flag on a node or a cntlr. A disabled node only leaves allocation, losing its capacity key while its sides or cntlrs keep running, and no agent learns of it; a disabled cntlr leaves primary eligibility and the cdc entries, its agent moves its namespaces inaccessible, and it is neither replaced nor repaired, though a disabled primary fails over at once.

**disconnect registry** — The cn agent's in-memory set of subsystems whose `nvme disconnect` a sweep has set going off its locks. A later pass issues no second disconnect, and no converge adopts or connects such a subsystem until the disconnect has returned.

**discovery log, GENCTR** — The log page the cdc renders for a host from the owned entries visible to it, one record per entry per transport, with a generation counter kept per instance and connected host that advances only when that host's rendered log changes.

**disk node, DN** — A machine contributing one raw block device formatted with the dnv disk format, from whose data area extents are handed to sides. It runs a dn agent and exports its sides over that agent's nvmet port.

**dm kind** — The role letter plus hex digit in every dnv dm-device name that says which agent role and which device kind it is (`DmKind`, `ParseDmName`): `d` kinds for the dn role, `c` kinds for the cn role. A sweep attributes a device by it, and a name that does not decode exactly is not a dnv name, except under a cn's kind-`cb` prefix, where that cn's node-level sweep matches names by the prefix alone and removes the unwanted ones without decoding them (see `architecture.md`, dm-device kinds).

**dm-error, reload target** — The permanent error device every dnv stack reloads a path onto to take it out of service: per thin device on a controller node, where parked ns-devs point (`CnErrorName`), and per cntlr on a side, where a standby's dm-linear points (`DnErrorName`). A reload onto it fails IO instead of replaying it.

**dnv** — The project: a distributed NVMe-oF block storage system that aggregates the raw disks of disk nodes into storage pools, runs the volume logic on controller nodes, and exports virtual volumes to hosts with native NVMe multipath and ANA.

**dnv disk format** — The self-describing layout a disk node writes on its `--disk` device: a checksummed header block, alternating checksummed volume-table slots, the clone-metadata area and the extent area. The on-disk table, not the local store, is authoritative for extent placement.

**dnvctl** — The operator CLI: a noun-grouped command tree with one leaf command per Gateway RPC, taking the cluster and pool names as global flags and a presence-based `--rev` token, and printing one canonical JSON result document on stdout.

**drain, sp drain** — The sp worker's asynchronous teardown of a storage pool that `DeleteStoragePool` latched: every cntlr in one transaction, then each slice in turn, its groups in bounded batches that never span two slices, then the final keys, each batch returning node budget in the transaction that shrinks the record describing it. A lost describing record wedges the drain for an operator rather than drifting the ledger.

**driver** — A suite's test client outside the system under test: the agent suites' `dnagentctl` and `cnagentctl` speak the agent services, `gatewayctl` drives the gateway RPCs with a barrier runner, `workerctl` plays the gateway for the worker suite, and `cdcctl` writes the `CdcEntry` keys the cdc suite serves. Drivers are never linked into a shipped binary; the e2e suite also calls the machine it runs on the driver.

**echo field** — The handle a revision token message carries beside its revision, the node's address in `DnRev` and `CnRev` and the pool's name in `SpRev`, which a get returns: the gateway ignores it, a message carrying nothing else is a present zero token and is refused, and dnvctl never sends it.

**effective desired state** — The desired state a cn agent derives from the stored one by applying the provisioning deferral, and both converges and probes against; the wanted set ignores the deferral and carries the level (see `cnagent.md` CN9).

**effective membership** — The registrations of a role that an observer has committed after a full grace window; shard ownership is computed from it and never from the observed state (see `dnv-worker.md` VW5, VW11).

**effectively suspended** — A namespace that is stored suspended or named by an auto-suspend transfer, unless an auto-resume clone targets its thin device. Such a namespace is parked on every cntlr and placed in the inaccessible group.

**emulated host** — The agent suites' stand-in for an initiator that is not a dnv process: a plain `nvme connect` from a lab VM under a chosen host NQN and the host id derived from it (`NvmeHostId`), playing the controller nodes in the dn suite and the host in the cn suite. In the dn suite a VM holds several such identities, and an identity may reach its own VM's exports as well as the other VM's; the cn suite uses one fixed host NQN.

**entry point** — A place that mints a trace id, which the interceptors never do (see `grpc.md` T4): each daemon at startup; the agent per attempt of a background task, with the one exception T4 names; the cdc per accepted host connection, per scan attempt and per applied watch event; dnvctl per invocation unless `--trace-id` is given; the gateway for a request that arrives without one; and the worker per unit of work. The suites' drivers mint none: each carries the id its suite gives it through `--trace-id`, if any (see `grpc.md`, Drivers and fakes, and `log.md` R5).

**enumeration** — A sweep's listing of what the node holds: the dm devices, the nvmet tree and the nvme-host subsystems, and on the cn the md arrays. An enumeration that did not answer makes the verdict unclean and licenses no removal, on the cn any of the four stopping the whole sweep and on the dn the dm listing (see `architecture.md`, Teardown by sweep).

**err_epoch, health epoch** — The time the worker found an object unhealthy after last seeing it healthy, zero while it is healthy (`err_epoch`); it stays put while the object stays unhealthy, so the event thresholds that trigger reactions measure from it. It never bumps a revision, and a node with one set loses its capacity key.

**event thresholds** — The per-pool durations an object must stay unhealthy before a reaction fires: primary unhealthy, cntlr unhealthy, side unhealthy and leg unhealthy (`EventThreshold`). They are stored as sent and resolved when read.

**export, side export** — The nvmet subsystem a side presents to one controller node, keyed by the leg and the controller node and carrying no disk-node id, so the two sides of a migrating leg export the same NQN (`SideToCnNqn`). Its single namespace is backed by the side's per-CN dm-linear for that controller node.

**export gate, connect verdict** — A suite's two guards around a host connect: it waits until the agent itself reports the subsystem and namespace rows OK, because the discovery log is a control-plane reading that advertises an export before the agent has built it, and afterwards it reads the host's own controllers rather than believing nvme-cli's exit status.

**extent** — The allocation unit of disk-node space and controller-node budget, sized per cluster (`extent_size`); counts round down.

**failfast window** — The time after a connected target stops answering during which the connecting kernel still queues IO to the lost path instead of failing it: the fast IO fail timeout every agent connection is made with (`DefaultNvmeFastIoFailTmo`; see `dnagent.md` SH20), while its controller-loss timeout reconnects forever. A command that opens a device over such a path blocks for the whole window, so a sweep that meets one on a dead remote reports what it could not reach and succeeds on a later round, once the window has passed (see `architecture.md`, Teardown by sweep).

**failover** — The sp worker's reaction that makes a healthy, enabled standby the primary when the primary is disabled or has been unhealthy past the primary threshold, held to the cntlr threshold when that is longer while it is settling, and not where a failover cannot help, such as an error any primary would read alike (see `dnv-worker.md` AR5). The old primary becomes a standby and the new one is settling until its stack is seen built and clean.

**fake agent, fake gateway** — The suites' stand-ins for the processes they do not run, each behind the real server interceptors so its log records every message it receives: `fakeagent` plays the dn and cn agents for the worker and gateway suites, answering from its behavior file and from each object's last accepted request, which its state file keeps across a restart. `fakegateway` plays the gateway for the dnvctl suite, recording each method's call count and last request before answering from its behavior file.

**fan-out, sync fan-out** — The delivery a revision bump sets off: the worker syncs every object the revision covers, and for a pool the coordinator builds every side and cntlr request once and hands each child its own, unordered within each kind and sides before cntlrs (see `dnv-worker.md` RW14).

**fence** — The dn agent's cut of a side's per-CN path by reloading its dm-linear onto the dm-error: a primary flip reloads the old primary's linear at once, and a migration source fences in two phases, suspending each linear in place for the cutover window and then reloading it (see `dnagent.md` DN12). A reload that fails leaves the linear suspended on its old table, so the path fails closed rather than serving stale bytes; a worker's own fence is the self-fence.

**flip** — A worker-owned, one-way write of a readiness flag once the agents report it earned: the provisioned flip on a side whose zeroing is complete (`FlipProvisioned`) and the created flip on a thin device whose volume is OK in every slice (`FlipCreated`). Each flip bumps the pool's revision so the next syncup carries the flag.

**footprint** — The controller-node budget a cntlr reserves, the sum of the extents of every group of its pool: it is debited from the node's free extents when the cntlr is created and by each group a grow appends, and returned in the transaction that removes the cntlr.

**foreign, unowned** — The two outcomes of a sweep's attribution for an object that is not this agent's own. A foreign object is another agent's, another cluster's or another node's — a dm name of the other role's kind, a per-CN linear naming another node, an md array with a member that is not a wrapper of ours, a host-facing subsystem whose namespace maps an ns-dev that is not ours — and is never touched; an export whose attribute read did not answer is foreign for that pass. An unowned object is claimed by nobody — nothing this agent holds state for wants it and nothing marks it as another's — and only the node-level sweep removes it (see `architecture.md`, Teardown by sweep; `dnagent.md` DN6; `cnagent.md` CN21). A formatted disk whose header does not carry this node's cluster id, dn id and extent size is a foreign disk, which the dn agent never overwrites (see `dnagent.md` DN5).

**full sync** — The rule that every syncup request carries the complete desired state of its object, pointer lists included, so an agent never needs an earlier request to know what to build or remove.

**gateway** — `dnv-gateway`, the stateless, active-active gRPC server that implements the Gateway API over etcd: every mutation is one transaction, and it calls an agent only for a size, an inspection, a hydration check or a bitmap read.

**geometry** — The device parameters a pool's `bdev_conf` holds concrete, such as the block size, the stripe size and the bitmap chunk; the gateway fills every member the caller omits from the cluster's conf, else from a constant, when it writes the pool, so a stored zero is an invalid stored conf that the worker refuses to drive and an agent refuses to converge.

**global flags** — dnvctl's root persistent flags, such as the gateway address, the cluster and pool scope, `--rev` and the timeout: every one but `--rev` is env-backed, read from the flag, then the environment, then the config file (see `dnvctl.md` CT9). They are not the id counters named globals.

**globals** — The per-cluster counters each kind of node and pool draws from (`DnGlobal`, `CnGlobal`, `SpGlobal`): the next id, and a count of live objects per shard code whose smallest entry gives the next object its shard code. dnvctl's root flags are its global flags.

**grace window** — The time an observed change of a worker registration must hold before the observer commits it into its effective membership (`DefaultVoteWorkerGraceTime`), so a flapping worker never moves shard ownership; a worker drives nothing during its own first window. A migration source's grace window is the cutover window.

**group** — `Group`, a contiguous run of pool space in a slice: a meta group backs the slice's thin-pool metadata and a data group backs its data, and a grow, by an operator or by auto-grow, appends groups to a slice's lists (`GrowSlice`). A group is one leg (`RedundNone`), which the primary wraps in a group device (`CnGrpName`), or an md-raid1 array over its legs (`RedundMdRaid1`).

**guest** — A lab VM a suite drives over ssh; the e2e suite takes each guest from its command line in one role, control plane, controller node, disk node or host.

**health block** — The reserved block every leg carries in its meta region, behind the md superblock and write-intent bitmap of a redundant leg, that the primary's leg health probe reads and writes; probe IO never touches the data region.

**health bookkeeping** — The worker's maintenance of each object's health epoch and of a new primary's settling flag: written only on a health transition or a settle, in one transaction that bumps no revision, and logged. Each observation is compared with the health state, an in-memory cache of the record that the record's loads re-seed (see `dnv-worker.md` HL3).

**hole punch** — The discard the cn agent issues through the arena's loop device over a newly chosen clone-metadata range before it creates the slot's wrapper: on the tmpfs-backed arena file it zeroes the range by file semantics and frees its pages, so a recycled unit never shows a previous clone's dm-clone superblock. A wrapper that already exists and matches is never re-punched, since that would wipe a live superblock (see `cnagent.md` CN18).

**host** — The NVMe-oF initiator that reads a pool's namespaces: a kernel nvme host that discovers them through the cdc and aggregates every cntlr's export of a subsystem into one multipath device, following ANA to the primary.

**host NQN** — The NQN an nvme host connects under and an allowed-hosts list names, from which its host id is derived (`NvmeHostId`): an agent connects as its controller node's `CnHostNqn` for legs and clone sources, or as `DnHostNqn` for a migration destination, and a side's export to a controller node admits that node's `CnHostNqn` alone.

**host state** — The cdc's record of one host on one instance: created at the host NQN's first live connection with a fresh GENCTR, no pending bit and its view rendered, shared by all its connections and dropped at its last disconnect, so GENCTR restarts when it is recreated (see `cdc.md` DS7).

**hydration** — dm-clone's background copy, from the source to the destination, of every region not yet hydrated, which its "enable_hydration" message starts; a region the skip bitmap lets the agent discard needs no copy. An unforced `DeleteClone` or `FinishMigration` first proves through the agent that hydration is complete.

**identity flag** — A dnvctl flag that names the object a leaf command acts on, such as `--name`, `--addr` for a node or `--nqn` for a subsystem; a pool is named by the global `--sp`.

**idle** — A revision worker's state while its cluster is absent from the conf cache: no stream and no syncup, one record that the cluster conf is missing, and a retry every `DefaultHealthCheckInterval` until the cache holds the conf (see `dnv-worker.md` RW9).

**idle child** — A coordinator's side or cntlr child with nothing to drive: its endpoint has no node record, or, for every side child, a cntlr of the pool cannot be resolved or none is left. While something is unresolved the coordinator re-resolves on its own ticker, since no node revision event reaches the sp role.

**impact** — The effect of one watched change on an active host whose rendered view it changes: the host's GENCTR advances, its view is marked dirty and the pending bit is set on each of its connections, so an AEN reaches exactly the hosts whose filtered log changed (see `cdc.md` DS6).

**incarnation** — One lifetime under a name, told from the next by a fresh identity. A worker's incarnation is one seed's: a process start, and every rejoin after a self-fence, mints a new seed and drives nothing for one grace window (see `dnv-worker.md` VW1, VW7). A cluster's incarnation is one creation under its name, told apart by the creation epoch folded into its id, so a recreated cluster's keys and node-local names, the md superblock name apart, never collide with a leftover of the old one (see `architecture.md`, Naming, and [D9]).

**info** — The live-state report an agent builds per object (`DnInfo`, `SideInfo`, `CnInfo`, `CntlrInfo`): one row per resource the desired state calls for, each with a status and details, built by the converge as it runs for a syncup reply and from probes alone for a check round or an info read.

**inspect** — The gateway's live read of an object's info and of the revision of the last request its agent accepted for it (`InspectDiskNode`, `InspectControllerNode`, `InspectSide`, `InspectCntlr`; `applied_revision`), answered from the info read of the agent that hosts the object rather than from etcd.

**interceptor** — One of the four shared gRPC interceptors in `common` that every dnv client connection and server chains (`GrpcUnaryClientInterceptor` and its stream and server siblings): they move the trace id between context and gRPC metadata and log every request and reply message, and they never mint an id (see `grpc.md`, T1 to T4).

**invariant key** — A key the data model guarantees present while its parent exists: the revision keys, the globals, the record of every object a conf lists, and the node record a stored side or cntlr names. A read that reaches one through its parent and finds it absent has found a lost invariant, which the gateway never answers `NOT_FOUND` (see `architecture.md`, Key table).

**lab fact** — A behaviour of the lab guests' kernel or tools that a suite relies on, stated in its document apart from dnv's own rules — how the guests' dd handles direct IO, how IO behaves with no serving path, what a loop device's zero-out does — so that a reader can tell a suite's assumption from a design rule (see `dnagent_integtest.md`, Assumptions and preflight checks, and `cnagent_integtest.md`, Lab facts).

**latch** — The one-way `deleting` flag that `DeleteStoragePool` and `DeleteClone` set in their transaction: every other mutating call on the object is refused, a repeated delete is a no-op, and the sp worker's drain takes it from there.

**late member** — A leg of a group's `leg_list` that is not available when the primary converges the group, which then stays unbuilt or is assembled without it; the cn agent's connect retry re-runs the whole converge until the member is available and added (see `cnagent.md` CN12).

**layers, stop rule** — The order a sweep removes a node's objects in, top-down, from the nvmet exports down to the side devices and their records on a disk node, or to the legs on a controller node. The descent stops below any layer that left something, since a lower object may still back what remains.

**leaf command** — One dnvctl command per Gateway RPC, grouped under its noun; global flags such as the gateway address, the cluster and the pool come from flags, the environment or the config file, while leaf flags come from the command line alone, with no client-side validation and no hidden RPCs.

**leaf package** — `common`, the package every dnv binary imports and which imports no other dnv package, nor the etcd client nor viper, so it stays the leaf of the dependency rules (see `layout.md`, Dependency rules).

**ledger** — The node-budget bookkeeping of a transaction: the gateway's DN and CN ledgers (`dnLedger`, `cnLedger`) read each node a transaction touches once, write it once, maintain its capacity key once and bump its revision once. A drain returns budget to the node records, the receiving ledger, in the transaction that shrinks the record describing it.

**leftover** — An object a node holds that its desired state does not want and a sweep has not yet removed, reported as `ReplyCodeLeftover` with its name in the reply's details: the request is accepted and stored, and the worker re-issues the syncup every round until the verdict is clean. The same code carries an enumeration that did not answer and the conditions only a syncup cures.

**leg** — `Leg`, one replica of a group: a side on one disk node, two while the leg migrates, and on every cntlr a connected multipath namespace wrapped by a leg device; its index orders the group's md members.

**leg device, leg wrapper** — The cn-local dm-linear over a leg's multipath namespace device (`CnLegName`) that every cntlr builds, spare legs included: the member the group device or md array consumes. A migration's change of side happens below it, in nvme multipath.

**leg health probe** — The primary's periodic direct write and read-back of each leg's health block (`LegProbeIO`), run outside the OS client and free to block, since a hung probe is how a stalled target is found; it decides the leg's row, an attempt in flight past its stall bound reading as an error, and through the worker the leg's health epoch.

**leg repair** — The sp worker's reaction for a leg unhealthy past its threshold: switch in a ready spare of the group, or wait for a pending one, or create a spare leg on a fresh disk node and switch it in when it is ready. The replaced leg is parked.

**level, sp level** — A pool's degradation ladder (`SpLevel`), from read-write through read-only, no clone, no thin pool, no redundancy, no migration and no side, to disabled; each step suppresses the resources above it so an operator can take a damaged pool apart in stages, and read-only is enforced on the controller node by failing writes.

**local store** — The directory an agent keeps the last accepted request per object and one file per received bitmap chunk in (`--local-store`), each written through a temp file, fsync and rename. After a restart it is the desired state the agent reconciles to before serving, and its temp leftovers are deleted at startup.

**location** — The failure-domain string a node registers (`location`), defaulting to its address: the allocator never places two legs in one location within one allocation round, and keeps a pool's new cntlrs, a spare leg and a migration destination out of occupied locations at its first tier only.

**lock hierarchy** — The agent's lock set (`LockSet`): the node write lock for a node syncup or the startup reconcile, the node read lock plus the object's own lock for every object-scoped call, check rounds and background converges included, and the node read lock alone for node-scoped reads and rounds; the size reads, the leg health probers and the background disconnects take none (see `dnagent.md` SH10 to SH13).

**log record** — One JSON line on stderr per operation, written on completion with its outcome and the trace id, by every dnv binary through the one shared handler chain; stdout stays the result channel. Record names and attributes are normative, since the suites match them.

**low water mark** — The pool-usage fraction above which auto-grow fires (`low_water_mark_pct`); a mark that cannot be reached disables auto-grow for the pool.

**md assembly mask, md udev mask** — The udev rule every node running a dnv agent carries so that only the agent assembles dnv md arrays: ordered before the stock incremental-assembly rule, it marks a device whose array name carries the dnv prefix not ready, and the stock rule skips it (see `architecture.md`, Components: invocation reference). The cn and end-to-end suites install it for their run.

**mechanism, policy** — The split of the agent code: what both roles need verbatim is mechanism and lives in `agent`, and what knows which resource to build is policy and lives in `dnagent` or `cnagent`.

**member reconciliation** — The primary's comparison of an md array's live members with the group's leg list on every converge: a listed leg that is missing is added once available, a member that is not listed is failed and removed, and a spare is never a member until it is switched in; a deferred group runs none of it.

**meta region, data region** — The two parts of every leg: the meta region at the head, holding the md superblock and write-intent bitmap of a redundant leg and the health block of every leg, and the data region behind it, which is the only part the pool sees.

**migration** — `Migration`, the live move of one side of a leg to another disk node: once the destination side is provisioned the source side fences its per-CN paths and exports its side device to the destination disk node, whose dm-clone serves the leg while it pulls the bytes, and the finish removes the source side. While it runs the leg has a source side and a destination side, each side's syncup request carrying its role (`migr_src_conf`, `migr_dst_conf`).

**migration source export** — The nvmet subsystem a source side adds for its destination (`DnMigrSrcName`, `MigrSrcNqn`), admitting the destination disk node's host NQN only, and built only once the destination side is provisioned.

**model, etcdutil** — The two packages between etcd and the processes that use it, the gateway, the worker and the cdc: `model` holds the data model as Go, the key builders, the typed records and the shared mutations the gateway and the worker both run inside their transactions, and `etcdutil` is the one door to etcd, with typed get, put, delete, range and watch helpers that log each operation decoded, and the STM runners.

**multipath namespace** — The kernel's merge of a leg's sides, which export the same NQN and namespace identity, into one namespace device on the controller node; each side's path carries its own ANA state and the leg device sits on the merged device.

**mutation-free** — A cn suite assertion that an agent log, whole or for one trace, holds no mutating command and no configfs or block write; the leg probers' records fall outside it by construction.

**namespace** — `Namespace`, the host-facing volume of a subsystem: its index is the NSID, it names the thin device it exposes, and it may be stored suspended. On every cntlr it is an nvmet namespace over the ns-dev, optimized on a primary that serves it and inaccessible otherwise (see `cnagent.md` CN16).

**namespace identity** — The uuid and nguid a namespace presents: for a side export, the per-leg identity (`DnNsIdentity`) with the leg id as its serial and the fixed dnv model beside them, identical on both sides of a migrating leg so that the two merge into one multipath namespace; for a host-facing namespace, the uuid and nguid every cntlr of the pool presents, so that the host merges its paths (see `architecture.md`, Host view).

**node selector, black list, white list** — The black list and white list an allocating request may carry (`NodeSelector`): a black-listed node is never picked, and a non-empty white list restricts picks to it.

**not given** — dnvctl's convention for an optional sub-message: it is sent only when one of its flags holds a non-zero value, a zero meaning the member was not given. For `--rev`, given means typed on the command line.

**noun group** — One of dnvctl's per-resource command groups, "dnvctl <group> <verb>", under which the leaf commands sit; a group issues no RPC itself, and each lives in a file of its own under `ctl/`.

**NQN kind** — The digit in every dnv NQN that names the object kind (`ParseNqn`): a disk-node host, a controller-node host, a side export, a migration source export or a transfer. A user-facing subsystem NQN must lie outside the dnv namespace (`IsDnvNqn`).

**ns-dev** — The namespace's own dm-linear on a controller node (`CnNsDevName`), the device nvmet exports, whose table follows a backing state machine: the thin device's raid0 while the namespace serves, the dm-error while it is parked, and the clone device while a clone fills the thin device.

**nvme-stas** — The production host stack: it holds a persistent discovery connection to every configured cdc endpoint and connects, re-points and disconnects the host's controllers on the discovery-log-change AENs; plain nvme-cli is equally supported.

**object** — In an agent, a side or a cntlr as opposed to the node that is its parent (see `dnagent.md` SH5, SH10). In the worker, the unit of driving: a disk node, a controller node, a side or a cntlr, each with one check stream, one goroutine and one desired request at a time, where a newer request replaces one not yet sent.

**observed state** — A registration's live or dead state as one observer judges it, by its own monotonic clock since the last put it saw and never by the stored epoch (see `dnv-worker.md` VW3, VW4).

**origin, snapshot** — A snapshot is a thin device created from another, its origin, by a pool snapshot message in every slice (`ori_id`); the origin must be created first and is not deleted while an uncreated snapshot of it exists.

**OS client** — The one door through which agents run OS commands and do file, block and protobuf IO (`OsClient`, implemented by `LimitedOsClient` and faked by `FakeOsClient`): it bounds concurrency with a semaphore, answers the caller's deadline with SIGTERM and then SIGKILL at the hard timeout, and logs each operation; the leg health probers' direct IO is its one sanctioned bypass.

**P0** — The step before every layer of the cn sweep's chain that parks every unwanted ns-dev by its own live table, so that the namespace above it can be disabled and its in-flight host IO completes instead of being replayed onto a stack about to go (see `cnagent.md` CN21).

**parked** — A namespace is parked when its ns-dev is reloaded onto the thin device's dm-error, keeping the device live while it serves nothing. A leg is parked when a spare switch moves it to the group's spare list, still connected and probed; the worker never deletes it, and leg repair may switch it back in once it reads healthy.

**pass, converge pass** — One run of an agent's converge, at node level for a node syncup or the startup reconcile and at object level for a side or a cntlr; its sweep, its wait budget and its leftover record belong to it. On the sp worker, a pass is a reaction pass.

**pass budget, wait budget** — The bounded wait one converge pass may spend on the node catching up with what the pass asked of it (`WaitBudget`), made at the start of the pass and never carried into another: the cn connect step's budget, shared by every leg and clone source of the pass (`CnConnectPassBudget`; see `cnagent.md` CN10), and the dn agent's wait for a migration source's namespace (see `dnagent.md` DN13).

**pending bit** — The per-connection flag an impact sets: an armed asynchronous event request completes as soon as it is set and delivery clears it, so impacts while none is armed coalesce into one AEN (see `cdc.md` NP11).

**pending spare, ready spare** — A ready spare has one side, provisioned, and the primary's latest row of it reads OK, so leg repair switches it in; a pending spare is not ready yet and not dead, so leg repair waits for it rather than creating another (see `dnv-worker.md` AR8).

**per-CN export stack** — What a side builds for each cntlr of its pool: a dm-error, a dm-linear over the side device for the primary or over the dm-error for a standby, and an nvmet subsystem with one namespace over the linear (`DnErrorName`, `DnLinearName`, `SideToCnNqn`).

**persistent discovery connection** — A host's long-lived connection to a cdc instance, which keeps its host state and receives the discovery-log-change AENs; a one-shot discover creates and drops its host state each time.

**placement bound** — The e2e suite's default count of dn agents per disk-node guest: the least count per guest that rules out, by counting, a pick starving for disk nodes in distinct locations, sized by the ops case (see `e2e_integtest.md` E2E3).

**planning snapshot** — A read-only etcd snapshot (`Snapshot`) an RPC plans from before its deciding STM: `DeleteThinDevice` walks its thin devices in one, and `GrowSlice`, `CreateSpareLeg` and `SwitchSpareLeg` read the cluster conf in theirs rather than in their deciding STM (see `gateway.md` GW5).

**plays the worker** — What the gateway suite does when it writes a worker-owned flip or runs a worker-owned drain through `workerctl`, so a gateway precondition can be exercised without a running worker.

**pointer list** — The ids of the sides or cntlrs a node hosts, kept in its record (`side_ptr_list`, `cntlr_ptr_list`) and sent in its syncup request (`side_pointer_list`, `cntlr_pointer_list`); the list is authoritative, so an object it omits is removed by the node sweep.

**port** — The nvmet port an agent converges at its port id (`--nvmet-port-id`) as part of its node's base state, with the fixed ANA groups; every export of the node is linked to it. A dn agent and a cn agent side by side on one node co-own one port at the default port id with identical transport values (see `dnagent.md` CM2, SH19).

**positive control** — A suite's proof that the read behind a negative assertion can see what it asserts absent, such as the same read seeing a path live before it counts the path gone, so a read that failed cannot pass the negative.

**pre-steps** — The transitions a converge pass makes before its sweep's layers. On the cn every namespace the plan wants inaccessible moves there first, on every pass, and then planned ns-devs are parked and unserved transfer devices demoted, those two only on a pass whose listings all answered (see `cnagent.md` CN9). On the dn they are the side-level transitions ahead of the sweep's layers: the end of a destination role's retry, the fence's clear, resume and mark, and the repoint of the per-CN dm-linears off a dm-clone about to be removed, made only by a pass whose `dmsetup ls` answered (see `dnagent.md` DN6).

**preflight** — A suite's checks before any case runs: the tools and kernel features each guest needs, the freshness of the binaries, the lab wiring, and the absence of residue from an earlier run.

**presence rule** — The rule that a node holds a capacity key exactly while it is allocatable: its pointer list under its cap, no health epoch set, not disabled, and its free extents not below the floor (`DnAllocatable`, `CnAllocatable`). Whichever transaction changes one of those inputs deletes or rewrites the key in the same transaction, so the allocator never filters by health or flags (see `architecture.md`, Capacity index keys).

**primary** — The cntlr that runs a pool's full stack: it connects every leg, assembles the groups, activates the thin pools, raid0s, clones and transfers, and exports the namespaces optimized. Only the sp worker changes which cntlr is primary.

**probe** — A read-only reading of live state through the OS client, per object: a converge probes first and mutates only the differences, and a check round's info is built from probes alone.

**probe-IO carve-out** — The one sanctioned bypass of the OS client: the leg health probers write and read their health block through `WriteBlockAt` and `ReadBlockDirectAt`, which take no context, hold no OS client slot and log nothing, so the probers log their own probe records (see `osclient.md`, Exported raw helpers and the probe-IO carve-out).

**provisioned, zeroing** — The side flag the sp worker sets once the disk node reports every extent of the side zeroed (`provisioned`): zeroing runs in the dn agent's background as batched zero-out writes through the side device, tracked in the volume table's zeroed bits, and a leg none of whose sides has the flag is deferred on the controller node. A migration destination carries its own flag that gates the source export and the clone.

**QoS** — The cluster-wide ratio of size-proportional IO limits (`qos_ratio`) that every controller-node syncup carries; the cn agent persists it and enforces nothing.

**quarantined** — Stored migration bitmap chunks whose recorded migration id is not the one of the destination role the side now holds: they are never applied, and the destination converge that finds them drops them, files included, ahead of the steps that would apply them, because a migration id is never reused and such chunks describe a different copy (see `dnagent.md` DN13).

**quiesce, snapshot pre-pass** — The primary's suspension of a thin device's raid0 around the pool messages that create its snapshot in every slice, so the snapshot is consistent across slices.

**quiet set, reacting set** — The event thresholds the e2e suite builds each case's pool under: the quiet set, longer than any wait, so that no reaction fires mid-case, and the short reacting set under which the react case waits out cntlr replacement and leg repair.

**raid0** — The dm-striped device over a thin device's per-slice thin volumes (`CnRaid0Name`), the block device a namespace, a clone or a transfer is built on, striped by the pool's geometry.

**range, twins** — A range is the part of the shard-code space a cdc instance serves, named by one hex digit, and twins are the instances configured for the same range. Each twin serves the whole range, so a host that lists every twin keeps its view while one is down.

**reaction** — One of the sp worker's automatic reactions: failover, auto-grow, cntlr replacement and leg repair. A pass applies at most one and logs it as applied or skipped with its reason.

**reaction pass** — The coordinator's periodic evaluation of a pool from a fresh snapshot against the event thresholds and the pool's level: it applies at most one reaction per pass, so that each reaction's effect is seen before the next decision, and it also runs the drain steps of a latched pool or clone (see `dnv-worker.md` AR1, AR2).

**reconcile** — An agent's startup pass: load the stored requests, converge each object once under the node write lock, then serve. A file that does not load is skipped, and with it every file of a node whose own file did not load; a skipped object is neither converged nor swept and answers unknown until its syncup comes again.

**record step** — The dn node-level pass's run of the record rule over the volume table itself (`sweepOrphanRecords`): it frees an allocation record only when its owner is provably gone and its device has been probed gone, and it removes no device (see `dnagent.md` DN6).

**region** — dm-clone's unit of hydration, one destination block. The agent maps each region through raid0 bitmap math onto the source's blocks, and a region is skippable only when every source bit it covers is set.

**residue** — What a sweep or a cleanup leaves behind; an agent reports it as leftover, and a suite reports it and never passes it silently.

**ResInfo** — One resource's row in an agent's info (`ResInfo`): the resource's name, its status (see ResStatus), its details and the epoch of its last status change, which a details change alone does not move. The agent's per-object tracker (`ResTracker`) keeps the history behind each row and drops it when the resource leaves the desired state, so the next object built with the same id inherits nothing (see `dnagent.md` SH14; `architecture.md`, Live-state reporting).

**resolution** — The in-STM reads that turn a request's cluster name and pool name into the cluster id and the pool's conf (see `gateway.md` GW5); filling the conf members a caller omitted is the separate resolve-at-write.

**resolve-at-write** — The rule that the RPC writing a conf fills every member the caller omitted, from the cluster conf and then the constants, so every stored conf is concrete and nothing downstream substitutes; the event thresholds and the hydration pair are the exceptions, stored as sent and resolved when read (see `gateway.md` GW11).

**ResStatus** — The status of one info row (`ResStatus`): missing, error, ok, provisioning for a healthy resource that is not ready, pending for a leg no probe round has completed, and unknown, which only the worker assigns to an object it cannot reach.

**revision** — The monotonic counter per disk node, controller node and storage pool (`DnRev`, `CnRev`, `SpRev`) that every transaction changing agent-visible desired state bumps once; the worker syncs an object when its revision moves, and the agent keeps the revision of the last request it accepted for the object (see `dnagent.md` SH8).

**revision gate** — An agent's check of a syncup's revision against its stored request's (`GateRevision`): a lower one is refused as stale (`ReplyCodeStaleRevision`) and an equal one re-applies idempotently, before the request becomes desired state. An object the node does not hold is refused beside it as unknown (`ReplyCodeUnknownObject`).

**revision key, handle** — The id-addressed key per node and pool that the worker watches, whose value holds the revision and the mutable handle the worker needs to reach the object: the agent's address for a node (`addr_port`) and the name for a pool (`sp_name`). The key is stable, so a watcher sees one put rather than a removal and a re-add.

**revision token** — The optimistic-concurrency message a mutating request may carry: absent, the transaction skips the check; present, the stored revision must equal it or the call is aborted as stale, so a present zero is always refused. dnvctl sends it exactly when `--rev` is given.

**revision worker** — The worker goroutine per revision key that pushes each revision of its object to the agent and runs its check stream and health rounds; for the sp role it is the coordinator with a child per side and per cntlr.

**rootCtx** — The agent's lifetime context (`rootCtx`) that background tasks run on, so the end of a request never cancels a connect retry, a zeroing goroutine, a fence timer or a background disconnect. Process exit cancels it and waits only for the dn agent's zeroing workers and connect retries (see `dnagent.md` SH27).

**rotation** — The operator's replacement of a cntlr to give it a free cntlid slot or another controller node: disable it, delete it, then create a cntlr with a free slot (`UpdateCntlrEnabled`, `DeleteCntlr`, `CreateCntlr`).

**seed, registration** — Each worker process mints a fresh seed per incarnation and keeps one registration per role under it (`WorkerReg`), re-put every vote interval with no etcd lease: observers judge it by their own clock and delete the key of one they commit dead (see `dnv-worker.md` VW2, VW6). The seed enters the worker's tickets, and a worker that fences itself rejoins under a new one.

**self-fence** — A worker's exit when its heartbeat has not fully reached etcd, or its own puts have not been echoed by its watch, for the dead threshold plus one vote interval, when a peer deletes its key, or when its own observer would commit its own key nonmember: it stops every shard worker at once, deletes its registrations and rejoins under a fresh seed, driving nothing for one grace window (see `dnv-worker.md` VW5, VW8).

**sentinel, start gate** — The e2e cleanup's check of its own completion: every cleanup verb prints a sentinel line when it finishes, and after the start and between-cases sweeps the start gate (`cleanup_start_gate`) stops the run when a verb never printed it (see `e2e_integtest.md` E2E6).

**settling** — The state of a cntlr just made primary (`settling`): while it is set, an unhealthy primary is held to the cntlr threshold instead of the primary one when that is the longer, though a disabled one fails over at once. The worker clears it once the cntlr reports its primary stack built and clean at the revision it drives, and a failover that demotes it clears it too.

**shard code** — The small number every node and pool is given at creation (`shard_code`), the index of its kind's least-filled shard bucket, that partitions ownership: a worker owns whole shards of a role, and the cdc owns ranges of shard codes.

**shard ownership, ticket** — How workers divide shards without coordination: every live worker computes, per role and shard, a ticket from its seed, the role and the shard, and the largest ticket among the committed membership owns the shard; a membership change moves only the shards whose winner changes.

**shard worker** — The per-role, per-shard worker that scans and watches one revision prefix and keeps one revision worker per key, started and stopped by the vote worker as ownership moves.

**shared-state row** — An error row a cntlr reports for the stack of a created thin device whose pool does not hold its thin id, which every cntlr in the primary role reads alike; it sets the cntlr's health epoch, but a report whose error rows are all of this class triggers neither a failover nor the replacement of a primary with no failover candidate (see `dnv-worker.md` HL2).

**sibling agent** — Another dnv agent on the same kernel: the dn agent beside a cn agent, or one of several dn agents each with a disk, port id and transport address of its own (see `architecture.md`, Disk node). Its dm devices, nvmet objects and host connections appear in this agent's enumerations too, so attribution tells them from this agent's own and a sweep leaves a sibling's build that is in flight alone (see `architecture.md`, Teardown by sweep).

**side** — `Side`, the disk-node resident part of a leg: runs of extents in the node's volume table, the side device over them (`DnSideName`, the dm-linear that zeroing writes through and the primary's per-CN linear maps), and one export stack per cntlr of the pool. It carries the provisioned flag and, while the leg migrates, a source or destination role.

**sides first, sides-first hold** — The coordinator's hold of a fan-out's cntlr requests until every running side child has reported that revision applied or one cntlr interval has passed, a timer release being logged (see `dnv-worker.md` RW14). It narrows the races of a cntlr outrunning its sides' exports and ANA flips, and is no correctness dependency.

**skip bitmap** — The chunked bitmap of a clone's or a migration's source that the worker pushes, a set bit marking a source block as never written: the agent folds the chunks it holds through raid0 bitmap math and discards on the dm-clone every region all of whose source bits are set, so a missing chunk only costs extra copying.

**slice** — `Slice`, a vertical shard of a storage pool: one thin pool on the primary, built from the slice's own meta and data groups; every thin device of the pool is striped across all slices.

**source connection** — The nvme host connection a dm-clone reads its source through: a migration destination's connection to the source side's export, or a clone's connection to its external source, the clone-source connection, which belongs to no pool. A sweep removes it only after the dm-clone above it.

**space guard** — The e2e suite's allocation caps, asserted after every case: one per backing file, which bounds how many extents its disk node zeroed, and one for the whole run at the pool's own extents plus slack (see `e2e_integtest.md` E2E5).

**spare leg** — A leg in a group's `spare_leg_list`, connected and probed but not an md member: created by an operator or by leg repair, or parked there by a switch, and switched into the array in place of a failed leg only once its side is provisioned.

**stage** — One step of a suite case, run under its own trace id, which every call the step makes carries into the records it causes; work the worker then does for it runs under the worker's own ids.

**standby** — A cntlr that is not primary: it keeps its connection to every leg so a failover is fast, and exports every namespace over the dm-error in the inaccessible group, so hosts see the path and a failover only moves the stack.

**STM** — The etcd software transaction (`RunSTM`) every mutation of the gateway and the worker runs in: reads are collected, the writes commit only if nothing read has changed, and the function is retried otherwise; the vote worker's registration writes are plain puts and deletes. A deciding STM re-validates its preconditions inside it.

**storage pool, SP, sp** — `SpConf`, the unit of volume service: a set of cntlrs on controller nodes, slices made of groups of legs on disk nodes, and the thin devices, subsystems, clones, transfers and migrations it owns, with one conf, one level and one revision.

**subsystem** — `Subsystem`, the host-facing NVMe-oF object a pool exports from every cntlr under one NQN, each cntlr's export within that cntlr's cntlid slot: it carries its allowed hosts and its namespaces, and the gateway publishes it to the cdc as a `CdcEntry`.

**suite** — One of the integration test scripts under `integtest/` that drives real binaries on lab VMs over ssh: the dn agent, cn agent, worker, gateway, dnvctl, cdc and end-to-end suites.

**suppressed** — A resource a pool's level excludes: the cn agent reports it missing with the level in the row's details (`ResDetailsSpLevel`), which the worker reads as expected absence, not as failure, and the dn agent reports no row for it.

**sweep** — The removal half of every converge: enumerate what the node holds for the scope, attribute each object, and remove top-down whatever the wanted set does not name, counting an object as removed only when a probe reads it absent afterwards. Removals derive only from live enumeration minus desired state, never from remembered failures.

**syncup** — The unary delivery of an object's desired state to its agent (`SyncupDn`, `SyncupSide`, `SyncupCn`, `SyncupCntlr`): gated by revision and conf, converged, persisted when accepted, and answered with a reply code and details.

**sysfs walk** — An agent's reading of its nvme host connections from sysfs rather than nvme list-subsys: the subsystem entry whose NQN matches, the namespace entry that names the multipath head, and the controller entries, each path read through its address, state and ANA state (see `cnagent.md` CN10).

**thin device, td** — `ThinDevice`, a pool volume: one thin volume per slice under the same thin id (`CnThinDevName`, created or snapshotted by a pool message and deleted by one), joined by a raid0 on the primary. It may be an origin, a snapshot or both, and carries the created flag.

**thin id** — The id of a thin device's volume inside every slice's thin pool (`dev_id`), minted by the gateway and shared across the slices; the activation sweep deletes ids no stored thin device owns.

**thin metadata snapshot** — The way an agent reads a live thin pool's mappings without stopping it: reserve the pool's metadata snapshot, dump it, release it. The activation sweep, the thin-device and leg bitmap reads and clone crash recovery read pools this way.

**thin pool** — The dm-thin pool each slice activates on the primary (`CnPoolFinalName`) over a concat of the slice's meta groups and a concat of its data groups (`CnPoolMetaName`, `CnPoolDataName`); appending a group grows the concat and reloads the pool.

**token carrier** — A dnvctl leaf command whose request has a revision token field, the only kind that takes `--rev`; a `--rev` typed on any other leaf is a usage error (see `dnvctl.md` CT1, CT3).

**token check** — The gateway's comparison of a mutator's revision token with the stored revision, made right after resolution and before any other state check, and only when the token message is present (see `gateway.md` GW6).

**trace id** — The identifier that joins the log records of one operation across processes (`trace_id`): minted at an entry point, carried in the context, moved between context and gRPC metadata by the interceptors, and sent in the request of every check round, which runs under an id of its own.

**transfer, xfer** — `Transfer`, the source side of a copy: the primary exports the raid0 of a local namespace's thin device over a transfer subsystem (`XferNqn`) admitting the hosts its record lists, the destination cntlrs' host NQNs, while auto-suspend parks the origin namespace; a finalizing delete leaves it parked.

**transport probe** — A standby's reading of a leg's health from the kernel alone, by the liveness and ANA state of each path, since no probe IO runs there.

**two-phase RPC** — A gateway RPC that must ask an agent between its reads and its commit: a read-only first phase resolves the objects, the agent call runs outside any transaction, and the deciding second phase re-runs the full resolution and the token check, trusting the first phase's facts only as hints, so a token-carrying request sees any interleaved mutation as stale (see `gateway.md` AG4).

**verdict** — The sweep's comparison of what a scope holds with what it wants, answered as the reply code: clean, or leftover while the scope holds an unwanted object, a listing did not answer, or a condition only a syncup cures is found. A check round or an info read computes it with the removals left out, a syncup from the sweep it has just run; it is recomputed every time and stored nowhere, and it is distinct from the worker's health verdict on an object's health epoch.

**view** — The rendered discovery records of one connected host: the owned entries visible to it, in a fixed order, materialized only while the host holds a connection. An impact marks the view dirty, and it is re-rendered at the host's next read of its log page or by a rescan that comes first (see `cdc.md` DS5, DS6).

**view registry** — The cdc's in-memory state of its active hosts, their host states and views, fed by the watcher and serialized under one mutex so that no served state is mutated concurrently.

**volume table** — The on-disk record in the dnv disk format of which extent runs each side holds and which of them are zeroed (`zeroed_bits`), written to alternating checksummed slots; it is the authority for extent placement, not the local store.

**vote interval** — The period at which a worker re-puts each of its registrations (`--vote-interval`, defaulting to `DefaultVoteWorkerInterval`); the dead threshold is twice it.

**vote worker** — The per-process layer that registers the worker's seed, watches every worker's registration, commits a membership after the grace window, computes shard ownership and starts and stops shard workers.

**wanted set** — The names a converge computes from its request's plan: every dm device, md array, export, connection and record the object should hold at the plan's level and role. The sweep removes what is held and not wanted, and the build ensures what is wanted; on the cn a resource the provisioning gate defers stays wanted, and on the dn so does everything above the side device of a side not yet zeroed and provisioned (see `dnagent.md` DN6), so the sweep does not remove what the build is about to create.

**watcher** — The cdc's reader of the entry keys: one scan to fill the registry, then a watch that applies each event, and a wholesale rescan when the watch breaks; until the first scan completes it serves no host.

**wipe** — A lab-only scrub of a guest: the e2e suite's host-side disconnect of every dnv NQN, and the agent suites' kind-blind `--wipe` over every dnv-named dm device, nvmet subsystem and md array and every fabrics controller on the node, repeated while dnv dm devices remain and failing on dm or nvmet residue.

**worker** — `dnv-worker`, the control-plane process that turns etcd's desired state into agent calls: it watches revision keys by role and shard, syncs and checks each object, applies flips, runs health rounds, reacts, and drains deleted pools and clones. Several run active-active, divided by shard ownership.
