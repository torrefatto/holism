# Holism — Agent Swarm Framework Design

Status: **Draft for review** — design only, no implementation yet.

## 1. Context and objectives

Holism is a framework for cooperating AI agents. This document designs the first
core of the framework: a **swarm** composed of two kinds of actors, coordinated
over a shared gRPC contract, with every component replaceable and containerized.

Objectives:

1. **Simple, replaceable pieces.** Every external dependency (task storage,
   event transport, planning, execution, knowledge access) sits behind a small
   Go interface (a *port*). Swapping an implementation must not require touching
   the coordinator or the drones' business logic.
2. **Portability.** Each component ships as its own Docker container and can be
   orchestrated by any runtime that can start containers and let them reach each
   other over the network (Docker Compose locally, Nomad, a plain VM fleet, …).
   No component assumes a specific host, network, orchestrator, or cloud.
3. **Two actors.**
   - **Mind** — the central coordinator. It decomposes goals into tasks, assigns
     tasks to drones, tracks progress, and retries or re-plans on failure.
   - **Drone** — a worker. It registers with the mind, receives dispatched tasks,
     executes them (using knowledge connectors), and reports results.
4. **Shared contract.** Mind and drones speak gRPC and share a single protobuf
   contract, versioned under `proto/holism/v1`.
5. **Knowledge repository.** Agents reach a shared knowledge base (Linear,
   Notion, …) through **connectors that exist independently of this project**.
   Holism only defines the narrow port through which a drone consumes knowledge;
   connector implementations are injected, not built here.

Non-goals for this phase: LLM prompting strategy, connector implementations,
fine-grained authorization policy (transport authentication via mTLS is in
scope — see §6.3), multi-tenancy, and UI.

## 2. Architecture at a glance

```
              ┌──────────────────────────────────────────┐
              │        Mind (single instance)            │
              │  Planner ──► Scheduler ──► TaskStore     │
              │       ▲           │            │         │
              │       └──── EventBus ◄───────────┘        │
              │  serves MindService (gRPC + mTLS)         │
              └────────────────┬─────────────────────────┘
                               │ bidirectional mTLS gRPC
              ┌────────────────┼────────────────┐
              ▼                ▼                ▼
          ┌───────┐       ┌───────┐       ┌───────┐
          │ Drone │       │ Drone │       │ Drone │  … N drones
          │ serves│       │ serves│       │ serves│
          │ Drone-│       │ Drone-│       │ Drone-│
          │ Service│      │ Service│      │ Service│
          └───────┘       └───────┘       └───────┘
              │
              ▼ gRPC (connector's own auth — optional uniform facade)
   ┌────────────────────────────────────────┐
   │  Knowledge connectors (external to     │
   │  Holism): Linear, Notion, …            │
   └────────────────────────────────────────┘
```

Each box in the diagram is a container. Traffic between mind and drones is
**bidirectional gRPC over mTLS** (see §6.3): the mind calls drones to dispatch
tasks, drones call the mind to register, heartbeat, and report results.

The mind is a **single instance by design**. All coordination state — the task
store and the drone registry — lives in the mind, so running several minds
would require externalizing that state. High availability of the mind is
explicitly out of scope; if the mind dies, tasks in flight are lost until an
operator restarts it (the durable `TaskStore` port keeps `queued`/`succeeded`
history safe across restarts). Drones are homogeneous and stateless.

## 3. Ports (replaceable abstractions)

All ports are Go interfaces defined in the core packages. Adapters live behind
build-time wiring (composition root in `cmd/`). Replacing a port implementation
is a config change, not a code change.

| Port | Interface (sketch) | Reference adapters |
|---|---|---|
| `TaskStore` | `Create(ctx, Task) (Task, error)`, `Get(ctx, id)`, `Update(ctx, Task)`, `List(ctx, Filter)`, `Claim(ctx, workerID) (Task, bool)` | in-memory (dev), PostgreSQL |
| `EventBus` | `Publish(ctx, Event)`, `Subscribe(ctx, Filter) (<-chan Event, error)` | in-memory, NATS |
| `Planner` | `Plan(ctx, Goal) ([]TaskSpec, error)` | rule-based, LLM-based |
| `Executor` (drone) | `Execute(ctx, Task) (TaskResult, error)` | tool-calling agent, scripted |
| `Knowledge` (drone) | `Search(ctx, Query) ([]Document, error)`, `Get(ctx, ID)`, `Create(ctx, Document)` | Linear connector, Notion connector |
| `Clock` / `IDGen` | trivial ports to keep tests deterministic | real, fake |

Wiring rule: **core packages depend only on ports, never on adapters.** Adapters
import ports; the composition root (`cmd/mind`, `cmd/drone`) picks concrete
implementations from environment configuration.

## 4. Shared contract (protobuf)

One contract, one module, versioned:

```
proto/holism/v1/
├── common.proto     # Task, TaskSpec, TaskResult, TaskStatus, Artifact, Money/Time? (none — keep minimal)
├── events.proto     # Event envelope for EventBus-over-gRPC and subscriptions
├── mind.proto       # MindService — the coordinator's API
├── drone.proto      # DroneService — the worker's API
└── knowledge.proto  # KnowledgeService — optional uniform gRPC facade for connectors
```

Managed with **buf** (`buf.yaml`, lint + breaking-change checks in CI). Generated
Go code is produced by `buf generate` and committed under `api/holism/v1` so
consumers don't need protoc toolchains. Every service in this contract is served
and dialed over **mTLS** (see §6.3).

### 4.1 `MindService` (served by the mind)

```proto
service MindService {
  // --- drone-facing RPCs (drones are gRPC clients of the mind) ---
  rpc RegisterDrone(RegisterDroneRequest) returns (RegisterDroneResponse); // id, address, labels, capabilities
  rpc UnregisterDrone(UnregisterDroneRequest) returns (UnregisterDroneResponse);
  rpc Heartbeat(HeartbeatRequest) returns (HeartbeatResponse);             // carries load, liveness
  rpc ReportProgress(ReportProgressRequest) returns (ReportProgressResponse);
  rpc SubmitResult(SubmitResultRequest) returns (Task);
  // --- client-facing RPCs (operators, upstream systems) ---
  rpc SubmitTask(SubmitTaskRequest) returns (Task);        // submit a goal or a task
  rpc GetTask(GetTaskRequest) returns (Task);
  rpc ListTasks(ListTasksRequest) returns (ListTasksResponse);
  rpc CancelTask(CancelTaskRequest) returns (Task);
  // Server-streaming event feed for observability and reactive UIs.
  rpc SubscribeEvents(SubscribeEventsRequest) returns (stream Event);
}
```

### 4.2 `DroneService` (served by each drone, called by the mind)

```proto
service DroneService {
  // The mind dispatches a task with a gRPC call. The drone replies ACCEPTED
  // (and executes asynchronously) or REJECTED (e.g. at capacity); the mind
  // requeues and tries another drone on REJECTED.
  rpc AssignTask(AssignTaskRequest) returns (AssignTaskResponse);
  // The mind asks the drone to finish its in-flight task and exit cleanly.
  rpc Drain(DrainRequest) returns (DrainResponse);
  // Liveness/readiness, for the mind and the orchestrator.
  rpc Health(HealthRequest) returns (HealthResponse);
}
```

Assignment is a **gRPC call made by the mind**: the scheduler picks a ready
drone from its registry (heartbeated within the lease window, labels match) and
calls `DroneService.AssignTask` on the drone's registered address. The drone
executes the task and reports progress and the final result back to the mind
via `MindService.ReportProgress` / `MindService.SubmitResult`. `TaskStore.Claim`
remains available on the port as an alternative dispatch strategy, but the
default is mind-initiated calls.

### 4.3 Task lifecycle

```
queued ──► assigned ──► running ──► succeeded
   │          │            │
   │          │            ├─► failed ──► queued (retry, up to maxAttempts)
   │          │            │
   │          ▼            ▼
   └──────► cancelled ◄───┘
```

- `queued → assigned`: the scheduler dispatches the task to a ready drone
  (heartbeated within the lease window, labels match) via a
  `DroneService.AssignTask` call.
- `assigned → running`: drone acks the assignment.
- Heartbeats renew a lease. If a drone dies, its running tasks return to `queued`
  after lease expiry (at-least-once delivery; idempotency is the executor's
  responsibility — documented, enforced by convention).
- `maxAttempts` and backoff policy live on `TaskSpec`; the mind enforces them.

### 4.4 `KnowledgeService` (optional facade)

Connectors for Linear/Notion exist independently of Holism. To keep drone code
uniform, they may expose the same tiny contract:

```proto
service KnowledgeService {
  rpc Search(SearchRequest) returns (SearchResponse);
  rpc Get(GetRequest) returns (Document);
  rpc Create(CreateRequest) returns (Document);
}
```

The drone's `Knowledge` port has two adapters: a **gRPC client** (talks to any
connector exposing `KnowledgeService`) and a **direct client** (in-process
connector libraries). Drones resolve which knowledge sources to use from their
own configuration — knowledge access is **not** proxied through the mind, so
connectors stay independently deployable and the mind never sees credentials.

## 5. Component responsibilities

### 5.1 Mind (single instance)

- **Planner** (`Planner` port): turns a submitted goal into an ordered set of
  `TaskSpec`s (initially rule-based; LLM-based planner is a drop-in adapter).
- **Scheduler**: picks a ready drone from the in-memory registry (heartbeated
  within the lease window, labels match) and dispatches the task with a
  `DroneService.AssignTask` call; on REJECTED or call failure it requeues and
  tries another drone. Owns leases and retries.
- **TaskStore** (`TaskStore` port): single source of truth for task state.
  Backed by the in-memory adapter for v1; the port allows a durable adapter
  (PostgreSQL) later so task history survives mind restarts.
- **Drone registry**: in-memory, built from `RegisterDrone`/`Heartbeat`;
  entries expire after a missed-heartbeat timeout. Single-instance by design —
  the registry is why the mind must not be replicated.
- **EventBus** (`EventBus` port): publishes lifecycle events; backs
  `SubscribeEvents` and future reactive behaviors.
- **gRPC server**: serves `MindService` over mTLS; dials each drone's
  `DroneService` over mTLS.

### 5.2 Drone

- **Mind client**: registers with the mind (`RegisterDrone`, carrying its
  dialable address, labels, capabilities), heartbeats, reports progress and
  results; handles `UnregisterDrone` on shutdown.
- **gRPC server**: serves `DroneService` over mTLS — this is how the mind
  reaches the drone to dispatch tasks (`AssignTask`) and drain it. Every drone
  must therefore be network-reachable from the mind.
- **Assignment handler**: on `AssignTask`, validates capacity, replies
  ACCEPTED or REJECTED, and executes accepted tasks asynchronously.
- **Executor** (`Executor` port): the actual "do the work" unit. Default
  adapter: a tool-calling agent whose tools include the `Knowledge` port.
- **Knowledge** (`Knowledge` port): read/write access to Linear, Notion, …
  through injected connectors.

Drones are stateless and disposable: no local persistence beyond an in-flight
task; restart = re-register with a fresh identity.

## 6. Deployment and portability

- **One process per container.** Images: `holism/mind`, `holism/drone`.
  Connectors ship as their own images (`holism-connector-linear`,
  `holism-connector-notion`, …) maintained outside this repo.
- **Configuration is 100% environment variables** (12-factor): listen
  addresses, adapter selection, store DSNs, connector endpoints, and paths to
  TLS material, with credentials via env-from-secret.
- **Runtime-agnostic.** Holism does not assume any orchestrator. The only
  requirement is that every component can reach the others over the network:
  the mind must reach each drone's `DroneService` address, and drones must
  reach the mind's `MindService` address. Locally this is
  `deploy/docker-compose.yaml`; in production it can be any container runtime
  (Nomad, systemd-nspawn, a VM fleet with static addresses, …).
- **Observability**: OpenTelemetry traces/metrics from every gRPC boundary
  (interceptors), structured logs, `/healthz` + `/readyz` on all services.

### 6.1 PKI and certificate management

Mind and drones authenticate each other with **mutual TLS**, using certificates
issued by a Holism-internal PKI:

- One **root CA** per swarm deployment (long-lived, offline key recommended).
- The mind holds a server certificate; every drone holds a **client+server
  certificate** (drone-to-mind calls and mind-to-drone calls are mutually
  authenticated). Certificates carry the component identity (`mind`,
  `drone:<id>`) in the SAN/CN and are used for both authentication and, later,
  coarse authorization (e.g. only `mind` may call `AssignTask`).
- Certificates are mounted into containers as files (or injected by the
  runtime's secret mechanism) and referenced by path via env config.

### 6.2 Setup CLI (`holism`)

A small CLI, shipped as `holism/cli` (single static binary), bootstraps and
maintains the PKI and the swarm:

```
holism init                  # generate root CA (key + cert) into ./pki/
holism cert issue mind       # issue the mind's server certificate
holism cert issue drone <id> # issue a drone's client+server certificate
holism cert renew <name>     # re-issue before expiry
holism cert revoke <name>    # revoke (CRL / short-lived certs)
holism cert list             # show issued certs and expiry
```

Design choices: short-lived leaf certificates (e.g. 24–72 h) with renewal, so
revocation is rarely needed; the CA key never leaves the machine running
`holism init`; the CLI only ever reads/writes the `./pki/` directory, which the
operator distributes to containers out of band. Generated material is git-ignored.

### 6.3 mTLS transport contract

- Both sides present certificates; gRPC `RequireAndVerifyClientCert` /
  `tls.RequireAndVerifyClientCert` on servers, verified root CA pool on clients.
- Identity is taken from the peer certificate (SPIFFE-style URI SAN
  `spiffe://holism/<mind|drone>/<id>` is the target format; plain DNS SANs for
  v1).
- Mind → drone and drone → mind connections are both mTLS; there is no plaintext
  listener. `/healthz` is served on the same mTLS listener.
- Knowledge connectors are out of the mTLS scope: they use their own
  authentication (API tokens), owned by the connector deployments.

## 7. Repository layout (proposal)

```
holism/
├── proto/holism/v1/          # the shared contract (buf module)
├── api/holism/v1/            # generated Go (committed)
├── mind/
│   ├── cmd/mind/             # composition root
│   └── internal/
│       ├── planner/          # port + adapters
│       ├── scheduler/
│       ├── taskstore/        # port + adapters (memory, postgres)
│       ├── eventbus/         # port + adapters (memory, nats)
│       └── server/           # MindService gRPC server
├── drone/
│   ├── cmd/drone/            # composition root
│   └── internal/
│       ├── agent/            # register/heartbeat/assignment loop
│       ├── executor/         # port + adapters
│       └── knowledge/        # port + adapters (grpc client, direct)
├── cli/                      # the `holism` setup CLI (PKI, cert lifecycle)
│   └── cmd/holism/
├── connectors/               # contract conformance tests for external connectors
├── deploy/
│   └── docker-compose.yaml   # local dev only; production runtime is operator's choice
├── docs/design/              # this document
└── buf.yaml, go.work, Makefile
```

Go workspace (`go.work`) with one module per component (`mind`, `drone`,
`cli`, `api`) so ports can evolve independently while sharing the contract.

## 8. Open questions for review

1. **Delivery semantics**: at-least-once with executor idempotency (chosen) vs
   exactly-once via transactional outbox — confirm at-least-once is acceptable.
2. **Connector auth ownership**: connectors read credentials from their own env
   (chosen); the mind never handles connector secrets.
3. **Goal decomposition**: is `Planner.Plan(Goal) → []TaskSpec` the right first
   cut, or should v1 accept only pre-decomposed tasks and add goals later?
4. **Streaming granularity**: `ReportProgress` as discrete messages (chosen) vs
   a bidi stream per task for token-level progress.
5. **Drone addressing**: drones register a dialable address (chosen). In
   NATed/private networks the mind may not reach drones — is a drone-initiated
   reverse connection acceptable as a fallback?
6. **Certificate lifetime**: short-lived leaves with renewal (chosen, 24–72 h)
   vs longer-lived certs with a CRL.

## 9. Roadmap

1. **Phase 1 — skeleton**: buf module + contract, the `holism` CLI (`init` +
   `cert issue`), in-memory adapters for every port, single mind + N drones
   over docker-compose with mTLS, happy-path task dispatch via
   `AssignTask`.
2. **Phase 2 — real adapters**: Postgres `TaskStore`, NATS `EventBus`,
   LLM-planner adapter, gRPC `Knowledge` client; conformance tests for
   connectors; cert renewal automation.
3. **Phase 3 — hardening**: drain/graceful-shutdown semantics, certificate
   identity-based coarse authorization, richer observability, durable store
   failover runbook for the single-instance mind.

## 10. Glossary

- **Mind**: central coordinator; plans, schedules, dispatches, tracks. Single
  instance by design.
- **Drone**: stateless worker; serves `DroneService`, executes dispatched tasks.
- **Port**: a small Go interface hiding an external dependency.
- **Adapter**: a concrete implementation of a port.
- **Contract**: the versioned protobuf module shared by all components.
- **Connector**: an external service (Linear, Notion, …) giving agents access
  to a knowledge repository; pluggable via the `Knowledge` port.
- **PKI**: the Holism-internal certificate authority and the certificates it
  issues for mTLS between mind and drones, managed by the `holism` CLI.
