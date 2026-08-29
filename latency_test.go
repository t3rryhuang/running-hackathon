package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// budget is deliberately generous: these guard against a regression that puts
// something slow (a provider call, an unbounded scan) back on a path the caller
// hears as silence, not against millisecond drift on a busy CI box.
const toolBudget = 150 * time.Millisecond

func timedToolPost(t *testing.T, srv *Server, path string, body map[string]any) (int, time.Duration) {
	t.Helper()
	start := time.Now()
	code, _ := toolPostCode(t, srv, path, body)
	return code, time.Since(start)
}

// TestToolWebhooksStayWithinTheirLatencyBudget pins the part of the silence on
// a live call that this service is responsible for.
func TestToolWebhooksStayWithinTheirLatencyBudget(t *testing.T) {
	srv, store, _, _ := newTestServer(t, &fakeAnthropic{})
	u := settleChecklist(t, store, "+447700900201")
	if _, err := store.SaveOnboarding(u, "Keanu", "hackathons", "daily"); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	for _, path := range []string{"/tools/get_context", "/tools/next_question", "/tools/suggest_event"} {
		var worst time.Duration
		for i := 0; i < 25; i++ {
			code, d := timedToolPost(t, srv, path, map[string]any{"phone": u.Phone})
			if d > worst {
				worst = d
			}
			if code >= 500 {
				t.Fatalf("%s: status %d", path, code)
			}
		}
		if worst > toolBudget {
			t.Errorf("%s worst case %s exceeds the %s budget", path, worst.Round(time.Millisecond), toolBudget)
		}
	}
}

// TestTranscriptWebhookAnswersBeforeItStores is the reason the handler defers
// its work: the provider retries slow deliveries, so the response must not wait
// on the database.
func TestTranscriptWebhookAnswersBeforeItStores(t *testing.T) {
	srv, store := webhookServer(t)
	u, _ := store.EnsureUser("+447700900202")
	body := transcriptPayload("conv_fast", u.Phone)

	var worst time.Duration
	for i := 0; i < 10; i++ {
		start := time.Now()
		rec := postTranscript(t, srv, "whsec_test", strings.Replace(body, "conv_fast", fmt.Sprintf("conv_fast_%d", i), 1), time.Now())
		if d := time.Since(start); d > worst {
			worst = d
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("delivery %d: %d", i, rec.Code)
		}
	}
	if worst > toolBudget {
		t.Errorf("transcript delivery worst case %s exceeds the %s budget", worst.Round(time.Millisecond), toolBudget)
	}
}

// TestCallCarriesTheOpeningLineSoTheAgentNeedNotAskForIt keeps the first-audio
// path free of a round trip: the greeting travels with the dial, so the agent
// speaks from its dynamic variables rather than calling get_context first.
func TestCallCarriesTheOpeningLineSoTheAgentNeedNotAskForIt(t *testing.T) {
	srv, store, _, voice := newTestServer(t, &fakeAnthropic{})
	u := settleChecklist(t, store, "+447700900203")
	if _, err := store.SaveOnboarding(u, "Keanu", "hackathons", "daily"); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	if err := store.MarkPhoneVerified(u.ID, "sms"); err != nil {
		t.Fatalf("verify phone: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/call", strings.NewReader(url.Values{"phone": {u.Phone}}.Encode()))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("call: %d %s", rec.Code, rec.Body.String())
	}
	if len(voice.calls) != 1 {
		t.Fatalf("want one dial, got %d", len(voice.calls))
	}
	vars := callVariables(voice.calls[0])
	for _, key := range []string{"greeting", "user_name", "caller_known", "phone_verified"} {
		if strings.TrimSpace(vars[key]) == "" {
			t.Errorf("dial is missing %s, so the agent has to ask for it before it can speak", key)
		}
	}
	if !strings.Contains(vars["greeting"], "Keanu") {
		t.Errorf("greeting does not use the stored name: %q", vars["greeting"])
	}
}

// TestMetricsCoverEveryPathTheCallerWaitsOn keeps the instrumentation honest:
// if a new hop lands on the conversational path it should show up here.
func TestMetricsCoverEveryPathTheCallerWaitsOn(t *testing.T) {
	srv, store, _, _ := newTestServer(t, &fakeAnthropic{})
	u := settleChecklist(t, store, "+447700900204")
	toolPost(t, srv, "/tools/get_context", map[string]any{"phone": u.Phone}, nil)
	toolPost(t, srv, "/tools/next_question", map[string]any{"phone": u.Phone}, nil)
	postTranscript(t, srv, "whsec_test", transcriptPayload("conv_metrics", u.Phone), time.Now())

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics: %d", rec.Code)
	}
	var body struct {
		Ops []Stat `json:"ops"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("metrics json: %v (%s)", err, rec.Body.String())
	}
	seen := map[string]bool{}
	for _, s := range body.Ops {
		seen[s.Op] = true
	}
	for _, op := range []string{"tool.get_context", "tool.next_question", "http.transcript"} {
		if !seen[op] {
			t.Errorf("%s is not measured (have %v)", op, seen)
		}
	}
}
