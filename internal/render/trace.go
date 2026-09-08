package render

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/pathcl/notrace/internal/tempo"
)

// Traces writes a summary list of traces (no per-trace detail fetch required).
func Traces(w io.Writer, traces []tempo.TraceSearchMetadata, format string) error {
	if len(traces) == 0 {
		return nil
	}
	switch format {
	case "json":
		return renderNDJSON(w, traces)
	default:
		return renderTable(w, traces)
	}
}

// TracesDetailed writes traces with resource and span attributes.
// details[i] corresponds to traces[i]; a nil entry means the fetch failed and is skipped gracefully.
func TracesDetailed(w io.Writer, traces []tempo.TraceSearchMetadata, details []*tempo.TraceDetail, format string) error {
	if len(traces) == 0 {
		return nil
	}
	switch format {
	case "json":
		return renderDetailedNDJSON(w, traces, details)
	default:
		return renderDetailedTable(w, traces, details)
	}
}

// --- table ---

func renderTable(w io.Writer, traces []tempo.TraceSearchMetadata) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TRACE ID\tSERVICE\tROOT SPAN\tDURATION\tSTARTED")
	for _, tr := range traces {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%dms\t%s\n",
			tr.TraceID,
			tr.RootServiceName,
			tr.RootTraceName,
			tr.DurationMs,
			fmtStartTime(tr.StartTimeUnixNs),
		)
	}
	return tw.Flush()
}

func renderDetailedTable(w io.Writer, traces []tempo.TraceSearchMetadata, details []*tempo.TraceDetail) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TRACE ID\tSERVICE\tROOT SPAN\tDURATION\tSTARTED")
	for i, tr := range traces {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%dms\t%s\n",
			tr.TraceID,
			tr.RootServiceName,
			tr.RootTraceName,
			tr.DurationMs,
			fmtStartTime(tr.StartTimeUnixNs),
		)
		if i < len(details) && details[i] != nil {
			writeAttributes(tw, details[i])
		}
	}
	return tw.Flush()
}

func writeAttributes(w io.Writer, d *tempo.TraceDetail) {
	if len(d.Batches) == 0 {
		return
	}

	// Resource attributes from the first batch.
	resAttrs := fmtOTLPAttrs(d.Batches[0].Resource.Attributes)
	if resAttrs != "" {
		fmt.Fprintf(w, "  resource\t%s\t\t\t\n", resAttrs)
	}

	// Collect all spans across all batches, print up to 5.
	var allSpans []tempo.OTLPSpan
	for _, batch := range d.Batches {
		for _, ss := range batch.ScopeSpans {
			allSpans = append(allSpans, ss.Spans...)
		}
	}
	limit := 5
	if len(allSpans) < limit {
		limit = len(allSpans)
	}
	for _, span := range allSpans[:limit] {
		attrs := fmtOTLPAttrs(span.Attributes)
		if attrs != "" {
			fmt.Fprintf(w, "  span\t[%s]\t%s\t\t\n", span.Name, attrs)
		} else {
			fmt.Fprintf(w, "  span\t[%s]\t\t\t\n", span.Name)
		}
	}
	if len(allSpans) > 5 {
		fmt.Fprintf(w, "  ...\t(%d more spans)\t\t\t\n", len(allSpans)-5)
	}
}

// --- NDJSON ---

func renderNDJSON(w io.Writer, traces []tempo.TraceSearchMetadata) error {
	enc := json.NewEncoder(w)
	for _, tr := range traces {
		if err := enc.Encode(tr); err != nil {
			return err
		}
	}
	return nil
}

func renderDetailedNDJSON(w io.Writer, traces []tempo.TraceSearchMetadata, details []*tempo.TraceDetail) error {
	enc := json.NewEncoder(w)
	for i, tr := range traces {
		obj := map[string]any{
			"traceID":         tr.TraceID,
			"rootServiceName": tr.RootServiceName,
			"rootTraceName":   tr.RootTraceName,
			"durationMs":      tr.DurationMs,
			"startTimeUnixNano": tr.StartTimeUnixNs,
		}
		if i < len(details) && details[i] != nil {
			obj["detail"] = details[i]
		}
		if err := enc.Encode(obj); err != nil {
			return err
		}
	}
	return nil
}

// --- helpers ---

func fmtOTLPAttrs(attrs []tempo.OTLPKeyValue) string {
	parts := make([]string, 0, len(attrs))
	for _, kv := range attrs {
		v := kv.Value.String()
		if v != "" {
			parts = append(parts, kv.Key+"="+v)
		}
	}
	return strings.Join(parts, "  ")
}

func fmtStartTime(nsStr string) string {
	n, err := strconv.ParseUint(nsStr, 10, 64)
	if err != nil {
		return "?"
	}
	return time.Unix(0, int64(n)).UTC().Format("15:04:05")
}
