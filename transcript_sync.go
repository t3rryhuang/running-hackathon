package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// A webhook can only ever deliver the calls that happened after it was created,
// and only the ones we did not turn away. The provider's own record of a
// conversation is the complete one, so the service reads it directly: at boot,
// and on a timer afterwards. Both paths write the same row, keyed on the
// conversation id, so whichever sees a call first records it and the other
// updates it.
const (
	syncInterval  = 10 * time.Minute
	syncPageSize  = 100
	syncMaxPages  = 20
	syncBackfill  = 90 * 24 * time.Hour
	syncCallLimit = 15 * time.Second
)

// ConversationLister is the provider's view of which calls exist. It is an
// interface so the sync can be driven from a stub in tests.
type ConversationLister interface {
	// Conversations lists conversation ids that started after `since`.
	Conversations(since time.Time) ([]string, error)
	// Conversation fetches one conversation in full.
	Conversation(id string) (elevenLabsConversation, error)
}

type elevenLabsAPI struct {
	apiKey  string
	agentID string
	base    string
	client  *http.Client
}

// NewConversationLister returns a reader of the provider's conversation history,
// or nil when there is no API key: the service still runs, it just cannot
// backfill.
func NewConversationLister(cfg Config) ConversationLister {
	if cfg.ElevenLabsAPIKey == "" {
		log.Printf("elevenlabs sync: no ELEVENLABS_API_KEY, transcripts will only arrive by webhook")
		return nil
	}
	base := strings.TrimSuffix(cfg.ElevenLabsAPIBase, "/")
	if base == "" {
		base = "https://api.elevenlabs.io"
	}
	return &elevenLabsAPI{
		apiKey:  cfg.ElevenLabsAPIKey,
		agentID: cfg.ElevenLabsAgentID,
		base:    base,
		client:  &http.Client{Timeout: syncCallLimit},
	}
}

func (a *elevenLabsAPI) get(path string, query url.Values, out any) error {
	u := a.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("xi-api-key", a.apiKey)
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		// The body of an error is the provider's explanation, not call
		// content, so it is safe and useful to keep.
		return fmt.Errorf("elevenlabs %s: status %d: %s", path, resp.StatusCode, truncate(string(body), 200))
	}
	return json.Unmarshal(body, out)
}

type conversationPage struct {
	Conversations []struct {
		ConversationID string `json:"conversation_id"`
		StartUnix      int64  `json:"start_time_unix_secs"`
	} `json:"conversations"`
	NextCursor string `json:"next_cursor"`
	HasMore    bool   `json:"has_more"`
}

func (a *elevenLabsAPI) Conversations(since time.Time) ([]string, error) {
	var ids []string
	cursor := ""
	for page := 0; page < syncMaxPages; page++ {
		q := url.Values{"page_size": {fmt.Sprint(syncPageSize)}}
		if a.agentID != "" {
			q.Set("agent_id", a.agentID)
		}
		if !since.IsZero() {
			q.Set("call_start_after_unix", fmt.Sprint(since.Unix()))
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var out conversationPage
		if err := a.get("/v1/convai/conversations", q, &out); err != nil {
			return ids, err
		}
		for _, c := range out.Conversations {
			if c.ConversationID != "" {
				ids = append(ids, c.ConversationID)
			}
		}
		if !out.HasMore || out.NextCursor == "" {
			break
		}
		cursor = out.NextCursor
	}
	return ids, nil
}

func (a *elevenLabsAPI) Conversation(id string) (elevenLabsConversation, error) {
	var out elevenLabsConversation
	err := a.get("/v1/convai/conversations/"+url.PathEscape(id), nil, &out)
	if out.ConversationID == "" {
		out.ConversationID = id
	}
	return out, err
}

// syncTranscripts pulls the provider's conversation history and files anything
// missing. It is safe to run repeatedly: an already-stored call is rewritten
// with the same content rather than duplicated.
func (s *Server) syncTranscripts(api ConversationLister, since time.Time) (stored int, err error) {
	if api == nil {
		return 0, nil
	}
	ids, err := api.Conversations(since)
	if err != nil {
		// A partial list is still worth filing: half the calls on the
		// dashboard beats none because page four timed out.
		log.Printf("elevenlabs sync: listing conversations: %v", err)
	}
	for _, id := range ids {
		conv, err := api.Conversation(id)
		if err != nil {
			log.Printf("elevenlabs sync: fetching %s: %v", id, err)
			continue
		}
		s.storeConversation(conv, transcriptFromSync)
		stored++
	}
	if stored > 0 {
		log.Printf("elevenlabs sync: reconciled %d conversation(s)", stored)
	}
	return stored, err
}

// RunTranscriptSync backfills at boot and keeps up on a timer, so a call is on
// the dashboard whether or not its webhook ever arrived.
func (s *Server) RunTranscriptSync(api ConversationLister, stop <-chan struct{}) {
	if api == nil {
		return
	}
	// The first pass reaches back over the retention window, so calls that
	// predate the webhook are picked up; later passes only need the recent
	// past, plus an overlap so nothing falls between two ticks.
	since := time.Now().Add(-syncBackfill)
	t := time.NewTicker(syncInterval)
	defer t.Stop()
	for {
		start := time.Now()
		if _, err := s.syncTranscripts(api, since); err != nil {
			metrics.RecordErr("transcripts.sync", err.Error())
		}
		metrics.Record("transcripts.sync", time.Since(start))
		since = start.Add(-2 * syncInterval)
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
