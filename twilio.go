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

// Telephony is the seam for outbound SMS so tests (and a Twilio-less dev box)
// never hit the network. Voice calls go through ElevenLabs, not Twilio's REST
// API - see voice.go.
type Telephony interface {
	SendSMS(to, body string) error
	// HangUp completes an in-progress call by SID. Used as a backstop when the
	// voice agent finishes onboarding but does not hang up itself.
	HangUp(callSID string) error
}

// twilioAPIBase resolves the REST host for a processing region. Twilio routes
// every request through its nearest edge automatically; the hostname only
// decides where the request is *processed*, and non-US regions need the edge
// label in the name (api.dublin.ie1.twilio.com). An unset region keeps the
// global host, because IE1 processing needs IE1-scoped credentials.
func twilioAPIBase(edge, region string) string {
	region = strings.ToLower(strings.TrimSpace(region))
	edge = strings.ToLower(strings.TrimSpace(edge))
	if region == "" || region == "us1" {
		return "https://api.twilio.com"
	}
	if edge == "" {
		edge = defaultEdges[region]
	}
	if edge == "" {
		log.Printf("twilio: TWILIO_REGION=%s needs TWILIO_EDGE too, falling back to the global API host", region)
		return "https://api.twilio.com"
	}
	return fmt.Sprintf("https://api.%s.%s.twilio.com", edge, region)
}

var defaultEdges = map[string]string{"ie1": "dublin", "au1": "sydney"}

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
		base:       twilioAPIBase(cfg.TwilioEdge, cfg.TwilioRegion),
	}
}

func (t *twilioClient) SendSMS(to, body string) error {
	form := url.Values{"To": {to}, "From": {t.from}, "Body": {body}}
	return t.post("Messages.json", form)
}

func (t *twilioClient) HangUp(callSID string) error {
	if callSID == "" {
		return nil
	}
	return t.post("Calls/"+callSID+".json", url.Values{"Status": {"completed"}})
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

func (logTelephony) HangUp(callSID string) error {
	log.Printf("[twilio-stub] hang up call %s", callSID)
	return nil
}
