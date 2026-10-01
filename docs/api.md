# HTTP API reference

The SDK is one client of Logwolf's HTTP API. Any other client can use it too, from any language. The API is described by an [OpenAPI 3.1](https://spec.openapis.org/oas/v3.1.0) spec:

- **[openapi.yaml](/openapi.yaml)** on this site, the API these docs describe
- attached to each [GitHub release](https://github.com/logwolf-app/logwolf/releases), the API of that release: pick the one your instance runs
- [`openapi.yaml`](https://github.com/logwolf-app/logwolf/blob/main/openapi.yaml) at the repository root, where it is maintained

You can load the spec into any OpenAPI tool to browse it, generate a client, or validate requests. It is the contract: the broker's tests check every route, scope, bound and status code against it, so a change to the API that leaves the spec behind fails them.

## Base URL

Your instance serves the API under `/api`, e.g. `https://logs.your-domain.com/api`. Every path below is relative to it.

## Endpoints

| Method   | Path          | Scope    | Description                               |
| -------- | ------------- | -------- | ----------------------------------------- |
| `POST`   | `/logs`       | `ingest` | Send one event                            |
| `POST`   | `/logs/batch` | `ingest` | Send up to 1000 events                    |
| `GET`    | `/logs`       | `read`   | List events, newest first, a page at once |
| `GET`    | `/logs/{id}`  | `read`   | Get one event                             |
| `DELETE` | `/logs`       | `delete` | Delete one event                          |
| `GET`    | `/health`     | —        | Whether the instance can take events now  |
| `GET`    | `/ping`       | —        | Whether the broker is running             |

## Authentication

The `/logs` routes take a project API key as a Bearer token:

```http
POST /api/logs HTTP/1.1
Authorization: Bearer lw_...
Content-Type: application/json

{ "name": "checkout.completed", "severity": "info", "tags": ["web"], "data": "{\"plan\":\"pro\"}", "duration": 120 }
```

The key decides the project, so no request names one. A key has scopes, picked when it is created on the dashboard's **API Keys** page, and each route demands the one in the table above. A new key gets `ingest` alone unless you pick more: a key that ships in a browser bundle can then send events, but not read or delete them.

| Status | Means                                                                                          |
| ------ | ---------------------------------------------------------------------------------------------- |
| `401`  | The `Authorization` header is missing, is not a Bearer token, or names no active key           |
| `403`  | The key lacks the route's scope                                                                |
| `429`  | This address failed to authenticate 10 times within a minute; every request is refused until the minute is up |

## Responses

Responses are JSON envelopes. `error` says whether the request failed and `message` describes the outcome for people; on success, `data` holds the result:

```json
{ "error": false, "message": "OK!", "data": { "id": "66f1c0ffee0000000000abcd", "name": "checkout.completed", "severity": "info" } }
```

Go by the status code rather than the message, whose wording may change. Besides the authentication errors above:

| Status | Means                                                                                                  |
| ------ | ------------------------------------------------------------------------------------------------------ |
| `202`  | The events are queued durably. They are readable once stored, usually within a moment                  |
| `400`  | The body or the query is malformed, or an event's severity is not `info`, `warning`, `error` or `critical` |
| `404`  | `GET /logs/{id}`: no event of the key's project has that id                                            |
| `413`  | `POST /logs/batch`: more than 1000 events                                                              |
| `500`  | The key could not be checked, or the logger could not be reached. Retry                                |
| `503`  | RabbitMQ did not confirm the events, so they may not be stored. Retry                                  |

Delivery is at least once: a batch retried after a `503` may store some of its events twice.

## Pagination

`GET /logs` takes `page` (1 to 1,000,000, default 1) and `pageSize` (1 to 100, default 20). A value out of bounds, or not a whole number, is a `400`. There is no total count: a page shorter than `pageSize` is the last.
