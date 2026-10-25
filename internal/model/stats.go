package model

import "time"

// LatencySummary summarises a latency distribution. All values are
// milliseconds.
type LatencySummary struct {
	Count int64   `json:"count"`
	Min   float64 `json:"min"`
	Mean  float64 `json:"mean"`
	P50   float64 `json:"p50"`
	P90   float64 `json:"p90"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	P999  float64 `json:"p999"`
	Max   float64 `json:"max"`
}

// EndpointSummary is live or final traffic for one endpoint.
type EndpointSummary struct {
	Endpoint string         `json:"endpoint"`
	Count    int64          `json:"count"`
	Errors   int64          `json:"errors"`
	Latency  LatencySummary `json:"latency"`
}

// SeriesPoint is one second of traffic for a collector (a service or a replay
// job). T is the start of the second in Unix milliseconds.
type SeriesPoint struct {
	T        int64   `json:"t"`
	Requests int64   `json:"requests"` // completed in this second (= req/s)
	Errors   int64   `json:"errors"`
	P50      float64 `json:"p50"` // ms
	P95      float64 `json:"p95"`
	P99      float64 `json:"p99"`
	Max      float64 `json:"max"`
	InFlight int64   `json:"in_flight"` // at the end of the second
	BytesIn  int64   `json:"bytes_in"`
	BytesOut int64   `json:"bytes_out"`
}

// CollectorKind distinguishes metric collectors.
type CollectorKind string

const (
	KindService CollectorKind = "service"
	KindReplay  CollectorKind = "replay"
)

// StatsSnapshot is the live state of one metrics collector.
type StatsSnapshot struct {
	Name     string        `json:"name"` // "service:<id>" or "replay:<id>"
	Kind     CollectorKind `json:"kind"`
	ID       string        `json:"id"` // service or replay job ID
	Since    time.Time     `json:"since"`
	Requests int64         `json:"requests"` // cumulative
	Errors   int64         `json:"errors"`
	BytesIn  int64         `json:"bytes_in"`  // request bytes received from clients / sent by replay
	BytesOut int64         `json:"bytes_out"` // response bytes sent to clients / received by replay
	InFlight int64         `json:"in_flight"`
	// StatusClasses maps "1xx".."5xx" and "err" (no response) to counts.
	StatusClasses map[string]int64 `json:"status_classes"`
	// RPS is the rate over the last complete second; RPS10 over the last 10s.
	RPS   float64 `json:"rps"`
	RPS10 float64 `json:"rps10"`
	// ErrorRate10 is errors / requests over the last 10 seconds.
	ErrorRate10 float64 `json:"error_rate10"`
	// Latency is cumulative since Since; Recent covers the last 10 seconds.
	Latency LatencySummary `json:"latency"`
	Recent  LatencySummary `json:"recent"`
	// Endpoints are the busiest endpoints (cumulative), most requests first.
	Endpoints []EndpointSummary `json:"endpoints,omitempty"`
}
