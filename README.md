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

### Global flags

Available on every subcommand:

| Flag | Short | Env var | Default | Description |
|------|-------|---------|---------|-------------|
| `--tempo-url` | | `NOTRACE_TEMPO_URL` | | Tempo base URL |
| `--token` | | `NOTRACE_TOKEN` | | Bearer token (Grafana Cloud) |
| `--org-id` | | `NOTRACE_ORG_ID` | | Org ID for multi-tenant deployments |
| `--timeout` | | `NOTRACE_TIMEOUT` | `10s` | HTTP request timeout |
| `--verbose` | `-v` | | `false` | Log each HTTP request and response to stderr |

### `notrace tempo search`

One-shot query for traces over a time range.

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--start` | | `1h` | Start time: relative (`1h`, `30m`, `2d`, `1d12h`) or RFC3339 |
| `--end` | | `now` | End time: relative or RFC3339 |
| `--query` | `-q` | `{}` | TraceQL expression |
| `--limit` | | `20` | Max traces to return |
| `--output` | `-o` | `table` | Output format: `table`, `json` |
| `--details` | `-d` | `false` | Fetch and display resource + span attributes per trace |

```bash
notrace tempo search --start 2h
notrace tempo search --start 30m -q '{resource.service.name="checkout"}'
notrace tempo search --start 2026-09-08T10:00:00Z --end 2026-09-08T11:00:00Z
notrace tempo search --start 1h -q '{status=error}' --limit 100 -o json | jq .traceID
notrace tempo search --start 1h --details -v
```

### `notrace tempo tail`

Polls Tempo on a sliding 30-second window, deduplicates by traceID, and prints new traces as they arrive. Each poll logs a status line to stderr (`polled=N new=N seen=N`). Press Ctrl-C to stop.

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--query` | `-q` | `{}` | TraceQL expression |
| `--interval` | | `5s` | Poll interval |
| `--limit` | | `100` | Max traces fetched per poll |
| `--output` | `-o` | `table` | Output format: `table`, `json` |
| `--details` | `-d` | `false` | Fetch and display resource + span attributes per trace |

```bash
notrace tempo tail
notrace tempo tail -q '{resource.service.name="frontend"}' --interval 3s
notrace tempo tail -q '{status=error}' -o json | jq .rootTraceName
notrace tempo tail -o json --details >> notrace.json
notrace tempo tail --limit 500 -v
```

## Offline analysis

Capture traces to a file and query them with `lab/query.py`, a DuckDB-backed helper that filters by span and resource attributes.

**Capture:**

```bash
./notrace tempo tail -o json --details >> notrace.json
# or a one-shot search
./notrace tempo search --start 1h -o json --details > notrace.json
```

**Discover attribute keys and values:**

```bash
# unique values for a resource attribute key
python3 lab/query.py -f notrace.json --list-resource-attr service.name

# unique values for a span attribute key
python3 lab/query.py -f notrace.json --list-span-attr http.method
python3 lab/query.py -f notrace.json --list-span-attr http.status_code

# add --detail to show one sample trace per unique value
python3 lab/query.py -f notrace.json --list-span-attr http.route --detail
```

**Filter traces:**

```bash
# all traces (sorted by duration desc)
python3 lab/query.py -f notrace.json

# filter by span attribute
python3 lab/query.py -f notrace.json --span-attr hola.code=M1234

# filter by resource attribute
python3 lab/query.py -f notrace.json --resource-attr service.name=frontend

# combine (ANDed)
python3 lab/query.py -f notrace.json \
  --resource-attr service.name=frontend \
  --span-attr http.method=GET

# show full OTLP detail for each matched trace
python3 lab/query.py -f notrace.json --span-attr hola.code=M1234 --detail

# inspect the generated SQL
python3 lab/query.py -f notrace.json --span-attr hola.code=M1234 --sql
```

**Drill into a specific trace:**

```bash
python3 lab/query.py -f notrace.json --trace-id 38f26ee12443bc2ef4ccb638808bb449

# pipe to jq for further slicing
python3 lab/query.py -f notrace.json --trace-id 38f26ee12443bc2ef4ccb638808bb449 \
  | jq '.batches[].scopeSpans[].spans[].name'
```

Typical workflow: use `--list-span-attr` to discover values → filter with `--span-attr` to find traceIDs → drill in with `--trace-id`.

Requires `pip install duckdb`. The `--details` flag must be used when capturing, otherwise span and resource attributes are not included in the output.

## Configuration

Priority order: `--flag` > environment variable > config file.

Config file is loaded from `~/.config/notrace/config.yaml` or `./config.yaml`:

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
│   ├── traffic-gen/        # synthetic load generator
│   └── query.py            # offline DuckDB query helper
└── Makefile
```
