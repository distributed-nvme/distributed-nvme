# distributed-nvme (dnv)

dnv is a distributed NVMe-oF block storage system. It aggregates the raw disks
of disk nodes into storage pools, runs the volume logic (thin provisioning,
striping, redundancy, snapshots, cloning, live migration) on controller nodes,
and exports virtual volumes to hosts over NVMe-oF with native NVMe multipath
and ANA. etcd holds all desired state; the control-plane processes and the
per-node agents converge the data plane to it. It can be the data plane of a
multi-tenant block storage system, for example behind Kubernetes or OpenStack
Cinder: it leaves the authentication of users to the layer above, and it keeps
the data of one storage pool unreadable from any other storage pool unless
that layer exports it there itself, for example with a transfer.

- [`doc/human/core_glossary.md`](doc/human/core_glossary.md) — the core words
  of dnv in plain language, the short list to read first.
- [`doc/human/tutorial_all_in_one.md`](doc/human/tutorial_all_in_one.md) — dnv
  on one machine, run from its binaries: the whole system, then a host
  connected to a volume with nvme-stas, every command complete.
- [`doc/human/tutorial_csi.md`](doc/human/tutorial_csi.md) — dnv under
  Kubernetes on one machine: a one-node Kubernetes gets its volumes from dnv
  through `dnv-csi`, every command complete.
- [`LICENSE`](LICENSE) — the MIT license dnv is released under.

The design documents under `doc/llm/` are written for maintainers and LLM
agents.
