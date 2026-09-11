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

A local stack with Tempo, ClickHouse, an OpenTelemetry Collector, Prometheus, and a synthetic traffic generator.

```bash
make lab-up      # start (builds traffic-gen image, waits for healthy)
make lab-logs    # follow all logs
make lab-down    # stop and remove volumes
```

Services once up:

| Service          | URL                       | Purpose                                      |
|------------------|---------------------------|----------------------------------------------|
| OTel Collector   | localhost:4317 (gRPC)     | OTLP ingestion, fans out to Tempo + ClickHouse |
|                  | localhost:4318 (HTTP)     |                                              |
| Tempo API        | http://localhost:3200     | Live tail, TraceQL, trace lookup             |
| ClickHouse HTTP  | http://localhost:8123     | Long-term analytics (7-day TTL)              |
| ClickHouse TCP   | localhost:9000            | Native client / `clickhouse-client`          |
| Prometheus       | http://localhost:9090     | Metrics                                      |
| Metrics          | http://localhost:8080     | traffic-gen Prometheus endpoint              |

The traffic generator sends OTLP to the collector, which fans out to both stores:

```
traffic-gen → otel-collector → Tempo      (live tail, TraceQL, short-term)
                             → ClickHouse  (analytical queries, 7-day retention)
```

Traces land in `otel.otel_traces` in ClickHouse automatically. Query them directly:

```bash
docker exec lab-clickhouse-1 clickhouse-client \
  --query "SELECT ServiceName, count(), round(avg(Duration)/1e6,2) avg_ms
           FROM otel.otel_traces GROUP BY ServiceName"
```

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

One-shot query for traces over a time range. Calls `GET /api/search` once with the given time bounds and TraceQL filter, then exits. If `--details` is set, it follows up with `GET /api/traces/{traceID}` for each result (streaming output as each trace resolves, retrying on 404 up to 30s for Tempo's eventual consistency).

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

**How it works**

Tempo has no streaming API, so `tail` simulates it with a poll loop:

```
start immediately → poll → wait 5s → poll → wait 5s → ...
```

Each poll calls `GET /api/search` with a sliding time window (`start=now-30s, end=now`). A `seen` map tracks which traceIDs have already been emitted; entries older than 30s are evicted before each poll so traces that re-enter the window are shown again.

```
every 5s:
  GET /api/search?start=now-30s&end=now&q={}   ← sliding window, always 30s wide
            │ N trace IDs returned
            ▼
    filter against seen{}
            │ M new IDs
            ▼
    (if --details) GET /api/traces/{id}         ← one per new trace
            │ retries on 404 up to 30s          ← Tempo eventual consistency
            ▼
    emit to stdout
```

If `--details` is set, each new trace triggers a second call to `GET /api/traces/{traceID}` to fetch the full OTLP span tree. Tempo can return 404 here even though the trace appeared in search (the search index is updated before the block is flushed). The client retries on 404 every 2s for up to 30s before giving up.

> **Limitation**: traces longer than 30 seconds (i.e. whose root span duration exceeds the lookback window) will not be captured reliably. The lookback is fixed at 30s.

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--query` | `-q` | `{}` | TraceQL expression |
| `--interval` | | `5s` | Poll interval |
| `--lookback` | | `30s` | Sliding window size — increase if p99 trace duration exceeds the default |
| `--limit` | | `100` | Max traces fetched per poll |
| `--output` | `-o` | `table` | Output format: `table`, `json` |
| `--details` | `-d` | `false` | Fetch and display resource + span attributes per trace |

```bash
notrace tempo tail
notrace tempo tail -q '{resource.service.name="frontend"}' --interval 3s
notrace tempo tail -q '{status=error}' -o json | jq .rootTraceName
notrace tempo tail -o json --details >> notrace.json
notrace tempo tail --lookback 60s -o json --details | python3 lab/query.py --stats
notrace tempo tail --limit 500 -v
```

> **Note**: without `--details`, output contains only search metadata (traceID, service name, duration). Span and resource attributes are **not** included. If you plan to analyse the output with `query.py`, you must pass `--details`.

> **Sizing `--lookback`**: if your p99 root span duration exceeds 30s, traces that are still in-flight when the window slides past their start time will be missed. Use `--duration-stats` to measure your p99 and set `--lookback` accordingly — the recommended value is printed automatically.

> **Performance**: widening `--lookback` does not slow down the poll loop — it only changes the time range of the `GET /api/search` request. The `seen` map deduplicates everything already captured so a wider window does not cause re-processing. The one case to watch is a very large lookback (hours) combined with a high `--limit` and `--details`: each poll may return many new traces, each triggering a `GET /api/traces/{id}` call. For typical adjustments (30s → 60s) the difference is negligible.

## Live pipe analysis

Pipe `notrace tempo tail` directly into `query.py --stats` for real-time stream analysis — no file or DB needed.

> **`--details` is required.** Without it the output contains no span or resource attributes.

```bash
./notrace tempo tail -o json --details | python3 lab/query.py --stats
```

Press Ctrl-C (or let the pipe close) to print a summary:

```
LIVE STREAM STATS  (48 traces, 9s)

  root span duration
    min    11.4 ms
    p50    45.3 ms
    p95    435.2 ms
    p99    487.2 ms
    max    487.2 ms
    recommended --lookback: 30s

  errors (STATUS_CODE_ERROR): 11

  top span attributes (by occurrence)
    component                           97
    http.route                          42
    http.method                         42
    http.status_code                    42

  top services (by root span count)
    traffic-gen                         48
```

**Live heatmap — `--watch KEY`**

Add `--watch KEY` to see a heatmap of a span attribute's values bucketed by the span's actual `startTimeUnixNano`. Each column is a unique value; bar width is proportional to the busiest bucket. Rows print on EOF or Ctrl-C sorted by real trace time — accurate for both live tail and offline file analysis.

```bash
# which downstream services are being called, and how often
./notrace tempo tail -o json --details | python3 lab/query.py --stats --watch component

# HTTP route activity over time
./notrace tempo tail -o json --details | python3 lab/query.py --stats --watch http.route

# works the same on a captured file — timestamps reflect when traces actually happened
cat notrace.json | python3 lab/query.py --stats --watch http.method
```

```
http.method  (10s buckets)

                       POST          GET
  2026-09-11 08:42:00  ████ 1
  2026-09-11 08:42:10  ████ 1        ████ 1
  2026-09-11 08:42:20  ████████ 2
  2026-09-11 08:42:30                ████ 1
  2026-09-11 08:42:40  ████ 1        ████████ 2
```

New columns appear automatically as new attribute values are seen. The full `--stats` summary prints below the heatmap.

**Neighbour graph — `--watch KEY=VALUE`**

Passing a value filters traces to those containing at least one span where `KEY=VALUE`, then builds a service graph from those traces. The heatmap columns become co-occurring services rather than attribute values, and a `NEIGHBOURS` block prints on Ctrl-C showing direct caller→callee edges (derived from `parentSpanId`) and all services that appear alongside the matching spans.

```bash
# find which services neighbour spans with my.baggage.attr=example
cat sample.json | python3 lab/query.py --stats --watch my.baggage.attr=example

# live: which services co-occur with error status codes
./notrace tempo tail -o json --details | python3 lab/query.py --stats --watch http.status_code=500
```

```
http.status_code=500  (live, 10s buckets)

              frontend      checkout
  10:15:00    ████████ 8    ████ 4
  10:15:10    ████ 4        ██ 2

NEIGHBOURS for http.status_code=500  (12/48 traces matched)

  direct edges (caller → callee):
    frontend    →  checkout     8
    checkout    →  payment-svc  4

  co-occurring services (same trace):
    frontend      12
    checkout       8
    payment-svc    4
```

Direct edges require each service to emit its own OTLP resource batch. Co-occurring services work regardless of how spans are grouped in the export.

If you're not sure which attribute keys are available, let `--stats` run for a minute — the "top span attributes" block tells you what's flowing through.

`--stats` and `--watch` also work on a captured file — pipe it the same way:

```bash
# summary stats from a captured file
cat notrace.json | python3 lab/query.py --stats

# heatmap from a captured file
cat notrace.json | python3 lab/query.py --stats --watch component

# one-shot search piped directly
./notrace tempo search --start 1h -o json --details | python3 lab/query.py --stats --watch http.route
```

When reading from a file all traces land in a single bucket (disk reads at full speed), so the heatmap shows one aggregated row rather than a time series — but the summary stats and relative column sizes are still accurate.

## Offline analysis

Capture traces to a file and query them with `lab/query.py`, a DuckDB-backed helper that filters by span and resource attributes.

**Capture:**

> **`--details` is required.** Without it, the output contains no span or resource attributes and `query.py` will exit with an error.

```bash
# continuous capture (append as new traces arrive)
./notrace tempo tail -o json --details >> notrace.json

# one-shot over a time range
./notrace tempo search --start 1h -o json --details > notrace.json
```

Requires `pip install duckdb`.

### File mode (one-shot)

Re-parses the NDJSON file on each query. Good for quick exploration on small captures.

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
python3 lab/query.py -f notrace.json --span-attr http.method=GET

# filter by resource attribute
python3 lab/query.py -f notrace.json --resource-attr service.name=frontend

# combine (ANDed)
python3 lab/query.py -f notrace.json \
  --resource-attr service.name=frontend \
  --span-attr http.method=GET

# show full OTLP detail for each matched trace
python3 lab/query.py -f notrace.json --resource-attr service.name=frontend --detail

# inspect the generated SQL
python3 lab/query.py -f notrace.json --span-attr http.method=GET --sql
```

**Drill into a specific trace:**

```bash
python3 lab/query.py -f notrace.json --trace-id 38f26ee12443bc2ef4ccb638808bb449

# pipe to jq for further slicing
python3 lab/query.py -f notrace.json --trace-id 38f26ee12443bc2ef4ccb638808bb449 \
  | jq '.batches[].scopeSpans[].spans[].name'
```

### DB mode (persistent, indexed)

Imports NDJSON into a DuckDB file once, then queries the indexed tables. Faster for large captures and repeated queries.

**Import:**

```bash
# first import — creates notrace.db
python3 lab/query.py --db notrace.db --import notrace.json
# → imported 42 trace(s), skipped 0 duplicate(s)

# append more captures later — duplicates are skipped automatically
python3 lab/query.py --db notrace.db --import more.json
```

**Explore the schema:**

```bash
# show all attribute keys, cardinality, and sample values
python3 lab/query.py --db notrace.db --schema
```

```
ATTRIBUTE SCHEMA  (from notrace.db — 42 traces)

SCOPE     KEY                  TRACES  CARDINALITY  SAMPLE VALUES
--------  -------------------  ------  -----------  ----------------------------
resource  service.name         42      3            checkout, frontend, ...
span      http.method          38      2            GET, POST
span      http.route           38      6            /api/orders, /api/payments ...
span      http.status_code     38      2            (int) 200, (int) 500
```

Integer-valued attributes are tagged `(int)` in the schema and work transparently in filters — `--span-attr http.status_code=500` matches whether the value was stored as a string or integer.

**Filter and query:**

```bash
# list unique values for an attribute
python3 lab/query.py --db notrace.db --list-resource-attr service.name
python3 lab/query.py --db notrace.db --list-span-attr http.status_code

# with one sample trace ID per value
python3 lab/query.py --db notrace.db --list-span-attr http.route --detail

# filter by span attribute
python3 lab/query.py --db notrace.db --span-attr http.status_code=500

# filter by resource attribute
python3 lab/query.py --db notrace.db --resource-attr service.name=checkout

# combine (ANDed)
python3 lab/query.py --db notrace.db \
  --resource-attr service.name=checkout \
  --span-attr http.method=POST

# show full OTLP detail for matched traces
python3 lab/query.py --db notrace.db --resource-attr service.name=checkout --detail

# drill into a specific trace
python3 lab/query.py --db notrace.db --trace-id 38f26ee12443bc2ef4ccb638808bb449
```

**Span relationship analysis:**

```bash
# cross-service call graph (caller → callee, per operation)
python3 lab/query.py --db notrace.db --service-graph
```

```
CALLER    CALLEE       OPERATION          CALLS  AVG_MS  MAX_MS
--------  -----------  -----------------  -----  ------  ------
frontend  db           SELECT products    27     0.02    0.08
frontend  cache        GET products:list  27     0.00    0.02
checkout  payment-svc  ChargeCard         17     0.01    0.08
checkout  db           INSERT orders      17     0.02    0.07
```

```bash
# span tree for a single trace (depth-indented)
python3 lab/query.py --db notrace.db --span-tree <trace-id>
```

```
GET /api/products  [frontend · SERVER]  90.36ms
  SELECT products  [frontend · CLIENT]  0.06ms
  GET products:list  [frontend · CLIENT]  0.00ms
```

`--service-graph` and `--span-tree` require `--db` and a prior `--import`. If the `spans` table is empty (e.g. imported with an older version), re-run `--import` on the same file to populate it.

**Attribute activity over time:**

```bash
# how many spans had a given attribute active, per time bucket
python3 lab/query.py --db notrace.db --time-series http.route
python3 lab/query.py --db notrace.db --time-series http.status_code --bucket minute
python3 lab/query.py --db notrace.db --time-series component --bucket day
```

```
http.route  (per hour)

  2026-09-11 08:00  ████████████████████████████████████████  11
  2026-09-11 09:00  ██████████████                            4
```

`--bucket` accepts `minute`, `hour` (default), or `day`. The bar chart is proportionally scaled to the busiest bucket. Useful as a starting point for heatmap analysis — each row is a time bucket, the count is how many spans carried that attribute during that window.

**Root span duration stats:**

```bash
python3 lab/query.py --db notrace.db --duration-stats
```

```
ROOT SPAN DURATION STATS

  traces  : 20
  min     : 19.4 ms
  avg     : 104.2 ms
  p50     : 55.8 ms
  p95     : 343.7 ms
  p99     : 455.1 ms
  max     : 483.0 ms

  recommended --lookback for `notrace tempo tail`: 30s
```

Useful for sizing the `--lookback` window on `notrace tempo tail`. The default lookback is 30 seconds — if your p99 root span duration exceeds that, traces that are still in-flight when the window slides past their start time will be missed. `--duration-stats` computes a recommended value (`ceil(p99 × 1.5)`, rounded up to the nearest 30s) so you can set it explicitly:

```bash
notrace tempo tail --lookback 90s   # if --duration-stats recommends 90s
```

Typical workflow: `--import` → `--schema` to discover keys → `--duration-stats` to size your tail window → `--list-span-attr` to see values → filter with `--span-attr`/`--resource-attr` → `--span-tree` to inspect a trace → `--service-graph` to see call topology → `--time-series` to spot activity patterns over time.

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
