package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
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
	return &elevenLabsVoice{
		apiKey:  cfg.ElevenLabsAPIKey,
		agentID: cfg.ElevenLabsAgentID,
		phoneID: cfg.ElevenLabsPhoneID,
		client:  &http.Client{Timeout: 20 * time.Second},
		baseURL: base + elevenLabsOutboundPath,
	}
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
	resp, err := v.client.Do(req)
	if err != nil {
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
