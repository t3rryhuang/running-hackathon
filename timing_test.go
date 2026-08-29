package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLatencyRecorderPercentiles(t *testing.T) {
	l := NewLatencyRecorder(100)
	for i := 1; i <= 100; i++ {
		l.Record("op", time.Duration(i)*time.Millisecond)
	}
	stats := l.Stats()
	if len(stats) != 1 || stats[0].Count != 100 {
		t.Fatalf("stats = %#v", stats)
	}
	if stats[0].P50Ms < 45 || stats[0].P50Ms > 55 {
		t.Errorf("p50 = %v", stats[0].P50Ms)
	}
	if stats[0].P95Ms < 90 || stats[0].MaxMs != 100 {
		t.Errorf("p95 = %v max = %v", stats[0].P95Ms, stats[0].MaxMs)
	}
}

func TestLatencyRecorderKeepsOnlyRecentSamples(t *testing.T) {
	l := NewLatencyRecorder(3)
	for i := 0; i < 10; i++ {
		l.Record("op", time.Millisecond)
	}
	if got := l.Stats()[0].Count; got != 3 {
		t.Fatalf("ring buffer should cap at 3, got %d", got)
	}
}

// The tool webhooks are the part of call latency we own, so /metrics must show
// their timings after they are hit.
func TestMetricsEndpointReportsToolLatency(t *testing.T) {
	srv, store, _, _ := newTestServer(t, nil)
	store.EnsureUser("+447700900160")

	req := httptest.NewRequest(http.MethodPost, "/tools/get_context", strings.NewReader(`{"phone":"+447700900160"}`))
	req.Header.Set("X-Webhook-Secret", "s3cret")
	srv.Routes().ServeHTTP(httptest.NewRecorder(), req)

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	var out struct {
		Ops []Stat `json:"ops"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	for _, s := range out.Ops {
		if s.Op == "tool.get_context" && s.Count > 0 {
			return
		}
	}
	t.Fatalf("tool.get_context missing from %s", rec.Body.String())
}

// A slow calendar feed must not hold a live call open: the first cold fetch is
// capped, and later calls are served from cache while a refresh runs behind.
func TestCalendarDoesNotBlockOnSlowFeed(t *testing.T) {
	release := make(chan struct{})
	var hits int
	var mu sync.Mutex
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		first := hits == 1
		mu.Unlock()
		if first {
			<-release
		}
		w.Write([]byte("BEGIN:VCALENDAR\nBEGIN:VEVENT\nSUMMARY:Standup\nDTSTART:20260901T090000Z\nEND:VEVENT\nEND:VCALENDAR\n"))
	}))
	defer feed.Close()
	defer close(release)

	cal := NewCalendar()
	cal.maxBlock = 100 * time.Millisecond

	start := time.Now()
	if got := cal.Today(feed.URL); got != nil {
		t.Fatalf("cold fetch should give up rather than wait, got %#v", got)
	}
	if waited := time.Since(start); waited > 500*time.Millisecond {
		t.Fatalf("cold fetch blocked for %s", waited)
	}
}

func TestCalendarServesStaleWhileRefreshing(t *testing.T) {
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("BEGIN:VCALENDAR\nBEGIN:VEVENT\nSUMMARY:Standup\nDTSTART;VALUE=DATE:" +
			time.Now().In(londonLoc).Format("20060102") + "\nEND:VEVENT\nEND:VCALENDAR\n"))
	}))
	defer feed.Close()

	cal := NewCalendar()
	cal.Warm(feed.URL)
	deadline := time.Now().Add(2 * time.Second)
	for len(cal.Today(feed.URL)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(cal.Today(feed.URL)) == 0 {
		t.Fatal("warmed calendar should be populated")
	}

	// Expire the entry: the stale copy still answers immediately.
	cal.mu.Lock()
	e := cal.cache[feed.URL]
	e.fetched = time.Now().Add(-time.Hour)
	cal.cache[feed.URL] = e
	cal.mu.Unlock()

	start := time.Now()
	if len(cal.Today(feed.URL)) == 0 {
		t.Fatal("stale entry should still be served")
	}
	if waited := time.Since(start); waited > 50*time.Millisecond {
		t.Fatalf("stale read waited %s", waited)
	}
}

// Timing must be attributable to a phase, not just a total.
func TestOutboundCallRecordsPhaseTimings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":true,"callSid":"CA1","conversation_id":"conv1"}`))
	}))
	defer srv.Close()

	v := &elevenLabsVoice{
		apiKey: "k", agentID: "a", phoneID: "p",
		client:  srv.Client(),
		baseURL: srv.URL + elevenLabsOutboundPath,
	}
	before := statCount("voice.outbound_call")
	if _, err := v.Call(CallRequest{To: "+447700900161", Name: "Keanu"}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if statCount("voice.outbound_call") != before+1 {
		t.Fatalf("outbound call latency was not recorded")
	}
}

func statCount(op string) int {
	for _, s := range metrics.Stats() {
		if s.Op == op {
			return s.Count
		}
	}
	return 0
}

func TestHostPortDefaultsToTLSPort(t *testing.T) {
	if got := hostPort("https://api.elevenlabs.io/v1/convai/twilio/outbound-call"); got != "api.elevenlabs.io:443" {
		t.Fatalf("got %q", got)
	}
	if got := hostPort("http://127.0.0.1:9000/x"); got != "127.0.0.1:9000" {
		t.Fatalf("got %q", got)
	}
}
