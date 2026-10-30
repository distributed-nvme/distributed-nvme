# README.md — the documents for the LLM and maintainers

The documents under `doc/llm/` are the design the code follows, written for
the LLM and for maintainers; where every markdown file lives and who may
name whom is `layout.md`, Human docs and LLM docs.

## What each document owns

Read [`core_glossary.md`](../human/core_glossary.md) first: the core words
in plain language. `glossary.md` defines the full vocabulary the documents
use. Then read `architecture.md`, the design as a whole: the object model,
the device stacks, naming, the etcd data model, allocation, common
validation, the `Gateway` API contract, the agent and worker contracts, the
procedures that span components and the design decisions. The other
documents each own a part:

- `gateway.md` — `dnv-gateway`: how it carries out each RPC.
- `dnv-worker.md` — `dnv-worker` and the `etcdutil` and `model` packages.
- `dnagent.md` — `dnv-agent`: what both roles share, the command, the dn role.
- `cnagent.md` — the cn role of `dnv-agent`.
- `cdc.md` — `dnv-cdc`: the discovery service and its etcd watcher.
- `dnvctl.md` — `dnvctl`: invocation, the command tree, output and errors.
- `log.md` — the JSON logging and the trace id every binary shares.
- `grpc.md` — the gRPC interceptors every dnv connection and server chains, and the message limit every one sets.
- `osclient.md` — the `OsClient`, the one path to the operating system.
- `layout.md` — the directory tree, the dependency rules, protobuf generation, and where the documents live.
- `dependencies.md` — the direct module dependencies and why each is allowed.
- `csi.md` — dnv-csi, the CSI driver for Kubernetes, and its suite.

## The integration suites

The integration suites drive real binaries on lab VMs over ssh, one script
under `integtest/` each; what a suite proves, its topology and its cleanup
are its document's:

- `integtest/dnagent_test.sh` — `dnagent_integtest.md`
- `integtest/cnagent_test.sh` — `cnagent_integtest.md`
- `integtest/e2e_test.sh` — `e2e_integtest.md`
- `integtest/worker_test.sh` — `dnv-worker.md`, Integration test plan
- `integtest/gateway_test.sh` — `gateway.md`, Integration test plan
- `integtest/cdc_test.sh` — `cdc.md`, Integration test plan
- `integtest/dnvctl_test.sh` — `dnvctl.md`, Integration test plan
- `integtest/csi_test.sh` — `csi.md`, Integration test plan

`integtest/runner.sh` runs the suites one after another, as listed in
`integtest/runner.conf`, and ends with a summary of every run (`layout.md`,
Directory tree).

## Build and test

The build and test entry points are the `Makefile` targets; what each does
is the `Makefile` row of `layout.md`, Directory tree. `make test` runs
`go test ./doclint/` with every other test.

## Rules for docs and code comments

- The LLM docs under `doc/llm/` are the high-level guide the code follows;
  the code is the source of truth for every detail. The rules of this list
  are the LLM docs'; a human doc, `README.md` at the root or a file under
  `doc/human/`, is bound by `layout.md`, Human docs and LLM docs, alone.
- An LLM doc states only the current design, in present tense: roles,
  contracts, invariants, orderings and gates, and each decision with its
  reason. No history: no dates, no amendment or change logs, no "was",
  "previously", "amended" or "now".
- An LLM doc never restates what the code holds: no constant values, no
  line numbers, no signatures, no flag, test or log-record tables, no
  step-by-step test choreography, no copied source. It names a code
  identifier only if it exists in the tree.
- Every rule has exactly one owner doc. A component doc owns its component's
  behaviour; `architecture.md` owns the object model, naming, the etcd data
  model, allocation, validation and the procedures that span components.
  Any other doc cites the rule and never restates it.
- Rule ids (CN11, GW6, [D12]) are permanent: never renumbered; a new rule
  takes the next free number; a deleted id is retired forever. Headings are
  unnumbered, and nothing cites a section number, in a doc or a code
  comment.
- A citation is the doc's bare name with a rule id or a heading text, as in
  "`gateway.md` GW6" or "`architecture.md`, System overview", never a path
  and never a line number; the bold lead-in of a paragraph under the heading
  may follow the heading text. A code comment or a message names code by
  identifier, as in "ProbeSubsystem (agent/nvmet.go)", and carries no date
  and no citation of a note kept outside the repository.
- A change that alters a rule edits the owning doc in the same change and
  explains the change in the commit message, never in the doc.
- Undecided ideas and inconsistency lists live in `tmp_doc/`, which is
  untracked, never under `doc/`.
- `glossary.md` defines the project vocabulary; an LLM doc uses its terms
  and adds a term there when it needs a new one.
- `go test ./doclint/`, which `make test` also runs, holds the docs, the
  root README and the citations, pointers and dates of the code to these
  rules, and passes before a change is done.
