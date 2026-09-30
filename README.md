# Logwolf 🐺

Self-hosted event logging and observability. Your logs stay on your server.

No vendor lock-in. No per-seat pricing. One `docker compose up` and you're logging.

---

## What it is

Logwolf is a lightweight alternative to Sentry and Datadog for developers who want to own their data. Applications instrument themselves via the [JS SDK](https://github.com/logwolf-app/client-js), which ships structured events to a Go backend pipeline. Events are stored in MongoDB and surfaced through a React dashboard.

This repository is the Logwolf server: a Go `broker` API, `logger` and `listener` services, the React dashboard (`frontend`), and the infrastructure they run on (`mongo`, `rabbitmq`), all orchestrated with Docker Compose. All traffic is served over HTTPS via Caddy, which handles TLS termination and reverse proxying.

- **Capture events** with severity levels, tags, and arbitrary key/value payloads
- **Batched delivery** — events are buffered in memory and flushed in configurable batches, keeping your application's hot path free
- **Retry with backoff** — failed sends are retried automatically; auth errors are not
- **Track durations** automatically — `LogwolfEvent` acts as a stopwatch, frozen at enqueue time
- **Sample intelligently** — configurable rates for info, warning, and error events; critical always sends
- **See everything** in a dashboard with metrics, event rate, error rate, and tag breakdowns

## Quick start

### Prerequisites

- Docker and Docker Compose v2
- A GitHub OAuth App (for dashboard login)

### 1. Clone the repository

```bash
git clone https://github.com/logwolf-app/logwolf.git
cd logwolf
```

### 2. Create a GitHub OAuth App

Go to **GitHub → Settings → Developer settings → OAuth Apps → New OAuth App** and fill in:

- **Homepage URL:** `https://localhost`
- **Authorization callback URL:** `https://localhost/auth`

Copy the **Client ID** and generate a **Client Secret**.

### 3. Configure environment variables

Copy `.env.example` to `.env` at the repo root (gitignored) and fill it in:

```
GITHUB_CLIENT_ID=your_client_id
GITHUB_CLIENT_SECRET=your_client_secret
LOGWOLF_ALLOWED_GITHUB_USERS=your_github_username
SESSION_SECRET=a_long_random_string
INTERNAL_API_SECRET=another_long_random_string
MONGO_USERNAME=logwolf
MONGO_PASSWORD=a_third_random_string
RABBITMQ_USERNAME=logwolf
RABBITMQ_PASSWORD=a_fourth_random_string
```

Compose refuses to start while any of these is missing. Generate secrets with `openssl rand -hex 32`.

### 4. Trust Caddy's local CA (first run only)

Caddy issues a self-signed certificate for `localhost`. To avoid browser warnings, trust it once:

```bash
docker compose up -d caddy
docker exec $(docker ps -qf "name=caddy") caddy trust
```

Then restart your browser.

### 5. Start the stack

```bash
docker compose up --build -d
```

- **Dashboard:** https://localhost
- **API:** https://localhost/api

### 6. Generate an API key

Sign in via GitHub, navigate to **API Keys**, and generate a key. Copy it immediately — it is shown only once.

To stop the stack, run `docker compose down`.

Full instructions in the [getting started guide](https://logwolf-docs.vercel.app/getting-started.html); for production config, TLS and persistence see the [self-hosting guide](https://logwolf-docs.vercel.app/self-hosting.html).

## Authentication

Logwolf uses two separate auth mechanisms:

| Surface   | Mechanism                                                               |
| --------- | ----------------------------------------------------------------------- |
| SDK / API | Static API keys (`lw_` prefix, passed as `Authorization: Bearer <key>`) |
| Dashboard | GitHub OAuth 2.0, signed HTTP-only session cookie                       |

Access to the dashboard is restricted to GitHub users or organizations listed in `LOGWOLF_ALLOWED_GITHUB_USERS` or `LOGWOLF_ALLOWED_GITHUB_ORGS`.

> **Security note:** The Logger RPC port (`5001`) and MongoDB are not exposed to the host. All internal service communication happens over an isolated Docker network. Never expose these ports externally.

## JS SDK

The SDK lives in its own repository, [logwolf-app/client-js](https://github.com/logwolf-app/client-js).

```bash
npm install @logwolf/client-js
```

```ts
import Logwolf, { LogwolfEvent } from '@logwolf/client-js';

const logwolf = new Logwolf({
	url: 'https://your-domain.com/api/',
	apiKey: process.env.LOGWOLF_API_KEY,
	sampleRate: 0.5,
	errorSampleRate: 1,
	flushIntervalMs: 5000,
	maxBatchSize: 20,
	maxQueueSize: 500,
	retryDelaysMs: [1000, 3000, 10000],
	requestTimeoutMs: 10000,
});

const event = new LogwolfEvent({
	name: 'checkout.completed',
	severity: 'info',
	tags: ['payments'],
});

event.set('userId', '123');
event.set('amount', 9900);

logwolf.capture(event); // enqueues and returns immediately
```

Full SDK reference at [logwolf-docs.vercel.app/sdk/js](https://logwolf-docs.vercel.app/sdk/js.html).

## Repository layout

```
logwolf/
├── broker/                 # HTTP API gateway (Go + chi)
├── listener/               # RabbitMQ consumer (Go)
├── logger/                 # MongoDB writer + RPC server (Go)
├── toolbox/                # Shared Go module
├── integration/            # End-to-end tests (Go + testcontainers)
├── frontend/               # React Router v7 SSR dashboard (TypeScript)
├── docs/                   # Documentation site (VitePress)
├── scripts/                # Operational scripts (MongoDB upgrade)
├── Caddyfile               # Reverse proxy / TLS config
├── docker-compose.yml      # Full stack orchestration
└── go.work                 # Go workspace
```

## Local development

- **Frontend only:** `docker compose up -d broker mongo rabbitmq caddy`, then run the dev server in `frontend/`:

  ```bash
  cd frontend
  pnpm install
  pnpm run dev   # http://localhost:5173
  ```

  Set `API_URL` in `frontend/.env` (defaults to `http://localhost:8080/`). `pnpm run build` writes the production build to `build/` (`client/` and `server/`).

- **Backend only:** `docker compose up -d mongo rabbitmq`, then run the Go services individually with `go run ./cmd/api` from `broker/`, `logger/` and `listener/`. Give them the credentials from `.env`: `MONGO_USERNAME` and `MONGO_PASSWORD` for the logger, and `RABBITMQ_URL=amqp://<user>:<password>@<host>` for the broker and listener.

- **Full stack:** `docker compose up --build -d`

See [CONTRIBUTING.md](./CONTRIBUTING.md) for running the test suites and the PR process.

## Documentation

[logwolf-docs.vercel.app](https://logwolf-docs.vercel.app)

- [Getting started](https://logwolf-docs.vercel.app/getting-started.html) — up and running in 5 minutes
- [Self-hosting guide](https://logwolf-docs.vercel.app/self-hosting.html) — production config, TLS, persistence
- [JS SDK reference](https://logwolf-docs.vercel.app/sdk/js.html) — full API
- [Architecture overview](https://logwolf-docs.vercel.app/architecture.html) — how the pieces fit together

## Stack

| Layer               | Technology                               |
| ------------------- | ---------------------------------------- |
| API gateway         | Go, chi                                  |
| Message queue       | RabbitMQ                                 |
| Database            | MongoDB                                  |
| Dashboard           | React Router v7, Tailwind CSS, shadcn/ui |
| TLS / reverse proxy | Caddy                                    |
| Orchestration       | Docker Compose                           |

## Contributing

Fork, create a feature branch, send a PR. Keep changes focused; add tests and update docs. See [CONTRIBUTING.md](./CONTRIBUTING.md).

## License

GNU GPL v3 — see [LICENSE](./LICENSE).
