package main

import (
	"log"
	"os"
)

// Config holds all runtime configuration, sourced entirely from environment variables.
type Config struct {
	TwilioAccountSID  string
	TwilioAuthToken   string
	TwilioNumber      string
	TwilioEdge        string
	TwilioRegion      string
	ElevenLabsAPIKey  string
	ElevenLabsAPIBase string
	ElevenLabsAgentID string
	ElevenLabsPhoneID string
	AnthropicAPIKey   string
	AnthropicModel    string
	TavilyAPIKey      string
	GCalICSURL        string
	EventsFeedURL     string
	ToolWebhookSecret string
	DatabasePath      string
	Port              string
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// LoadConfig reads config from the environment. Missing required values are logged
// loudly but never fatal: the service degrades instead of refusing to boot, which
// keeps the demo alive when a single provider is unconfigured.
func LoadConfig() Config {
	c := Config{
		TwilioAccountSID:  os.Getenv("TWILIO_ACCOUNT_SID"),
		TwilioAuthToken:   os.Getenv("TWILIO_AUTH_TOKEN"),
		TwilioNumber:      os.Getenv("TWILIO_NUMBER"),
		TwilioEdge:        os.Getenv("TWILIO_EDGE"),
		EventsFeedURL:     os.Getenv("EVENTS_FEED_URL"),
		TwilioRegion:      os.Getenv("TWILIO_REGION"),
		ElevenLabsAPIKey:  os.Getenv("ELEVENLABS_API_KEY"),
		ElevenLabsAPIBase: env("ELEVENLABS_API_BASE", "https://api.elevenlabs.io"),
		ElevenLabsAgentID: os.Getenv("ELEVENLABS_AGENT_ID"),
		ElevenLabsPhoneID: os.Getenv("ELEVENLABS_PHONE_ID"),
		AnthropicAPIKey:   os.Getenv("ANTHROPIC_API_KEY"),
		AnthropicModel:    env("ANTHROPIC_MODEL", "claude-sonnet-5"),
		TavilyAPIKey:      os.Getenv("TAVILY_API_KEY"),
		GCalICSURL:        os.Getenv("GCAL_ICS_URL"),
		ToolWebhookSecret: os.Getenv("TOOL_WEBHOOK_SECRET"),
		DatabasePath:      env("DATABASE_PATH", "./data.db"),
		Port:              env("PORT", "8090"),
	}

	required := map[string]string{
		"TWILIO_ACCOUNT_SID":  c.TwilioAccountSID,
		"TWILIO_AUTH_TOKEN":   c.TwilioAuthToken,
		"TWILIO_NUMBER":       c.TwilioNumber,
		"ELEVENLABS_API_KEY":  c.ElevenLabsAPIKey,
		"ELEVENLABS_AGENT_ID": c.ElevenLabsAgentID,
		"ELEVENLABS_PHONE_ID": c.ElevenLabsPhoneID,
		"ANTHROPIC_API_KEY":   c.AnthropicAPIKey,
		"TOOL_WEBHOOK_SECRET": c.ToolWebhookSecret,
	}
	for k, v := range required {
		if v == "" {
			log.Printf("config: MISSING required env %s - related features are disabled", k)
		}
	}
	for _, k := range []string{"GCAL_ICS_URL", "TAVILY_API_KEY"} {
		if os.Getenv(k) == "" {
			log.Printf("config: optional env %s not set", k)
		}
	}
	return c
}
