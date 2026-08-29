package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ErrNoMatch means nothing in the candidate set is a defensible suggestion for
// this user. Callers say so plainly instead of offering a weak event.
var ErrNoMatch = errors.New("no confident event match")

// EventMatch is a scored candidate plus the human-readable reasons behind the
// score, so every suggestion can explain itself (and every rejection can too).
type EventMatch struct {
	Event   Event
	Score   int
	Reasons []string
}

// Why renders the reasons as one sentence for logs and tool responses.
func (m EventMatch) Why() string { return strings.Join(m.Reasons, "; ") }

// normaliseInterests cleans a comma-separated interests string from the wizard
// or the voice agent: trimmed, lower-cased, de-duplicated, order preserved.
func normaliseInterests(raw string) string {
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		p := strings.ToLower(strings.TrimSpace(part))
		p = strings.Trim(p, ".!?")
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return strings.Join(out, ", ")
}

// interestTags maps what a user says they like onto the vocabulary of the
// events export, which uses tags like "non_uni_hackathon" and "founder_vc".
var interestTags = map[string][]string{
	"hackathon":  {"hackathon"},
	"meetup":     {"meetup", "networking"},
	"conference": {"conference", "summit", "expo"},
	"workshop":   {"workshop", "talk", "training", "course"},
	"social":     {"social", "drinks", "party", "networking"},
	"ai":         {"ai", "ml", "machine_learning", "llm", "data_science"},
	"web":        {"web", "software", "frontend", "backend", "javascript", "dev"},
	"data":       {"data", "data_science", "analytics"},
	"hardware":   {"hardware", "robotics", "iot", "electronics"},
	"startup":    {"startup", "founder", "founder_vc", "vc", "entrepreneur"},
	"design":     {"design", "ux", "product"},
	"security":   {"security", "infosec", "cyber"},
}

// audienceRule marks events that are only appropriate for people who are
// explicitly part of the audience. The user has to have said so themselves:
// nothing here is inferred from a name, a phone number or past check-ins.
type audienceRule struct {
	label   string
	markers []string
	optIn   []string
}

var audienceRules = []audienceRule{
	{
		label:   "aimed at women in tech",
		markers: []string{"women in", "women who", "women's", "womens ", "womxn", "female founder", "females in", "girls who code", "ladies "},
		optIn:   []string{"women", "womxn", "female", "girls who code"},
	},
	{
		label: "aimed at students",
		// "non_uni_hackathon" is a tag in the export, so match on phrases that
		// really do mean students-only rather than on the substring "uni".
		markers: []string{"students only", "student-only", "student only", "university students", "undergraduate", "freshers", "sixth_form", "sixth form"},
		optIn:   []string{"student", "university", "undergrad", "sixth form"},
	},
	{
		label:   "aimed at a specific community",
		markers: []string{"black in tech", "black tech", "lgbtq", "queer ", "pride in tech", "muslims in", "jewish tech", "latinx"},
		optIn:   []string{"black in tech", "lgbtq", "queer", "pride", "muslim", "jewish", "latinx"},
	},
	{
		label:   "invite-only or members-only",
		markers: []string{"invite only", "invite-only", "members only", "members-only", "by invitation", "private dinner"},
		optIn:   nil, // never eligible from a cold suggestion
	},
	{
		label:   "aimed at executives",
		markers: []string{"c-suite", "cto only", "executive roundtable", "board members"},
		optIn:   []string{"executive", "c-suite", "cto", "leadership"},
	},
}

// preferredCity is where the service is based; other cities are still offered
// but only when the user's own interests point elsewhere.
const preferredCity = "london"

// MatchEvent picks the best-justified event for the user, or ErrNoMatch.
func MatchEvent(u *User, candidates []Event, now time.Time) (*EventMatch, error) {
	matches := RankEvents(u, candidates, now)
	if len(matches) == 0 {
		return nil, ErrNoMatch
	}
	best := matches[0]
	return &best, nil
}

// RankEvents scores every eligible candidate, best first. Ineligible events are
// dropped entirely rather than ranked low, so they can never leak out.
func RankEvents(u *User, candidates []Event, now time.Time) []EventMatch {
	interests := u.InterestList()
	wanted, unwanted := splitPreferences(interests)
	var out []EventMatch
	for _, e := range candidates {
		m, ok := scoreEvent(e, wanted, unwanted, now)
		if !ok {
			continue
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Event.StartsAt.Before(out[j].Event.StartsAt)
	})
	return out
}

// splitPreferences separates "hackathons" from "no conferences" / "not
// networking", which the voice agent hears often enough to be worth honouring.
func splitPreferences(interests []string) (wanted, unwanted []string) {
	for _, in := range interests {
		switch {
		case strings.HasPrefix(in, "no "):
			unwanted = append(unwanted, strings.TrimSpace(strings.TrimPrefix(in, "no ")))
		case strings.HasPrefix(in, "not "):
			unwanted = append(unwanted, strings.TrimSpace(strings.TrimPrefix(in, "not ")))
		case strings.HasPrefix(in, "avoid "):
			unwanted = append(unwanted, strings.TrimSpace(strings.TrimPrefix(in, "avoid ")))
		default:
			wanted = append(wanted, in)
		}
	}
	return wanted, unwanted
}

func scoreEvent(e Event, wanted, unwanted []string, now time.Time) (EventMatch, bool) {
	haystack := strings.ToLower(e.Title + " " + e.Tags + " " + e.City)
	m := EventMatch{Event: e}

	if !e.StartsAt.After(now) {
		return m, false
	}
	if strings.TrimSpace(e.URL) == "" {
		return m, false
	}

	// Explicit dislikes are hard exclusions.
	for _, no := range unwanted {
		if no != "" && matchesInterest(haystack, no) {
			return m, false
		}
	}

	// Audience restrictions: only offer when the user opted into that audience.
	for _, rule := range audienceRules {
		if !containsAny(haystack, rule.markers) {
			continue
		}
		if !optedIn(wanted, rule.optIn) {
			return m, false
		}
		m.Reasons = append(m.Reasons, "you told me you're part of this community ("+rule.label+")")
	}

	// Interest fit.
	var hits []string
	for _, in := range wanted {
		if matchesInterest(haystack, in) {
			hits = append(hits, in)
		}
	}
	switch {
	case len(hits) > 0:
		m.Score += 3 * len(hits)
		m.Reasons = append(m.Reasons, "matches your interest in "+strings.Join(hits, " and "))
	case len(wanted) > 0:
		// The user told us what they like and this is not it.
		return m, false
	default:
		m.Score++
		m.Reasons = append(m.Reasons, "you haven't told me what you're into yet, so this is a general pick")
	}

	// Location.
	city := strings.ToLower(strings.TrimSpace(e.City))
	switch {
	case city == preferredCity:
		m.Score += 2
		m.Reasons = append(m.Reasons, "it's in London")
	case city == "" || city == "online" || city == "virtual" || city == "remote":
		m.Score++
		m.Reasons = append(m.Reasons, "it's online, so location isn't a problem")
	default:
		if !mentionsCity(wanted, city) {
			// Somewhere else entirely and nothing says the user travels.
			return m, false
		}
		m.Score += 2
		m.Reasons = append(m.Reasons, "it's in "+e.City+", which you mentioned")
	}

	// Timing.
	days := int(e.StartsAt.Sub(now).Hours() / 24)
	switch {
	case days <= 30:
		m.Score += 2
		m.Reasons = append(m.Reasons, fmt.Sprintf("it's soon (%s)", humanDays(days)))
	case days <= 90:
		m.Score++
		m.Reasons = append(m.Reasons, "it's within the next few months")
	default:
		m.Reasons = append(m.Reasons, "it's a way off yet")
	}

	return m, true
}

func humanDays(days int) string {
	switch {
	case days <= 0:
		return "today"
	case days == 1:
		return "tomorrow"
	case days < 14:
		return fmt.Sprintf("in %d days", days)
	default:
		return fmt.Sprintf("in %d weeks", days/7)
	}
}

// matchesInterest checks the interest itself plus its tag synonyms, so "ai"
// finds "machine_learning" and "hackathons" finds "non_uni_hackathon".
func matchesInterest(haystack, interest string) bool {
	interest = interestStem(interest)
	if interest == "" {
		return false
	}
	if strings.Contains(haystack, interest) {
		return true
	}
	return containsAny(haystack, interestTags[interest])
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if n != "" && strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// optedIn reports whether the user explicitly named this audience. An empty
// optIn list means the restriction can never be satisfied from a suggestion.
func optedIn(wanted, optIn []string) bool {
	if len(optIn) == 0 {
		return false
	}
	for _, w := range wanted {
		for _, o := range optIn {
			if strings.Contains(w, o) || strings.Contains(o, w) {
				return true
			}
		}
	}
	return false
}

func mentionsCity(wanted []string, city string) bool {
	for _, w := range wanted {
		if strings.Contains(city, w) || strings.Contains(w, city) {
			return true
		}
	}
	return false
}
