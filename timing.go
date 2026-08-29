package main

import (
	"log"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Latency instrumentation. The voice path is a chain of hops we do not all
// control (caller -> Twilio -> ElevenLabs -> our webhooks -> LLM), so every hop
// we DO control is measured and exposed at /metrics. Numbers, not guesses.

// Sample is one timed operation.
type Sample struct {
	Op    string
	Dur   time.Duration
	At    time.Time
	Extra string
}

// LatencyRecorder keeps the last N samples per operation in memory. No external
// metrics dependency: this has to run on a Raspberry Pi with no scrape target.
type LatencyRecorder struct {
	mu      sync.Mutex
	perOp   map[string][]time.Duration
	keep    int
	lastErr map[string]string
}

func NewLatencyRecorder(keep int) *LatencyRecorder {
	return &LatencyRecorder{perOp: map[string][]time.Duration{}, lastErr: map[string]string{}, keep: keep}
}

// metrics is process-wide so the voice client, the calendar and the HTTP
// handlers can all record into one place without threading it through.
var metrics = NewLatencyRecorder(200)

func (l *LatencyRecorder) Record(op string, d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := append(l.perOp[op], d)
	if len(s) > l.keep {
		s = s[len(s)-l.keep:]
	}
	l.perOp[op] = s
}

func (l *LatencyRecorder) RecordErr(op, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastErr[op] = msg
}

// Stat is the summary of one operation, in milliseconds.
type Stat struct {
	Op      string  `json:"op"`
	Count   int     `json:"count"`
	P50Ms   float64 `json:"p50_ms"`
	P95Ms   float64 `json:"p95_ms"`
	MaxMs   float64 `json:"max_ms"`
	LastErr string  `json:"last_error,omitempty"`
}

func (l *LatencyRecorder) Stats() []Stat {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Stat
	for op, samples := range l.perOp {
		sorted := append([]time.Duration(nil), samples...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		out = append(out, Stat{
			Op:      op,
			Count:   len(sorted),
			P50Ms:   ms(percentile(sorted, 0.50)),
			P95Ms:   ms(percentile(sorted, 0.95)),
			MaxMs:   ms(sorted[len(sorted)-1]),
			LastErr: l.lastErr[op],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Op < out[j].Op })
	return out
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

func ms(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

// track times an operation and records it. Use with defer:
//
//	defer track("tool.get_context")()
func track(op string) func() {
	start := time.Now()
	return func() { metrics.Record(op, time.Since(start)) }
}

// timed wraps a handler so every request to it is measured. The voice agent's
// tool webhooks sit in the middle of a live phone conversation, so their server
// time is the part of the silence we are responsible for.
func timed(op string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next(w, r)
		d := time.Since(start)
		metrics.Record(op, d)
		// Anything over a beat of silence on a live call is worth a log line.
		if d > 400*time.Millisecond {
			log.Printf("slow: %s took %s", op, d.Round(time.Millisecond))
		}
	}
}
