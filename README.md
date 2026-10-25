# Holism

A framework for cooperating AI agents: a **mind** (central coordinator, single
instance, the swarm's only network endpoint) and **drones** (stateless workers)
that connect to the mind over a durable mTLS gRPC stream.

See [docs/design/agent-swarm-framework.md](docs/design/agent-swarm-framework.md)
for the full design.

## Layout

- `proto/holism/v1/` — the shared protobuf contract (buf module)
- `api/` — generated Go code for the contract (committed)
- `mind/` — the coordinator (`cmd/mind`, `internal/…`)
- `drone/` — the worker (`cmd/drone`, `internal/…`)
- `cli/` — the `holism` setup CLI (PKI, certificate lifecycle)
- `deploy/` — local docker-compose setup

## Development

```sh
make generate   # regenerate Go code from proto/ (requires buf)
make build      # build mind, drone, holism into ./bin
make test       # run tests in all modules
make lint       # buf lint + go vet
make docker     # build container images (holism/mind, holism/drone, holism/cli)
```

Requires Go 1.24+ and [buf](https://buf.build). Generated code is committed;
contributors do not need protoc toolchains.
