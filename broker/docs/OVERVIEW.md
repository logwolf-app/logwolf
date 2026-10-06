# Broker — Overview

## Purpose

Public-facing HTTP API gateway. It is the only Logwolf service reachable from the internet (via Caddy). It:

- Authenticates API keys and routes write requests to RabbitMQ
- Proxies read/delete requests to the Logger service via RPC
- Exposes internal admin routes for the dashboard (API key management, settings, metrics)

## Source layout

```
cmd/api/
├── main.go          # Server bootstrap, graceful shutdown
├── routes.go        # Route registration (chi)
├── handlers.go      # Request handlers
├── middleware.go    # Auth middleware (Bearer token, internal secret)
├── access.go        # Project access: authorizeProject, requireProject
├── organizations.go # Organization access (requireOrganization) and routes
├── clientip.go      # Client address behind trusted proxies (TRUSTED_PROXIES)
├── ingestlimit.go   # Per-API-key ingestion rate (token buckets)
├── usage.go         # Usage metering: accepted events and bytes, flushed to the logger
├── rpcerrors.go     # Maps the logger's RPC errors to HTTP statuses
└── helpers.go       # JSON read/write utilities
```

## HTTP routes

### Public routes (Bearer token required)

| Method   | Path          | Scope    | Description                              |
| -------- | ------------- | -------- | ---------------------------------------- |
| `POST`   | `/logs`       | `ingest` | Submit a single log event (async, 202)   |
| `POST`   | `/logs/batch` | `ingest` | Submit up to 1000 events at once         |
| `GET`    | `/logs`       | `read`   | Retrieve events (RPC → Logger → MongoDB) |
| `GET`    | `/logs/{id}`  | `read`   | Retrieve one event of the key's project  |
| `DELETE` | `/logs`       | `delete` | Delete matching events (RPC → Logger)    |

A key without the route's scope gets 403.

Both log reads, `GET /logs` and the dashboard's `GET /projects/{id}/logs`, take `page` and `pageSize` from the query (`paginationFromQuery`). A missing one means the first page, or 20 logs. One that is given must be a whole number within `data.PaginationParams.Validate`'s bounds: `page` from 1 to 1,000,000, `pageSize` from 1 to 100 (`data.MaxPageSize`). Anything else is a 400 rather than a quiet fallback, so a client asking for 1000 logs learns it did not get them. The logger enforces the same bounds in `AllLogs`, whoever calls it: a page is decoded whole in the logger and sent back in one RPC reply, so an unbounded one could load a project's every log into memory.

### Internal routes (`X-Internal-Secret` header required)

Not reachable from the internet: Caddy forwards only the public routes above and the health checks, and `caddy_test.go` fails if the `Caddyfile` would forward any of these, or stop forwarding a public one.

Everything that acts on one project is under `/projects/{id}`, and on one organization under `/organizations/{id}`. `PUT /users/me` is the one route about the caller rather than a project or an organization.

| Method   | Path                                | Access | Description                                                 |
| -------- | ----------------------------------- | ------ | ----------------------------------------------------------- |
| `PUT`    | `/users/me`                         | —      | Record a sign-in (below)                                    |
| `GET`    | `/projects`                         | —      | Projects the caller belongs to, with `role`                 |
| `POST`   | `/projects`                         | —      | Create a project in the Default organization, caller-owned  |
| `GET`    | `/projects/{id}`                    | member | Get one project                                             |
| `PATCH`  | `/projects/{id}`                    | owner  | Rename a project (the slug stays)                           |
| `DELETE` | `/projects/{id}`                    | owner  | Delete a project and everything under it                    |
| `GET`    | `/projects/{id}/members`            | member | List members                                                |
| `POST`   | `/projects/{id}/members`            | owner  | Add a member: `{"login", "user_id", "role"}` (below)        |
| `PATCH`  | `/projects/{id}/members/{memberID}` | owner  | Change a member's `role` (owner or member)                  |
| `DELETE` | `/projects/{id}/members/{memberID}` | owner  | Remove a member                                             |
| `GET`    | `/projects/{id}/logs`               | member | List a project's events (paginated)                         |
| `POST`   | `/projects/{id}/logs`               | member | Submit an event to a project (async, 202)                   |
| `GET`    | `/projects/{id}/logs/{logID}`       | member | Get one event                                               |
| `DELETE` | `/projects/{id}/logs/{logID}`       | member | Delete one event                                            |
| `GET`    | `/projects/{id}/keys`               | member | List API keys                                               |
| `POST`   | `/projects/{id}/keys`               | member | Create an API key; `scopes` default: ingest                 |
| `DELETE` | `/projects/{id}/keys/{keyID}`       | member | Revoke an API key; another project's is a 404               |
| `GET`    | `/projects/{id}/retention`          | member | Get retention (`{"days": n, "choices": [...]}`)             |
| `PATCH`  | `/projects/{id}/retention`          | member | Update retention; lowering it is owner-only (below)         |
| `GET`    | `/projects/{id}/metrics`            | member | Usage analytics                                             |

| Method   | Path                                     | Access | Description                                                        |
| -------- | ---------------------------------------- | ------ | ------------------------------------------------------------------ |
| `GET`    | `/organizations`                         | —      | Organizations the caller belongs to, with `role`                   |
| `POST`   | `/organizations`                         | —      | Create an organization, owned by the caller: `{"name"}`            |
| `GET`    | `/organizations/{id}`                    | member | Get one organization, with the caller's `role`                     |
| `PATCH`  | `/organizations/{id}`                    | admin  | Rename an organization (the plan stays)                            |
| `POST`   | `/organizations/{id}/projects`           | member | Create a project in the organization, owned by the caller          |
| `GET`    | `/organizations/{id}/plan`               | member | The plan's limits and the organization's usage (below)             |
| `GET`    | `/organizations/{id}/members`            | member | List members                                                       |
| `POST`   | `/organizations/{id}/members`            | admin  | Add a member: `{"login", "user_id", "role"}`; an owner, owner-only |
| `PATCH`  | `/organizations/{id}/members/{memberID}` | admin  | Change a member's `role`; to or from owner, owner-only             |
| `DELETE` | `/organizations/{id}/members/{memberID}` | admin  | Remove a member; an owner, owner-only                              |

Internal routes also require the signed-in user: `X-User-ID`, their GitHub user
ID, and `X-User-Login`, their login (401 without either, or with an ID that is
not a positive integer). `requireUserLogin` stores both, the login lowercased:
GitHub logins are case-insensitive. Project access is checked against the user
ID on every call, so a member who renames their GitHub account keeps their
access, and whoever takes the old login gets none of it. The login only finds
memberships stored before user IDs, which carry none (`data.MemberFilter`);
a membership linked to a user ID is never matched by login.

A membership belongs to a user ID. Invites still name a login: the dashboard
resolves it to the GitHub user ID through GitHub's API before it calls `POST
/projects/{id}/members`, which takes both, and refuses a `user_id` that is
missing or not a positive integer with a 400. Adding a user who is already a
member, under any login, is a 409. The member listing returns each membership's
own `id`, which is what `PATCH` and `DELETE` take as `{memberID}`: it names one
row whether or not it is linked to a user yet, and another project's is a 404.
`user_id` comes with each member that has one, and a member who has signed in
is listed under the login of their last sign-in.

The dashboard calls `PUT /users/me` at each sign-in with `{"email": "..."}`, and the broker hands
it to `RPCServer.UpsertUser` with the caller's GitHub ID and login from the headers. The user is
keyed by the GitHub ID, so a renamed account stays the same user with its new login. The logger
also links the memberships stored before user IDs under the caller's login to their GitHub ID
(`data.LinkMemberships`), so from then on they follow the user, not the login. The reply is the
user as stored; a failure in either step is a 500, and the dashboard's sign-in fails with it.

Every route that acts on a project denies access the same way (`access.go`):

- **404** if the project does not exist, or the id is not an ObjectID;
- **403** if it exists and the caller is not a member, or is a member on an owner-only route.

`authorizeProject` is that rule. It costs one logger call, `RPCServer.ProjectAccess`, which answers both whether the project exists and the caller's role. Every `/projects/{id}/...` route gets it from the `requireProject(anyMember|ownerOnly)` middleware, so `routes.go` states each route's access level. The middleware also hands the handler the checked project (with its canonical lower-case id) and the logger connection it checked over, which the handler reuses and the middleware closes. No route takes a project id from the query or the body.

The caller's role in a project is their own membership's, except that an owner of the project's organization is owner of the project, member or not (`data.EffectiveProjectRole`; the logger works it out in the same `ProjectAccess` call). `GET /projects` lists those projects too, as owner. The organization's admins and members get nothing from it.

Organization routes deny the same way (`organizations.go`): **404** if the organization does not exist or the id is not an ObjectID, **403** if the caller is not a member or holds a role below the route's. `authorizeOrganization` decides with one `RPCServer.OrganizationAccess` call, by the caller's user ID alone (organization memberships always have one), and `requireOrganization(anyMember|adminOnly|ownerOnly)` declares each route's level in `routes.go`, handing the handler the checked organization, the caller's role and the logger connection. On a project route anything above `anyMember` is owner-only, since projects have no admins. No route takes an organization id from the query or the body.

Admins manage an organization's members, but only owners decide who the owners are: an admin adding an owner is refused with a 403 before the logger is called, and an admin removing, demoting or promoting an owner is refused by the logger (`data.ErrOwnerRequired`, 403), which reads the member's role in the same transaction as the change; the handler passes the caller's role from the access check as `ActorRole`. An organization always keeps one owner, like a project (400).

A new organization starts on the plan the edition picks (`limits.Provider.NewOrganizationPlan`: `selfhosted` self-hosted, `free` hosted), never one from the request. `GET /organizations/{id}/plan` answers `{"plan": {"name", "monthly_events", "max_retention_days", "max_projects", "max_members"}, "usage": {"projects", "members"}}`, the plan resolved by the edition from the name the organization stores (`OrganizationPlan`: self-hosted is always the one unlimited plan; hosted, a name not in the table is a 500, never a plan without limits). A limit of 0 is none. Events are not counted yet, so usage has no events.

Creating a project is one logger call, `POST /projects` and
`POST /organizations/{id}/projects` alike (`{"name", "slug"}`; the second
names the organization in `RPCCreateProjectArgs.OrganizationID`, the first
leaves it to the logger's Default organization). Any member of an organization
may create a project in it: the organization role grants nothing in the
project, so the creator's owner membership is what lets them in. The logger
writes the project and the caller's owner membership in one transaction, so a
failure leaves no project behind that nobody could reach. Slugs are display
labels, fixed at creation and not unique, so creating a project never reveals
that someone else's has the same one.

Renaming or deleting a project and adding, removing or changing the role of a
member are owner-only. A project always keeps one owner: removing or demoting the last one is a 400
(`cannot remove the last owner` / `cannot demote the last owner`). An owner may
demote themselves while another owner remains, which is how a project changes
hands: promote the new owner, then step down.

Any member may raise a project's retention, but only an owner may lower it:
`UpdateRetention` reads the current value first and answers 403 when
`data.LowersRetention` says the new one is shorter (0, forever, is the longest).
Lowering it is a bulk delete, as the next cleanup pass drops everything outside
the new window. Creating and revoking API keys stays open to every member.

The `/projects/{id}/logs` routes are the dashboard's way into events. They do the
same work as the public `/logs` routes, but take the project from the path and
check the caller's membership instead of reading it off an API key — the
dashboard authenticates as a user and has no key of its own to scope it. An id
that belongs to another project is a 404, never another project's event.

### Errors from the logger

`net/rpc` turns the logger's errors into plain strings, so `rpcerrors.go` reads
the cause back out of the message in one place (`classifyRPCError`) and
`rpcErrorJSON` maps it to a status, replacing Mongo's wording with a message of
the handler's choosing:

| Cause                                                  | Status | Example                                            |
| ------------------------------------------------------ | ------ | -------------------------------------------------- |
| Unique index violation (`E11000`)                      | 409    | Adding an existing member                          |
| No document matched, or the id is not a valid ObjectID | 404    | A malformed project id on any project-scoped route |
| `data.ErrKeyNotFound`                                  | 404    | Revoking a key that does not exist                 |
| `data.ErrLastOwner`, `data.ErrLastOrganizationOwner`   | 400    | Removing or demoting the last owner                |
| `data.ErrOwnerRequired`                                | 403    | An admin removing an organization's owner          |
| Anything else                                          | 500    | The logger or MongoDB failed                       |

Retention days are checked against the project's choices before the logger is
called: what the edition's `limits.Provider` offers it (`Config.Limits`; a
`Config` without one is self-hosted, which offers every one of
`data.ValidRetentionDays`). The hosted edition's offers the retention of the
plan of the project's organization, which it asks the logger for
(`projectPlan`, in `plans.go`, over a connection of its own). Anything else is a 400, and so is a missing `days`,
rather than 0 (keep forever). `GET` and `PATCH` both answer with the choices, so
the dashboard lists what the project may pick. A provider that fails is a 500.

### Health

| Method | Path      | Description                                                                             |
| ------ | --------- | --------------------------------------------------------------------------------------- |
| `GET`  | `/ping`   | Liveness (no auth)                                                                      |
| `GET`  | `/health` | RabbitMQ and logger status (no auth); 503 unless both are `up`                          |

The logger check calls `RPCServer.Status`. It is `down` if the logger cannot be reached or does not answer within 2s, and `degraded` while the logger's startup tasks are failing and being retried; the error carries the logger's last startup error.

## Authentication

Two middleware layers:

- **`requireAPIKey`** — validates the `Authorization: Bearer lw_...` token through the logger (`RPCServer.ValidateAPIKey`); results are cached with TTL + rate limiting so the hot path does not make an RPC call per request. A cached key is evicted as soon as it is revoked (`DELETE /projects/{id}/keys/{keyID}`) or its project deleted (`DELETE /projects/{id}`), via `forgetCachedKeys`, so it stops working at once rather than after the 60s TTL. A validation already in flight during an eviction is not cached, since it may have read the key before the revoke. The eviction is local: a second broker replica would keep its entry until it expires. The key cache and the per-IP failure counters are each capped at 10,000 entries. A background sweep deletes expired entries every minute, so a flood of bogus keys from many addresses cannot grow them without bound.
  - **`requireScope`** — runs after it, per public route, and refuses with 403 a key that lacks the route's scope. Keys can end up in browser bundles, so `POST /projects/{id}/keys` gives a key only `ingest` unless the caller asks for `read` or `delete`. A key created before scopes existed has none stored and is read back with all three, so it keeps working. The key cache holds the scopes too.
- **`requireInternalSecret`** — validates the `X-Internal-Secret` header; used exclusively by the dashboard backend.

## Write path

```
Client → POST /logs → requireAPIKey → publish to RabbitMQ → confirmed → 202 Accepted
```

A 202 means RabbitMQ holds the event on disk (`event.Emitter`, `events.go`):

- **Persistent** messages on the durable `logwolf_logs` queue survive a RabbitMQ restart.
- **Publisher confirms:** the broker answers only once RabbitMQ has confirmed every event of the request, within 10s. If it does not, the answer is **503**, which the SDK retries. A batch is published on one channel and confirmed as a whole; one that fails part-way may have queued some events, so a retry can store those twice.
- **The broker declares the queue** and its `log.*` binding at start, as the listener does. The exchange drops what no queue is bound for, so before, events sent before the listener had first run went nowhere.
- **Reconnects:** the emitter dials RabbitMQ again once its connection has closed, as it does when RabbitMQ restarts. The request that finds it closed tries once; `/health` reconnects too.

**Ingestion rate** (`ingestlimit.go`): events sent with an API key are then held to the key's rate. Each key has a token bucket sized from the plan of its project (`limits.Plan.IngestRate`, events a second, and `IngestBurst`, what the bucket holds). Each event takes a token. A request whose events do not fit is a **429** with `Retry-After` (seconds until they would, rounded up), and nothing of it is queued; a batch larger than the bucket needs a full one, and empties it. The plan is asked once per key and again after `ingestPlanTTL` (1 minute), so a plan change reaches a busy key within it; a key whose plan cannot be looked up before it has a bucket gets a **500**, and one that has a bucket keeps its size. Self-hosted plans set no rate. Events from the dashboard (`POST /projects/{id}/logs`) carry no key and are not limited. The buckets are this broker's own, capped at 10,000, and swept with the auth caches once full and idle; with more than one replica each holds a key to the rate separately, until they move to shared storage. The per-IP 429 sends `Retry-After` too, the rest of its window.

**Usage metering** (`usage.go`): once RabbitMQ has confirmed a request's events, they are counted against their project: one per event, and the size of its queued message as its bytes. That is every event the broker answers **202** for, the dashboard's included, and none it refuses or cannot queue. The counts are kept in memory per project per hour, and flushed to the logger (`RPCServer.RecordUsage`) every `USAGE_FLUSH_INTERVAL` (1 minute), and once more on shutdown, after the last request has drained. A flush sends the running totals of the hours that changed since the last successful one, under a source naming this broker run (host and a random suffix); the logger keeps the larger of what it has and what it is sent, so a failed flush is simply sent again by the next, and one whose reply was lost counts nothing twice. Hours that are over are forgotten once the logger has them.

The loss window: a broker that is killed or crashes loses what it counted since its last flush, at most `USAGE_FLUSH_INTERVAL` of events. One stopped cleanly loses nothing, unless the logger cannot be reached for the final flush. A restarted broker is a new source, so what the previous run flushed stays counted, and the new run's counts add to it.

Every event's severity is normalized before anything is published (`data.NormalizeSeverity`): trimmed and lower-cased, so `ERROR` is stored as `error`, which is what the metrics count. A severity that is not `info`, `warning`, `error` or `critical`, a missing one included, is a **400** naming it (`event N: …` within a batch), and nothing of the request is queued. Events stored before this keep the casing they were sent with; they are not migrated.

Events are published to the `logs_topic` exchange with routing key `log.<severity>` (`data.SeverityRoutingKey`): `log.info`, `log.warning`, `log.error` or `log.critical`. A batch publishes each event under its own. The broker has no MongoDB client at all: API keys, like everything else it stores or reads, go through the logger's RPC methods.

## Read path

```
Client    → GET /logs                   → requireAPIKey       → RPC call to Logger:5001 → response
Dashboard → GET /projects/{id}/logs     → requireProject      → RPC call to Logger:5001 → response
```

## Environment variables

| Variable               | Default                       | Description                 |
| ---------------------- | ----------------------------- | --------------------------- |
| `RABBITMQ_URL`         | `amqp://guest:guest@rabbitmq` | RabbitMQ connection string  |
| `BROKER_PORT`          | `80`                          | HTTP listen port            |
| `LOGGER_RPC_ADDR`      | `logger:5001`                 | Logger RPC address          |
| `INTERNAL_API_SECRET`  | —                             | Shared secret for dashboard |
| `TRUSTED_PROXIES`      | — (trust no one)              | See below                   |
| `LOGWOLF_EDITION`      | `selfhosted`                  | Picks the `limits.Provider` |
| `USAGE_FLUSH_INTERVAL` | `1m`                          | How often usage is flushed  |

`TRUSTED_PROXIES` is a comma-separated list of IPs and CIDR ranges. A request from one of them is attributed to the right-most `X-Forwarded-For` entry that is not itself trusted (`clientIP` in `clientip.go`); any other request is attributed to its peer address, and its `X-Forwarded-For` is ignored. The failed-auth rate limiter counts per that address. Behind Caddy it must cover Caddy, or every internet client shares Caddy's counter and ten bad keys from anyone lock out all SDK clients for a minute. `docker-compose.yml` trusts the private ranges, which is safe only while the broker publishes no port. An entry that is not an IP or range stops the broker at start.

## Key dependencies

| Dependency            | Role                            |
| --------------------- | ------------------------------- |
| `go-chi/chi`          | HTTP router                     |
| `go-chi/cors`         | CORS middleware                 |
| `rabbitmq/amqp091-go` | RabbitMQ producer               |
| `logwolf-toolbox`     | Shared models and queue helpers |

## Development

```bash
# Run locally
cd broker && go run ./cmd/api

# Unit tests
cd broker && go test ./cmd/api/... -v
```

## Relationship to other services

| Service  | Relationship                                        |
| -------- | --------------------------------------------------- |
| RabbitMQ | Broker publishes events here on write               |
| Logger   | Broker calls Logger via RPC on read/delete and keys |
| Caddy    | Reverse-proxies public traffic to Broker            |
| Frontend | Calls internal routes using the shared `API_SECRET` |
