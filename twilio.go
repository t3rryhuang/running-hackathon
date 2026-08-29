package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TODO(operator): point this at the ElevenLabs-provided TwiML / inbound URL for
// the CheckIn agent once the phone integration is wired up.
const elevenLabsTwiMLURL = "https://api.us.elevenlabs.io/twilio/inbound_call"

// Telephony is the seam for outbound SMS and calls so tests (and a Twilio-less
// dev box) never hit the network.
type Telephony interface {
	SendSMS(to, body string) error
	StartCall(to string) error
}

type twilioClient struct {
	accountSID string
	authToken  string
	from       string
	client     *http.Client
	base       string
}

func NewTelephony(cfg Config) Telephony {
	if cfg.TwilioAccountSID == "" || cfg.TwilioAuthToken == "" || cfg.TwilioNumber == "" {
		log.Printf("twilio: not configured, outbound SMS/calls will be logged only")
		return logTelephony{}
	}
	return &twilioClient{
		accountSID: cfg.TwilioAccountSID,
		authToken:  cfg.TwilioAuthToken,
		from:       cfg.TwilioNumber,
		client:     &http.Client{Timeout: 15 * time.Second},
		base:       "https://api.twilio.com",
	}
}

func (t *twilioClient) SendSMS(to, body string) error {
	form := url.Values{"To": {to}, "From": {t.from}, "Body": {body}}
	return t.post("Messages.json", form)
}

func (t *twilioClient) StartCall(to string) error {
	form := url.Values{"To": {to}, "From": {t.from}, "Url": {elevenLabsTwiMLURL}, "Method": {"POST"}}
	return t.post("Calls.json", form)
}

func (t *twilioClient) post(resource string, form url.Values) error {
	endpoint := fmt.Sprintf("%s/2010-04-01/Accounts/%s/%s", t.base, t.accountSID, resource)
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.SetBasicAuth(t.accountSID, t.authToken)
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("twilio %s: status %d: %s", resource, resp.StatusCode, string(raw))
	}
	return nil
}

// logTelephony stands in for Twilio when credentials are absent.
type logTelephony struct{}

func (logTelephony) SendSMS(to, body string) error {
	log.Printf("[twilio-stub] SMS to %s: %s", to, body)
	return nil
}

func (logTelephony) StartCall(to string) error {
	log.Printf("[twilio-stub] call to %s via %s", to, elevenLabsTwiMLURL)
	return nil
}
