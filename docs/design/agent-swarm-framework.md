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
2. **Portability.** Each component ships as its own Docker container and is
   orchestrated by a generic runtime (Docker Compose locally, Kubernetes in
   production). No component assumes a specific host, network, or cloud.
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
authentication/authorization policy, multi-tenancy, and UI.

## 2. Architecture at a glance

```
                ┌─────────────────────────────────────────────┐
                │                    Mind                     │
                │  Planner ──► Scheduler ──► TaskStore       │
                │       ▲            │              │         │
                │       └──── EventBus ◄────────────┘         │
                │  gRPC: MindService (SubmitTask, Subscribe…)  │
                └───────────┬─────────────────────────────────┘
                            │  gRPC (shared contract, proto/holism/v1)
            ┌───────────────┼───────────────┐
            ▼               ▼               ▼
        ┌───────┐       ┌───────┐       ┌───────┐
        │ Drone │       │ Drone │       │ Drone │   … N drones
        │Executor│      │Executor│      │Executor│
        └───┬───┘       └───┬───┘       └───┬───┘
            │ gRPC: DroneService (Register, StreamAssignments, SubmitResult)
            │
            ▼ gRPC (optional uniform access)
   ┌────────────────────────────────────────┐
   │  Knowledge connectors (external to     │
   │  Holism): Linear, Notion, …            │
   └────────────────────────────────────────┘
```

Each box in the diagram is a container. The mind is a single logical component
(horizontally scalable later behind a leader-election-capable task store). Drones
are homogeneous and stateless; all durable state lives in the mind's ports.

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
consumers don't need protoc toolchains.

### 4.1 `MindService` (served by the mind)

```proto
service MindService {
  // Client (e.g. human operator, upstream system) submits a goal or a task.
  rpc SubmitTask(SubmitTaskRequest) returns (Task);
  rpc GetTask(GetTaskRequest) returns (Task);
  rpc ListTasks(ListTasksRequest) returns (ListTasksResponse);
  rpc CancelTask(CancelTaskRequest) returns (Task);
  // Server-streaming event feed for observability and reactive UIs.
  rpc SubscribeEvents(SubscribeEventsRequest) returns (stream Event);
}
```

### 4.2 `DroneService` (served by each drone)

```proto
service DroneService {
  rpc Register(RegisterRequest) returns (RegisterResponse);   // capabilities, labels
  rpc Unregister(UnregisterRequest) returns (UnregisterResponse);
  rpc Heartbeat(HeartbeatRequest) returns (HeartbeatResponse); // carries load, liveness
  // Server-streaming assignment feed: the mind pushes dispatched tasks.
  rpc StreamAssignments(StreamAssignmentsRequest) returns (stream TaskAssignment);
  rpc AckAssignment(AckAssignmentRequest) returns (AckAssignmentResponse);
  rpc ReportProgress(ReportProgressRequest) returns (ReportProgressResponse);
  rpc SubmitResult(SubmitResultRequest) returns (Task);
}
```

Assignment is **push-based over a server stream** (mind → drone) with an
acknowledgement handshake, and **pull (`Claim`) remains available on the
`TaskStore` port** as a fallback adapter strategy. The default is push: drones
stay connected, the mind dispatches to the least-loaded ready drone.

### 4.3 Task lifecycle

```
queued ──► assigned ──► running ──► succeeded
   │          │            │
   │          │            ├─► failed ──► queued (retry, up to maxAttempts)
   │          │            │
   │          ▼            ▼
   └──────► cancelled ◄───┘
```

- `queued → assigned`: scheduler claims a task for a drone that heartbeated
  within the lease window and matches the task's required labels.
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

### 5.1 Mind

- **Planner** (`Planner` port): turns a submitted goal into an ordered set of
  `TaskSpec`s (initially rule-based; LLM-based planner is a drop-in adapter).
- **Scheduler**: matches `queued` tasks to ready drones by labels/capacity;
  owns leases and retries.
- **TaskStore** (`TaskStore` port): single source of truth for task state.
- **EventBus** (`EventBus` port): publishes lifecycle events; backs
  `SubscribeEvents` and future reactive behaviors.
- **gRPC server**: serves `MindService`; tracks drone registry (from
  `Register`/`Heartbeat`).

### 5.2 Drone

- **Registry client**: registers capabilities/labels, heartbeats, handles
  mind-initiated drain (finish current task, then exit).
- **Assignment loop**: consumes `StreamAssignments`, acks, executes, streams
  progress, submits the result.
- **Executor** (`Executor` port): the actual "do the work" unit. Default
  adapter: a tool-calling agent whose tools include the `Knowledge` port.
- **Knowledge** (`Knowledge` port): read/write access to Linear, Notion, …
  through injected connectors.

Drones are stateless and disposable: no local persistence beyond an in-flight
task; restart = re-register.

## 6. Deployment and portability

- **One process per container.** Images: `holism/mind`, `holism/drone`.
  Connectors ship as their own images (`holism-connector-linear`,
  `holism-connector-notion`, …) maintained outside this repo.
- **Configuration is 100% environment variables** (12-factor): ports, adapter
  selection, store DSNs, connector endpoints, credentials via env-from-secret.
- **Local**: `deploy/docker-compose.yaml` brings up mind, N drones, Postgres,
  NATS, and reference connectors.
- **Production**: Kubernetes manifests under `deploy/kubernetes/`
  (Deployment + Service per component, HPA on drones by CPU/queue depth).
  Nothing in the code depends on Kubernetes; compose and k8s are interchangeable
  runtimes over the same images.
- **Observability**: OpenTelemetry traces/metrics from every gRPC boundary
  (interceptors), structured logs, `/healthz` + `/readyz` on all services.
- **Security boundary**: mTLS between mind and drones is delegated to the
  runtime (service mesh or compose TLS sidecars); the contract itself carries
  a `drone_id` / `mind_id` identity field for application-level checks later.

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
├── connectors/               # contract conformance tests for external connectors
├── deploy/
│   ├── docker-compose.yaml
│   └── kubernetes/
├── docs/design/              # this document
└── buf.yaml, go.work, Makefile
```

Go workspace (`go.work`) with one module per component (`mind`, `drone`,
`api`) so ports can evolve independently while sharing the contract.

## 8. Open questions for review

1. **Push vs pull**: default push over `StreamAssignments` (chosen), but should
   `TaskStore.Claim` remain the primary mechanism for v1 simplicity?
2. **Delivery semantics**: at-least-once with executor idempotency (chosen) vs
   exactly-once via transactional outbox — confirm at-least-once is acceptable.
3. **Mind scalability**: single mind for v1 (chosen) vs active-passive HA.
   Postgres-backed store makes HA a config change later.
4. **Connector auth ownership**: connectors read credentials from their own env
   (chosen); the mind never handles connector secrets.
5. **Goal decomposition**: is `Planner.Plan(Goal) → []TaskSpec` the right first
   cut, or should v1 accept only pre-decomposed tasks and add goals later?
6. **Streaming granularity**: `ReportProgress` as discrete messages (chosen) vs
   a bidi stream per task for token-level progress.

## 9. Roadmap

1. **Phase 1 — skeleton**: buf module + contract, in-memory adapters for every
   port, one mind + one drone over docker-compose, happy-path task execution.
2. **Phase 2 — real adapters**: Postgres `TaskStore`, NATS `EventBus`,
   LLM-planner adapter, gRPC `Knowledge` client; conformance tests for
   connectors.
3. **Phase 3 — production shape**: Kubernetes manifests, HPA, OpenTelemetry,
   mTLS via mesh, drain/graceful-shutdown semantics.

## 10. Glossary

- **Mind**: central coordinator; plans, schedules, tracks.
- **Drone**: stateless worker; executes dispatched tasks.
- **Port**: a small Go interface hiding an external dependency.
- **Adapter**: a concrete implementation of a port.
- **Contract**: the versioned protobuf module shared by all components.
- **Connector**: an external service (Linear, Notion, …) giving agents access
  to a knowledge repository; pluggable via the `Knowledge` port.
