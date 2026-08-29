package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"time"
)

const elevenLabsOutboundPath = "/v1/convai/twilio/outbound-call"

// CallRequest is what the agent needs to open the conversation. Name and
// onboarding state travel as dynamic variables so the agent can greet people by
// name instead of spending the first turn asking who it is talking to.
type CallRequest struct {
	To        string
	Name      string
	Onboarded bool
}

// CallResult carries the identifiers ElevenLabs hands back, so the call can be
// hung up later (see Server.finishCall).
type CallResult struct {
	ConversationID string
	CallSID        string
}

// Voice places outbound agent calls. The interface keeps ElevenLabs out of the
// request path in tests and lets the service boot without a key.
type Voice interface {
	Call(req CallRequest) (CallResult, error)
}

type outboundCallRequest struct {
	AgentID            string             `json:"agent_id"`
	AgentPhoneNumberID string             `json:"agent_phone_number_id"`
	ToNumber           string             `json:"to_number"`
	ClientData         *clientInitialData `json:"conversation_initiation_client_data,omitempty"`
}

type clientInitialData struct {
	DynamicVariables map[string]string `json:"dynamic_variables"`
}

type outboundCallResponse struct {
	Success        bool   `json:"success"`
	Message        string `json:"message"`
	ConversationID string `json:"conversation_id"`
	CallSID        string `json:"callSid"`
}

type elevenLabsVoice struct {
	apiKey  string
	agentID string
	phoneID string
	client  *http.Client
	baseURL string
}

// NewVoice returns a live ElevenLabs caller when fully configured, and a
// logging stub otherwise so /call still answers during a dry-run demo.
func NewVoice(cfg Config) Voice {
	if cfg.ElevenLabsAPIKey == "" || cfg.ElevenLabsAgentID == "" || cfg.ElevenLabsPhoneID == "" {
		log.Printf("elevenlabs: not configured (need ELEVENLABS_API_KEY, ELEVENLABS_AGENT_ID, ELEVENLABS_PHONE_ID), outbound calls will be logged only")
		return logVoice{}
	}
	base := strings.TrimSuffix(cfg.ElevenLabsAPIBase, "/")
	if base == "" {
		base = "https://api.elevenlabs.io"
	}
	v := &elevenLabsVoice{
		apiKey:  cfg.ElevenLabsAPIKey,
		agentID: cfg.ElevenLabsAgentID,
		phoneID: cfg.ElevenLabsPhoneID,
		client: &http.Client{
			Timeout: 20 * time.Second,
			// Call setup is a single POST, so the handshake dominates it. Keep
			// the connection warm between calls and let HTTP/2 reuse it.
			Transport: &http.Transport{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     5 * time.Minute,
				ForceAttemptHTTP2:   true,
				DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout: 5 * time.Second,
			},
		},
		baseURL: base + elevenLabsOutboundPath,
	}
	go v.warm()
	return v
}

// warm opens the TLS connection to the provider at boot so the first real call
// does not pay DNS + TCP + TLS (~200-400ms from London to a US endpoint). It is
// a handshake only: no API request, no call, nothing billable.
func (v *elevenLabsVoice) warm() {
	host := hostPort(v.baseURL)
	start := time.Now()
	conn, err := tls.Dial("tcp", host, nil)
	if err != nil {
		metrics.RecordErr("voice.warm", err.Error())
		return
	}
	conn.Close()
	metrics.Record("voice.warm", time.Since(start))
	log.Printf("elevenlabs: warmed %s in %s", host, time.Since(start).Round(time.Millisecond))
}

// hostPort extracts host:443 from the configured base URL.
func hostPort(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "api.elevenlabs.io:443"
	}
	if u.Port() != "" {
		return u.Host
	}
	return u.Hostname() + ":443"
}

// newRequest builds the outbound-call HTTP request. Split out from Call so the
// body and headers can be asserted without a live key.
func (v *elevenLabsVoice) newRequest(cr CallRequest) (*http.Request, error) {
	payload := outboundCallRequest{
		AgentID:            v.agentID,
		AgentPhoneNumberID: v.phoneID,
		ToNumber:           cr.To,
		ClientData: &clientInitialData{DynamicVariables: map[string]string{
			"user_name":  cr.Name,
			"user_phone": cr.To,
			"onboarded":  boolWord(cr.Onboarded),
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, v.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("xi-api-key", v.apiKey)
	req.Header.Set("content-type", "application/json")
	return req, nil
}

func boolWord(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func (v *elevenLabsVoice) Call(cr CallRequest) (CallResult, error) {
	req, err := v.newRequest(cr)
	if err != nil {
		return CallResult{}, err
	}
	req, phases := traceRequest(req)
	start := time.Now()
	resp, err := v.client.Do(req)
	metrics.Record("voice.outbound_call", time.Since(start))
	log.Printf("elevenlabs: outbound-call timing %s", phases.summary(time.Since(start)))
	if err != nil {
		metrics.RecordErr("voice.outbound_call", err.Error())
		return CallResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return CallResult{}, fmt.Errorf("elevenlabs outbound-call: status %d: %s", resp.StatusCode, string(raw))
	}
	var out outboundCallResponse
	_ = json.Unmarshal(raw, &out)
	log.Printf("elevenlabs: outbound call queued to %s (call %s)", cr.To, out.CallSID)
	return CallResult{ConversationID: out.ConversationID, CallSID: out.CallSID}, nil
}

// logVoice stands in for ElevenLabs when the integration is unconfigured.
type logVoice struct{}

func (logVoice) Call(cr CallRequest) (CallResult, error) {
	log.Printf("[elevenlabs-stub] outbound agent call to %s (name %q, onboarded %v)", cr.To, cr.Name, cr.Onboarded)
	return CallResult{}, nil
}

// callPhases records where the time went inside one outbound HTTP request, so
// "the call takes ages to connect" can be attributed to DNS, TCP, TLS, the
// provider thinking, or our own code.
type callPhases struct {
	start      time.Time
	dns        time.Duration
	connect    time.Duration
	tls        time.Duration
	ttfb       time.Duration
	reusedConn bool
}

func traceRequest(req *http.Request) (*http.Request, *callPhases) {
	p := &callPhases{start: time.Now()}
	var dnsStart, connectStart, tlsStart time.Time
	trace := &httptrace.ClientTrace{
		GotConn:              func(i httptrace.GotConnInfo) { p.reusedConn = i.Reused },
		DNSStart:             func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:              func(httptrace.DNSDoneInfo) { p.dns = time.Since(dnsStart) },
		ConnectStart:         func(string, string) { connectStart = time.Now() },
		ConnectDone:          func(string, string, error) { p.connect = time.Since(connectStart) },
		TLSHandshakeStart:    func() { tlsStart = time.Now() },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { p.tls = time.Since(tlsStart) },
		GotFirstResponseByte: func() { p.ttfb = time.Since(p.start) },
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace)), p
}

func (p *callPhases) summary(total time.Duration) string {
	r := time.Millisecond
	metrics.Record("voice.outbound_call.ttfb", p.ttfb)
	return fmt.Sprintf("reused=%v dns=%s connect=%s tls=%s ttfb=%s total=%s",
		p.reusedConn, p.dns.Round(r), p.connect.Round(r), p.tls.Round(r), p.ttfb.Round(r), total.Round(r))
}
