# distributed-nvme (dnv)

dnv is a distributed NVMe-oF block storage system. It aggregates the raw disks
of disk nodes into storage pools, runs the volume logic (thin provisioning,
striping, redundancy, snapshots, cloning, live migration) on controller nodes,
and exports virtual volumes to hosts over NVMe-oF with native NVMe multipath
and ANA. etcd holds all desired state; the control-plane processes and the
per-node agents converge the data plane to it. It can be the data plane of a
multi-tenant block storage system, for example behind Kubernetes or OpenStack
Cinder. It leaves the authentication of users to the layer above. It promises
that layer that a host never reads, through the volumes of one storage pool,
the data of another storage pool, unless that layer exports the data there
itself, for example with a transfer. The promise covers only what a host
reads through the volumes that layer gives it, and it needs a trusted
network and trusted root users on the nodes.
[`doc/human/scope.md`](doc/human/scope.md#3-the-promise-one-pool-never-reads-another-pools-bytes)
lists the steps that can break the promise: steps a caller gets wrong and
steps an operator forces. Root on a host can pose as another host and read
that host's volumes, which the promise does not cover, so keeping tenants
apart also needs hosts where a tenant has no root.
[The introduction](doc/human/introduction.md#status-and-limits) says who can
read what.

- [`doc/human/introduction.md`](doc/human/introduction.md) — what dnv is for,
  its six programs, how it is built up and how it behaves, with diagrams;
  what it needs, what 0.1 is, and how to get it.
- [`doc/human/core_glossary.md`](doc/human/core_glossary.md) — the core words
  of dnv in plain language, the short list to read first.
- [`doc/human/scope.md`](doc/human/scope.md) — what dnv promises, the data
  loss it accepts, what it does not do, and every known limit of dnv beyond
  the test suites and the enforced sizes, with its state.
- [`doc/human/operations.md`](doc/human/operations.md) — the procedures an
  operator follows by hand, each with the steps that can lose data where
  there are any, and the test that checked it, or a note that none did:
  first how to read what is wrong across a cluster, then the forced-start
  guide, which brings back a group whose every leg failed, and the
  procedures for nodes, the networks, pools, volumes, the control plane,
  Kubernetes and hosts, listed by subject at its top.
- [`doc/human/tutorial_all_in_one.md`](doc/human/tutorial_all_in_one.md) — dnv
  on one machine, run from its binaries: the whole system, then a host
  connected to a volume with nvme-stas, every command complete.
- [`doc/human/tutorial_csi.md`](doc/human/tutorial_csi.md) — dnv under
  Kubernetes on one machine: a one-node Kubernetes gets its volumes from dnv
  through `dnv-csi`, every command complete.
- [`LICENSE`](LICENSE) — the MIT license dnv is released under.

The design documents under `doc/llm/` are written for maintainers and LLM
agents.

Report a flaw as an issue at
https://github.com/distributed-nvme/distributed-nvme/issues. An issue is
public, so for a flaw that lets someone read or change data that is not
theirs, open an issue that says only that and asks for a private contact.
