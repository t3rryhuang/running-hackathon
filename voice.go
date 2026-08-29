package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

const elevenLabsOutboundURL = "https://api.elevenlabs.io/v1/convai/twilio/outbound-call"

// Voice places outbound agent calls. The interface keeps ElevenLabs out of the
// request path in tests and lets the service boot without a key.
type Voice interface {
	Call(to string) error
}

type outboundCallRequest struct {
	AgentID            string `json:"agent_id"`
	AgentPhoneNumberID string `json:"agent_phone_number_id"`
	ToNumber           string `json:"to_number"`
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
	return &elevenLabsVoice{
		apiKey:  cfg.ElevenLabsAPIKey,
		agentID: cfg.ElevenLabsAgentID,
		phoneID: cfg.ElevenLabsPhoneID,
		client:  &http.Client{Timeout: 20 * time.Second},
		baseURL: elevenLabsOutboundURL,
	}
}

// newRequest builds the outbound-call HTTP request. Split out from Call so the
// body and headers can be asserted without a live key.
func (v *elevenLabsVoice) newRequest(to string) (*http.Request, error) {
	body, err := json.Marshal(outboundCallRequest{
		AgentID:            v.agentID,
		AgentPhoneNumberID: v.phoneID,
		ToNumber:           to,
	})
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

func (v *elevenLabsVoice) Call(to string) error {
	req, err := v.newRequest(to)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("elevenlabs outbound-call: status %d: %s", resp.StatusCode, string(raw))
	}
	log.Printf("elevenlabs: outbound call queued to %s", to)
	return nil
}

// logVoice stands in for ElevenLabs when the integration is unconfigured.
type logVoice struct{}

func (logVoice) Call(to string) error {
	log.Printf("[elevenlabs-stub] outbound agent call to %s", to)
	return nil
}
