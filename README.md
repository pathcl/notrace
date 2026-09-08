# notrace

CLI for querying Grafana Tempo and Prometheus/Mimir without touching the Grafana UI.

## Prerequisites

- Go 1.26+
- Docker + Docker Compose v2 (`docker compose`, not `docker-compose`) — the legacy v1 Python CLI has a [known bug](https://github.com/docker/compose/issues/10657) with newer OCI image formats and will error with `KeyError: 'ContainerConfig'`

## Quick start

```bash
# Build
go build -o notrace .

# Point at Tempo
export NOTRACE_TEMPO_URL=http://localhost:3200

# Search last 30 minutes
./notrace tempo search --start 30m

# Filter by TraceQL
./notrace tempo search --start 1h --query '{status=error}' --limit 50

# Stream new traces live (Ctrl-C to stop)
./notrace tempo tail

# Pipe to jq
./notrace tempo tail --output json | jq '{id: .traceID, svc: .rootServiceName}'
```

## Lab

A local stack with Tempo, Prometheus, and a synthetic traffic generator.

```bash
make lab-up      # start (builds traffic-gen image, waits for healthy)
make lab-logs    # follow all logs
make lab-down    # stop and remove volumes
```

Services once up:

| Service    | URL                       |
|------------|---------------------------|
| Tempo API  | http://localhost:3200      |
| Prometheus | http://localhost:9090      |
| Metrics    | http://localhost:8080      |

The traffic generator runs three goroutines simulating `frontend`, `checkout`, and `background-worker` services, with ~15% error injection, sending OTLP traces to Tempo and exposing Prometheus metrics.

## Commands

### `notrace tempo search`

```
Flags:
  --start string     Start time: relative (1h, 30m, 2d, 1d12h) or RFC3339 (default "1h")
  --end string       End time: relative or RFC3339 (default "now")
  -q, --query string TraceQL query (default "{}")
  --limit int        Max traces to return (default 20)
  -o, --output string  table | json (default "table")
```

Examples:

```bash
notrace tempo search --start 2h
notrace tempo search --start 30m --query '{resource.service.name="checkout"}'
notrace tempo search --start 2026-09-08T10:00:00Z --end 2026-09-08T11:00:00Z
notrace tempo search --start 1h --query '{status=error}' --output json | jq .traceID
```

### `notrace tempo tail`

Polls Tempo every `--interval` seconds with a 30-second sliding window and prints traces as they appear. Deduplicates by traceID across polls.

```
Flags:
  -q, --query string   TraceQL query (default "{}")
  --interval duration  Poll interval (default 5s)
  -o, --output string  table | json (default "table")
```

Examples:

```bash
notrace tempo tail
notrace tempo tail --query '{resource.service.name="frontend"}' --interval 3s
notrace tempo tail --query '{status=error}' --output json | jq .rootTraceName
```

## Configuration

Priority order: `--flag` > environment variable > `~/.config/notrace/notrace.yaml`

| Flag          | Env var              | Config key   | Description                          |
|---------------|----------------------|--------------|--------------------------------------|
| `--tempo-url` | `NOTRACE_TEMPO_URL`  | `tempo.url`  | Tempo base URL                       |
| `--token`     | `NOTRACE_TOKEN`      | `tempo.token`| Bearer token (Grafana Cloud)         |
| `--org-id`    | `NOTRACE_ORG_ID`     | `tempo.org_id` | Org ID for multi-tenant deployments|

Config file example (`~/.config/notrace/config.yaml`):

```yaml
tempo:
  url: http://localhost:3200
  token: ""
  org_id: ""
```

## Testing

```bash
# Unit tests (no docker required)
go test ./...

# Integration tests (requires lab running)
make lab-up
make test-integration

# Everything at once
make test-all
```

Integration tests are gated behind `//go:build integration` and skip automatically if `NOTRACE_TEMPO_URL` is not set.

## Project layout

```
notrace/
├── cmd/                    # cobra commands
│   ├── root.go
│   ├── tempo.go
│   ├── tempo_search.go
│   └── tempo_tail.go
├── internal/
│   ├── config/             # config loading
│   ├── tempo/              # HTTP client, TraceQL search, tail logic, time parsing
│   └── render/             # table and NDJSON output
├── integration/            # integration tests (build tag: integration)
├── lab/
│   ├── docker-compose.yaml
│   ├── config/             # tempo.yaml, prometheus.yaml
│   └── traffic-gen/        # synthetic load generator
└── Makefile
```
