# CLAUDE.md

## Repo overview

Logwolf is a self-hosted logging platform. This repository is the server: the backend services, the dashboard frontend, the documentation site (`docs/`), and Docker orchestration. The JavaScript SDK (`@logwolf/client-js`) for sending events from applications lives in its own repository, [logwolf-app/client-js](https://github.com/logwolf-app/client-js); the frontend consumes it from npm.

### Backend services (Go)

Managed as a Go workspace (`go.work` at the repository root):

| Service    | Path        | Role                                                                                                       |
| ---------- | ----------- | ---------------------------------------------------------------------------------------------------------- |
| `broker`   | `broker/`   | Public HTTP API gateway (chi router). Accepts events, pushes to RabbitMQ; proxies reads to logger via RPC. |
| `listener` | `listener/` | RabbitMQ consumer. Forwards events to logger via RPC.                                                      |
| `logger`   | `logger/`   | Only service with MongoDB access. Dual-server: RPC on port 5001, HTTP health check on port 80.             |
| `toolbox`  | `toolbox/`  | Shared library: data models, RabbitMQ helpers, MongoDB utilities.                                          |

### Frontend (TypeScript)

- `frontend/` — React Router v7 SSR dashboard (React 19, Tailwind CSS 4, shadcn/ui)

### Infrastructure

- RabbitMQ for async event ingestion
- MongoDB 8.0 for persistence, run as a single-member replica set (`rs0`) because the data layer uses transactions. Compose's `mongo` healthcheck initiates the set the first time it runs, including on existing standalone volumes. Data from the `mongo:4.2` of earlier releases is brought forward with `scripts/upgrade-mongo.sh` (4.4 → 5.0 → 6.0 → 7.0 → 8.0); the integration suite runs the same images as compose
- Caddy as reverse proxy (TLS termination)
- Full stack via `docker-compose.yml`

## Common commands

### Go services

```bash
# Run a service locally
cd broker && go run ./cmd/api

# Unit tests (broker + toolbox + logger)
cd broker && go test ./cmd/api/... -v
cd toolbox && go test ./... -v
cd logger && go test ./... -v

# Integration tests (requires Docker — spins up real MongoDB + RabbitMQ)
cd integration && go test -tags integration ./... -v -timeout 5m

# ...with the service subprocesses' output on stderr, when the stack won't come up
LOGWOLF_TEST_VERBOSE=1 go test -tags integration ./... -v -timeout 5m
```

The frontend uses pnpm, pinned by `packageManager` in its `package.json`. Its `pnpm-workspace.yaml` sets `minimumReleaseAge: 1440`: pnpm will not install a version published less than 24 hours ago. It exempts `@logwolf/client-js`, our own SDK, so a new release can be adopted at once, and allows `esbuild`'s build script.

### Frontend (`frontend/`)

```bash
pnpm run dev       # Vite dev server
pnpm run build     # react-router build
pnpm test          # vitest (single run): libraries, and route loaders/actions with real sessions (app/test/routes.ts)
pnpm run typecheck # react-router typegen + tsc
pnpm run lint      # oxlint
pnpm run format    # oxfmt
```

### Full stack

```bash
# From the repository root
docker compose up
```

## Architecture notes

**Event flow:** Client SDK → Broker (HTTP) → RabbitMQ → Listener → Logger (RPC) → MongoDB. The broker's `202` means RabbitMQ has confirmed the event: `event.Emitter` publishes persistent messages with publisher confirms (503 otherwise), declares the `logwolf_logs` queue itself so events queue before the listener runs, and redials after a RabbitMQ restart. Delivery is at least once end to end

**Networks:** Only Caddy, Broker, and Frontend are on the public network. Logger, Listener, RabbitMQ, and MongoDB are isolated on an internal network. Caddy forwards only the broker's public routes (`@public` in the `Caddyfile`: `/api/logs`, `/api/logs/*`, `/api/health`, `/api/ping`); the dashboard routes are internal-only, since they trust `X-User-Login` once the secret checks out. `broker/cmd/api/caddy_test.go` holds the `Caddyfile` against the broker's routes, so a new public route outside those paths, or a wider Caddy rule, fails the tests

**Public API contract:** `openapi.yaml` at the repository root (OpenAPI 3.1) describes the public routes: auth, scopes (as the security requirement's list), pagination bounds, and every status each route answers. `broker/cmd/api/openapi_test.go` holds it against the broker: the operations must be exactly the public routes, each one's scope must match `publicRoutes` in `scope_test.go`, the bounds must match `data.MaxPage`/`MaxPageSize`/`DefaultPageSize` and `maxBatchSize`, and `apiCases` sends requests through the real router to check every status the broker answers is documented with a body of the documented shape, and every documented status is produced. Changing a public route's behavior means changing the spec, and adding a case for any new status. The docs site serves a copy (`docs/scripts/copy-openapi.mjs`, run before `dev` and `build`), and `.github/workflows/release.yml` attaches it, stamped with the version, to each published release

**API authentication:**

- SDK/API clients: Bearer tokens with `lw_` prefix, validated and cached with TTL + rate limiting per client IP (in broker middleware; the IP comes from `X-Forwarded-For` only when the peer is in `TRUSTED_PROXIES`)
- API keys carry scopes: `ingest` (`POST /logs`, `/logs/batch`), `read` (`GET /logs`, `GET /logs/{id}`), `delete` (`DELETE /logs`). `requireScope` answers 403 without the route's scope. New keys get `ingest` alone unless the creator picks more, because keys ship in browser bundles. Keys stored before scopes existed have none and are read back with all three (`Legacy`), so they keep working
- Dashboard: GitHub OAuth 2.0 (who may sign in is the edition's `SignupPolicy`; self-hosted, a user/org allowlist via env vars), signed cookie sessions (React Router's; readable, not encrypted) + CSRF tokens on mutations. The session also keeps the user's GitHub token, sealed with AES-256-GCM (`lib/token.server.ts`), which the invite check uses to see allowed orgs' private members. Sign-in is deny-by-default: a login must be in `LOGWOLF_ALLOWED_GITHUB_USERS` or belong to an org in `LOGWOLF_ALLOWED_GITHUB_ORGS`; with both empty nobody gets in (`app/lib/allowlist.server.ts`)
- GitHub logins are case-insensitive: memberships store them lowercase and every lookup normalizes with `data.NormalizeGithubLogin` (the broker does it once, in `requireUserLogin`). The session keeps GitHub's casing for display

**Editions:** self-hosted and hosted Logwolf differ only behind two extension points, picked by `LOGWOLF_EDITION` (`selfhosted`, the default, or `cloud`, which has no implementation in this build yet and stops the service at start): `limits.Provider` (`toolbox/limits`) answers whether a project may ingest more events and which retention values it may pick, and the broker takes retention choices from it (`GET /projects/{id}/retention` returns them as `choices`, which the settings page lists); `SignupPolicy` (`frontend/app/lib/signup.server.ts`) answers whether a login may sign in. Self-hosted defaults: unlimited, every supported retention, and the allowlist. Anything edition-specific goes behind one of them, not behind `if edition == ...` checks

**Reading vs. writing:** Broker handles writes asynchronously (via RabbitMQ) and reads synchronously (via RPC to logger). Do not add direct DB calls to broker or listener. This holds for both entry points: SDK clients scoped by API key, and the dashboard scoped by project id + membership.

**Project ids:** every `project_id` is stored as an ObjectID, like `projects._id`: a filter with a hex string matches nothing, silently. Services pass project ids to each other (RPC args, event payloads, JSON) as hex strings; the logger parses them once in its RPC layer, and the `data` functions take `primitive.ObjectID`. A project's `slug` is a display label fixed at creation, not unique and never used for lookups; the migrated `Default` project is found by its `default` flag.

**RabbitMQ topology:** Topic exchange `logs_topic`. The broker normalizes each event's severity first (`data.NormalizeSeverity`: trimmed and lower-cased, so `ERROR` is stored as `error`) and refuses, with a 400, any event whose severity is not `info`, `warning`, `error` or `critical`; events stored before this keep their casing, unmigrated. It publishes each under `data.SeverityRoutingKey`: `log.info`, `log.warning`, `log.error` or `log.critical` (`log.unknown` is left only as a guard, and is never published). The listener binds `log.*` (`event.LogBindingKey`), so it stores every severity; another consumer can bind to one, e.g. `log.error`. Queue declarations live in `toolbox/event/event.go`.

**Data retention:** Retention is per project (default 90 days; supported values are 30/60/90/180/365, or 0 for forever). A cleanup loop in Logger deletes expired logs project by project every `CLEANUP_INTERVAL`, each project under its own timeout so a large one cannot starve the rest. Logs of a deleted project never persist. `DeleteProject` leaves them out of its transaction, so a project with millions of logs still deletes at once, and hands them to this loop, which purges them in batches. `LogInfo` refuses events whose project does not exist, and each pass also sweeps logs whose `project_id` matches no project, which finishes a purge that failed. The global TTL index used before multi-tenancy is dropped on startup.

**Startup migration:** Logger adopts pre-multi-tenancy `logs`, `api_keys`, and `settings` (documents with no `project_id`) into a project named `Default` before it serves traffic, making every login in `LOGWOLF_ALLOWED_GITHUB_USERS` and `LOGWOLF_DEFAULT_PROJECT_OWNERS` an owner. On every start, whatever the orphan count, it also gives an ownerless `Default` those owners. That covers a failed owner step, owners configured after the upgrade, and org-only deployments. Before any of that it converts every `project_id` still stored as a hex string to an ObjectID, in batches (the retention cleanup stays off until a pass has converted everything); flags the `Default` project of builds that found it by its then-unique slug, and drops that index; and rewrites `project_members` logins to lowercase, merging case-only duplicates and keeping the higher role. It is idempotent and silent when there is nothing to do. It runs with the index creation as one startup pass (`logger/cmd/api/startup.go`); a failed pass does not stop Logger, which serves degraded, retries the pass in the background with back-off until it succeeds, and reports the state on its `/health` (and `RPCServer.Status`, which the broker's `/health` shows). See `toolbox/data/migrate.go` and `logger/cmd/api/migrate.go`.

## Service details

### Broker (`broker/`)

Entry point: `cmd/api/main.go`. Key files: `routes.go`, `handlers.go`, `middleware.go`.

- `POST /logs`, `POST /logs/batch` — enqueue events (async, 202)
- `GET /logs`, `GET /logs/{id}`, `DELETE /logs` — proxy to Logger RPC, scoped to the key's project
- Internal routes (`X-Internal-Secret` + `X-User-Login`): `/projects` plus everything that acts on one project under `/projects/{id}/...` — members, `logs` (the dashboard's project-scoped read/write path for events), `keys`, `retention` and `metrics`. No route takes a project id from the query or the body
- Project access on internal routes: 404 when the project does not exist (or the id is malformed), 403 for a non-member or a member on an owner-only route, the same on every route. `authorizeProject` (`access.go`) decides with one `RPCServer.ProjectAccess` call; every `/projects/{id}/...` route declares its level with `requireProject(anyMember|ownerOnly)` in `routes.go` and reads the project and logger connection from the context
- `requireAPIKey` middleware validates keys over logger RPC and caches the result for 60s. Revoking a key or deleting its project evicts it from that broker's cache at once (`forgetCachedKeys`); another broker replica would keep it until the entry expires. `requireInternalSecret` guards dashboard routes. The broker has no MongoDB client: key storage (`/projects/{id}/keys`) goes through logger RPC too

### Listener (`listener/`)

Entry point: `cmd/api/main.go`. No external dependencies beyond toolbox. Pure consumer — no HTTP server. The consumer loop lives in `toolbox/event`: it keeps one RPC connection to logger, acknowledges a message only once the event is stored or dropped for good, and retries an unreachable logger with back-off, so a logger outage delays events instead of losing them. Delivery is at least once.

### Logger (`logger/`)

Entry point: `cmd/api/main.go`. Key files: `rpc.go`, `routes.go`, `migrate.go`, `cleanup.go`, `projects.go`.

RPC methods (Go stdlib `net/rpc`):

- `RPCServer.LogInfo` — insert event (refused if its project does not exist)
- `RPCServer.GetLogs` — query with pagination/filtering. Pages are bounded (`data.PaginationParams.Validate`: `pageSize` 1–100, `page` 1–1,000,000), in the broker's `paginationFromQuery` (400) and again in `AllLogs`
- `RPCServer.GetLog` — fetch one event by id within a project
- `RPCServer.DeleteLog` — delete by filter, returns count
- `RPCServer.ValidateAPIKey`, `ListAPIKeys`, `CreateAPIKey`, `RevokeAPIKey` — API key storage for the broker; replies never carry the hash, and revoke matches the project as well as the id

### Toolbox (`toolbox/`)

Packages: `data` (Models, LogEntry, APIKey, Settings), `event` (emitter + consumer), `limits` (`Provider`, the edition's limits), `rabbitmq` (connection), `json` (helpers).

The `data.Models` struct is the sole database accessor passed between services.

### Frontend (`frontend/`)

Key files: `app/root.tsx`, `app/lib/api.ts` (dashboard API client), `app/lib/auth.server.ts`.

Routes: `/` (public), `/auth`, `/dashboard`, `/events`, `/events/new`, `/events/:id`, `/keys`, `/projects`, `/projects/new`, `/projects/switch`, `/projects/:id/settings`. `/settings` is a redirect to the current project's settings page.

The layout loader keeps `currentProjectID` in the session honest and redirects a user with no projects to `/projects/new`, the only protected page that renders without a current project.

Pages take the current project from the session (`getCurrentProjectID`), never from the URL or a form field, so the redirect back from `/projects/switch` revalidates them into the new project. `/events` included: it goes through the broker's `/projects/:id/logs` routes, not the SDK. The SDK's key belongs to one fixed project, so `lib/logwolf.ts` is now only the dashboard's own error tracking.

Project name, retention, members and deletion all live on `/projects/:id/settings`. The retention options are the broker's `choices` for the project. Any member may raise retention, but only owners may lower it (`data.LowersRetention`, mirrored by `lib/retention.ts`), since a shorter window deletes logs on the next cleanup pass; renaming, member changes and deletion are owner-only. All of it is enforced in the broker and mirrored in the route so the UI can explain itself. Any member may create and revoke API keys. Adding a member first asks GitHub about the login (`checkInvitee`), org membership as the inviting owner so private members count: an unknown login or an organization is refused, and someone the allowlist does not clear is added with a warning that they cannot sign in yet.

`lib/api.ts` → calls Broker internal routes via `X-Internal-Secret`, plus `X-User-Login` from the session for the broker's membership checks; project-scoped methods take the project id as an argument. Never calls public SDK routes.

The frontend instruments itself with `@logwolf/client-js` (`lib/logwolf.ts`) for error tracking.

## CI

GitHub Actions (`.github/workflows/ci.yml`) runs on every push to `main` and all PRs:

1. Go unit tests (broker + toolbox + logger + listener)
2. Integration tests, with `LOGWOLF_EDITION=selfhosted`
3. Frontend tests

`.github/workflows/release.yml` runs when a GitHub release is published and attaches `openapi.yaml` to it, with `info.version` set from the tag. For a `v*` tag it also publishes the `broker`, `listener`, `logger` and `frontend` images to `ghcr.io/logwolf-app/<service>` for `linux/amd64` and `linux/arm64`, tagged with the version without its `v`, and `latest` when GitHub marks the release as its latest. The Go Dockerfiles cross-compile on the build platform and the frontend's builds there too, so only its production install runs emulated. `docker-compose.images.yml` runs those images in place of building them (`docs/self-hosting.md`, "Docker images").

The JS SDK is tested and released from its own repository, [logwolf-app/client-js](https://github.com/logwolf-app/client-js).

## Environment

Copy `.env.example` to `.env` and fill in GitHub OAuth credentials before running the stack locally. Required vars: `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET`, `LOGWOLF_ALLOWED_GITHUB_USERS` or `LOGWOLF_ALLOWED_GITHUB_ORGS`, `SESSION_SECRET`, `INTERNAL_API_SECRET`, `MONGO_USERNAME`, `MONGO_PASSWORD`, `RABBITMQ_USERNAME`, `RABBITMQ_PASSWORD`. Compose refuses to start without the secrets and credentials; none of them has a default.

Per-service env vars:

| Variable                         | Service          | Default                       | Description                                                            |
| -------------------------------- | ---------------- | ----------------------------- | ---------------------------------------------------------------------- |
| `MONGO_URL`                      | logger           | `mongodb://mongo:27017`       | MongoDB connection                                                     |
| `MONGO_USERNAME`                 | logger, mongo    | —                             | MongoDB credentials; unset, `MONGO_URL`'s own apply                    |
| `MONGO_PASSWORD`                 | logger, mongo    | —                             | With `MONGO_USERNAME`; one without the other is refused                |
| `RABBITMQ_USERNAME`              | rabbitmq         | —                             | RabbitMQ user; compose builds `RABBITMQ_URL` from it                   |
| `RABBITMQ_PASSWORD`              | rabbitmq         | —                             | Its password; URL-safe characters only                                 |
| `RABBITMQ_URL`                   | broker, listener | `amqp://guest:guest@rabbitmq` | RabbitMQ connection                                                    |
| `BROKER_PORT`                    | broker           | `80`                          | HTTP listen port                                                       |
| `TRUSTED_PROXIES`                | broker           | private ranges (compose)      | Peers whose `X-Forwarded-For` names the client for rate limiting       |
| `LOGGER_RPC_PORT`                | logger           | `5001`                        | RPC listen port                                                        |
| `LOGGER_HTTP_PORT`               | logger           | `80`                          | HTTP health check port                                                 |
| `CLEANUP_INTERVAL`               | logger           | `1h`                          | Per-project retention cleanup frequency                                |
| `LOGWOLF_EDITION`                | broker, frontend | `selfhosted`                  | Picks `limits.Provider` and `SignupPolicy`; `cloud` is not built yet   |
| `LOGWOLF_ALLOWED_GITHUB_USERS`   | frontend, logger | —                             | Dashboard allowlist; also the owners of the migrated `Default` project |
| `LOGWOLF_ALLOWED_GITHUB_ORGS`    | frontend         | —                             | Dashboard allowlist by org membership                                  |
| `LOGWOLF_DEFAULT_PROJECT_OWNERS` | logger           | —                             | More `Default` owners; how org-only deployments get one                |
| `API_URL`                        | frontend         | —                             | Broker base URL                                                        |
| `INTERNAL_API_SECRET`            | frontend         | —                             | Shared secret for internal Broker routes                               |
| `SESSION_SECRET`                 | frontend         | —                             | Session cookie signing key, and the key sealing GitHub tokens         | 

## Detailed docs

Each project has an `OVERVIEW.md` under its `docs/` folder:

- [`broker/docs/OVERVIEW.md`](broker/docs/OVERVIEW.md)
- [`listener/docs/OVERVIEW.md`](listener/docs/OVERVIEW.md)
- [`logger/docs/OVERVIEW.md`](logger/docs/OVERVIEW.md)
- [`toolbox/docs/OVERVIEW.md`](toolbox/docs/OVERVIEW.md)
- [`frontend/docs/OVERVIEW.md`](frontend/docs/OVERVIEW.md)

The user-facing documentation site (VitePress) lives in the top-level `docs/`. The SDK's overview is in the [client-js repository](https://github.com/logwolf-app/client-js/blob/main/docs/OVERVIEW.md).
