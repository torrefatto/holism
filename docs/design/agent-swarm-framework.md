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
   coordinator or the drones' business logic.
2. **Portability.** Each component ships as its own Docker container and can be
   orchestrated by any runtime that can start containers and give them outbound
   network access (Docker Compose locally, Nomad, a plain VM fleet, …). No
   component assumes a specific host, network, orchestrator, or cloud.
3. **Two actors.**
   - **Mind** — the central coordinator. It decomposes goals into tasks, submits
     tasks to drones, tracks progress, and retries or re-plans on failure.
   - **Drone** — a worker. It connects to the mind, declares itself able to
     receive tasks, executes them, and reports progress and results.
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
                 │  Planner ──► Scheduler ──► TaskStore   │
                 │       ▲           │           │        │
                 │       └──── EventBus ◄────────┘        │
                 │  gRPC API (mTLS) — the only endpoint   │
                 └───────────┬───────────────┬────────────┘
             bidi stream     │               │  plain RPCs
        ┌────────────────────┘               └───────────┐
        │  (Connect: register, task submit/ack,        │ (SubmitTask, GetTask,
        │   progress, result, heartbeat)               │  ListTasks, CancelTask,
        ▼                                              ▼  SubscribeEvents)
   ┌─────────┐   ┌─────────┐   ┌─────────┐        ┌──────────┐
   │ Drone 1 │   │ Drone 2 │   │ Drone N │  …     │ Operator │
   │ (agent) │   │ (agent) │   │ (agent) │        │ (human)  │
   └─────────┘   └─────────┘   └─────────┘        └──────────┘
        │
        ▼ knowledge harness (agent-internal, outside Holism)
   ┌────────────────────────────────────────┐
   │  Knowledge repository: Linear, Notion… │
   └────────────────────────────────────────┘
```

Each box is a container. **Only the mind needs to be network-reachable.**
Drones and the human operator dial out to the mind's gRPC API over mTLS (see
§6.3); drones never accept inbound connections.

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
| `TaskStore` | `Create(ctx, Task) (Task, error)`, `Get(ctx, id)`, `Update(ctx, Task)`, `List(ctx, Filter)` | in-memory (dev), PostgreSQL |
| `EventBus` | `Publish(ctx, Event)`, `Subscribe(ctx, Filter) (<-chan Event, error)` | in-memory, NATS |
| `Planner` (mind) | `Plan(ctx, Goal) ([]TaskSpec, error)` | rule-based, LLM-based |
| `Executor` (drone) | `Execute(ctx, Task) (Progress, error)` | tool-calling agent, scripted |
| `Clock` / `IDGen` | trivial ports to keep tests deterministic | real, fake |

There is deliberately **no knowledge port**: knowledge harnesses are wired
straight into the planner (mind) and the executor (drone) at the composition
root, as part of the agent's own toolset. Holism neither defines nor proxies
knowledge access.

Wiring rule: **core packages depend only on ports, never on adapters.** Adapters
import ports; the composition root (`cmd/mind`, `cmd/drone`) picks concrete
implementations from environment configuration.

## 4. Shared contract (protobuf)

One contract, one module, versioned:

```
proto/holism/v1/
├── common.proto     # Task, TaskSpec, TaskResult, TaskStatus, Artifact (keep minimal)
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
  // The drone opens it, registers, and declares itself ready to receive
  // tasks. The mind submits tasks and receives acks, progress, and results
  // on the same stream. See §4.2 for the message flow.
  rpc Connect(stream DroneMessage) returns (stream MindMessage);

  // --- operator / upstream-facing RPCs ---
  rpc SubmitTask(SubmitTaskRequest) returns (Task);   // submit a goal or a task
  rpc GetTask(GetTaskRequest) returns (Task);
  rpc ListTasks(ListTasksRequest) returns (ListTasksResponse);
  rpc CancelTask(CancelTaskRequest) returns (Task);
  // Server-streaming event feed for observability and reactive UIs.
  rpc SubscribeEvents(SubscribeEventsRequest) returns (stream Event);
}
```

### 4.2 The drone channel (`Connect`)

One long-lived **bidirectional stream** per drone. All drone↔mind traffic —
registration, task submission, ack, progress, result, heartbeats — flows over
this single connection, so a drone only ever opens one outbound connection.

Message envelopes (oneof payloads, so the contract can grow without breaking
changes):

```proto
message DroneMessage {          // drone → mind
  oneof payload {
    Register      register = 1; // drone_id, labels, capabilities (first message)
    Ack           ack = 2;      // task_id, ACCEPTED | REJECTED
    Progress      progress = 3;// task_id, status, message, percent
    Result        result = 4;  // task_id, TaskResult (terminal)
    Heartbeat     heartbeat = 5;// keepalive + current load
    Bye           bye = 6;      // graceful unregister before closing
  }
}

message MindMessage {           // mind → drone
  oneof payload {
    Task          task = 1;     // a submitted task to execute
    Cancel        cancel = 2;   // task_id — stop and report
    Drain         drain = 3;    // finish in-flight work, then close
    Pong          pong = 4;     // heartbeat reply
  }
}
```

Connection flow:

```
Drone                          Mind
  │ ── Connect() (mTLS) ───────►│
  │ ── Register{id, labels} ──►│   drone enters the registry as READY
  │ ◄── Pong / queued backlog ──│   (tasks queued while offline are sent)
  │                             │
  │ ◄────── Task{task_id} ─────│   scheduler picks this READY drone
  │ ── Ack{task_id, ACCEPTED} ─►│   task marked ASSIGNED on ack
  │ ── Progress{...} ──────────►│   task RUNNING; progress recorded
  │ ── Progress{...} ──────────►│
  │ ── Result{task_id, ...} ───►│   task SUCCEEDED / FAILED
  │                             │
  │ ◄────── Drain ─────────────│   graceful shutdown: finish, then close
  │ ── Bye ────────────────────►│
```

Semantics:

- **Registration**: the first message on the stream must be `Register`; the
  mind adds the drone to its in-memory registry as able to receive tasks.
  Reconnecting with the same `drone_id` replaces the previous stream.
- **Task submission and ack**: the mind sends `Task`; the drone replies `Ack`
  with ACCEPTED (it will execute) or REJECTED (e.g. at capacity). The ack
  marks the task as **assigned**; REJECTED causes the mind to requeue and try
  another ready drone.
- **Progress**: the drone sends zero or more `Progress` messages; the mind
  records them against the task and republishes them on the `EventBus`.
- **Result**: `Result` is terminal for the task (succeeded or failed).
- **Heartbeats**: application-level keepalives on the same stream renew the
  drone's lease; the mind also relies on gRPC keepalive/HTTP2 PING at the
  transport level.
- **Reconnect**: if the stream breaks, the drone re-dials with backoff and
  re-registers. Tasks still marked assigned/running for that drone return to
  `queued` after lease expiry (at-least-once delivery; idempotency is the
  executor's responsibility — documented, enforced by convention).

### 4.3 Task lifecycle

```
queued ──► assigned ──► running ──► succeeded
   │          │            │
   │          │            ├─► failed ──► queued (retry, up to maxAttempts)
   │          │            │
   │          ▼            ▼
   └──────► cancelled ◄───┘
```

- `queued → assigned`: the scheduler picks a READY drone (registered,
  heartbeated within the lease window, labels match) and sends `Task` on that
  drone's stream; the drone's `Ack(ACCEPTED)` transitions the task to
  `assigned`.
- `assigned → running`: first `Progress` message (or an explicit
  `Progress{status: RUNNING}`).
- If the stream drops, tasks assigned/running on it return to `queued` after
  lease expiry.
- `maxAttempts` and backoff policy live on `TaskSpec`; the mind enforces them.

## 5. Component responsibilities

### 5.1 Mind (single instance)

- **Planner** (`Planner` port): turns a submitted goal into an ordered set of
  `TaskSpec`s (initially rule-based; LLM-based planner is a drop-in adapter).
  The planner itself may use a knowledge harness to ground its plans.
- **Scheduler**: watches the `TaskStore` for `queued` tasks and the registry for
  READY drones; writes `Task` messages onto the chosen drone's stream queue and
  tracks the pending ack. Owns leases and retries.
- **TaskStore** (`TaskStore` port): single source of truth for task state.
  Backed by the in-memory adapter for v1; the port allows a durable adapter
  (PostgreSQL) later so task history survives mind restarts.
- **Drone registry**: in-memory, keyed by `drone_id`, holding the live stream
  (or its send queue) and readiness/lease state. Built from `Register` and
  renewed by `Heartbeat`; entries expire after a missed-heartbeat timeout.
  Single-instance by design — the registry is why the mind must not be
  replicated.
- **EventBus** (`EventBus` port): publishes lifecycle events; backs
  `SubscribeEvents` and future reactive behaviors.
- **gRPC server**: serves `MindService` over mTLS — the only listener in the
  swarm. Also exposes `/healthz` + `/readyz` on the same mTLS listener.

### 5.2 Drone

- **Mind client**: dials the mind, opens the `Connect` bidi stream, sends
  `Register` (id, labels, capabilities), then heartbeats. Reconnects with
  backoff on stream failure and re-registers with the same identity.
- **Inbound loop**: reads `MindMessage`s from the stream — `Task` (validate
  capacity, reply `Ack`), `Cancel`, `Drain` (finish in-flight work, send `Bye`,
  exit).
- **Executor** (`Executor` port): the actual "do the work" unit. Default
  adapter: a tool-calling agent whose tools include the injected knowledge
  harnesses.
- **Reporting**: sends `Progress` and the terminal `Result` on the same stream.

Drones are stateless and disposable: no local persistence beyond an in-flight
task, no inbound listener; restart = reconnect and re-register.

## 6. Deployment and portability

- **One process per container.** Images: `holism/mind`, `holism/drone`.
- **Configuration is 100% environment variables** (12-factor): the mind's
  listen address, adapter selection, store DSNs, and paths to TLS material,
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
  in the SAN/CN. There is no mind→drone TLS: drones accept no inbound
  connections, so no drone server certificates are needed.
- Certificates are mounted into containers as files (or injected by the
  runtime's secret mechanism) and referenced by path via env config.

### 6.2 Setup CLI (`holism`)

A small CLI, shipped as `holism/cli` (single static binary), bootstraps and
maintains the PKI:

```
holism init                  # generate root CA (key + cert) into ./pki/
holism cert issue mind       # issue the mind's server certificate
holism cert issue drone <id> # issue a drone's client certificate
holism cert issue operator <id>  # issue an operator's client certificate
holism cert renew <name>     # re-issue before expiry
holism cert revoke <name>    # revoke (CRL / short-lived certs)
holism cert list             # show issued certs and expiry
```

Design choices: short-lived leaf certificates (e.g. 24–72 h) with renewal, so
revocation is rarely needed; the CA key never leaves the machine running
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
│       ├── scheduler/
│       ├── taskstore/        # port + adapters (memory, postgres)
│       ├── eventbus/         # port + adapters (memory, nats)
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

## 8. Open questions for review

1. **Delivery semantics**: at-least-once with executor idempotency (chosen) vs
   exactly-once via transactional outbox — confirm at-least-once is acceptable.
2. **Goal decomposition**: is `Planner.Plan(Goal) → []TaskSpec` the right first
   cut, or should v1 accept only pre-decomposed tasks and add goals later?
3. **Progress granularity**: `Progress` as discrete messages on the stream
   (chosen) vs finer-grained token-level events.
4. **Backpressure**: if a drone's send queue grows unboundedly (slow mind),
   should the mind drop progress messages or apply flow control? (v1: drop
   progress, never drop ack/result.)
5. **Certificate lifetime**: short-lived leaves with renewal (chosen, 24–72 h)
   vs longer-lived certs with a CRL.
6. **Operator authentication**: v1 uses the same client-cert mTLS as drones
   (chosen); a token-based path for human operators may come later.

## 9. Roadmap

1. **Phase 1 — skeleton**: buf module + contract (`MindService` with `Connect`),
   the `holism` CLI (`init` + `cert issue`), in-memory adapters for every port,
   single mind + N drones over docker-compose with mTLS, happy-path task
   dispatch over the drone stream.
2. **Phase 2 — real adapters**: Postgres `TaskStore`, NATS `EventBus`,
   LLM-planner adapter, cert renewal automation, executor idempotency helpers.
3. **Phase 3 — hardening**: drain/graceful-shutdown semantics, certificate
   identity-based coarse authorization, richer observability, durable store
   failover runbook for the single-instance mind.

## 10. Glossary

- **Mind**: central coordinator; plans, schedules, submits, tracks. Single
  instance by design; the swarm's only network endpoint.
- **Drone**: stateless worker; connects to the mind over a durable bidi stream
  and executes submitted tasks.
- **Port**: a small Go interface hiding an external dependency.
- **Adapter**: a concrete implementation of a port.
- **Contract**: the versioned protobuf module shared by all components.
- **Knowledge harness**: an agent-internal component (outside Holism) giving an
  agent read/write access to the knowledge repository (Linear, Notion, …).
  Injected into the planner (mind) and executor (drone); never proxied by Holism.
- **PKI**: the Holism-internal certificate authority and the certificates it
  issues for mTLS, managed by the `holism` CLI.
