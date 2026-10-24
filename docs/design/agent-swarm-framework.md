# Holism — Agent Swarm Framework Design

Status: **Draft for review** — design only, no implementation yet.

## 1. Context and objectives

Holism is a framework for cooperating AI agents. This document designs the first
core of the framework: a **swarm** composed of two kinds of actors, coordinated
over a shared gRPC contract, with every component replaceable and containerized.

Objectives:

1. **Simple, replaceable pieces.** Every external dependency (task storage,
   event transport, planning, execution) sits behind a small Go interface (a
   *port*). Swapping an implementation must not require touching the
   coordinator or the drones' business logic. Ports may be backed by Holism's
   own infrastructure **or delegated to external systems** (e.g. tasks stored as
   Linear tickets, events delivered to a Slack channel).
2. **Portability.** Each component ships as its own Docker container and can be
   orchestrated by any runtime that can start containers and give them outbound
   network access (Docker Compose locally, Nomad, a plain VM fleet, …). No
   component assumes a specific host, network, orchestrator, or cloud.
3. **Two actors.**
   - **Mind** — the central coordinator. It decomposes project goals into tasks,
     submits tasks to drones, tracks progress, and retries or re-plans on
     failure.
   - **Drone** — a worker. It connects to the mind, declares itself able to
     receive tasks (with its capabilities), executes them, and reports progress
     and results.
4. **Shared contract.** Mind, drones, and human operators all speak gRPC to the
   mind, using a single protobuf contract, versioned under `proto/holism/v1`.
5. **The mind is the only well-known endpoint.** Drones and human operators
   connect *to* the mind; nothing needs to reach a drone. The mind's gRPC API is
   the single entry point of the swarm.
6. **Knowledge is delegated to the agents.** Both the mind and the drones are
   assumed to be able to read and write the shared knowledge repository
   (Linear, Notion, …) through **knowledge harnesses that live outside this
   project and are injected into each agent**. Holism defines no knowledge
   contract and proxies nothing: knowledge access is an internal concern of each
   agent's planner/executor.

Non-goals for this phase: LLM prompting strategy, knowledge harness
implementations, fine-grained authorization policy (transport authentication
via mTLS is in scope — see §6.3), multi-tenancy, and UI.

## 2. Architecture at a glance

```
                 ┌────────────────────────────────────────┐
                 │       Mind (single instance)           │
                 │  Planner ──► Scheduler ──► TaskStore*  │
                 │       ▲           │           │        │
                 │       └──── EventBus* ◄───────┘        │
                 │  gRPC API (mTLS) — the only endpoint   │
                 └───────────┬───────────────┬────────────┘
             bidi stream     │               │  plain RPCs
        ┌────────────────────┘               └───────────┐
        │  (Connect: register, task submit/ack,        │ (projects, tasks,
        │   progress, result, heartbeat)               │  events)
        ▼                                              ▼
   ┌─────────┐   ┌─────────┐   ┌─────────┐        ┌──────────┐
   │ Drone 1 │   │ Drone 2 │   │ Drone N │  …     │ Operator │
   │ (agent) │   │ (agent) │   │ (agent) │        │ (human)  │
   └─────────┘   └─────────┘   └─────────┘        └──────────┘
        │
        ▼ knowledge harness (agent-internal, outside Holism)
   ┌────────────────────────────────────────┐
   │  Knowledge repository: Linear, Notion… │
   └────────────────────────────────────────┘

   * TaskStore and EventBus are ports: they may be backed by external
     systems (Linear board, Slack channel) instead of mind-owned services.
```

Each box is a container. **Only the mind needs to be network-reachable.**
Drones and the human operator dial out to the mind's gRPC API over mTLS (see
§6.3); drones never accept inbound connections.

The mind is a **single instance by design**. All coordination state — the
in-memory view of the task store and the drone registry — lives in the mind, so
running several minds would require externalizing that state. High availability
of the mind is explicitly out of scope; if the mind dies, tasks in flight are
lost until an operator restarts it. Drones are homogeneous and stateless.

## 3. Ports (replaceable abstractions)

All ports are Go interfaces defined in the core packages. Adapters live behind
build-time wiring (composition root in `cmd/`). Replacing a port implementation
is a config change, not a code change.

| Port | Interface (sketch) | Reference adapters |
|---|---|---|
| `TaskStore` | `CreateProject(ctx, Project)`, `GetProject(ctx, id)`, `ListProjects(ctx)`, `Create(ctx, Task)`, `Get(ctx, id)`, `Update(ctx, Task)`, `List(ctx, Filter)` | in-memory (dev), PostgreSQL, **Linear (tasks as tickets on a board, projects as boards)** |
| `EventBus` | `Publish(ctx, Event)`, `Subscribe(ctx, Filter) (<-chan Event, error)` | in-memory, NATS, **Slack connector (each event becomes a message on a channel)** |
| `Planner` (mind) | `Plan(ctx, Project, Goal) ([]TaskSpec, error)` | rule-based, LLM-based |
| `Executor` (drone) | `Execute(ctx, Task) (stream Progress, error)` | tool-calling agent, scripted |
| `Clock` / `IDGen` | trivial ports to keep tests deterministic | real, fake |

Two delegation notes:

- **`TaskStore` may live outside the mind.** The mind only talks to the port.
  With the Linear adapter, tasks are tickets on a board and projects are boards;
  the mind's scheduler still owns assignment state in memory, while durable
  task state lives in Linear. A PostgreSQL adapter keeps everything in a
  Holism-owned database. Both are interchangeable by configuration.
- **`EventBus` may be a notification pipeline.** With the Slack connector
  adapter, publishing an event means posting a message to a channel. Events
  carry project/task ids and a human-readable summary so external adapters can
  render them without knowing Holism internals.

There is deliberately **no knowledge port**: knowledge harnesses are wired
straight into the planner (mind) and the executor (drone) at the composition
root, as part of the agent's own toolset. Holism neither defines nor proxies
knowledge access.

Wiring rule: **core packages depend only on ports, never on adapters.** Adapters
import ports; the composition root (`cmd/mind`, `cmd/drone`) picks concrete
implementations from environment configuration.

### 3.1 Projects

A **project** groups tasks into something with context:

- `id`, `name`
- `goal` — why the project exists; this is what the planner decomposes
- `context` — free-form context for agents (links, docs, constraints, repo
  names), surfaced to drones inside every task of the project
- `labels` — for filtering and for scheduler matching hints

The contract **enforces that every task belongs to a project**: `Task` and
`SubmitTaskRequest` carry a `project_id` that is required — declared with
`buf.validate` (`min_len: 1`) so generated validation code rejects a task
without a project at the API boundary, and the mind additionally validates it
server-side (referential check against the `TaskStore`). Tasks can never exist
outside a project. Operators create projects and submit goals/tasks within
them; drones see the project context when executing. Projects are stored
through the `TaskStore` port like tasks.

## 4. Shared contract (protobuf)

One contract, one module, versioned:

```
proto/holism/v1/
├── common.proto     # Project, Task (project_id required), TaskSpec, Capability, TaskResult, TaskStatus, Ack, Artifact (keep minimal)
├── events.proto     # Event envelope for SubscribeEvents
└── mind.proto       # MindService — the swarm's single gRPC API
```

Managed with **buf** (`buf.yaml`, lint + breaking-change checks in CI). Generated
Go code is produced by `buf generate` and committed under `api/holism/v1` so
consumers don't need protoc toolchains. Every RPC in this contract is served by
the mind and dialed over **mTLS** (see §6.3).

### 4.1 `MindService` (the swarm's only gRPC API)

```proto
service MindService {
  // --- drone channel: one durable bidirectional stream per drone ---
  // The drone opens it, registers (with its capabilities), and declares itself
  // ready to receive tasks. The mind submits tasks and receives acks, progress,
  // and results on the same stream. See §4.2 for the message flow.
  rpc Connect(stream DroneMessage) returns (stream MindMessage);

  // --- projects ---
  rpc CreateProject(CreateProjectRequest) returns (Project);
  rpc GetProject(GetProjectRequest) returns (Project);
  rpc ListProjects(ListProjectsRequest) returns (ListProjectsResponse);

  // --- tasks (every task lives inside a project) ---
  rpc SubmitTask(SubmitTaskRequest) returns (Task);   // project_id is required (buf.validate)
  rpc GetTask(GetTaskRequest) returns (Task);
  rpc ListTasks(ListTasksRequest) returns (ListTasksResponse);  // project_id required in the filter
  rpc CancelTask(CancelTaskRequest) returns (Task);

  // --- observability ---
  rpc SubscribeEvents(SubscribeEventsRequest) returns (stream Event);
}
```

### 4.2 The drone channel (`Connect`)

One long-lived **bidirectional stream** per drone. All drone↔mind traffic —
registration, task submission, acks, progress, results, heartbeats — flows over
this single connection, so a drone only ever opens one outbound connection.

Message envelopes (oneof payloads, so the contract can grow without breaking
changes). Every `DroneMessage` carries a client-generated `id` so the mind can
acknowledge status updates (§4.2.1):

```proto
message DroneMessage {            // drone → mind
  string id = 1;                  // client-generated, referenced by mind Acks
  oneof payload {
    Register      register = 2;   // first message on the stream
    TaskAck       task_ack = 3;   // acks a Task submitted by the mind
    Progress      progress = 4;   // "working on it" / "done"
    Result        result = 5;     // terminal payload for a task
    Heartbeat     heartbeat = 6;  // keepalive + current load
    Bye           bye = 7;       // graceful unregister before closing
  }
}

message MindMessage {             // mind → drone
  string id = 1;
  oneof payload {
    Task          task = 2;       // a submitted task to execute
    Cancel        cancel = 3;     // task_id — stop and report
    Drain         drain = 4;      // finish in-flight work, then close
    Pong          pong = 5;       // heartbeat reply
    Ack           ack = 6;        // acks a Progress/Result, by DroneMessage.id
  }
}
```

Payload messages (in `common.proto`):

```proto
message Capability {              // an agent skill, declared at registration
  string name = 1;                 // short name, e.g. "tool:kubectl"
  string description = 2;          // what the skill actually does, in detail
}

message Register {
  string drone_id = 1;
  repeated string labels = 2;         // free-form, e.g. "gpu", "region:eu"
  repeated Capability capabilities = 3;
}

message TaskAck {                 // drone → mind, acks a submitted Task
  string task_id = 1;
  bool accepted = 2;              // false ⇒ REJECTED (e.g. at capacity)
  string reason = 3;              // rejection detail
}

message Progress {
  string task_id = 1;
  enum Status { WORKING = 0; DONE = 1; }  // deliberately minimal
  Status status = 2;
  string note = 3;                // optional human-readable detail
}

message Result {
  string task_id = 1;
  string output = 2;              // summary, artifacts, links
  string error = 3;               // non-empty ⇒ mind will typically mark FAILED
}

message Ack {                     // mind → drone, acks a DroneMessage by id
  string ref = 1;                 // DroneMessage.id being acknowledged
}
```

Connection flow:

```
Drone                          Mind
  │ ── Connect() (mTLS) ───────►│
  │ ── Register{id, labels,      │   drone enters the registry as READY
  │    capabilities} ──────────► │
  │ ◄── Pong ───────────────────│
  │                             │
  │ ◄────── Task{task_id} ─────│   scheduler: labels + required capabilities match
  │ ── TaskAck{task_id,         │   task marked ASSIGNED — only now (§4.2.1)
  │    accepted=true} ─────────►│
  │ ── Progress{WORKING} ──────►│   task RUNNING
  │ ◄────── Ack{ref} ──────────│
  │ ── Progress{WORKING} ──────►│   retried until acked (§4.2.2)
  │ ── Progress{DONE} ─────────►│
  │ ── Result{task_id, …} ─────►│   mind examines the result and sets
  │ ◄────── Ack{ref} ──────────│   SUCCEEDED / FAILED (§4.2.3)
  │                             │
  │ ◄────── Drain ─────────────│   graceful shutdown: finish, then close
  │ ── Bye ────────────────────►│
```

#### 4.2.1 Exactly-once assignment

A task is marked **assigned only when the drone's `TaskAck(accepted=true)`
arrives on the stream**. Until then the task stays `queued` in the store; if no
ack arrives within a dispatch timeout, the mind requeues it and may submit it to
a different ready drone. Drones deduplicate by `task_id` (a repeated `Task` for
a task already in flight is ignored). Net effect: each task is *assigned* to
exactly one drone at a time, at-least-once *delivery* of the submission, and
executor-level idempotency remains the drone's responsibility.

#### 4.2.2 Status updates are retried until acked

Progress and Result are **reliable**: the drone keeps each unacked status
update in a small retry buffer and resends it with backoff until the mind's
`Ack{ref}` arrives. The mind acks idempotently (acking the same `DroneMessage.id`
twice is harmless). Task submission acks (`TaskAck`) are *not* retried by the
drone — the mind owns that timeout and resubmits (§4.2.1).

#### 4.2.3 Minimal progress, mind decides the final status

The drone only reports `WORKING` ("working on it") or `DONE`, plus the terminal
`Result`. The mind sets `RUNNING` on the first `WORKING`, and when the `Result`
arrives it **examines the result** (output, error, artifacts) and decides the
final status — `SUCCEEDED` or `FAILED` — possibly involving the planner for
re-planning on failure.

#### 4.2.4 Reconnects

If the stream breaks, the drone re-dials with backoff and re-registers with the
same identity; re-registering with an existing `drone_id` replaces the previous
stream. Tasks still marked assigned/running for that drone return to `queued`
after lease expiry (heartbeats renew the lease).

### 4.3 Capabilities (agent skills)

Agents declare their **capabilities** at registration. A capability is a
**short name** plus a **detailed description** of what the skill does:

| Name (short) | Description (example) |
|---|---|
| `tool:kubectl` | Can run `kubectl` against the production cluster (read and apply) |
| `vcs:git-push` | Can push branches and open pull requests on the org's GitHub repos |
| `vcs:sign-commits` | Can sign commits with the team's GPG/SSH signing key |
| `notify:slack` | Can post messages to the `#eng-alerts` Slack channel |

The name is the machine-comparable handle: a task's
`TaskSpec.required_capabilities` lists capability **names**, and the scheduler
only submits a task to a READY drone whose declared capability names are a
superset of the required ones (in addition to label matching). The description
is human/agent-facing — it lets a planner (or a human) understand what a skill
entails when decomposing goals or choosing drones. Namespacing by prefix
(`tool:`, `vcs:`, …) is a convention, not enforced by the contract. An empty
`required_capabilities` means any drone may run the task.

### 4.4 Task lifecycle

```
queued ──► assigned ──► running ──► succeeded
   │          │            │
   │          │            ├─► failed ──► queued (retry, up to maxAttempts)
   │          │            │
   │          ▼            ▼
   └──────► cancelled ◄───┘
```

- `queued → assigned`: the scheduler picks a READY drone (registered,
  heartbeated within the lease window, labels match, **capabilities cover the
  task's required capabilities**) and sends `Task` on that drone's stream. The
  transition happens **only** on the drone's `TaskAck(accepted=true)`.
- `assigned → running`: first `Progress{WORKING}`.
- `running → succeeded | failed`: the `Progress{DONE}` + `Result` pair; the mind
  examines the result and sets the final status.
- If the stream drops, tasks assigned/running on it return to `queued` after
  lease expiry.
- `maxAttempts` and backoff policy live on `TaskSpec`; the mind enforces them.

## 5. Component responsibilities

### 5.1 Mind (single instance)

- **Planner** (`Planner` port): turns a project's goal into an ordered set of
  `TaskSpec`s (initially rule-based; LLM-based planner is a drop-in adapter),
  assigning each task the `required_capabilities` (names) it needs. The planner
  itself may use a knowledge harness to ground its plans.
- **Scheduler**: watches the `TaskStore` for `queued` tasks and the registry for
  READY drones; submits `Task` messages onto the chosen drone's stream and
  tracks pending acks. Matches on labels **and capabilities**. Owns dispatch
  timeouts, leases, and retries.
- **TaskStore** (`TaskStore` port): projects and tasks. May be backed by an
  external system (Linear) — the mind never assumes it owns the storage.
- **Drone registry**: in-memory, keyed by `drone_id`, holding the live stream
  (or its send queue), labels, capabilities, and readiness/lease state. Built
  from `Register` and renewed by `Heartbeat`; entries expire after a
  missed-heartbeat timeout. Single-instance by design — the registry is why the
  mind must not be replicated.
- **EventBus** (`EventBus` port): publishes lifecycle events; backs
  `SubscribeEvents` and may deliver them to external systems (e.g. a Slack
  channel).
- **gRPC server**: serves `MindService` over mTLS — the only listener in the
  swarm. Also exposes `/healthz` + `/readyz` on the same mTLS listener.

### 5.2 Drone

- **Mind client**: dials the mind, opens the `Connect` bidi stream, sends
  `Register` (id, labels, **capabilities** with names and descriptions), then
  heartbeats. Reconnects with backoff on stream failure and re-registers with
  the same identity.
- **Inbound loop**: reads `MindMessage`s from the stream — `Task` (validate
  capacity, reply `TaskAck`; deduplicate by `task_id`), `Cancel`, `Drain`
  (finish in-flight work, send `Bye`, exit).
- **Executor** (`Executor` port): the actual "do the work" unit. Default
  adapter: a tool-calling agent whose tools include the injected knowledge
  harnesses and whatever the declared capabilities imply (e.g. a `kubectl` tool
  for drones advertising `tool:kubectl`).
- **Reporting**: sends `Progress{WORKING|DONE}` and the terminal `Result` on the
  same stream, retrying each until the mind acks it.

Drones are stateless and disposable: no local persistence beyond an in-flight
task and the unacked-status retry buffer; no inbound listener; restart =
reconnect and re-register.

## 6. Deployment and portability

- **One process per container.** Images: `holism/mind`, `holism/drone`.
- **Configuration is 100% environment variables** (12-factor): the mind's
  listen address, adapter selection (including which TaskStore/EventBus
  backends), store DSNs or connector endpoints, and paths to TLS material,
  with credentials via env-from-secret.
- **Runtime-agnostic.** Holism does not assume any orchestrator. The only
  networking requirement is that **the mind is reachable from everywhere** —
  drones and operators dial out to it; drones may sit behind NAT/firewalls.
  Locally this is `deploy/docker-compose.yaml`; in production it can be any
  container runtime (Nomad, systemd-nspawn, a VM fleet, …).
- **Observability**: OpenTelemetry traces/metrics from every gRPC boundary
  (interceptors), structured logs, `/healthz` + `/readyz`.

### 6.1 PKI and certificate management

Drones and operators authenticate to the mind with **mutual TLS**, using
certificates issued by a Holism-internal PKI:

- One **root CA** per swarm deployment (long-lived, offline key recommended).
- The mind holds a **server certificate**; every drone (and operator client)
  holds a **client certificate** identifying it (`drone:<id>`, `operator:<id>`)
  in the SAN/CN, valid for about **one month** and renewed via the CLI.
- Certificates are mounted into containers as files (or injected by the
  runtime's secret mechanism) and referenced by path via env config.

### 6.2 Setup CLI (`holism`)

A small CLI, shipped as `holism/cli` (single static binary), bootstraps and
maintains the PKI:

```
holism init                      # generate root CA (key + cert) into ./pki/
holism cert issue mind           # issue the mind's server certificate
holism cert issue drone <id>     # issue a drone's client certificate
holism cert issue operator <id>  # issue an operator's client certificate
holism cert renew <name>         # re-issue before expiry (~1 month lifetime)
holism cert revoke <name>        # revoke (CRL / short-lived certs)
holism cert list                 # show issued certs and expiry
```

Design choices: leaf certificates live for about one month and are renewed with
`holism cert renew` before expiry; the CA key never leaves the machine running
`holism init`; the CLI only ever reads/writes the `./pki/` directory, which the
operator distributes to containers out of band. Generated material is git-ignored.

### 6.3 mTLS transport contract

- Drones and operators present client certificates; the mind requires and
  verifies them (`tls.RequireAndVerifyClientCert`, verified root CA pool).
- Identity is taken from the peer certificate (SPIFFE-style URI SAN
  `spiffe://holism/<mind|drone|operator>/<id>` is the target format; plain DNS
  SANs for v1). Identity is used for registry keys today and for coarse
  authorization later (e.g. only `drone:*` certs may open `Connect`).
- The mind's listener is mTLS-only; there is no plaintext listener. `/healthz`
  is served on the same mTLS listener.
- Knowledge repositories are out of the mTLS scope: they use their own
  authentication (API tokens), owned by the knowledge harnesses inside each
  agent.

## 7. Repository layout (proposal)

```
holism/
├── proto/holism/v1/          # the shared contract (buf module)
├── api/holism/v1/            # generated Go (committed)
├── mind/
│   ├── cmd/mind/             # composition root
│   └── internal/
│       ├── planner/          # port + adapters
│       ├── scheduler/        # label + capability matching, dispatch, leases
│       ├── taskstore/        # port + adapters (memory, postgres, linear)
│       ├── eventbus/         # port + adapters (memory, nats, slack)
│       ├── registry/         # drone registry (live streams, leases)
│       └── server/           # MindService gRPC server (incl. Connect)
├── drone/
│   ├── cmd/drone/            # composition root
│   └── internal/
│       ├── agent/            # connect/register/heartbeat/reconnect loop
│       └── executor/         # port + adapters
├── cli/                      # the `holism` setup CLI (PKI, cert lifecycle)
│   └── cmd/holism/
├── deploy/
│   └── docker-compose.yaml   # local dev only; production runtime is operator's choice
├── docs/design/              # this document
└── buf.yaml, go.work, Makefile
```

Go workspace (`go.work`) with one module per component (`mind`, `drone`,
`cli`, `api`) so components can evolve independently while sharing the contract.

## 8. Resolved design decisions

1. **Assignment is exactly-once.** The drone must acknowledge (`TaskAck`) for the
   mind to mark a task as assigned. No ack within the dispatch timeout ⇒ the
   task stays queued and may be submitted to another drone; drones dedupe by
   `task_id`.
2. **The planner splits goals into tasks.** `Planner.Plan(ctx, Project, Goal) →
   []TaskSpec` is the right first cut.
3. **Progress is minimal.** The drone only reports "working on it" (`WORKING`)
   or "done" (`DONE`); the mind examines the result and decides the final task
   status.
4. **Status updates are reliable.** The drone retries progress/result messages
   until the mind acks them (`Ack{ref}`).
5. **Certificate lifetime is ~1 month**, renewed via `holism cert renew`.
6. **Operators authenticate with client certificates**, same mTLS path as
   drones.

Open questions that remain:

1. **Capability taxonomy**: free-form strings with a prefix convention (chosen)
   vs a registry of well-known capabilities enforced by the contract.
2. **Project context schema**: free-form string (chosen) vs structured fields
   (repo, docs, constraints).

## 9. Roadmap

1. **Phase 1 — skeleton**: buf module + contract (`MindService` with `Connect`,
   projects, capabilities), the `holism` CLI (`init` + `cert issue`), in-memory
   adapters for every port, single mind + N drones over docker-compose with
   mTLS, happy-path task dispatch over the drone stream.
2. **Phase 2 — external adapters**: Linear-backed `TaskStore` (tasks as
   tickets), Slack-backed `EventBus`, PostgreSQL `TaskStore`, NATS `EventBus`,
   LLM-planner adapter, cert renewal automation, executor idempotency helpers.
3. **Phase 3 — hardening**: drain/graceful-shutdown semantics, certificate
   identity-based coarse authorization, richer observability, failover runbook
   for the single-instance mind.

## 10. Glossary

- **Mind**: central coordinator; plans, schedules, submits, tracks. Single
  instance by design; the swarm's only network endpoint.
- **Drone**: stateless worker; connects to the mind over a durable bidi stream
  and executes submitted tasks.
- **Project**: a group of tasks with shared context (goal, description,
  labels); the unit the planner decomposes.
- **Capability**: an agent skill declared at registration — a short name (e.g.
  `tool:kubectl`, `vcs:git-push`) plus a detailed description; tasks declare
  required capability names and the scheduler matches them.
- **Port**: a small Go interface hiding an external dependency.
- **Adapter**: a concrete implementation of a port.
- **Contract**: the versioned protobuf module shared by all components.
- **Knowledge harness**: an agent-internal component (outside Holism) giving an
  agent read/write access to the knowledge repository (Linear, Notion, …).
  Injected into the planner (mind) and executor (drone); never proxied by Holism.
- **PKI**: the Holism-internal certificate authority and the certificates it
  issues for mTLS, managed by the `holism` CLI.
