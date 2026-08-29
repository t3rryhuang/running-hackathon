package main

import (
	"strings"
	"testing"
	"time"
)

// Nothing is stored without consent, and nothing is invented when it is
// missing: the service says unknown and why.
func TestSignalsRequireConsentAndReportUnknown(t *testing.T) {
	store := testStore(t)
	u := mustUser(t, store, "+447700900301")

	if r := store.LatestSignal(u.ID, KindHeartRate, time.Now()); r.Known || !strings.Contains(r.Unknown, "consent") {
		t.Fatalf("unconsented signal should be unknown: %#v", r)
	}
	if _, err := store.IngestSignal(u.ID, Signal{
		Kind: KindHeartRate, Value: "61", Unit: "bpm", Source: "whoop", ObservedAt: time.Now(),
	}); err == nil {
		t.Fatal("ingest without consent should be refused")
	}

	if err := store.SetConsent(u.ID, KindHeartRate, true, "sms:yes"); err != nil {
		t.Fatalf("consent: %v", err)
	}
	if _, err := store.IngestSignal(u.ID, Signal{
		Kind: KindHeartRate, Value: "61", Unit: "bpm", ObservedAt: time.Now(),
	}); err == nil {
		t.Fatal("a signal with no source should be refused")
	}

	observed := time.Now().Add(-10 * time.Minute)
	if _, err := store.IngestSignal(u.ID, Signal{
		Kind: KindHeartRate, Value: "61", Unit: "bpm", Source: "whoop", ObservedAt: observed,
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	r := store.LatestSignal(u.ID, KindHeartRate, time.Now())
	if !r.Known || r.Value != "61" || r.Source != "whoop" {
		t.Fatalf("reading wrong: %#v", r)
	}
	if !strings.Contains(r.Describe(), "observed") {
		t.Errorf("reading must carry its source timestamp: %q", r.Describe())
	}

	// Past the TTL the same row reads as unknown rather than as a stale fact.
	if r := store.LatestSignal(u.ID, KindHeartRate, time.Now().Add(3*time.Hour)); r.Known || !strings.Contains(r.Unknown, "stale") {
		t.Fatalf("expired signal should read stale: %#v", r)
	}

	// Revoking consent hides the data again.
	if err := store.SetConsent(u.ID, KindHeartRate, false, "sms:stop"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if r := store.LatestSignal(u.ID, KindHeartRate, time.Now()); r.Known {
		t.Error("revoked consent still exposes the value")
	}
}

// Retention deletes sensitive observations even if nobody asks.
func TestRetentionPurgesOldSignals(t *testing.T) {
	store := testStore(t)
	u := mustUser(t, store, "+447700900302")
	if err := store.SetConsent(u.ID, KindLocation, true, "test"); err != nil {
		t.Fatalf("consent: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if _, err := store.IngestSignal(u.ID, Signal{
		Kind: KindLocation, Value: "London E2", Source: "phone", ObservedAt: old,
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	n, err := store.PurgeExpiredSignals(time.Now())
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 purged row, got %d", n)
	}
	if r := store.LatestSignal(u.ID, KindLocation, old.Add(time.Minute)); r.Known {
		t.Error("purged signal still readable")
	}
}

// Signals are per user: consent granted by one person does not expose another.
func TestSignalsAreTenantScoped(t *testing.T) {
	store := testStore(t)
	alice := mustUser(t, store, "+447700900303")
	bob := mustUser(t, store, "+447700900304")
	if err := store.SetConsent(alice.ID, KindLocation, true, "test"); err != nil {
		t.Fatalf("consent: %v", err)
	}
	if _, err := store.IngestSignal(alice.ID, Signal{
		Kind: KindLocation, Value: "London E2", Source: "phone", ObservedAt: time.Now(),
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if store.HasConsent(bob.ID, KindLocation) {
		t.Error("alice's consent applied to bob")
	}
	if r := store.LatestSignal(bob.ID, KindLocation, time.Now()); r.Known {
		t.Errorf("bob can read alice's location: %#v", r)
	}
}

// The ingestion endpoint enforces the same rules over HTTP.
func TestIngestEndpointRequiresConsentAndProvenance(t *testing.T) {
	srv, store, _, _ := newTestServer(t, nil)
	phone := "+447700900305"
	mustUser(t, store, phone)

	code, body := toolPostCode(t, srv, "/ingest", map[string]any{
		"phone": phone, "kind": KindHeartRate, "value": "58", "unit": "bpm",
		"source": "whoop", "observed_at": time.Now().Format(time.RFC3339),
	})
	if code != 403 {
		t.Fatalf("ingest without consent should be forbidden, got %d %s", code, body)
	}

	toolPost(t, srv, "/consent", map[string]any{"phone": phone, "scope": KindHeartRate, "granted": true}, nil)

	code, body = toolPostCode(t, srv, "/ingest", map[string]any{
		"phone": phone, "kind": KindHeartRate, "value": "58", "unit": "bpm", "source": "whoop",
	})
	if code != 400 {
		t.Fatalf("missing observed_at should be rejected, got %d %s", code, body)
	}

	payload := map[string]any{
		"phone": phone, "kind": KindHeartRate, "value": "58", "unit": "bpm",
		"source": "whoop", "observed_at": time.Now().Format(time.RFC3339),
		"idempotency_key": "whoop-1",
	}
	toolPost(t, srv, "/ingest", payload, nil)
	toolPost(t, srv, "/ingest", payload, nil) // retried delivery

	var out struct {
		Signals []struct {
			Kind           string `json:"kind"`
			Known          bool   `json:"known"`
			Value          string `json:"value"`
			ObservedAt     string `json:"observed_at"`
			UnknownBecause string `json:"unknown_because"`
		} `json:"signals"`
	}
	toolPost(t, srv, "/signals", map[string]any{"phone": phone}, &out)
	seen := map[string]bool{}
	for _, s := range out.Signals {
		seen[s.Kind] = true
		switch s.Kind {
		case KindHeartRate:
			if !s.Known || s.Value != "58" || s.ObservedAt == "" {
				t.Errorf("heart rate not reported with provenance: %#v", s)
			}
		default:
			if s.Known || s.UnknownBecause == "" {
				t.Errorf("%s should be an explicit unknown: %#v", s.Kind, s)
			}
		}
	}
	for _, kind := range []string{KindHeartRate, KindLocation, KindCalendar, KindCommitments} {
		if !seen[kind] {
			t.Errorf("no reading reported for %s", kind)
		}
	}

	u, _ := store.UserByPhone(phone)
	rows, err := store.query(`SELECT COUNT(*) FROM signals WHERE user_id=?`, u.ID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	defer rows.Close()
	rows.Next()
	var n int
	if err := rows.Scan(&n); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n != 1 {
		t.Errorf("retried ingest wrote %d rows, want 1", n)
	}
}
