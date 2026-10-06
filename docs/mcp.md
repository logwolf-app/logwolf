# AI agents (MCP)

Logwolf serves the [Model Context Protocol](https://modelcontextprotocol.io), so AI agents such as Claude Code, Claude Desktop, Cursor or VS Code can read and analyze your events: what is failing, since when, how often, and what the failing events carry.

The server lives in the broker, next to the HTTP API, at `/api/mcp` on your instance, e.g. `https://logs.your-domain.com/api/mcp`. It speaks MCP's Streamable HTTP transport and is stateless, so it works the same behind any number of brokers.

## Connecting an agent

1. On the dashboard's **API Keys** page, create a key with the `read` scope, and nothing else. An agent needs no more, and a key without `ingest` or `delete` can neither add events nor delete them.
2. Point your MCP client at `/api/mcp`, with the key as a Bearer token in the `Authorization` header.

With Claude Code:

```bash
claude mcp add --transport http logwolf https://logs.your-domain.com/api/mcp \
  --header "Authorization: Bearer lw_..."
```

Most other clients take a JSON configuration along these lines; the file and the exact field names vary from client to client, so check its documentation:

```json
{
	"mcpServers": {
		"logwolf": {
			"type": "http",
			"url": "https://logs.your-domain.com/api/mcp",
			"headers": { "Authorization": "Bearer lw_..." }
		}
	}
}
```

The key decides the project, as everywhere in the API: an agent sees that project's events, and no other's. To let it look at several projects, add one server per project, each with that project's key.

## Tools

Every tool is read-only.

| Tool                   | What it does                                                                                                                   |
| ---------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| `get_project_overview` | Totals: events and errors overall and in the last 24 hours, the average duration, the top tags, the retention, and the server's time |
| `count_events`         | Counts the events that match the filters, in total or per `severity`, `name`, `tag`, `hour` or `day` (UTC)                     |
| `search_events`        | Lists the events that match the filters, newest first, a page at a time                                                        |
| `get_event`            | Returns one event by its id, with all of its data                                                                              |

`count_events` and `search_events` take the same filters, and an event has to match every one given:

| Filter     | Keeps events                                                                                 |
| ---------- | -------------------------------------------------------------------------------------------- |
| `severity` | Of any of these severities: `info`, `warning`, `error`, `critical`                           |
| `name`     | With exactly this name                                                                       |
| `tags`     | That carry every one of these tags                                                           |
| `text`     | Whose name or data contains this text, ignoring case (up to 200 bytes)                  |
| `since`    | Created at or after this time                                                                |
| `until`    | Created before this time                                                                     |

`since` and `until` take an RFC 3339 timestamp (`2026-10-01T08:30:00Z`), a date, which is its UTC midnight (`2026-10-01`), or a duration back from now: `30m`, `24h`, `7d`.

`search_events` returns 20 events a page unless asked for up to 100, and says whether another page follows. It cuts each event's data to 1000 bytes, so a page cannot flood the agent's context; `get_event` returns the whole of it. `count_events` returns the 20 largest groups unless asked for up to 200 (a week of hours), or, per hour or day, the latest periods, in time order.

A tool that fails, because an argument is wrong or the events cannot be read for a moment, says so to the agent, which can correct the call or retry it.

## Asking questions

Agents pick the tools themselves. Questions like these work well:

- "What errors has checkout had since yesterday, and what do they have in common?"
- "Compare today's critical events per hour with the same hours a week ago."
- "Which tags carry the most warnings this week? Show me a few of each."

## Security

A `read` key gives whoever holds it every event of its project, data included, so treat it like a password. Keep it in the agent's configuration, not in a repository, and give each agent its own key so you can revoke one without the others, on the **API Keys** page. A revoked key stops working at once on the broker that revoked it, and within a minute on the others.

Whatever your events carry reaches the agent, and, through it, the model behind it. If they may hold personal or secret data, keep it out of the event payloads, or keep agents away from the project.

## Without an MCP client

The endpoint is plain HTTP, so you can try it with `curl`. Each request is a JSON-RPC message, and needs no session before it:

```bash
curl https://logs.your-domain.com/api/mcp \
  -H "Authorization: Bearer lw_..." \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"count_events","arguments":{"group_by":"severity","since":"24h"}}}'
```

The [OpenAPI spec](/api) documents the endpoint's statuses alongside the rest of the API.
