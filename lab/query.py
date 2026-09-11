#!/usr/bin/env python3
"""
notrace query helper — offline analysis of notrace NDJSON output using DuckDB.

Usage (file mode — one-shot, re-parses NDJSON each query):
    python3 lab/query.py -f notrace.json
    python3 lab/query.py -f notrace.json --resource-attr service.name=frontend
    python3 lab/query.py -f notrace.json --span-attr http.method=GET

Usage (DB mode — persistent, indexed, faster on large captures):
    python3 lab/query.py --db notrace.db --import notrace.json
    python3 lab/query.py --db notrace.db --schema
    python3 lab/query.py --db notrace.db --resource-attr service.name=frontend
    python3 lab/query.py --db notrace.db --span-attr http.status_code=500

Requires:
    pip install duckdb
"""

import argparse
import json
import math
import select
import signal
import sys
import time
from collections import Counter
from datetime import datetime, timezone

try:
    import duckdb
except ImportError:
    print("error: duckdb not installed — run: pip install duckdb", file=sys.stderr)
    sys.exit(1)


# ──────────────────────────────────────────────────────────────────────────────
# Streaming stats (--stats, reads stdin)
# ──────────────────────────────────────────────────────────────────────────────

class StreamStats:
    def __init__(self) -> None:
        self.traces = 0
        self.root_durations: list[float] = []   # ms
        self.span_attrs: Counter = Counter()
        self.services: Counter = Counter()
        self.errors = 0
        self.started = time.monotonic()

    def ingest(self, trace: dict) -> None:
        self.traces += 1
        detail = trace.get("detail") or {}
        for batch in detail.get("batches") or []:
            resource_attrs = {
                a["key"]: a.get("value", {})
                for a in batch.get("resource", {}).get("attributes") or []
            }
            svc = resource_attrs.get("service.name", {}).get("stringValue", "")

            for scope_spans in batch.get("scopeSpans") or []:
                for span in scope_spans.get("spans") or []:
                    parent = span.get("parentSpanId", "")
                    dur_ns = int(span.get("endTimeUnixNano", 0)) - int(span.get("startTimeUnixNano", 0))
                    if not parent:
                        self.root_durations.append(dur_ns / 1e6)
                        if svc:
                            self.services[svc] += 1

                    for attr in span.get("attributes") or []:
                        key = attr.get("key", "")
                        if key:
                            self.span_attrs[key] += 1

                    if span.get("status", {}).get("code") == "STATUS_CODE_ERROR":
                        self.errors += 1

    def print(self) -> None:
        elapsed = time.monotonic() - self.started
        mins, secs = divmod(int(elapsed), 60)
        elapsed_str = f"{mins}m {secs}s" if mins else f"{secs}s"

        print(f"\nLIVE STREAM STATS  ({self.traces} traces, {elapsed_str})\n", file=sys.stderr)

        # duration percentiles
        if self.root_durations:
            d = sorted(self.root_durations)
            n = len(d)
            def pct(p: float) -> float:
                idx = max(0, min(n - 1, int(math.ceil(p / 100 * n)) - 1))
                return d[idx]
            p99 = pct(99)
            rec_s = max(30, math.ceil(math.ceil(p99 / 1000 * 1.5) / 30) * 30)
            print("  root span duration", file=sys.stderr)
            print(f"    min    {d[0]:.1f} ms", file=sys.stderr)
            print(f"    p50    {pct(50):.1f} ms", file=sys.stderr)
            print(f"    p95    {pct(95):.1f} ms", file=sys.stderr)
            print(f"    p99    {p99:.1f} ms", file=sys.stderr)
            print(f"    max    {d[-1]:.1f} ms", file=sys.stderr)
            print(f"    recommended --lookback: {rec_s}s", file=sys.stderr)
            print(file=sys.stderr)

        if self.errors:
            print(f"  errors (STATUS_CODE_ERROR): {self.errors}", file=sys.stderr)
            print(file=sys.stderr)

        if self.span_attrs:
            print("  top span attributes (by occurrence)", file=sys.stderr)
            for key, cnt in self.span_attrs.most_common(8):
                print(f"    {key:<35} {cnt}", file=sys.stderr)
            print(file=sys.stderr)

        if self.services:
            print("  top services (by root span count)", file=sys.stderr)
            for svc, cnt in self.services.most_common(8):
                print(f"    {svc:<35} {cnt}", file=sys.stderr)
            print(file=sys.stderr)


_BAR = 8  # bar width in characters for heatmap cells


def _attr_str(v: dict) -> str:
    """Extract a string representation from an OTLP attribute value dict."""
    return (
        v.get("stringValue")
        or (str(v["intValue"]) if "intValue" in v else "")
        or (str(v["boolValue"]).lower() if "boolValue" in v else "")
    )


def _batch_service(batch: dict) -> str:
    for a in (batch.get("resource") or {}).get("attributes") or []:
        if a.get("key") == "service.name":
            return (a.get("value") or {}).get("stringValue", "")
    return ""


class NeighbourGraph:
    """Accumulates direct caller→callee edges and co-occurring services for traces
    that contain at least one span where key=val."""

    def __init__(self, key: str, val: str) -> None:
        self.key = key
        self.val = val
        self.matched = 0
        self.total = 0
        self.edges: Counter = Counter()        # (caller_svc, callee_svc) → count
        self.cooccurring: Counter = Counter()  # service → trace count

    def ingest(self, trace: dict) -> None:
        self.total += 1
        detail = trace.get("detail") or {}
        batches = detail.get("batches") or []

        # span_id → {service, parent_id}
        span_map: dict[str, dict] = {}
        matching: list[str] = []

        for batch in batches:
            svc = _batch_service(batch)
            for scope_spans in batch.get("scopeSpans") or []:
                for span in scope_spans.get("spans") or []:
                    sid = span.get("spanId", "")
                    span_map[sid] = {"service": svc, "parent": span.get("parentSpanId", "")}
                    for attr in span.get("attributes") or []:
                        if attr.get("key") == self.key and _attr_str(attr.get("value") or {}) == self.val:
                            matching.append(sid)

        if not matching:
            return
        self.matched += 1

        # build children index
        children: dict[str, list[str]] = {}
        for sid, info in span_map.items():
            p = info["parent"]
            if p:
                children.setdefault(p, []).append(sid)

        # direct edges around each matching span
        for sid in matching:
            svc = span_map.get(sid, {}).get("service", "")
            parent_id = span_map.get(sid, {}).get("parent", "")
            if parent_id in span_map:
                psvc = span_map[parent_id]["service"]
                if psvc and psvc != svc:
                    self.edges[(psvc, svc)] += 1
            for cid in children.get(sid, []):
                csvc = span_map[cid]["service"]
                if csvc and csvc != svc:
                    self.edges[(svc, csvc)] += 1

        # co-occurring services (one count per trace per service)
        for svc in {info["service"] for info in span_map.values() if info["service"]}:
            self.cooccurring[svc] += 1

    def print(self) -> None:
        print(f"\nNEIGHBOURS for {self.key}={self.val}  ({self.matched}/{self.total} traces matched)\n", file=sys.stderr)
        if self.edges:
            print("  direct edges (caller → callee):", file=sys.stderr)
            for (caller, callee), cnt in self.edges.most_common():
                print(f"    {caller:<25}  →  {callee:<25}  {cnt}", file=sys.stderr)
            print(file=sys.stderr)
        if self.cooccurring:
            print("  co-occurring services (same trace):", file=sys.stderr)
            for svc, cnt in self.cooccurring.most_common():
                print(f"    {svc:<25}  {cnt}", file=sys.stderr)
            print(file=sys.stderr)


class WatchHeatmap:
    """Heatmap bucketed by real span startTimeUnixNano — accurate for both
    live tail and offline file analysis."""

    def __init__(self, key: str, filter_val: str | None = None, interval: float = 10.0) -> None:
        self.key = key
        self.filter_val = filter_val  # when set, columns = co-occurring services
        self.interval = interval      # bucket width in seconds
        self.buckets: dict[int, Counter] = {}  # bucket_epoch_s → Counter
        self.values: list[str] = []            # ordered by first seen
        self._seen: set[str] = set()

    def add(self, value: str, span_ts_ns: int) -> None:
        bucket = int(span_ts_ns / 1e9 // self.interval * self.interval)
        if value not in self._seen:
            self.values.append(value)
            self._seen.add(value)
        self.buckets.setdefault(bucket, Counter())[value] += 1

    def col_width(self, val: str) -> int:
        return max(len(val), _BAR + 4)

    def flush(self) -> None:
        if not self.buckets or not self.values:
            return
        label = f"{self.key}={self.filter_val}" if self.filter_val else self.key
        print(f"\n{label}  ({self.interval:.0f}s buckets)\n", file=sys.stderr)
        parts = [f"{'':19}"]
        for val in self.values:
            parts.append(f"{val:<{self.col_width(val)}}")
        print("  " + "  ".join(parts), file=sys.stderr)

        max_cnt = max(max(c.values()) for c in self.buckets.values())
        for ts_s in sorted(self.buckets):
            cnt_map = self.buckets[ts_s]
            dt = datetime.fromtimestamp(ts_s, tz=timezone.utc).strftime("%Y-%m-%d %H:%M:%S")
            row = [f"{dt:<19}"]
            for val in self.values:
                cnt = cnt_map.get(val, 0)
                bar = "█" * int(cnt / max_cnt * _BAR) if max_cnt > 0 else ""
                cell = f"{bar} {cnt}" if cnt else ""
                row.append(f"{cell:<{self.col_width(val)}}")
            print("  " + "  ".join(row), file=sys.stderr)
        print(file=sys.stderr)


def _feed_heatmap(heatmap: WatchHeatmap, trace: dict) -> None:
    detail = trace.get("detail") or {}
    batches = detail.get("batches") or []

    if heatmap.filter_val is None:
        # key-only mode: bucket each matching span by its own start time
        for batch in batches:
            for scope_spans in batch.get("scopeSpans") or []:
                for span in scope_spans.get("spans") or []:
                    ts_ns = int(span.get("startTimeUnixNano") or 0)
                    if not ts_ns:
                        continue
                    for attr in span.get("attributes") or []:
                        if attr.get("key") == heatmap.key:
                            val = _attr_str(attr.get("value") or {})
                            if val:
                                heatmap.add(val, ts_ns)
    else:
        # key=value mode: find earliest matching span ts, bucket co-occurring services there
        match_ts: int = 0
        for batch in batches:
            for scope_spans in batch.get("scopeSpans") or []:
                for span in scope_spans.get("spans") or []:
                    ts_ns = int(span.get("startTimeUnixNano") or 0)
                    for attr in span.get("attributes") or []:
                        if attr.get("key") == heatmap.key and _attr_str(attr.get("value") or {}) == heatmap.filter_val:
                            if not match_ts or ts_ns < match_ts:
                                match_ts = ts_ns
        if match_ts:
            seen: set[str] = set()
            for batch in batches:
                svc = _batch_service(batch)
                if svc and svc not in seen:
                    heatmap.add(svc, match_ts)
                    seen.add(svc)


def run_stats(watch_key: str | None = None) -> None:
    stats = StreamStats()
    neighbours: NeighbourGraph | None = None
    heatmap: WatchHeatmap | None = None

    if watch_key:
        if "=" in watch_key:
            key, _, val = watch_key.partition("=")
            neighbours = NeighbourGraph(key.strip(), val.strip())
            heatmap = WatchHeatmap(key.strip(), filter_val=val.strip())
        else:
            heatmap = WatchHeatmap(watch_key)

    def _print_and_exit(signum=None, frame=None) -> None:
        print(file=sys.stderr)
        if heatmap:
            heatmap.flush()
        if neighbours:
            neighbours.print()
        stats.print()
        sys.exit(0)

    signal.signal(signal.SIGINT, _print_and_exit)
    signal.signal(signal.SIGTERM, _print_and_exit)

    try:
        for line in sys.stdin:
            line = line.strip()
            if not line:
                continue
            try:
                trace = json.loads(line)
            except json.JSONDecodeError:
                continue
            stats.ingest(trace)
            if heatmap:
                _feed_heatmap(heatmap, trace)
            if neighbours:
                neighbours.ingest(trace)
            print(f"\r{stats.traces} traces ingested...", end="", file=sys.stderr, flush=True)
    except (EOFError, BrokenPipeError):
        pass

    print(file=sys.stderr)
    if heatmap:
        heatmap.flush()
    if neighbours:
        neighbours.print()
    stats.print()


# ──────────────────────────────────────────────────────────────────────────────
# Helpers
# ──────────────────────────────────────────────────────────────────────────────

def parse_kv(values: list[str]) -> list[tuple[str, str]]:
    result = []
    for v in values:
        if "=" not in v:
            print(f"error: expected key=value, got {v!r}", file=sys.stderr)
            sys.exit(1)
        k, _, val = v.partition("=")
        result.append((k.strip(), val.strip()))
    return result


def show_sql(sql: str, params: list | None = None) -> None:
    if params:
        result = sql
        for p in params:
            result = result.replace("?", repr(p), 1)
        print(result)
    else:
        print(sql)


def print_table(rows: list, cols: list[str]) -> None:
    col_widths = [
        max(len(c), max((len(str(r[i])) for r in rows), default=0))
        for i, c in enumerate(cols)
    ]
    print("  ".join(c.upper().ljust(col_widths[i]) for i, c in enumerate(cols)))
    print("  ".join("-" * w for w in col_widths))
    for row in rows:
        print("  ".join(str(v).ljust(col_widths[i]) for i, v in enumerate(row)))


def print_detail_blobs(detail_rows: list) -> None:
    print()
    for trace_id, detail in detail_rows:
        print(f"{'─' * 72}")
        print(f"trace: {trace_id}")
        print(f"{'─' * 72}")
        if isinstance(detail, str):
            detail = json.loads(detail)
        print(json.dumps(detail, indent=2))
        print()


# ──────────────────────────────────────────────────────────────────────────────
# DB mode — schema, import, queries
# ──────────────────────────────────────────────────────────────────────────────

def init_db(con: duckdb.DuckDBPyConnection) -> None:
    con.execute("""
        CREATE TABLE IF NOT EXISTS traces (
            trace_id        VARCHAR PRIMARY KEY,
            service_name    VARCHAR,
            root_span_name  VARCHAR,
            duration_ms     INTEGER,
            started_at      TIMESTAMPTZ,
            raw_detail      JSON
        )
    """)
    con.execute("""
        CREATE TABLE IF NOT EXISTS attributes (
            trace_id    VARCHAR,
            span_name   VARCHAR,
            scope       VARCHAR,
            key         VARCHAR,
            value_str   VARCHAR,
            value_int   BIGINT,
            value_bool  BOOLEAN
        )
    """)
    con.execute("CREATE INDEX IF NOT EXISTS idx_attr_key_val ON attributes (key, value_str)")
    con.execute("CREATE INDEX IF NOT EXISTS idx_attr_trace   ON attributes (trace_id)")
    con.execute("""
        CREATE TABLE IF NOT EXISTS spans (
            trace_id        VARCHAR,
            span_id         VARCHAR,
            parent_span_id  VARCHAR,
            service_name    VARCHAR,
            component       VARCHAR,
            span_name       VARCHAR,
            kind            VARCHAR,
            start_ns        BIGINT,
            duration_ns     BIGINT
        )
    """)
    con.execute("CREATE INDEX IF NOT EXISTS idx_spans_trace  ON spans (trace_id)")
    con.execute("CREATE INDEX IF NOT EXISTS idx_spans_parent ON spans (parent_span_id)")


def _extract_attr(attr: dict) -> tuple:
    key = attr.get("key", "")
    val = attr.get("value", {})
    str_val = val.get("stringValue")
    int_val = val.get("intValue")
    bool_val = val.get("boolValue")
    if int_val is not None:
        try:
            int_val = int(int_val)
        except (TypeError, ValueError):
            int_val = None
    return key, str_val, int_val, bool_val


def import_ndjson(con: duckdb.DuckDBPyConnection, file_path: str) -> None:
    imported = 0
    skipped = 0
    errors = 0

    with open(file_path) as f:
        for line_num, line in enumerate(f, 1):
            line = line.strip()
            if not line:
                continue
            try:
                obj = json.loads(line)
            except json.JSONDecodeError as e:
                print(f"warn: line {line_num}: {e}", file=sys.stderr)
                errors += 1
                continue

            trace_id = obj.get("traceID", "")
            if not trace_id:
                continue

            existing = con.execute(
                "SELECT 1 FROM traces WHERE trace_id = ?", [trace_id]
            ).fetchone()
            if existing:
                skipped += 1
                continue

            service_name = obj.get("rootServiceName", "")
            root_span_name = obj.get("rootTraceName", "")
            duration_ms = obj.get("durationMs", 0)
            start_ns_raw = obj.get("startTimeUnixNano", "0") or "0"
            detail = obj.get("detail")

            try:
                ns = int(start_ns_raw)
                started_at = (
                    datetime.fromtimestamp(ns / 1e9, tz=timezone.utc).isoformat()
                    if ns > 0 else None
                )
            except (ValueError, OSError):
                started_at = None

            con.execute(
                "INSERT INTO traces VALUES (?, ?, ?, ?, ?, ?)",
                [trace_id, service_name, root_span_name, duration_ms, started_at, json.dumps(detail)],
            )

            if detail:
                attr_rows = []
                span_rows = []
                for batch in detail.get("batches") or []:
                    for attr in batch.get("resource", {}).get("attributes") or []:
                        key, str_val, int_val, bool_val = _extract_attr(attr)
                        attr_rows.append((trace_id, None, "resource", key, str_val, int_val, bool_val))

                    for scope_span in batch.get("scopeSpans") or []:
                        svc = scope_span.get("scope", {}).get("name", "")
                        for span in scope_span.get("spans") or []:
                            span_name = span.get("name", "")
                            span_id = span.get("spanId", "")
                            parent_id = span.get("parentSpanId", "")
                            kind = span.get("kind", "")
                            component = None
                            try:
                                start_ns = int(span.get("startTimeUnixNano", "0") or "0")
                                end_ns = int(span.get("endTimeUnixNano", "0") or "0")
                                duration_ns = max(0, end_ns - start_ns)
                            except (ValueError, TypeError):
                                start_ns = 0
                                duration_ns = 0

                            for attr in span.get("attributes") or []:
                                if attr.get("key") == "component":
                                    component = attr.get("value", {}).get("stringValue")
                                key, str_val, int_val, bool_val = _extract_attr(attr)
                                attr_rows.append((trace_id, span_name, "span", key, str_val, int_val, bool_val))

                            span_rows.append((trace_id, span_id, parent_id, svc, component, span_name, kind, start_ns, duration_ns))

                if attr_rows:
                    con.executemany(
                        "INSERT INTO attributes VALUES (?, ?, ?, ?, ?, ?, ?)", attr_rows
                    )
                if span_rows:
                    con.executemany(
                        "INSERT INTO spans VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)", span_rows
                    )

            imported += 1

    msg = f"imported {imported} trace(s)"
    if skipped:
        msg += f", skipped {skipped} duplicate(s)"
    if errors:
        msg += f", {errors} parse error(s)"
    print(msg, file=sys.stderr)


def _attr_condition(scope: str, key: str, val: str) -> tuple[str, list]:
    try:
        int_val = int(val)
        clause = (
            f"EXISTS (SELECT 1 FROM attributes "
            f"WHERE trace_id = t.trace_id AND scope = '{scope}' AND key = ? "
            f"AND (value_str = ? OR value_int = ?))"
        )
        return clause, [key, val, int_val]
    except ValueError:
        clause = (
            f"EXISTS (SELECT 1 FROM attributes "
            f"WHERE trace_id = t.trace_id AND scope = '{scope}' AND key = ? AND value_str = ?)"
        )
        return clause, [key, val]


def query_db(span_attrs: list, resource_attrs: list) -> tuple[str, list]:
    sql = """
SELECT DISTINCT t.trace_id, t.service_name, t.root_span_name, t.duration_ms
FROM traces t
WHERE 1=1
"""
    params: list = []
    for key, val in span_attrs:
        clause, p = _attr_condition("span", key, val)
        sql += f"  AND {clause}\n"
        params += p
    for key, val in resource_attrs:
        clause, p = _attr_condition("resource", key, val)
        sql += f"  AND {clause}\n"
        params += p
    sql += "ORDER BY t.duration_ms DESC"
    return sql, params


def list_db(key: str, scope: str, with_sample: bool = False) -> tuple[str, list]:
    if with_sample:
        return (
            "SELECT value_str, ANY_VALUE(trace_id) AS sample_traceID "
            "FROM attributes WHERE scope = ? AND key = ? AND value_str IS NOT NULL "
            "GROUP BY value_str ORDER BY value_str",
            [scope, key],
        )
    return (
        "SELECT DISTINCT value_str FROM attributes "
        "WHERE scope = ? AND key = ? AND value_str IS NOT NULL ORDER BY value_str",
        [scope, key],
    )


def schema_db(con: duckdb.DuckDBPyConnection) -> tuple[str, int]:
    total = con.execute("SELECT COUNT(*) FROM traces").fetchone()[0]
    # Coalesce str/int/bool into a display string for cardinality and samples.
    # Integer attributes (e.g. http.status_code) are tagged with "(int)".
    sql = """
WITH unified AS (
    SELECT scope, key, trace_id,
           COALESCE(
               value_str,
               CASE WHEN value_int IS NOT NULL THEN '(int) ' || CAST(value_int AS VARCHAR) ELSE NULL END,
               CASE WHEN value_bool IS NOT NULL THEN CAST(value_bool AS VARCHAR) ELSE NULL END
           ) AS display_val
    FROM attributes
),
attr_vals AS (
    SELECT scope, key, display_val,
           COUNT(DISTINCT trace_id) AS tc,
           ROW_NUMBER() OVER (PARTITION BY scope, key ORDER BY COUNT(DISTINCT trace_id) DESC) AS rn
    FROM unified
    WHERE display_val IS NOT NULL
    GROUP BY scope, key, display_val
),
summary AS (
    SELECT scope, key,
           COUNT(DISTINCT trace_id) AS traces,
           COUNT(DISTINCT display_val) AS cardinality
    FROM unified
    GROUP BY scope, key
),
samples AS (
    SELECT scope, key, STRING_AGG(display_val, ', ' ORDER BY rn) AS sample_values
    FROM attr_vals
    WHERE rn <= 3
    GROUP BY scope, key
)
SELECT s.scope, s.key, s.traces, s.cardinality, COALESCE(sa.sample_values, '') AS samples
FROM summary s
LEFT JOIN samples sa USING (scope, key)
ORDER BY s.scope, s.traces DESC, s.key
"""
    return sql, total


def service_graph_db() -> str:
    # callee: use component attribute when present (e.g. db, cache, payment-svc),
    # otherwise fall back to service_name from scope. This handles both real
    # multi-resource OTLP (different batches per service) and single-resource
    # traces where downstream components are tagged with a component attribute.
    return """
SELECT
    p.service_name                                      AS caller,
    COALESCE(NULLIF(c.component, ''), c.service_name)  AS callee,
    c.span_name                                        AS operation,
    COUNT(*)                                           AS calls,
    ROUND(AVG(c.duration_ns / 1e6), 2)                AS avg_ms,
    ROUND(MAX(c.duration_ns / 1e6), 2)                AS max_ms
FROM spans c
JOIN spans p
  ON c.parent_span_id = p.span_id
 AND c.trace_id = p.trace_id
WHERE c.parent_span_id != ''
  AND COALESCE(NULLIF(c.component, ''), c.service_name) != p.service_name
GROUP BY 1, 2, 3
ORDER BY calls DESC
"""


def span_tree_db(trace_id: str) -> tuple[str, list]:
    sql = """
WITH RECURSIVE
trace_spans AS (
    SELECT * FROM spans WHERE trace_id = ?
),
tree AS (
    SELECT span_id, parent_span_id, service_name, span_name, kind, duration_ns, start_ns, 0 AS depth
    FROM trace_spans
    WHERE parent_span_id = '' OR parent_span_id IS NULL
    UNION ALL
    SELECT s.span_id, s.parent_span_id, s.service_name, s.span_name, s.kind, s.duration_ns, s.start_ns, t.depth + 1
    FROM trace_spans s
    JOIN tree t ON s.parent_span_id = t.span_id
)
SELECT depth, service_name, span_name, kind, duration_ns
FROM tree
ORDER BY start_ns
"""
    return sql, [trace_id]


def print_span_tree(rows: list) -> None:
    for row in rows:
        depth, service, span_name, kind, duration_ns = row
        ms = (duration_ns or 0) / 1e6
        indent = "  " * depth
        kind_short = (kind or "").replace("SPAN_KIND_", "")
        print(f"{indent}{span_name}  [{service} · {kind_short}]  {ms:.2f}ms")


def time_series_db(key: str, bucket: str) -> tuple[str, list]:
    trunc = {"minute": "minute", "hour": "hour", "day": "day"}.get(bucket, "hour")
    # EXISTS avoids over-counting: attributes has no span_id, so a JOIN on
    # span_name would create a cartesian product when multiple attribute rows
    # share the same span_name within a trace.
    sql = f"""
SELECT
    DATE_TRUNC('{trunc}', to_timestamp(s.start_ns / 1e9)) AS bucket,
    COUNT(*)                                               AS occurrences
FROM spans s
WHERE EXISTS (
    SELECT 1 FROM attributes a
    WHERE a.trace_id  = s.trace_id
      AND a.span_name = s.span_name
      AND a.scope     = 'span'
      AND a.key       = ?
)
GROUP BY 1
ORDER BY 1
"""
    return sql, [key]


def print_time_series(rows: list, key: str, bucket: str) -> None:
    max_count = max(r[1] for r in rows)
    bar_width = 40
    fmt = {"minute": "%Y-%m-%d %H:%M", "hour": "%Y-%m-%d %H:00", "day": "%Y-%m-%d"}.get(bucket, "%Y-%m-%d %H:00")
    print(f"\n{key}  (per {bucket})\n")
    for ts, count in rows:
        bar = "█" * max(1, int(count / max_count * bar_width))
        label = ts.strftime(fmt) if hasattr(ts, "strftime") else str(ts)[:16]
        print(f"  {label}  {bar:<{bar_width}}  {count}")
    print()


def duration_stats_db() -> str:
    return """
SELECT
    COUNT(*)                                                              AS root_spans,
    MIN(duration_ns)  / 1e6                                              AS min_ms,
    AVG(duration_ns)  / 1e6                                              AS avg_ms,
    MAX(duration_ns)  / 1e6                                              AS max_ms,
    PERCENTILE_CONT(0.50) WITHIN GROUP (ORDER BY duration_ns) / 1e6     AS p50_ms,
    PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY duration_ns) / 1e6     AS p95_ms,
    PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY duration_ns) / 1e6     AS p99_ms
FROM spans
WHERE parent_span_id IS NULL OR parent_span_id = ''
"""


def print_duration_stats(row: tuple) -> None:
    root_spans, min_ms, avg_ms, max_ms, p50_ms, p95_ms, p99_ms = row
    # recommend a tail lookback window: ceil(p99 * 1.5), rounded to a clean interval
    import math
    rec_s = math.ceil(p99_ms / 1000 * 1.5)
    if rec_s < 30:
        rec_s = 30
    # round up to nearest 30s
    rec_s = math.ceil(rec_s / 30) * 30
    print("\nROOT SPAN DURATION STATS\n")
    print(f"  traces  : {int(root_spans)}")
    print(f"  min     : {min_ms:.1f} ms")
    print(f"  avg     : {avg_ms:.1f} ms")
    print(f"  p50     : {p50_ms:.1f} ms")
    print(f"  p95     : {p95_ms:.1f} ms")
    print(f"  p99     : {p99_ms:.1f} ms")
    print(f"  max     : {max_ms:.1f} ms")
    print(f"\n  recommended --lookback for `notrace tempo tail`: {rec_s}s")
    print()


def _check_spans(con: duckdb.DuckDBPyConnection, db_path: str) -> bool:
    span_count = con.execute("SELECT COUNT(*) FROM spans").fetchone()[0]
    trace_count = con.execute("SELECT COUNT(*) FROM traces").fetchone()[0]
    if span_count == 0 and trace_count > 0:
        print(
            f"warn: spans table is empty — re-import: "
            f"python3 lab/query.py --db {db_path} --import <file>",
            file=sys.stderr,
        )
        return False
    return True


# ──────────────────────────────────────────────────────────────────────────────
# File mode — CTE-based queries (unchanged from before)
# ──────────────────────────────────────────────────────────────────────────────

def _require_detail_field(con: duckdb.DuckDBPyConnection, file: str) -> None:
    """Exit with a clear message if the NDJSON was captured without --details.

    Uses WHERE detail IS NOT NULL so that early lines where GetTrace failed
    (and detail was omitted) do not trigger a false positive.
    """
    try:
        row = con.execute(
            f"SELECT 1 FROM read_json({file!r}, format='newline_delimited', auto_detect=true) WHERE detail IS NOT NULL LIMIT 1"
        ).fetchone()
        has_detail = row is not None
    except duckdb.Error:
        has_detail = False
    if not has_detail:
        print("error: file has no 'detail' field — re-capture with --details:", file=sys.stderr)
        print("  notrace tempo tail -o json --details >> notrace.json", file=sys.stderr)
        print("  notrace tempo search --start 1h -o json --details > notrace.json", file=sys.stderr)
        sys.exit(1)

def build_values_query(file: str, key: str, attr_type: str, with_sample: bool = False) -> str:
    cte = f"""
WITH
batches AS (
    SELECT traceID, UNNEST(detail.batches) AS b
    FROM read_json({file!r}, format = 'newline_delimited', auto_detect = true)
),
scope_spans AS (
    SELECT traceID,
           b.resource.attributes AS resource_attrs,
           UNNEST(b.scopeSpans) AS ss
    FROM batches
),
spans AS (
    SELECT traceID,
           resource_attrs,
           UNNEST(ss.spans) AS sp
    FROM scope_spans
),
span_attrs AS (
    SELECT traceID,
           resource_attrs,
           UNNEST(sp.attributes) AS attr
    FROM spans
),
res_attrs AS (
    SELECT traceID,
           UNNEST(resource_attrs) AS ra
    FROM span_attrs
)
"""
    if attr_type == "resource":
        if with_sample:
            return cte + f"""
SELECT ra.value.stringValue AS value, ANY_VALUE(traceID) AS sample_traceID
FROM res_attrs
WHERE ra.key = {key!r} AND ra.value.stringValue IS NOT NULL
GROUP BY 1
ORDER BY 1
"""
        return cte + f"""
SELECT DISTINCT ra.value.stringValue AS value
FROM res_attrs
WHERE ra.key = {key!r} AND ra.value.stringValue IS NOT NULL
ORDER BY 1
"""
    else:
        if with_sample:
            return cte + f"""
SELECT attr.value.stringValue AS value, ANY_VALUE(traceID) AS sample_traceID
FROM span_attrs
WHERE attr.key = {key!r} AND attr.value.stringValue IS NOT NULL
GROUP BY 1
ORDER BY 1
"""
        return cte + f"""
SELECT DISTINCT attr.value.stringValue AS value
FROM span_attrs
WHERE attr.key = {key!r} AND attr.value.stringValue IS NOT NULL
ORDER BY 1
"""


def build_detail_query(file: str, trace_ids: list[str]) -> str:
    ids = ", ".join(repr(t) for t in trace_ids)
    return f"""
SELECT traceID, detail
FROM read_json({file!r}, format = 'newline_delimited', auto_detect = true)
WHERE traceID IN ({ids})
ORDER BY durationMs DESC
"""


def build_query(file: str, span_attrs: list, resource_attrs: list) -> str:
    cte = f"""
WITH
batches AS (
    SELECT traceID, rootServiceName, rootTraceName, durationMs,
           UNNEST(detail.batches) AS b
    FROM read_json({file!r}, format = 'newline_delimited', auto_detect = true)
),
scope_spans AS (
    SELECT traceID, rootServiceName, rootTraceName, durationMs,
           b.resource.attributes AS resource_attrs,
           UNNEST(b.scopeSpans) AS ss
    FROM batches
),
spans AS (
    SELECT traceID, rootServiceName, rootTraceName, durationMs,
           resource_attrs,
           UNNEST(ss.spans) AS sp
    FROM scope_spans
),
span_attrs AS (
    SELECT traceID, rootServiceName, rootTraceName, durationMs,
           resource_attrs,
           UNNEST(sp.attributes) AS attr
    FROM spans
),
res_attrs AS (
    SELECT traceID,
           UNNEST(resource_attrs) AS ra
    FROM span_attrs
)
SELECT DISTINCT
    traceID,
    rootServiceName,
    rootTraceName,
    durationMs
FROM span_attrs
WHERE 1=1
"""
    conditions = []
    for key, val in span_attrs:
        conditions.append(
            f"  AND traceID IN ("
            f"SELECT traceID FROM span_attrs "
            f"WHERE attr.key = {key!r} AND attr.value.stringValue = {val!r})"
        )
    for key, val in resource_attrs:
        conditions.append(
            f"  AND traceID IN ("
            f"SELECT traceID FROM res_attrs "
            f"WHERE ra.key = {key!r} AND ra.value.stringValue = {val!r})"
        )
    return cte + "\n".join(conditions) + "\nORDER BY durationMs DESC"


# ──────────────────────────────────────────────────────────────────────────────
# ClickHouse mode  (--clickhouse host:port)
# ──────────────────────────────────────────────────────────────────────────────

try:
    import clickhouse_connect
    _CH_AVAILABLE = True
except ImportError:
    _CH_AVAILABLE = False

_CH_DB = "otel"
_CH_TABLE = "otel_traces"


def _ch_client(dsn: str):
    if not _CH_AVAILABLE:
        print("error: clickhouse-connect not installed — run: pip install clickhouse-connect", file=sys.stderr)
        sys.exit(1)
    host, _, port_str = dsn.partition(":")
    port = int(port_str) if port_str else 8123
    return clickhouse_connect.get_client(host=host, port=port, database=_CH_DB)


def schema_ch(ch) -> None:
    sql = f"""
SELECT scope, key,
       countDistinct(trace_id) AS traces,
       countDistinct(val)      AS cardinality,
       groupArray(5)(val)      AS samples
FROM (
    SELECT 'span'     AS scope, arrayJoin(mapKeys(SpanAttributes))     AS key,
           SpanAttributes[key] AS val, TraceId AS trace_id
    FROM {_CH_TABLE} WHERE val != ''
    UNION ALL
    SELECT 'resource' AS scope, arrayJoin(mapKeys(ResourceAttributes)) AS key,
           ResourceAttributes[key] AS val, TraceId AS trace_id
    FROM {_CH_TABLE} WHERE val != ''
)
GROUP BY scope, key
ORDER BY scope DESC, traces DESC
"""
    total = ch.command(f"SELECT countDistinct(TraceId) FROM {_CH_TABLE}")
    rows = ch.query(sql).result_rows
    if not rows:
        print("no attributes found", file=sys.stderr)
        return
    print(f"ATTRIBUTE SCHEMA  (from {_CH_DB}.{_CH_TABLE} — {total} traces)\n")
    print_table(rows, ["scope", "key", "traces", "cardinality", "sample values"])


def list_ch(ch, key: str, scope: str, with_sample: bool = False) -> None:
    attr_map = "SpanAttributes" if scope == "span" else "ResourceAttributes"
    if with_sample:
        sql = (f"SELECT {attr_map}['{key}'] AS val, any(TraceId) AS sample "
               f"FROM {_CH_TABLE} WHERE val != '' GROUP BY val ORDER BY val")
    else:
        sql = (f"SELECT DISTINCT {attr_map}['{key}'] AS val "
               f"FROM {_CH_TABLE} WHERE val != '' ORDER BY val")
    rows = ch.query(sql).result_rows
    if not rows:
        print(f"no values found for {scope} attribute {key!r}", file=sys.stderr)
        return
    for row in rows:
        print("  ".join(str(c) for c in row))


def query_ch(ch, span_attrs: list[tuple[str, str]], resource_attrs: list[tuple[str, str]]) -> None:
    wheres = ["ParentSpanId = ''"]
    for key, val in span_attrs:
        wheres.append(f"SpanAttributes['{key}'] = '{val}'")
    for key, val in resource_attrs:
        wheres.append(f"ResourceAttributes['{key}'] = '{val}'")
    where_clause = " AND ".join(wheres)
    sql = f"""
SELECT TraceId, ServiceName, SpanName,
       round(Duration / 1e6, 2) AS duration_ms,
       Timestamp
FROM {_CH_TABLE}
WHERE {where_clause}
ORDER BY Duration DESC
LIMIT 50
"""
    rows = ch.query(sql).result_rows
    if not rows:
        print("no traces matched", file=sys.stderr)
        return
    print_table(rows, ["trace_id", "service", "root_span", "duration_ms", "started"])


def stats_ch(ch, watch_key: str | None = None) -> None:
    """Print duration stats + optional heatmap directly from ClickHouse aggregations."""
    # Duration stats (root spans = ParentSpanId empty)
    row = ch.query(f"""
SELECT count()                                AS root_spans,
       min(Duration) / 1e6                   AS min_ms,
       avg(Duration) / 1e6                   AS avg_ms,
       max(Duration) / 1e6                   AS max_ms,
       quantile(0.50)(Duration) / 1e6        AS p50_ms,
       quantile(0.95)(Duration) / 1e6        AS p95_ms,
       quantile(0.99)(Duration) / 1e6        AS p99_ms
FROM {_CH_TABLE}
WHERE ParentSpanId = ''
""").result_rows
    if row:
        print_duration_stats(row[0])

    if not watch_key:
        return

    # Heatmap: bucket by real span time
    filter_val = None
    key = watch_key
    if "=" in watch_key:
        key, _, filter_val = watch_key.partition("=")

    if filter_val is None:
        # key-only: columns = unique values of SpanAttributes[key] per bucket
        sql = f"""
SELECT toStartOfInterval(Timestamp, INTERVAL 10 SECOND) AS bucket,
       SpanAttributes['{key}']                          AS val,
       count()                                          AS cnt
FROM {_CH_TABLE}
WHERE val != ''
GROUP BY bucket, val
ORDER BY bucket, val
"""
    else:
        # key=value: columns = co-occurring ServiceName in matching traces
        sql = f"""
SELECT toStartOfInterval(Timestamp, INTERVAL 10 SECOND) AS bucket,
       ServiceName                                      AS val,
       countDistinct(TraceId)                           AS cnt
FROM {_CH_TABLE}
WHERE TraceId IN (
    SELECT DISTINCT TraceId FROM {_CH_TABLE}
    WHERE SpanAttributes['{key}'] = '{filter_val}'
)
GROUP BY bucket, val
ORDER BY bucket, val
"""
    rows = ch.query(sql).result_rows
    if not rows:
        print(f"no data for {watch_key!r}", file=sys.stderr)
        return

    # Feed into WatchHeatmap using span timestamps
    heatmap = WatchHeatmap(key, filter_val=filter_val)
    for bucket_ts, val, cnt in rows:
        ts_ns = int(bucket_ts.timestamp() * 1e9)
        for _ in range(cnt):
            heatmap.add(val, ts_ns)
    heatmap.flush()


def service_graph_ch(ch) -> None:
    """Print cross-service call edges from ClickHouse using parentSpanId joins."""
    sql = f"""
SELECT
    p.ServiceName                                                     AS caller,
    coalesce(nullIf(c.SpanAttributes['component'], ''), c.ServiceName) AS callee,
    c.SpanName                                                        AS operation,
    count()                                                           AS calls,
    round(avg(c.Duration) / 1e6, 2)                                   AS avg_ms,
    round(max(c.Duration) / 1e6, 2)                                   AS max_ms
FROM {_CH_TABLE} c
JOIN {_CH_TABLE} p
  ON c.TraceId = p.TraceId
 AND c.ParentSpanId = p.SpanId
WHERE c.ParentSpanId != ''
  AND coalesce(nullIf(c.SpanAttributes['component'], ''), c.ServiceName) != p.ServiceName
GROUP BY caller, callee, operation
ORDER BY calls DESC
"""
    rows = ch.query(sql).result_rows
    if not rows:
        print("no cross-service calls found", file=sys.stderr)
        return
    print_table(rows, ["caller", "callee", "operation", "calls", "avg_ms", "max_ms"])


# ──────────────────────────────────────────────────────────────────────────────
# Main
# ──────────────────────────────────────────────────────────────────────────────

def main() -> None:
    parser = argparse.ArgumentParser(
        description="Query notrace NDJSON output with DuckDB",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )

    parser.add_argument("--stats", action="store_true", help="Read NDJSON from stdin and print live stream stats on EOF/Ctrl-C (pipe mode)")
    parser.add_argument("--watch", metavar="KEY", help="With --stats: show live heatmap of a span attribute value over 10s buckets")

    src = parser.add_mutually_exclusive_group()
    src.add_argument("--file", "-f", metavar="FILE", help="NDJSON file (one-shot, re-parsed each query)")
    src.add_argument("--db", metavar="PATH", help="DuckDB database file (persistent, indexed)")
    src.add_argument("--clickhouse", metavar="HOST:PORT", help="ClickHouse server (e.g. localhost:8123) — queries otel.otel_traces directly")

    parser.add_argument("--import", dest="import_file", metavar="FILE", help="Import NDJSON into --db, skipping duplicates (requires --db)")
    parser.add_argument("--schema", action="store_true", help="Print attribute cardinality report (requires --db)")
    parser.add_argument("--service-graph", action="store_true", help="Print cross-service call edges (requires --db)")
    parser.add_argument("--span-tree", metavar="TRACE_ID", help="Print span tree for a trace ID (requires --db)")
    parser.add_argument("--time-series", metavar="KEY", help="Show occurrences of a span attribute key over time (requires --db)")
    parser.add_argument("--bucket", choices=["minute", "hour", "day"], default="hour", help="Time bucket size for --time-series (default: hour)")
    parser.add_argument("--duration-stats", action="store_true", help="Show root span duration percentiles and recommended tail lookback (requires --db)")
    parser.add_argument("--span-attr", "-s", metavar="key=value", action="append", default=[], help="Filter by span attribute (repeatable, ANDed)")
    parser.add_argument("--resource-attr", "-r", metavar="key=value", action="append", default=[], help="Filter by resource attribute (repeatable, ANDed)")
    parser.add_argument("--detail", "-d", action="store_true", help="Pretty-print the full OTLP detail for each matched trace")
    parser.add_argument("--trace-id", metavar="ID", help="Pretty-print the full OTLP detail for a specific trace ID")
    parser.add_argument("--list-resource-attr", metavar="KEY", help="List unique values for a resource attribute key")
    parser.add_argument("--list-span-attr", metavar="KEY", help="List unique values for a span attribute key")
    parser.add_argument("--sql", action="store_true", help="Print the generated SQL instead of running it")
    args = parser.parse_args()

    if args.stats:
        run_stats(watch_key=args.watch)
        return

    # ── ClickHouse mode ────────────────────────────────────────────────────────
    if args.clickhouse:
        ch = _ch_client(args.clickhouse)
        span_attrs   = parse_kv(args.span_attr)
        resource_attrs = parse_kv(args.resource_attr)

        if args.schema:
            schema_ch(ch)
        elif args.service_graph:
            service_graph_ch(ch)
        elif args.list_span_attr:
            list_ch(ch, args.list_span_attr, "span", with_sample=args.detail)
        elif args.list_resource_attr:
            list_ch(ch, args.list_resource_attr, "resource", with_sample=args.detail)
        elif args.duration_stats or args.watch:
            stats_ch(ch, watch_key=args.watch)
        elif span_attrs or resource_attrs:
            query_ch(ch, span_attrs, resource_attrs)
        else:
            stats_ch(ch)
        return

    if args.import_file and not args.db:
        print("error: --import requires --db", file=sys.stderr)
        sys.exit(1)
    if args.schema and not args.db:
        print("error: --schema requires --db", file=sys.stderr)
        sys.exit(1)
    if args.service_graph and not args.db and not args.clickhouse:
        print("error: --service-graph requires --db or --clickhouse", file=sys.stderr)
        sys.exit(1)
    if args.span_tree and not args.db:
        print("error: --span-tree requires --db", file=sys.stderr)
        sys.exit(1)
    if args.time_series and not args.db:
        print("error: --time-series requires --db", file=sys.stderr)
        sys.exit(1)
    if args.duration_stats and not args.db:
        print("error: --duration-stats requires --db", file=sys.stderr)
        sys.exit(1)
    if not args.file and not args.db and not args.clickhouse:
        print("error: one of --file, --db, or --clickhouse is required", file=sys.stderr)
        sys.exit(1)

    con = duckdb.connect(args.db or ":memory:")

    if args.db:
        init_db(con)

    # ── --import ───────────────────────────────────────────────────────────────

    if args.import_file:
        import_ndjson(con, args.import_file)
        if not any([args.schema, args.list_resource_attr, args.list_span_attr,
                    args.trace_id, args.span_attr, args.resource_attr]):
            return

    # ── DB mode ────────────────────────────────────────────────────────────────

    if args.db:
        # --schema
        if args.schema:
            sql, total = schema_db(con)
            if args.sql:
                show_sql(sql)
                return
            try:
                rows = con.execute(sql).fetchall()
            except duckdb.Error as e:
                print(f"error: {e}", file=sys.stderr)
                sys.exit(1)
            if not rows:
                print("no attributes found — import traces first with --import", file=sys.stderr)
                return
            print(f"ATTRIBUTE SCHEMA  (from {args.db} — {total} traces)\n")
            print_table(rows, ["scope", "key", "traces", "cardinality", "sample values"])
            return

        # --service-graph
        if args.service_graph:
            if not _check_spans(con, args.db):
                return
            sql = service_graph_db()
            if args.sql:
                show_sql(sql)
                return
            try:
                rows = con.execute(sql).fetchall()
            except duckdb.Error as e:
                print(f"error: {e}", file=sys.stderr)
                sys.exit(1)
            if not rows:
                print("no cross-service calls found", file=sys.stderr)
                return
            print_table(rows, ["caller", "callee", "operation", "calls", "avg_ms", "max_ms"])
            return

        # --span-tree
        if args.span_tree:
            if not _check_spans(con, args.db):
                return
            sql, params = span_tree_db(args.span_tree)
            if args.sql:
                show_sql(sql, params)
                return
            try:
                rows = con.execute(sql, params).fetchall()
            except duckdb.Error as e:
                print(f"error: {e}", file=sys.stderr)
                sys.exit(1)
            if not rows:
                print(f"trace {args.span_tree!r} not found or has no spans", file=sys.stderr)
                sys.exit(1)
            print_span_tree(rows)
            return

        # --time-series
        if args.time_series:
            if not _check_spans(con, args.db):
                return
            sql, params = time_series_db(args.time_series, args.bucket)
            if args.sql:
                show_sql(sql, params)
                return
            try:
                rows = con.execute(sql, params).fetchall()
            except duckdb.Error as e:
                print(f"error: {e}", file=sys.stderr)
                sys.exit(1)
            if not rows:
                print(f"no spans found with attribute {args.time_series!r}", file=sys.stderr)
                return
            print_time_series(rows, args.time_series, args.bucket)
            return

        # --duration-stats
        if args.duration_stats:
            if not _check_spans(con, args.db):
                return
            sql = duration_stats_db()
            if args.sql:
                show_sql(sql)
                return
            try:
                row = con.execute(sql).fetchone()
            except duckdb.Error as e:
                print(f"error: {e}", file=sys.stderr)
                sys.exit(1)
            if not row or row[0] == 0:
                print("no root spans found — import traces first with --import", file=sys.stderr)
                return
            print_duration_stats(row)
            return

        # --list-*
        for key, scope in [(args.list_resource_attr, "resource"), (args.list_span_attr, "span")]:
            if not key:
                continue
            sql, params = list_db(key, scope, with_sample=args.detail)
            if args.sql:
                show_sql(sql, params)
                return
            try:
                rows = con.execute(sql, params).fetchall()
            except duckdb.Error as e:
                print(f"error: {e}", file=sys.stderr)
                sys.exit(1)
            if not rows:
                print(f"no values found for {key!r}", file=sys.stderr)
                return
            print(key)
            for row in rows:
                val = row[0]
                if not args.detail:
                    print(f"  {val}")
                    continue
                sample_id = row[1]
                print(f"  {val}  →  {sample_id}")
                detail_row = con.execute(
                    "SELECT trace_id, raw_detail FROM traces WHERE trace_id = ?", [sample_id]
                ).fetchone()
                if detail_row:
                    _, raw = detail_row
                    detail = json.loads(raw) if isinstance(raw, str) else raw
                    print(json.dumps(detail, indent=2))
                    print()
            return

        # --trace-id
        if args.trace_id:
            try:
                row = con.execute(
                    "SELECT trace_id, raw_detail FROM traces WHERE trace_id = ?", [args.trace_id]
                ).fetchone()
            except duckdb.Error as e:
                print(f"error: {e}", file=sys.stderr)
                sys.exit(1)
            if not row:
                print(f"trace {args.trace_id!r} not found", file=sys.stderr)
                sys.exit(1)
            _, raw = row
            detail = json.loads(raw) if isinstance(raw, str) else raw
            print(json.dumps(detail, indent=2))
            return

        # filter query
        span_attrs = parse_kv(args.span_attr)
        resource_attrs = parse_kv(args.resource_attr)
        sql, params = query_db(span_attrs, resource_attrs)
        if args.sql:
            show_sql(sql, params)
            return
        try:
            rows = con.execute(sql, params).fetchall()
        except duckdb.Error as e:
            print(f"error: {e}", file=sys.stderr)
            sys.exit(1)
        if not rows:
            print("no traces matched", file=sys.stderr)
            return
        print_table(rows, ["trace_id", "service_name", "root_span_name", "duration_ms"])
        print(f"\n{len(rows)} trace(s) matched", file=sys.stderr)
        if args.detail:
            trace_ids = [r[0] for r in rows]
            placeholders = ", ".join("?" * len(trace_ids))
            detail_rows = con.execute(
                f"SELECT trace_id, raw_detail FROM traces WHERE trace_id IN ({placeholders}) ORDER BY duration_ms DESC",
                trace_ids,
            ).fetchall()
            print_detail_blobs(detail_rows)
        return

    # ── File mode ──────────────────────────────────────────────────────────────

    file = args.file
    _require_detail_field(con, file)

    for key, attr_type in [(args.list_resource_attr, "resource"), (args.list_span_attr, "span")]:
        if not key:
            continue
        sql = build_values_query(file, key, attr_type, with_sample=args.detail)
        if args.sql:
            show_sql(sql)
            return
        try:
            rows = con.execute(sql).fetchall()
        except duckdb.Error as e:
            print(f"error: {e}", file=sys.stderr)
            sys.exit(1)
        if not rows:
            print(f"no values found for {key!r}", file=sys.stderr)
            return
        print(key)
        for row in rows:
            val = row[0]
            if not args.detail:
                print(f"  {val}")
                continue
            sample_id = row[1]
            print(f"  {val}  →  {sample_id}")
            detail_rows = con.execute(build_detail_query(file, [sample_id])).fetchall()
            if detail_rows:
                _, detail = detail_rows[0]
                print(json.dumps(detail, indent=2))
                print()
        return

    if args.trace_id:
        sql = build_detail_query(file, [args.trace_id])
        try:
            rows = con.execute(sql).fetchall()
        except duckdb.Error as e:
            print(f"error: {e}", file=sys.stderr)
            sys.exit(1)
        if not rows:
            print(f"trace {args.trace_id!r} not found", file=sys.stderr)
            sys.exit(1)
        _, detail = rows[0]
        print(json.dumps(detail, indent=2))
        return

    span_attrs = parse_kv(args.span_attr)
    resource_attrs = parse_kv(args.resource_attr)
    sql = build_query(file, span_attrs, resource_attrs)

    if args.sql:
        show_sql(sql)
        return

    try:
        rel = con.execute(sql)
        rows = rel.fetchall()
    except duckdb.Error as e:
        print(f"error: {e}", file=sys.stderr)
        sys.exit(1)

    if not rows:
        print("no traces matched", file=sys.stderr)
        return

    cols = [d[0] for d in rel.description]
    print_table(rows, cols)
    print(f"\n{len(rows)} trace(s) matched", file=sys.stderr)

    if not args.detail:
        return

    trace_ids = [row[0] for row in rows]
    try:
        detail_rows = con.execute(build_detail_query(file, trace_ids)).fetchall()
    except duckdb.Error as e:
        print(f"error fetching detail: {e}", file=sys.stderr)
        sys.exit(1)

    print_detail_blobs(detail_rows)


if __name__ == "__main__":
    main()
