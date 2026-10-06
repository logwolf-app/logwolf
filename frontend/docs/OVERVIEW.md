# Frontend — Overview

## Purpose

Server-side-rendered React dashboard for managing and viewing Logwolf data. Provides authenticated views for log events, API keys, system settings, and usage analytics.

## Tech stack

| Layer          | Technology                                                  |
| -------------- | ----------------------------------------------------------- |
| Framework      | React Router v7 (SSR)                                       |
| UI             | React 19, Tailwind CSS 4, Radix UI, Shadcn-style components |
| Charts         | Recharts                                                    |
| Auth           | GitHub OAuth 2.0 + React Router cookie sessions             |
| Build          | Vite + TypeScript                                           |
| Error tracking | `@logwolf/client-js` (the SDK, eating its own dog food)     |

## Source layout

```
app/
├── root.tsx              # App root layout, global error boundary, Logwolf middleware
├── entry.server.tsx      # SSR entry point, streaming HTML render
├── routes.ts             # Route definitions
├── context.ts            # React context for event tracking
├── app.css               # Global Tailwind CSS
├── components/
│   ├── nav/              # Header, sidebar, organization and project switchers, page wrapper
│   ├── settings/         # Layout and action result shared by the settings pages
│   └── ui/               # 25+ reusable UI primitives (button, table, dialog, etc.)
├── pages/
│   ├── layout.tsx        # Authenticated layout wrapper
│   ├── home/             # Public landing page
│   ├── auth/             # GitHub OAuth login flow
│   ├── dashboard/        # Metrics overview + charts
│   ├── events/           # Event list, detail view, create form
│   ├── keys/             # API key management
│   ├── projects/         # Project list, create, switch, per-project settings
│   ├── organizations/    # Organization switch and settings
│   └── settings/         # Redirect to the current project's settings
├── lib/
│   ├── api.ts            # Dashboard API client (calls Broker internal routes)
│   ├── logwolf.ts        # Logwolf SDK setup for client-side error tracking
│   ├── auth.server.ts    # Server-side GitHub OAuth logic
│   ├── signup.server.ts  # SignupPolicy: who may sign in, picked by LOGWOLF_EDITION
│   ├── allowlist.server.ts # The self-hosted policy's users/orgs allowlist (deny by default)
│   ├── session.server.ts # cookie session helpers
│   ├── token.server.ts   # seals the GitHub token kept in the session
│   ├── csrf.server.ts    # CSRF token generation + validation
│   ├── format.ts         # Formatting utilities (dates, numbers)
│   ├── parse.ts          # Parsing utilities
│   ├── organizations.ts  # Current organization and project, projects in view, roles
│   ├── redirect.ts       # Keeps the switchers' redirects on this origin
│   ├── slug.ts           # Project name -> URL-safe slug
│   └── utils.ts          # General utilities
├── hooks/
│   ├── use-csrf-token.ts # Fetch CSRF token for form submissions
│   ├── use-projects.ts   # Read the user's projects from the layout loader
│   ├── use-organizations.ts # Read the user's organizations from the layout loader
│   └── use-mobile.ts     # Detect mobile viewport
└── store/
    └── theme-provider.tsx # Dark/light mode provider (next-themes)
```

## Routes

| Path                          | Auth      | Description                                  |
| ----------------------------- | --------- | -------------------------------------------- |
| `/`                           | Public    | Landing page                                 |
| `/auth`                       | Public    | GitHub OAuth login                           |
| `/dashboard`                  | Protected | Metrics overview with charts                 |
| `/events`                     | Protected | Paginated event list                         |
| `/events/new`                 | Protected | Create a new event                           |
| `/events/:id`                 | Protected | Event detail view                            |
| `/keys`                       | Protected | API keys and their scopes                    |
| `/settings`                   | Protected | Redirects to the current project's settings  |
| `/projects`                   | Protected | Projects the user belongs to                 |
| `/projects/new`               | Protected | Create a project in the current organization |
| `/projects/switch`            | Protected | POST-only project switch                     |
| `/projects/:id/settings`      | Protected | Rename, retention, members, delete           |
| `/organizations/switch`       | Protected | POST-only organization switch                |
| `/organizations/:id/settings` | Protected | Rename, plan and usage, members              |

## Project selection

Every protected page except `/projects/new` is scoped to one project, held in the
session as `currentProjectID`. The layout loader owns that value: it re-points the
session when the stored project is gone or the user lost access to it, and it sends
a user with no project in view (no project at all, or none in their organization;
below) to `/projects/new` — the one page that renders without a current project.

Switching projects is a POST to `/projects/switch`, which re-checks membership
server-side before writing the session. The sidebar switcher and the `/projects`
list both go through it.

Pages take the project from the session and nowhere else — never from the URL or
a hidden form field. Every loader and action calls `getCurrentProjectID()` and
passes the result to the API client, so the redirect back from
`/projects/switch` revalidates them straight into the new project.

That includes `/events`, which reads and writes through the broker's
`/projects/:id/logs` routes rather than the SDK. The SDK authenticates with the
dashboard's own `API_KEY`, which belongs to one fixed project — it could never
follow the switcher, and it would have shown every user that project's events.
`lib/logwolf.ts` keeps using it for what it is: the dashboard's own telemetry.

## Organization selection

The organization the user works in is kept in the session too, as
`currentOrganizationID`, and the layout loader keeps it honest the same way
(`resolveCurrent` in `app/lib/organizations.ts`): the stored organization while
the user is still a member of it, else the stored project's, else their first.
A user in no organization, as most are on a self-hosted deployment, where only
the `Default` organization's owners belong to it, has none and sees the dashboard
as before organizations.

The organization decides which projects are in view (`projectGroups`): its own,
and those shared with the user project by project from organizations they are
not in, which no organization switch would reach. The current project must be
one of them, so the layout re-points a project left over from another
organization, and sends a user with none in view to `/projects/new`. The sidebar
switches organization above the project switcher, which lists the organization's
projects and, apart, the shared ones; `/projects` still lists every project.

Switching is a POST to `/organizations/switch`, which re-checks membership like
the project switch, and moves the session to the organization's first project
(or none, for the layout to settle) unless the current one is already in it. It
lands on the same page when that page only reads the session, the new
organization's settings from another's, and `/dashboard` otherwise
(`pathAfterOrganizationSwitch`). Opening a project of another of the user's
organizations through `/projects/switch` moves them into that organization.

`/projects/new` creates the project in the organization in session
(`POST /organizations/{id}/projects`), which any member may; a user in no
organization creates it through `POST /projects`, in the `Default` one.

## Project settings

`/projects/:id/settings` holds everything scoped to one project: its name, the
retention window, the member list, and deletion. Both the loader and the action
resolve the `:id` against the caller's own project list, so a project the user
does not belong to sends them back to `/projects` instead of surfacing a 403.

Every section except retention is owner-only — the broker enforces that as well,
so the role checks in the route are there to keep a stale tab from producing a
bare "forbidden". Retention is open to any member, but only upwards: shortening
it makes the next cleanup pass delete everything outside the new window, so a
member sees the shorter options disabled, and the action checks the stored value
with `lowersRetention` (`app/lib/retention.ts`) before calling the broker. The
options themselves come from the broker (`getRetention` answers `choices` from
its `limits.Provider`), so a plan can offer fewer; a current value the choices no
longer include is still shown, disabled (`retentionOptions`). API
keys are not project settings; any member may create and revoke them on `/keys`.
Owners change a member's role from a select in the members
table; the last owner shows a plain badge instead, since the broker would refuse
to demote them. Deleting a project clears `currentProjectID` when it was the
one in session and returns to `/projects`, where the layout takes over.

Adding a member checks the login first (`checkInvitee` in
`app/lib/allowlist.server.ts`). A login GitHub does not know, or an
organization, is refused. Otherwise GitHub's answer gives the user's numeric
ID, which the membership belongs to, and the member is added by that ID under
GitHub's casing of the login, with a warning when the allowlist does not clear
them. If GitHub cannot be asked who the login is, there is no ID and nobody is
added.

Role changes and removals name the member by the membership's `id`; the login
in the form only words the message. The "you" badge goes by user ID, or, for a
membership stored before user IDs, by login, as the broker matches it. Such a
membership, with no `user_id`, also carries a "not yet linked" badge: the logger
links it to whoever next signs in under its login, and until then the login is
all that decides who has it. One whose holder never signs in again stays that
way until an owner removes it.

Org membership is asked with the inviting owner's own GitHub token, kept from
sign-in. GitHub shows private members only to someone in the org, so an owner in
an allowed org gets a definite answer for its private members too. Where GitHub
will not answer the owner (they are not in that org, or the session predates
tokens being kept, or the token was revoked), the check falls back to public
membership, and a user it cannot clear gets a warning that allows for private
membership. The same warning, worded for it, covers GitHub failing the org
check after the user lookup answered: that never blocks an owner.

## Organization settings

`/organizations/:id/settings` resolves the `:id` against the caller's own
organizations (anyone else goes back to `/dashboard`), and shows the name, the
plan with its usage (`getOrganizationPlan`: projects and members against the
plan's limits, where 0 is unlimited; monthly events and the longest retention,
which are not counted yet), and the members. Owners and admins rename it and
manage members; only owners add, promote, demote or remove an owner, so an admin
sees an owner's row without controls and cannot pick `owner` when adding. The
broker enforces all of it, and the action repeats what the form alone shows (the
caller's role, the role asked for); whether a change touches an owner only the
broker knows, and its refusal is what the page shows. Members are added through
the same GitHub check as project members (`checkInvitee`, `inviteeFromCheck`),
by user ID, and named by their membership's `id` afterwards.

## Authentication

1. User initiates login via GitHub OAuth 2.0.
2. On callback, the server asks the edition's `SignupPolicy` (`lib/signup.server.ts`) whether the
   login may sign in; `LOGWOLF_EDITION` picks it once, at startup. Self-hosted, the default, checks
   the GitHub user against `LOGWOLF_ALLOWED_GITHUB_USERS` and
   `LOGWOLF_ALLOWED_GITHUB_ORGS` (`lib/allowlist.server.ts`). Access is denied by default: the login
   must be in the users list or belong to an org in the orgs list. With both lists empty nobody can
   sign in, and the server logs an error at startup. Both lists are parsed like the logger's
   `ParseGithubLogins` (trimmed, lowercased, blanks and duplicates dropped), and a failed org lookup
   denies the sign-in.
3. The sign-in is recorded with the broker (`PUT /users/me`): the user is upserted by their numeric
   GitHub ID, which a rename does not change, and their stored login and public email are refreshed.
   If it cannot be recorded, the sign-in fails (`/auth?error=unavailable`). Who may sign in is still
   decided by login, above, so self-hosted configuration is unchanged.
4. A session cookie is issued for subsequent requests (React Router's cookie session: signed with
   `SESSION_SECRET`, not encrypted, so readable by whoever holds it). It keeps the user's GitHub
   OAuth token too, for the invite check, **sealed** with AES-256-GCM under a key derived from
   `SESSION_SECRET` (`lib/token.server.ts`). The token carries the scopes sign-in asks for,
   `read:user read:org`, and goes nowhere but `api.github.com`. Changing `SESSION_SECRET` signs
   everyone out and makes old tokens unreadable. The session's `githubUser` carries the GitHub user
   ID (`id`) next to the login, which keeps GitHub's casing for display.
5. All protected routes validate the session server-side before rendering (`requireAuth`). A session
   from before the ID was kept has none, and is sent back to sign in, which records the user.

CSRF tokens are required on all mutating form submissions.

## API communication

`lib/api.ts` exports an `Api` class that calls Broker's **internal routes** using the `X-Internal-Secret` header (sourced from `INTERNAL_API_SECRET`). The frontend never calls the public Broker routes — those are for SDK clients only.

The client is request-scoped: `createApi(user)` takes the signed-in user (the session's `githubUser`) and sends their GitHub user ID as `X-User-ID` and their login as `X-User-Login` on every call. The broker checks project membership against the user ID; the login only finds memberships stored before user IDs. Project-scoped methods (`getKeys`, `createKey`, `deleteKey`, `getMetrics`, `getRetention`, `updateRetention`, `getLogs`, `getLog`, `createLog`, `deleteLog`, and everything under `projects`) take the project id as an argument, so a route has to state which project it means; they all call the broker's `/projects/{id}/...` routes. The organization methods (`getOrganizations`, `updateOrganization`, `getOrganizationPlan` and the `*OrganizationMember*` ones) do the same with `/organizations/{id}/...`, and `createProject` takes an optional organization id, which sends it to `POST /organizations/{id}/projects` rather than `POST /projects`.

Event payloads come back exactly as the broker stores them, so `getLogs`/`getLog` decode them with the SDK's own `LogwolfEventSchema` — `data` back into an object, timestamps back into `Date`s. Pages therefore keep working with `LogwolfEventData`, unchanged by the move off the SDK transport.

## Error tracking

`lib/logwolf.ts` initialises the Logwolf JS SDK. `root.tsx` wires it into the React Router middleware so every navigation and unhandled error is captured automatically.

It is optional: with `API_KEY` unset or blank, `logwolfFromEnv` returns no client, so a fresh install boots before anyone has made a key on the Keys page. The middleware then puts no event in context (routes already read it as `event?.`), and `handleError` logs server errors to the console instead. A key that is set but malformed still fails at start, from the SDK's own validation.

## Environment variables

| Variable                       | Description                                                          |
| ------------------------------ | -------------------------------------------------------------------- |
| `API_URL`                      | Broker base URL (e.g. `http://broker/`)                              |
| `INTERNAL_API_SECRET`          | Shared secret for internal Broker routes                             |
| `GITHUB_CLIENT_ID`             | GitHub OAuth app client ID                                           |
| `GITHUB_CLIENT_SECRET`         | GitHub OAuth app client secret                                       |
| `LOGWOLF_ALLOWED_GITHUB_USERS` | Comma-separated list of allowed GitHub usernames                     |
| `LOGWOLF_ALLOWED_GITHUB_ORGS`  | Comma-separated list of GitHub orgs whose members are allowed        |
| `SESSION_SECRET`               | Signs session cookies; the key sealing GitHub tokens derives from it |
| `API_KEY`                      | Optional `lw_` key for the dashboard's own telemetry                 |
| `LOGWOLF_EDITION`              | `selfhosted` (default) picks the allowlist as the `SignupPolicy`     |

Copy `.env.example` to `.env` before running locally.

## Development commands

```bash
pnpm run dev       # Vite dev server (hot reload)
pnpm run build     # react-router build → build/
pnpm run typecheck # react-router typegen + tsc --noEmit
pnpm run lint      # oxlint
pnpm test          # vitest, single run
```

## Tests

Vitest runs in node (`vitest.config.ts`) and covers the server side, where access and project scoping are decided:

- **Libraries:** the allowlist and invite check, retention, and `lib/api.ts`, whose tests check every project-scoped call names the project in the path, sends the internal secret and the user's login, and encodes what it puts in a path.
- **Route loaders and actions** (`*.test.ts` next to each route), called the way React Router calls them. Requests carry a real signed session cookie, built by the helpers in `app/test/routes.ts`, so sessions and CSRF are the real code. Only the broker client (`createApi`) is replaced, by `fakeApi`, whose every method rejects unless the test stubs it, and GitHub's API is stubbed on `fetch` where a test needs it.

Covered: the layout's session repair and first-project redirect, organization selection and the projects in view, the project and organization switchers (membership, CSRF, open redirects), creating a project in the organization in session, project settings (owner-only intents, retention, invites, delete), organization settings (admin-only intents, owner-only owners, invites), keys and event pages (always the project in session, whatever the form says). Components are not rendered in tests.

## Relationship to other services

| Service | Relationship                                                        |
| ------- | ------------------------------------------------------------------- |
| Broker  | Frontend calls Broker internal routes for data and admin operations |
| Caddy   | Reverse-proxies public traffic to the Frontend                      |
| GitHub  | OAuth provider for user authentication                              |
