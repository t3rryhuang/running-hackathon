package main

import (
	"fmt"
	"strings"
)

// Field is one thing the voice agent may or may not state about the caller.
// Known means it is on file for this caller; Source says where it came from,
// so an answer typed into the signup form is never presented as something they
// said on the phone.
type Field struct {
	Known  bool   `json:"known"`
	Value  string `json:"value,omitempty"`
	Source string `json:"source,omitempty"`
}

func known(value, source string) Field {
	value = strings.TrimSpace(value)
	if value == "" {
		return Field{}
	}
	return Field{Known: true, Value: value, Source: source}
}

// CallerContext is what the agent is told at the top of a call. It is a closed
// world: the agent may state what is in Known, must ask for what is in Missing,
// and has nothing else to work from.
type CallerContext struct {
	Phone         string           `json:"phone"`
	Resolved      bool             `json:"resolved"`
	PhoneVerified bool             `json:"phone_verified"`
	Known         map[string]Field `json:"known"`
	Missing       []string         `json:"missing"`
	DoNotAsk      []string         `json:"do_not_ask"`
	Greeting      string           `json:"greeting"`
	Instruction   string           `json:"instruction_to_agent"`
}

// buildCallerContext turns a resolved user and their session checklist into the
// agent's opening state. u is nil when the number matched no profile, which is
// reported as such rather than filled in from anywhere else.
func buildCallerContext(phone string, u *User, items []ChecklistItem) CallerContext {
	c := CallerContext{Phone: phone, Known: map[string]Field{}, Missing: []string{}, DoNotAsk: []string{}}
	if u == nil {
		c.Greeting = "Hi, this is CheckIn."
		c.Instruction = "This number does not match anyone on file. Do not use a name and do not assume any history. " +
			"Say plainly that you do not have their details yet, ask only for their name, and take it from there."
		c.Missing = []string{"name", "event_types", "frequency"}
		return c
	}

	c.Resolved = true
	c.PhoneVerified = u.PhoneVerified()
	// Name and frequency come off the profile the signup form wrote; the rest
	// come from the checklist. The order is fixed so every call asks for the
	// same things in the same sequence.
	for _, f := range []struct {
		key   string
		field Field
	}{
		{"name", known(u.DisplayName(), SourceProfile)},
		{"frequency", known(u.Frequency, SourceProfile)},
	} {
		c.Known[f.key] = f.field
		if f.field.Known {
			c.DoNotAsk = append(c.DoNotAsk, f.key)
		} else {
			c.Missing = append(c.Missing, f.key)
		}
	}
	for _, it := range items {
		if it.Status == StatusAnswered {
			c.Known[it.Key] = known(it.Answer, it.Source)
			c.DoNotAsk = append(c.DoNotAsk, it.Key)
			continue
		}
		// Skipped and declined are settled but not known: the agent must not
		// state a value, and must not ask again either.
		if it.Settled() {
			c.Known[it.Key] = Field{}
			c.DoNotAsk = append(c.DoNotAsk, it.Key)
			continue
		}
		c.Missing = append(c.Missing, it.Key)
	}

	c.Greeting = greetingFor(c)
	c.Instruction = instructionFor(c)
	return c
}

// DynamicVariables is the caller context flattened for the ElevenLabs agent
// prompt. Unknown fields are the literal string "unknown" so the prompt has
// nothing blank to improvise into.
func (c CallerContext) DynamicVariables() map[string]string {
	vars := map[string]string{
		"user_phone":     c.Phone,
		"caller_known":   boolWord(c.Resolved),
		"phone_verified": boolWord(c.PhoneVerified),
		"greeting":       c.Greeting,
		"do_not_ask":     strings.Join(c.DoNotAsk, ", "),
		"ask_only":       strings.Join(c.Missing, ", "),
	}
	for key, f := range c.Known {
		name := "user_" + key
		if f.Known {
			vars[name] = f.Value
			continue
		}
		vars[name] = "unknown"
	}
	if _, ok := vars["user_name"]; !ok {
		vars["user_name"] = "unknown"
	}
	return vars
}

func greetingFor(c CallerContext) string {
	name := c.Known["name"]
	// A name is only ever spoken when it is on file for this verified number.
	if !name.Known || !c.PhoneVerified {
		return "Hi, this is CheckIn."
	}
	return "Hi " + name.Value + ", it's CheckIn."
}

func instructionFor(c CallerContext) string {
	var b strings.Builder
	b.WriteString("Everything you know about this caller is in `known`; nothing else exists. ")
	if !c.PhoneVerified {
		b.WriteString("This number has not been verified, so do not use their name until they confirm who they are, " +
			"and say plainly that you need to check. ")
	}
	if len(c.DoNotAsk) > 0 {
		b.WriteString(fmt.Sprintf("Already on file, never ask for these again: %s. ", strings.Join(c.DoNotAsk, ", ")))
	}
	if len(c.Missing) == 0 {
		b.WriteString("Nothing is missing: do not run the interview, just do the check-in.")
		return b.String()
	}
	b.WriteString(fmt.Sprintf("Genuinely missing or stale, ask only these and only one at a time, in this order: %s.",
		strings.Join(c.Missing, ", ")))
	return b.String()
}
