# Toolbox — Overview

## Purpose

Shared Go library used by Broker, Listener, and Logger. It centralises data models, MongoDB collection helpers, RabbitMQ connection and queue setup, and JSON utilities so that each service stays thin.

Toolbox is a local module in the Go workspace (`go.work` at the repository root) and is imported as `logwolf-toolbox/...`. It is never published to an external registry.

## Package layout

```
toolbox/
├── data/
│   ├── models.go    # Models struct, LogEntry, APIKey, Settings types + CRUD methods
│   ├── apikey.go    # APIKey model, key generation, validation
│   ├── settings.go  # Per-project retention settings, index management
│   ├── project.go   # Project and ProjectMember models, membership queries
│   ├── organization.go # Organization and OrganizationMember models, membership queries
│   ├── user.go      # Users, keyed by GitHub user ID
│   ├── usage.go     # Usage metering: hourly buckets of accepted events, bytes and storage
│   ├── migrate.go   # Startup migration of pre-multi-tenancy data
│   └── log.go       # Log entry type aliases
├── event/
│   ├── event.go     # Exchange + queue declarations
│   ├── emitter.go   # RabbitMQ message publisher
│   ├── consumer.go  # RabbitMQ message consumer: manual acks, retries
│   └── logger_client.go # The consumer's one reused RPC connection to Logger
├── limits/
│   ├── limits.go    # LimitsProvider: what an edition lets a project do
│   └── plans.go     # The plans, in code: what each lets an organization do
├── rabbitmq/
│   └── connect.go   # RabbitMQ connection initialisation
└── json/
    └── helpers.go   # JSON encode/decode utilities
```

## `data` package

### `Models` struct

The central database accessor. Services initialise one `Models` value and pass it around:

```go
models := data.New(mongoClient)
// then use:
models.LogEntry.Insert(entry)
models.LogEntry.AllLogs(queryParams)
models.LogEntry.DeleteOne(filter)
models.APIKey.Insert(key)
models.Settings.Get()
```

### `LogEntry`

Represents a single log record in MongoDB. Key fields: `Name`, `Data`, `Severity`, `Tags`, `Duration`, `CreatedAt`, `UpdatedAt`.

### `APIKey`

Stores API key metadata: project ID, bcrypt hash, the key's first 10 characters in clear as `prefix` (`lw_` + 7), active flag, and created/revoked timestamps. `GenerateAPIKey` builds `lw_` + 32 random bytes in base64url, 46 characters in all.

`Scopes` lists what the key may do on the public `/logs` routes: `ingest`, `read`, `delete` (`AllScopes`). `NormalizeScopes` validates, deduplicates and orders them; an empty list becomes `DefaultScopes`, which is `ingest` alone. A key stored before scopes existed has none, and every read path fills in `AllScopes` and sets `Legacy`, which is not stored, so it keeps the full access it always had.

`ValidateAPIKey` refuses anything not shaped like that without a query. For the rest, it fetches only the active keys that share the prefix, normally exactly one, and bcrypts those. Its cost does not grow with the number of keys across projects. `EnsureAPIKeyIndexes` creates the `prefix` index that lookup uses; Logger calls it on startup.

`SaveAPIKey` sets the key's `ID` to the one it was stored under. `RevokeAPIKey` takes the project as well as the id and matches both, so an id from another project is `ErrKeyNotFound`, the same as one that never existed. Only Logger calls these; the Broker reaches them over RPC with the `RPC*APIKey*` argument types.

### `Settings`

Manages per-project settings documents (currently: retention in days), keyed by `(project_id, key)`. `LowersRetention(from, to)` says whether a change shortens retention, 0 (forever) being the longest; the Broker lets only owners make such a change.

### Project ids

Every `project_id` (`LogEntry`, `APIKey`, settings, `ProjectMember`) is a `primitive.ObjectID`, the type of `projects._id`, and every `Models` method takes one. A filter with the wrong type matches nothing without an error, so there is only one. RPC argument types (`QueryParams`, `ProjectArgs`, `RetentionArgs`, the `RPC*` structs) still carry the id as a hex string; Logger parses it in its RPC layer. JSON output is unchanged: an ObjectID marshals to its hex string.

### Projects and members (`project.go`)

A project's `Slug` is a display label, derived from its name when it is created and fixed after that. It is not unique and nothing looks a project up by it, so creating a project never reveals that another user has one with the same slug. The migration's `Default` project is found by its `Default` flag (`default` in BSON), which a partial unique index (`unique_default`) allows on one project only. `RenameProject` changes the name alone.

Three operations run in MongoDB transactions, so MongoDB must run as a replica set (a single member is enough, and that is how `docker-compose.yml` and the integration tests run it):

- `CreateProjectWithOwner` inserts a project and its first owner's membership as one unit. A project without an owner could never be reached, since only owners add members. The project is created inside `p.OrganizationID` (`organization_id`), which is required and must exist (`ErrUnknownOrganization`); a `project_organization_id` index serves lookups by organization.
- `DeleteProject` removes the project's API keys, settings, members and the project itself as one unit. A failure part-way rolls the whole thing back. The logs are left out, since a big project's would outlast the transaction; `PurgeProjectLogs` deletes them afterwards and refuses (`ErrProjectExists`) for a project that still exists.
- `RemoveProjectMember` and `UpdateProjectMemberRole` count the owners and remove or demote the member in one transaction (`changeMembers`), and write to the project document first (`members_updated_at`). A transaction on its own would still let two concurrent removals or demotions of different owners both pass the count. The write to a shared document forces a write conflict, `WithTransaction` retries the loser, and the retry sees `ErrLastOwner`. Setting the role a member already holds is a no-op.

A membership belongs to a user: `user_id` is the member's GitHub user ID, the key of `users`, and access is decided by it. `github_login` is the login the member was added under, kept for display. Memberships stored before user IDs have no `user_id` (the field is absent; `UserID` is 0); they are matched by login until they are linked to a user. `MemberFilter(userID, login)` is that rule, and `MemberRole` and `GetProjectsForUser` take both and use it: a membership linked to a user ID is never matched by login, so a login someone renamed away from, and another account then took, carries none of their memberships. A unique, partial `(project_id, user_id)` index (`unique_project_member_user`) keeps a user to one membership per project, whatever login they were added under, and a `user_id` index serves the lookup across projects. `CreateProjectWithOwner` takes the owner's user ID with their login. `RemoveProjectMember` and `UpdateProjectMemberRole` take the membership's own `_id`, which names one row of the project whether or not it is linked yet. `GetProjectMembers` lists a linked member who has signed in under the login of their last sign-in, read from `users`.

`LinkMemberships(githubID, login)` is how memberships get linked: Logger calls it at each sign-in, the one moment GitHub vouches for which account holds a login, and it sets `user_id` on every membership with none stored under that login (normalized). A membership linked to anyone is left alone, whatever login it stores. If the user already holds a membership in the project, the `(project_id, user_id)` index refuses the link, and the login-only row is merged into theirs instead (through `changeMembers`), which keeps the higher role and the older join date. It reports how many it linked and merged (`MembershipLinks`), and is idempotent. Memberships of people who never sign in again stay login-only until an owner removes them. A `github_login` index serves the lookup.

GitHub logins are case-insensitive, so `project_members` stores them normalized (`NormalizeGithubLogin`: trimmed, lowercase). `InsertProjectMember` and `MemberFilter` normalize the login they are given. That lets a login typed in any casing match the one GitHub returns at sign-in, and the unique `(project_id, github_login)` index refuses case-only duplicates.

`ProjectExists` answers whether a project id names a project. Logger uses it to refuse events for deleted projects, and `DeleteOrphanedLogs` (in `models.go`) removes the ones that got through: logs whose `project_id` matches no project and that are older than a minute, so it never races a project being created. It skips logs with no or an empty `project_id`, which belong to the startup migration. A `project_id` still stored as a string is compared by its text, so a live project's logs that `ConvertProjectIDs` has not reached yet are never taken for orphans. It, `PurgeProjectLogs` and `DeleteExpiredLogs` all delete in batches of 10,000, each with its own 30s timeout, so a project with millions of logs is purged over as long as it takes rather than failing on one `DeleteMany`.

### Users (`user.go`)

A `User` is someone who has signed in, keyed by their GitHub user ID (`github_id`), which a unique index (`unique_github_id`, from `EnsureUserIndexes`) keeps to one user per account. Logins are not identities: GitHub lets people rename their account, and another account can then take the old name. So `github_login` is only the login the user had at their last sign-in, normalized like membership logins, for display and invites; after a rename two users can briefly share one.

`UpsertUser(githubID, login, email)` records a sign-in: it creates the user, or refreshes the login and email of the existing one, and returns the user as stored. The `_id` and `created_at` never change. An empty email clears a stored one, since the user has made theirs private. Two simultaneous first sign-ins of one account leave one user: the unique index refuses the second insert, which retries as an update. It refuses (`ErrInvalidUser`) a user with no positive GitHub ID or no login. `GetUserByGithubID` returns `mongo.ErrNoDocuments`, wrapped, for someone who has never signed in. Logger exposes both over RPC (`RPCUpsertUserArgs`, `RPCGetUserArgs` → `RPCGetUserReply`).

### Organizations (`organization.go`)

An `Organization` sits above projects: it owns them, holds the plan, and so sets their limits. It stores `name`, `plan` (the plan's name), `billing_customer_id` (the billing provider's id for it, empty on self-hosted deployments, where nothing bills) and `created_at`. `organizations._id` and `organization_members.organization_id` are `primitive.ObjectID`s, like project ids, and every `Models` method takes one; RPC arguments carry them as hex strings.

An `OrganizationMember` is a user's membership: `organization_id`, `user_id` (the GitHub user ID, the key of `users`) and `role`, one of `owner`, `admin` (`RoleAdmin`) or `member` (`ValidOrganizationRole`). Organizations came after user IDs, so unlike project memberships none is login-only: lookups go by `user_id` alone. `github_login` is the login the member was added under, kept for display; `GetOrganizationMembers` lists a member who has signed in under the login of their last sign-in, read from `users`. `EnsureOrganizationIndexes` creates the unique `(organization_id, user_id)` index (`unique_organization_member`), one membership per user per organization, and a `user_id` index for `GetOrganizationsForUser`.

- `CreateOrganizationWithOwner` inserts an organization and its first owner's membership in one transaction; it refuses one without a name or a plan (`ErrInvalidOrganization`) or an owner (`ErrInvalidUser`). `RenameOrganization` changes the name alone.
- `RemoveOrganizationMember` and `UpdateOrganizationMemberRole` take the membership's own `_id` and run in a transaction that writes to the organization's document first, as `changeMembers` does for projects (both go through `changeMembersOf`), so two concurrent changes can never leave an organization without an owner: the loser gets `ErrLastOrganizationOwner`. An admin is not an owner, and does not count as one. Both take the role of whoever makes the change (`actorRole`), and refuse anyone but an owner a change that removes, demotes or promotes an owner (`ErrOwnerRequired`), deciding on the member's role as read in the transaction.
- `GetOrganizationUsage(ctx, orgID)` counts what the organization has of what its plan limits (`OrganizationUsage`): its projects, its members, and its events this calendar month, from `usage` (below).
- `ProjectPlan(ctx, projectID)` is the name of the plan of the project's organization, which the hosted edition's `limits.Provider` resolves the project's limits from; `ErrUnknownOrganization` for a project in no organization, or in one that does not exist.
- `OrganizationRole(orgID, userID)` is the user's role, `""` for a non-member or a missing organization; `OrganizationExists` tells the two apart. `GetOrganizationsForUser` lists the user's organizations with their role in each (`UserOrganization`).

Project roles stay what each project's memberships say. Organization roles do not carry into projects, except one: an organization owner is owner of every project in the organization. `EffectiveProjectRole(projectRole, orgRole)` is that rule. `AccessToProject(projectID, userID, login)` applies it for the logger's `ProjectAccess`: whether the project exists and the caller's role in it, from their own membership, or owner when they own its organization. `GetProjectsForUser` lists the projects of the organizations the user owns as well, as owner.

Every project belongs to an organization. The deployment has one the startup migration creates, `Default` (`DefaultOrganizationName`, on `SelfHostedPlan`), found by its `Default` flag, which a partial unique index (`unique_default`, from `EnsureOrganizationIndexes`) allows on one organization only; `GetDefaultOrganization` returns it. Its owners can include logins nobody has signed in under yet, which no membership can name, so those are kept on the organization as `pending_owners`. `ClaimPendingOwnerships(githubID, login)` runs at each sign-in, like `LinkMemberships`: it makes the user an owner of each organization waiting for their login (promoting a member), and takes the login off the list, in a transaction serialized with the organization's other member changes. A `pending_owners` index serves the lookup.

### Usage (`usage.go`)

The `usage` collection holds what each project uses, in hourly buckets: one document per `project_id`, `hour` (`UsageHour`: the start of the hour, in UTC) and `source`. `EnsureUsageIndexes` creates the unique `(project_id, hour, source)` index (`usage_project_hour_source`), the `(organization_id, hour)` index (`usage_organization_hour`) and a TTL index on `hour` (`usage_hour_ttl`) that expires a bucket `UsageRetention` (400 days) after its hour, whatever the project's log retention.

- `RecordUsage(ctx, source, counts)` stores a Broker run's running totals (`UsageCount`: `events` and `bytes` per project and hour), each bucket keeping the larger of its value and the one sent, so a count sent again, or late, changes nothing. Each bucket also gets the `organization_id` of its project's organization, read at the write; one whose project is gone keeps the one it has. It refuses (`ErrInvalidUsage`) an empty source, the storage job's (`StorageSource`), or a negative count, before any write. Over RPC the counts carry the project as a hex string (`RPCRecordUsageArgs`, `RPCUsageCount`).
- `MeasureProjectStorage(ctx, projectID)` counts a project's logs and adds up their BSON sizes (`ProjectStorage`); it reads them all, so only the Logger's periodic job calls it. `RecordProjectStorage(ctx, projectID, at, storage)` writes the measure to the hour's `StorageSource` bucket (`stored_events`, `stored_bytes`, `measured_at`), replacing an earlier one of the same hour.
- `GetProjectUsage(ctx, projectID, from, to)` adds up the events and bytes every Broker run recorded in the hour buckets from `from`'s up to `to`, and takes the last storage measure before `to` (`ProjectUsage`).
- `GetOrganizationEvents(ctx, orgID, from, to)` adds up the events of every bucket naming the organization in the window, deleted projects' included, so deleting a project gives back nothing of the monthly quota. `UsageMonth` and `NextUsageMonth` bound a calendar month in UTC.
- `ProjectQuota(ctx, projectID, now)` is what the monthly quota needs of a project (`ProjectQuota`): its organization's id (hex), that organization's plan name, the month of `now`, and the organization's events in it so far. It fails like `ProjectPlan`.

### Startup migration (`migrate.go`)

Adopts data written before projects existed, and projects created before organizations. Logger calls it on every start; Broker and Listener never do.

The owner memberships it writes for `Default` carry a login alone: Logger has no GitHub token, so it cannot tell which account a configured login is. `LinkMemberships` links each at that user's first sign-in. `ensureOwners` only matches memberships not linked yet; it skips a login a linked membership still stores rather than promote it or add a second row next to it.

| Function                         | Description                                                                             |
| -------------------------------- | --------------------------------------------------------------------------------------- |
| `ConvertProjectIDs`              | Rewrites hex string `project_id`s in `logs`, `api_keys`, `settings` to ObjectIDs        |
| `MarkDefaultProject`             | Flags the Default project of earlier builds, then drops the old unique slug index       |
| `CountOrphanedDocuments`         | Counts `logs`, `api_keys`, and `settings` documents with no project ID                  |
| `MigrateOrphansToDefaultProject` | Adopts those documents into the `Default` project, creating it and its owners if needed |
| `EnsureDefaultProjectOwners`     | Gives an ownerless `Default` project its owners, promoting existing members if listed   |
| `EnsureDefaultOrganization`      | Creates the `Default` organization, owned by `Default`'s owners, and moves every project without an organization into it |
| `NormalizeMemberLogins`          | Lowercases stored member logins, merging case-only duplicates into the higher role      |
| `DropLegacyTTLIndex`             | Removes the global TTL index that predates per-project retention                        |
| `ParseGithubLogins`              | Splits a comma-separated allowlist into normalized logins (deduplicated ignoring case)  |

`MigrateOrphansToDefaultProject` returns a nil `*MigrationReport` when there is nothing to adopt, which is what makes repeated runs a no-op.

That is also why it can't be trusted to add owners on its own. If its owner step fails, or runs with an empty owner list, the next start has no orphans left and returns early. `EnsureDefaultProjectOwners` runs independently of the orphan count and only acts while `Default` has no owner. It returns a nil `*OwnerRepair` when there is no `Default` project or it already has an owner.

`EnsureDefaultOrganization` runs after it, on every start, fresh installs included, so a first project always has an organization to be created in. It creates the `Default` organization with its owners in one transaction: the `Default` project's owners, as owner members where they are linked to a user ID and as pending owners where they are login-only; or, with no `Default` project or no owner on it, the configured logins, pending. An organization left with neither an owner nor a pending one is given them on a later start, as `EnsureDefaultProjectOwners` does. Then it sets the organization on every project without an `organization_id`. Its `*OrganizationReport` says whether it created the organization, how many owners and pending owners it gave it, how many projects it moved, and whether it is left `Ownerless`.

## `event` package

Declares the RabbitMQ topology used by all services:

- **Exchange**: `logs_topic` (topic type, durable)
- **Named queues**: durable, survive broker restarts
- **Random/exclusive queues**: temporary, used for one-off consumers

`emitter.go` wraps `amqp.Channel.Publish` for structured event publishing.  
`consumer.go` provides `NewConsumer` + `Listen`, the main loop used by Listener. It acknowledges a message only once Logger has stored the event or it has been dropped for good, and retries an unreachable Logger with back-off; see the [Listener overview](../../listener/docs/OVERVIEW.md#delivery-guarantees) for the rules. `logger_client.go` holds the one RPC connection it reuses for every event.

## `limits` package

`Provider` is the extension point between self-hosted and hosted Logwolf: it answers "which plan's limits apply to this project?" (`Plan`), "what monthly event quota do this project's events count toward, and how much of it is used?" (`MonthlyQuota`, a `Quota`: the organization, the plan's `MonthlyEvents` as `Limit`, the month and `Used`), "which retention values may it pick?" (`RetentionChoices`), "what plan is an organization that stores this plan name on?" (`OrganizationPlan`) and "what plan does a new organization start on?" (`NewOrganizationPlan`: `selfhosted` self-hosted, `free` hosted). Self-hosted answers `OrganizationPlan` with its one plan whatever the name; hosted, a name not in the table is an error. Project ids are hex strings, as services pass them to each other. Every retention choice must be one of `data.ValidRetentionDays`, the only values the logger stores.

Plans live in code (`plans.go`), not in the database: an organization stores only its plan's name (`organizations.plan`), and `PlanByName` turns that into a `Plan`: `MonthlyEvents`, `MaxRetentionDays`, `MaxProjects`, `MaxMembers`, and `IngestRate` and `IngestBurst` (each API key's token bucket in the Broker: events a second, and how many it holds; both set or both `Unlimited`), where `Unlimited` (0) sets no limit, and for retention allows forever. A plan's `RetentionChoices` are every supported value up to its maximum, in the dashboard's order, forever first and only when there is no maximum.

| Plan         | Monthly events | Max retention | Max projects | Max members | Ingest rate per key | Burst     |
| ------------ | -------------- | ------------- | ------------ | ----------- | ------------------- | --------- |
| `selfhosted` | unlimited      | forever       | unlimited    | unlimited   | unlimited           | unlimited |
| `free`       | 100,000        | 30 days       | 3            | 3           | 100/s               | 1,000     |
| `pro`        | 5,000,000      | 90 days       | 20           | 20          | 1,000/s             | 5,000     |
| `team`       | 25,000,000     | 365 days      | unlimited    | unlimited   | 5,000/s             | 20,000    |

`LOGWOLF_EDITION` picks the implementation (`FromEnv`, `ForEdition`): `selfhosted`, the default, is `SelfHosted`, which puts every project on the one `selfhosted` plan without looking anything up: no quota, and every supported retention (forever, 30, 60, 90, 180, 365 days, in that order). `cloud` is `Organizations`, which resolves a project's plan and quota through the `Lookups` the service supplies: a `PlanLookup` and a `QuotaLookup` (the Broker's ask the logger, `RPCServer.ProjectPlan` and `RPCServer.ProjectQuota`). A plan name not in the table is an error, never a plan without limits, and so is `cloud` without both lookups, or any unknown edition, so a hosted deployment never runs on self-hosted limits by accident. The Broker asks the provider for retention choices, for each API key's plan to size its ingestion rate, and for each project's monthly quota, which it counts against per organization.

## `rabbitmq` package

Single `Connect(url string) (*amqp.Connection, error)` function with retry logic for startup ordering (RabbitMQ may not be ready when a service starts).

## `json` package

Lightweight wrappers around `encoding/json` used consistently across services for reading request bodies and writing responses.

## Key dependencies

| Dependency            | Role                  |
| --------------------- | --------------------- |
| `rabbitmq/amqp091-go` | RabbitMQ client       |
| `mongo-driver`        | MongoDB client        |
| `golang.org/x/crypto` | Secure key generation |

## Development

Toolbox has its own unit tests:

```bash
cd toolbox && go test ./... -v
```

Because Toolbox is a library with no `main` package, it is not run or deployed independently — it is always compiled into the services that depend on it.
