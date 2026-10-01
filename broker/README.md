# Logwolf Broker

The **Broker** service acts as the public-facing API Gateway for the Logwolf system. It handles all incoming HTTP traffic, serving as the entry point for clients to submit and retrieve logs.

## Overview

This service is designed to decouple log ingestion from processing. It accepts log submissions and immediately offloads them to a message queue for asynchronous processing, ensuring high throughput and low latency for writing clients. For read operations, it acts as a proxy, fetching data from the storage service via RPC.

## Features

- **Log Ingestion (Async)**: Accepts JSON log payloads via HTTP `POST` and pushes them to **RabbitMQ** for processing.
- **Log Retrieval (Sync)**: Handles HTTP `GET` requests and retrieves stored logs by communicating with the **Logger** service via **RPC**.
- **Heartbeat**: Exposes a `/ping` endpoint for health checks.

## Endpoints

The public routes, which Caddy serves under `/api`, are specified in [`openapi.yaml`](../openapi.yaml) at the repository root; `cmd/api/openapi_test.go` holds the spec against the routes.

| Method   | Path          | Description                                                  |
| -------- | ------------- | ------------------------------------------------------------ |
| `POST`   | `/logs`       | Submit an event. `202 Accepted` once RabbitMQ has queued it. |
| `POST`   | `/logs/batch` | Submit up to 1000 events at once.                            |
| `GET`    | `/logs`       | One page of the key's project's events, newest first.        |
| `GET`    | `/logs/{id}`  | One event of the key's project.                              |
| `DELETE` | `/logs`       | Delete an event of the key's project.                        |
| `GET`    | `/health`     | Readiness: RabbitMQ and the logger.                          |
| `GET`    | `/ping`       | Liveness.                                                    |

The dashboard's routes, under `/projects`, are internal and not part of the spec.

## Dependencies

The Broker requires the following services to function:

- **RabbitMQ**: For publishing log events.
- **Logger Service**: For retrieving historical log data via RPC (TCP).

## Getting Started

The service is containerized and intended to run via Docker Compose.

```bash
# Runs on port 8080 by default in the provided composition
docker compose up -d broker
```
