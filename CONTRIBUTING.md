# Contributing to Logwolf

Thanks for your interest in contributing. This document covers how to run the stack locally, how to run the test suite, and the PR process.

## Prerequisites

- Go 1.25+
- Node.js 22+
- pnpm, the version pinned in `frontend/package.json`'s `packageManager` (`corepack enable` installs it)
- Docker and Docker Compose v2
- A GitHub account (for dashboard login during local development)

## Repository layout

```
logwolf/
├── broker/                 # HTTP API gateway (Go + chi)
├── listener/               # RabbitMQ consumer (Go)
├── logger/                 # MongoDB writer + RPC server (Go)
├── toolbox/                # Shared Go module (data models, event types, helpers)
├── integration/            # End-to-end tests (Go + testcontainers)
├── frontend/               # React Router v7 SSR dashboard (TypeScript)
├── docs/                   # Documentation site (VitePress)
├── scripts/                # Operational scripts (MongoDB upgrade)
├── openapi.yaml            # Spec of the public API (checked by the broker's tests)
├── docker-compose.yml      # Full stack orchestration (published images)
├── docker-compose.build.yml # Override that builds the images from source
└── go.work                 # Go workspace
```

The JS SDK (`@logwolf/client-js`) lives in its own repository, [logwolf-app/client-js](https://github.com/logwolf-app/client-js). Contributions to it go there.

## Running the stack locally

The recommended workflow depends on what you're working on.

`docker-compose.yml` runs the images each release publishes. To run your changes instead, add `docker-compose.build.yml`, which builds the services from your checkout (Docker Compose 2.24 or later). Set it once in `.env`, and every `docker compose` command below builds from source (on Windows, separate the files with `;`):

```bash
COMPOSE_FILE=docker-compose.yml:docker-compose.build.yml
```

### Full stack (simplest)

```bash
cp .env.example .env  # fill in your GitHub OAuth credentials and secrets, and COMPOSE_FILE as above
docker compose up --build -d
```

Everything runs at `https://localhost`. See the [self-hosting guide](https://logwolf-docs.vercel.app/self-hosting.html) for environment variable details.

### Frontend only

Bring up the infrastructure services and run the frontend dev server with HMR:

```bash
docker compose up -d mongo rabbitmq broker caddy

cd frontend
pnpm install
pnpm run dev  # http://localhost:5173
```

Set `API_URL=http://localhost:8080/` in `frontend/.env` if you're running the Broker outside Docker.

### Backend services only

Run infrastructure via Docker, then run Go services individually:

```bash
docker compose up -d mongo rabbitmq

# In separate terminals, from the repository root:
cd logger  && go run ./cmd/api
cd listener && go run ./cmd/api
cd broker   && go run ./cmd/api
```

The Broker listens on port `80` by default. Override with the `BROKER_PORT` env var.

## Running the tests

### Go unit tests

```bash
cd broker   && go test ./cmd/api/... -v
cd toolbox  && go test ./... -v
cd logger   && go test ./... -v
```

### Integration tests

Integration tests require Docker. They spin up real MongoDB and RabbitMQ containers via testcontainers-go, launch the full Go service stack as subprocesses, and assert end-to-end behaviour.

```bash
cd integration
go test -tags integration ./... -v -timeout 5m
```

These tests take 30–60 seconds on first run while container images are pulled.

### Frontend tests

```bash
cd frontend
pnpm test              # vitest (single run)
pnpm run typecheck     # react-router typegen + tsc
pnpm run lint          # oxlint
```

### CI

The Go unit tests, integration tests and frontend tests run on every push and pull request via GitHub Actions (`.github/workflows/ci.yml`), which also builds the images from source through `docker-compose.build.yml`. PRs must pass all checks before merging.

## Code style

**Go** — standard `gofmt` formatting. No linter configuration is enforced yet, but follow the conventions already in the codebase: structured JSON logging, explicit error returns, no package-level globals.

**TypeScript** — the frontend is formatted with oxfmt (`frontend/.oxfmtrc.json`) and linted with oxlint (`frontend/.oxlintrc.json`). Run both before committing:

```bash
cd frontend
pnpm run format
pnpm run lint
```

## Making changes

### Go services

The Go workspace is defined in `go.work` at the repository root. All services import shared code from `toolbox` via `replace` directives. When adding a new dependency to a service, run `go mod tidy` in that service's directory.

### Frontend

The frontend uses React Router v7 with file-based routing. New pages go in `frontend/app/pages/`. Shared UI components go in `frontend/app/components/ui/` — these are shadcn/ui components and should follow the existing pattern.

## Pull request process

1. Fork the repository and create a branch from `main`.
2. Make your changes. Add or update tests where relevant.
3. Make sure all tests pass locally before opening a PR.
4. Open a PR against `main` with a clear description of what changed and why.
5. Keep PRs focused — one concern per PR is easier to review than a sprawling change.
6. On your first PR, sign the [Contributor License Agreement](./CLA.md) when the CLA Assistant bot asks. It takes one click (you sign in with GitHub on cla-assistant.io) and covers all your future PRs. A PR cannot be merged until its author has signed.

There is no formal review SLA. Small, well-scoped PRs get reviewed faster.

## Reporting bugs

Open a GitHub issue. Include:

- What you expected to happen
- What actually happened
- Steps to reproduce
- Logwolf version (or commit SHA)
- Relevant logs (`docker compose logs <service>`)

## License

By contributing, you agree that your contributions will be licensed under the [GNU AGPL v3](./LICENSE) (`AGPL-3.0-only`), and that you have signed the [Contributor License Agreement](./CLA.md).

The CLA is a licence, not a transfer: you keep the copyright in your work. It also lets the maintainer license your contributions under other terms than the AGPL, which is what allows the hosted Logwolf Cloud to run them without publishing its own code.
