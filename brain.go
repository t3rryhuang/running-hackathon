package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// noMatchLine is what the agent should say when nothing clears the relevance
// bar: honest, and it asks for the detail that would fix it.
func noMatchLine(u *User) string {
	if strings.TrimSpace(u.Interests) == "" {
		return "I haven't got anything worth recommending right now - what kind of thing would you actually want to go to?"
	}
	return "Nothing in my list is a good fit for you at the moment, so I'd rather not push something random. Want me to widen it beyond " + u.Interests + "?"
}

const systemPrompt = `You are CheckIn, a warm, grounded journalling companion that talks to people over SMS.

Style rules (hard):
- Reply in at most 2-3 short sentences. This is a text message, not an essay.
- Ask exactly ONE question per reply.
- Sound like a thoughtful friend, never like a therapist bot or a survey. No bullet points, no emoji spam.

What you do:
- Help the person reflect on their day. Reference their past check-ins when it is genuinely relevant ("last week you said the deploy was stressing you out - how did that land?").
- Reference today's calendar items by name and time when they are in context ("you had the design review at 3, how did it go?").
- When you have learned something real about their day, call save_checkin with a 1-2 sentence summary, comma-separated topics, and a mood from 1 (awful) to 5 (great). Call it at most once per conversation turn.
- If they sound low (mood 1-2), or their calendar is empty and they seem at a loose end, call suggest_event ONCE to fetch a real London tech event, then offer it in a single friendly sentence and ask if you should put their name down. Never offer more than one event at a time, and never re-offer if a suggestion is already open.
- If there is an open suggestion and they say yes / go on / sign me up, call accept_suggestion and confirm warmly that they are on the list.
- If they decline, drop it gracefully and do not bring it up again this conversation.
- If suggest_event comes back with no_match, say so honestly in your own words using the "say" line as a guide, and ask what they would actually turn up to. Never fall back to an event you were not given.

Never invent events - only mention events returned by suggest_event. Do not guess anything about the person that they have not told you: no assumptions about gender, background or eligibility.`

var brainTools = []ToolDef{
	{
		Name:        "save_checkin",
		Description: "Record the user's journal check-in for today. Call when you have learned something concrete about how their day went.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"mood":    map[string]any{"type": "integer", "description": "Mood 1 (awful) to 5 (great)", "minimum": 1, "maximum": 5},
				"summary": map[string]any{"type": "string", "description": "1-2 sentence summary of the check-in"},
				"topics":  map[string]any{"type": "string", "description": "Comma-separated topics, e.g. 'work, sleep, running'"},
			},
			"required": []string{"summary"},
		},
	},
	{
		Name:        "suggest_event",
		Description: "Fetch one upcoming London tech event to offer the user. Use when they seem low or their calendar is empty.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		Name:        "accept_suggestion",
		Description: "Sign the user up for the event that is currently on offer. Use when they accept.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	},
}

// Brain turns an inbound message into a reply, running the Anthropic tool-use
// loop against the store.
type Brain struct {
	store  *Store
	cal    *Calendar
	client AnthropicClient
	model  string
	gcal   string
}

func NewBrain(store *Store, cal *Calendar, client AnthropicClient, model, gcal string) *Brain {
	return &Brain{store: store, cal: cal, client: client, model: model, gcal: gcal}
}

// CalendarFor returns today's events for the user, preferring their own ICS url
// and falling back to the service-wide GCAL_ICS_URL.
func (b *Brain) CalendarFor(u *User) []CalendarEvent {
	url := u.ICSURL
	if url == "" {
		url = b.gcal
	}
	return b.cal.Today(url)
}

// WarmCalendar pre-fetches the user's feed off the critical path, so the first
// tool call of a live conversation does not wait on an ICS round trip.
func (b *Brain) WarmCalendar(u *User) {
	url := u.ICSURL
	if url == "" {
		url = b.gcal
	}
	b.cal.Warm(url)
}

// contextBlock renders memory + calendar + suggestion state as a compact
// preamble the model reads before the live conversation.
func (b *Brain) contextBlock(u *User) string {
	var sb strings.Builder
	name := u.Name
	if name == "" {
		name = "(unknown)"
	}
	interests := u.Interests
	if interests == "" {
		interests = "(not stated yet)"
	}
	fmt.Fprintf(&sb, "<user>\nname: %s\nphone: %s\ninterests: %s\nlocal_time: %s\n</user>\n",
		name, u.Phone, interests, time.Now().In(londonLoc).Format("Mon 2 Jan 15:04"))

	sb.WriteString("<recent_checkins>\n")
	checkins, err := b.store.RecentCheckins(u.ID, 10)
	if err != nil || len(checkins) == 0 {
		sb.WriteString("(none yet - this is their first conversation)\n")
	}
	for _, c := range checkins {
		mood := "?"
		if c.Mood != nil {
			mood = fmt.Sprint(*c.Mood)
		}
		fmt.Fprintf(&sb, "- %s | mood %s | %s | topics: %s\n",
			c.CreatedAt.In(londonLoc).Format("Mon 2 Jan"), mood, c.Summary, c.Topics)
	}
	sb.WriteString("</recent_checkins>\n")

	sb.WriteString("<todays_calendar>\n")
	events := b.CalendarFor(u)
	if len(events) == 0 {
		sb.WriteString("(empty - no calendar events today)\n")
	}
	for _, e := range events {
		fmt.Fprintf(&sb, "- %s %s\n", e.When, e.Summary)
	}
	sb.WriteString("</todays_calendar>\n")

	sb.WriteString("<open_suggestion>\n")
	if sg, err := b.store.OpenSuggestion(u.ID); err == nil {
		fmt.Fprintf(&sb, "%s on %s - %s (awaiting their yes/no)\n",
			sg.Event.Title, sg.Event.StartsAt.In(londonLoc).Format("Mon 2 Jan 15:04"), sg.Event.URL)
	} else {
		sb.WriteString("(none)\n")
	}
	sb.WriteString("</open_suggestion>")
	return sb.String()
}

// Reply runs the conversation turn and returns the SMS body to send back. It
// never returns an error: on any failure it degrades to a canned reply so the
// webhook still answers with valid TwiML.
func (b *Brain) Reply(ctx context.Context, u *User, inbound string) string {
	_ = b.store.AddMessage(u.ID, "user", inbound)
	reply := b.reply(ctx, u, inbound)
	_ = b.store.AddMessage(u.ID, "assistant", reply)
	return reply
}

const fallbackReply = "Thanks for checking in - I've noted that down. My brain's offline for a moment, but tell me: what's the one thing that stood out about today?"

func (b *Brain) reply(ctx context.Context, u *User, inbound string) string {
	if b.client == nil {
		return fallbackReply
	}

	history, _ := b.store.RecentMessages(u.ID, 20)
	msgs := []AnthropicMessage{userText(b.contextBlock(u))}
	msgs = append(msgs, assistantText("Got it - I have their history and today's calendar in mind."))
	for _, m := range history {
		if m.Role == "assistant" {
			msgs = append(msgs, assistantText(m.Body))
		} else {
			msgs = append(msgs, userText(m.Body))
		}
	}

	// Bounded tool loop: model may call save_checkin, suggest_event and
	// accept_suggestion before producing its text reply.
	for turn := 0; turn < 5; turn++ {
		resp, err := b.client.CreateMessage(ctx, AnthropicRequest{
			Model:     b.model,
			MaxTokens: 512,
			System:    systemPrompt,
			Messages:  msgs,
			Tools:     brainTools,
		})
		if err != nil {
			log.Printf("brain: anthropic call failed: %v", err)
			return fallbackReply
		}

		var toolResults []ContentBlock
		var text strings.Builder
		for _, block := range resp.Content {
			switch block.Type {
			case "text":
				text.WriteString(block.Text)
			case "tool_use":
				result := b.runTool(u, block.Name, block.Input)
				toolResults = append(toolResults, ContentBlock{
					Type:      "tool_result",
					ToolUseID: block.ID,
					Content:   result,
				})
			}
		}
		if len(toolResults) == 0 {
			out := strings.TrimSpace(text.String())
			if out == "" {
				return fallbackReply
			}
			return out
		}
		msgs = append(msgs, AnthropicMessage{Role: "assistant", Content: resp.Content})
		msgs = append(msgs, AnthropicMessage{Role: "user", Content: toolResults})
	}
	return fallbackReply
}

func (b *Brain) runTool(u *User, name string, input json.RawMessage) string {
	switch name {
	case "save_checkin":
		var args struct {
			Mood    *int   `json:"mood"`
			Summary string `json:"summary"`
			Topics  string `json:"topics"`
		}
		_ = json.Unmarshal(input, &args)
		if err := b.SaveCheckin(u, args.Mood, args.Summary, args.Topics, string(input)); err != nil {
			return jsonStr(map[string]any{"ok": false, "error": err.Error()})
		}
		return jsonStr(map[string]any{"ok": true})

	case "suggest_event":
		match, err := b.SuggestEvent(u)
		if err != nil {
			if errors.Is(err, ErrNoMatch) {
				return jsonStr(map[string]any{
					"ok":       false,
					"no_match": true,
					"say":      noMatchLine(u),
				})
			}
			return jsonStr(map[string]any{"ok": false, "error": "no events available"})
		}
		ev := match.Event
		return jsonStr(map[string]any{"ok": true, "why": match.Why(), "event": map[string]any{
			"title":     ev.Title,
			"starts_at": ev.StartsAt.In(londonLoc).Format(time.RFC3339),
			"when":      ev.StartsAt.In(londonLoc).Format("Mon 2 Jan, 15:04"),
			"city":      ev.City,
			"url":       ev.URL,
			"tags":      ev.Tags,
		}})

	case "accept_suggestion":
		confirmation, err := b.AcceptSuggestion(u)
		if err != nil {
			// Nothing on offer: if they are already signed up for something,
			// say so rather than letting the model offer a fresh event.
			if accepted, aerr := b.store.AcceptedSuggestions(u.ID); aerr == nil && len(accepted) > 0 {
				last := accepted[len(accepted)-1]
				return jsonStr(map[string]any{"ok": true, "already_signed_up": true, "confirmation": fmt.Sprintf(
					"They are already on the list for %s on %s.",
					last.Event.Title, last.Event.StartsAt.In(londonLoc).Format("Mon 2 Jan at 15:04"))})
			}
			return jsonStr(map[string]any{"ok": false, "error": "no open suggestion"})
		}
		return jsonStr(map[string]any{"ok": true, "confirmation": confirmation})
	}
	return jsonStr(map[string]any{"ok": false, "error": "unknown tool " + name})
}

// SaveCheckin persists a check-in; shared by the SMS brain and the ElevenLabs
// tool webhook.
func (b *Brain) SaveCheckin(u *User, mood *int, summary, topics, raw string) error {
	if mood != nil && (*mood < 1 || *mood > 5) {
		mood = nil
	}
	summary = strings.TrimSpace(summary)
	if summary == "" && mood == nil {
		return fmt.Errorf("check-in needs a summary or a mood")
	}
	return b.store.AddCheckin(&Checkin{
		UserID:  u.ID,
		Mood:    mood,
		Summary: summary,
		Topics:  topics,
		Raw:     raw,
	})
}

// candidateLimit bounds how much of the events table the matcher scores per
// suggestion. The export is a few hundred rows, so this covers all of it.
const candidateLimit = 1000

// SuggestEvent returns the best-justified event for this user and records it as
// an open suggestion. It returns ErrNoMatch when nothing clears the relevance
// and eligibility rules, so callers can say "nothing good right now" instead of
// offering something the user shouldn't be sent.
func (b *Brain) SuggestEvent(u *User) (*EventMatch, error) {
	candidates, err := b.store.CandidateEvents(u.ID, candidateLimit)
	if err != nil {
		return nil, err
	}
	match, err := MatchEvent(u, candidates, time.Now())
	if err != nil {
		return nil, err
	}
	if _, err := b.store.AddSuggestion(u.ID, match.Event.ID); err != nil {
		return nil, err
	}
	log.Printf("suggest: %s -> %q (score %d: %s)", u.Phone, match.Event.Title, match.Score, match.Why())
	return match, nil
}

// AcceptSuggestion marks the open suggestion accepted and returns a human
// confirmation line.
func (b *Brain) AcceptSuggestion(u *User) (string, error) {
	sg, err := b.store.OpenSuggestion(u.ID)
	if err != nil {
		return "", err
	}
	if err := b.store.SetSuggestionStatus(sg.ID, "accepted"); err != nil {
		return "", err
	}
	return fmt.Sprintf("You're on the list for %s on %s. Details: %s",
		sg.Event.Title, sg.Event.StartsAt.In(londonLoc).Format("Mon 2 Jan at 15:04"), sg.Event.URL), nil
}

func jsonStr(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"ok":false}`
	}
	return string(b)
}
