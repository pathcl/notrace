package tempo

import "fmt"

// SearchQuery holds parameters for a Tempo trace search.
type SearchQuery struct {
	Query string
	Start int64 // Unix seconds
	End   int64 // Unix seconds
	Limit int
}

type SearchResponse struct {
	Traces  []TraceSearchMetadata `json:"traces"`
	Metrics SearchMetrics         `json:"metrics"`
}

type TraceSearchMetadata struct {
	TraceID         string    `json:"traceID"`
	RootServiceName string    `json:"rootServiceName"`
	RootTraceName   string    `json:"rootTraceName"`
	StartTimeUnixNs string    `json:"startTimeUnixNano"`
	DurationMs      uint32    `json:"durationMs"`
	SpanSets        []SpanSet `json:"spanSets,omitempty"`
}

type SpanSet struct {
	Spans   []Span `json:"spans"`
	Matched uint32 `json:"matched"`
}

type Span struct {
	SpanID          string     `json:"spanID"`
	StartTimeUnixNs string     `json:"startTimeUnixNano"`
	DurationNanos   string     `json:"durationNanos"`
	Attributes      []KeyValue `json:"attributes"`
}

type KeyValue struct {
	Key   string `json:"key"`
	Value Value  `json:"value"`
}

type Value struct {
	StringValue string `json:"stringValue,omitempty"`
	IntValue    string `json:"intValue,omitempty"`
	BoolValue   bool   `json:"boolValue,omitempty"`
	DoubleValue string `json:"doubleValue,omitempty"`
}

func (v Value) String() string {
	switch {
	case v.StringValue != "":
		return v.StringValue
	case v.IntValue != "":
		return v.IntValue
	case v.DoubleValue != "":
		return v.DoubleValue
	case v.BoolValue:
		return "true"
	default:
		return ""
	}
}

// SearchMetrics holds informational counters from Tempo's search response.
// inspectedBytes is encoded as a quoted string by Tempo's protobuf-JSON layer
// (uint64 → string to preserve precision in JavaScript), so we accept it as-is.
type SearchMetrics struct {
	InspectedTraces uint32 `json:"inspectedTraces"`
	InspectedBytes  string `json:"inspectedBytes"`
}

// --- Full trace detail (GET /api/traces/{traceID}) ---
// Follows the OTLP JSON encoding: uint64 fields are quoted strings,
// binary IDs are base64-encoded.

type TraceDetail struct {
	Batches []ResourceSpans `json:"batches"`
}

type ResourceSpans struct {
	Resource   OTLPResource `json:"resource"`
	ScopeSpans []ScopeSpans `json:"scopeSpans"`
}

type OTLPResource struct {
	Attributes []OTLPKeyValue `json:"attributes"`
}

type ScopeSpans struct {
	Scope InstrumentationScope `json:"scope"`
	Spans []OTLPSpan           `json:"spans"`
}

type InstrumentationScope struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type OTLPSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId"`
	Name              string         `json:"name"`
	Kind              string         `json:"kind"` // proto3 JSON encodes SpanKind enum as string
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []OTLPKeyValue `json:"attributes"`
	Status            OTLPStatus     `json:"status"`
}

type OTLPKeyValue struct {
	Key   string    `json:"key"`
	Value OTLPValue `json:"value"`
}

func (kv OTLPKeyValue) String() string {
	return fmt.Sprintf("%s=%s", kv.Key, kv.Value.String())
}

type OTLPValue struct {
	StringValue string  `json:"stringValue,omitempty"`
	IntValue    string  `json:"intValue,omitempty"`
	BoolValue   bool    `json:"boolValue,omitempty"`
	DoubleValue float64 `json:"doubleValue,omitempty"`
}

func (v OTLPValue) String() string {
	switch {
	case v.StringValue != "":
		return v.StringValue
	case v.IntValue != "":
		return v.IntValue
	case v.BoolValue:
		return "true"
	case v.DoubleValue != 0:
		return fmt.Sprintf("%g", v.DoubleValue)
	default:
		return ""
	}
}

type OTLPStatus struct {
	Code    string `json:"code,omitempty"` // proto3 JSON encodes StatusCode enum as string
	Message string `json:"message,omitempty"`
}
