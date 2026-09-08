#!/usr/bin/env python3
"""
notrace query helper — offline analysis of notrace NDJSON output using DuckDB.

Usage:
    python3 lab/query.py --file notrace.json
    python3 lab/query.py --file notrace.json --resource-attr service.name=frontend
    python3 lab/query.py --file notrace.json --span-attr hola.code=M1234
    python3 lab/query.py --file notrace.json \
        --resource-attr service.name=frontend \
        --span-attr hola.code=M1234 \
        --span-attr http.method=GET

Requires:
    pip install duckdb
"""

import argparse
import json
import sys

try:
    import duckdb
except ImportError:
    print("error: duckdb not installed — run: pip install duckdb", file=sys.stderr)
    sys.exit(1)


def parse_kv(values: list[str]) -> list[tuple[str, str]]:
    result = []
    for v in values:
        if "=" not in v:
            print(f"error: expected key=value, got {v!r}", file=sys.stderr)
            sys.exit(1)
        k, _, val = v.partition("=")
        result.append((k.strip(), val.strip()))
    return result


def build_values_query(file: str, key: str, attr_type: str, with_sample: bool = False) -> str:
    # attr_type: "span" queries span_attrs CTE, "resource" queries res_attrs CTE
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


def build_query(file: str, span_attrs: list[tuple[str, str]], resource_attrs: list[tuple[str, str]]) -> str:
    # DuckDB requires chained unnests to go through CTEs — you cannot reference
    # an alias from one UNNEST in a subsequent UNNEST in the same FROM clause.
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


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Query notrace NDJSON output with DuckDB",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    parser.add_argument("--file", "-f", required=True, help="NDJSON file produced by notrace (--details --output json)")
    parser.add_argument("--span-attr", "-s", metavar="key=value", action="append", default=[], help="Filter by span attribute (repeatable, ANDed)")
    parser.add_argument("--resource-attr", "-r", metavar="key=value", action="append", default=[], help="Filter by resource attribute (repeatable, ANDed)")
    parser.add_argument("--detail", "-d", action="store_true", help="Pretty-print the full OTLP detail for each matched trace")
    parser.add_argument("--trace-id", metavar="ID", help="Pretty-print the full OTLP detail for a specific trace ID")
    parser.add_argument("--list-resource-attr", metavar="KEY", help="List unique values for a resource attribute key")
    parser.add_argument("--list-span-attr", metavar="KEY", help="List unique values for a span attribute key")
    parser.add_argument("--sql", action="store_true", help="Print the generated SQL instead of running it")
    args = parser.parse_args()

    con = duckdb.connect()

    # --list-* flags: enumerate unique values and exit
    for key, attr_type in [(args.list_resource_attr, "resource"), (args.list_span_attr, "span")]:
        if not key:
            continue
        sql = build_values_query(args.file, key, attr_type, with_sample=args.detail)
        if args.sql:
            print(sql)
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
            detail_rows = con.execute(build_detail_query(args.file, [sample_id])).fetchall()
            if detail_rows:
                _, detail = detail_rows[0]
                print(json.dumps(detail, indent=2))
                print()
        return

    if args.trace_id:
        sql = build_detail_query(args.file, [args.trace_id])
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

    sql = build_query(args.file, span_attrs, resource_attrs)

    if args.sql:
        print(sql)
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
    col_widths = [max(len(c), max((len(str(r[i])) for r in rows), default=0)) for i, c in enumerate(cols)]

    header = "  ".join(c.upper().ljust(col_widths[i]) for i, c in enumerate(cols))
    print(header)
    print("  ".join("-" * w for w in col_widths))
    for row in rows:
        print("  ".join(str(v).ljust(col_widths[i]) for i, v in enumerate(row)))

    print(f"\n{len(rows)} trace(s) matched", file=sys.stderr)

    if not args.detail:
        return

    trace_ids = [row[0] for row in rows]
    detail_sql = build_detail_query(args.file, trace_ids)
    try:
        detail_rows = con.execute(detail_sql).fetchall()
    except duckdb.Error as e:
        print(f"error fetching detail: {e}", file=sys.stderr)
        sys.exit(1)

    print()
    for trace_id, detail in detail_rows:
        print(f"{'─' * 72}")
        print(f"trace: {trace_id}")
        print(f"{'─' * 72}")
        print(json.dumps(detail, indent=2))
        print()


if __name__ == "__main__":
    main()
