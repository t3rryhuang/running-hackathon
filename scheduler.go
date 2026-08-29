package main

import (
	"log"
	"time"
)

// Scheduler fires check-ins on a 60s tick according to each user's frequency.
type Scheduler struct {
	srv   *Server
	store *Store
	now   func() time.Time
}

func NewScheduler(srv *Server, store *Store) *Scheduler {
	return &Scheduler{srv: srv, store: store, now: time.Now}
}

func (s *Scheduler) Run(stop <-chan struct{}) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			s.tick()
		}
	}
}

func (s *Scheduler) tick() {
	users, err := s.store.AllUsers()
	if err != nil {
		log.Printf("scheduler: list users: %v", err)
		return
	}
	now := s.now().In(londonLoc)
	for i := range users {
		u := users[i]
		if !DueNow(&u, now) {
			continue
		}
		// Onboarding asked whether to check in at all. "No thanks" is an
		// answer, and it is honoured here rather than being treated as a
		// question that was never settled.
		if status, _, ok := s.store.SettledStatus(u.ID, "checkin_consent"); ok && status != StatusAnswered {
			continue
		}
		if err := s.srv.TriggerCheckin(&u); err != nil {
			log.Printf("scheduler: trigger %s: %v", u.Phone, err)
			continue
		}
		log.Printf("scheduler: checked in %s (%s)", u.Phone, u.Channel)
	}
}

// slotHours are the Europe/London hours at which each frequency fires.
func slotHours(frequency string) []int {
	switch frequency {
	case "twice-daily":
		return []int{9, 20}
	default:
		return []int{9}
	}
}

// DueNow reports whether a check-in should fire for the user at time now. A user
// is due when the current hour matches one of their slots and they have not
// already been triggered inside that slot.
func DueNow(u *User, now time.Time) bool {
	now = now.In(londonLoc)
	// Nobody is put on a schedule before they have been introduced and asked
	// whether they want one.
	if !u.Onboarded() {
		return false
	}
	if u.Frequency == "weekdays" {
		switch now.Weekday() {
		case time.Saturday, time.Sunday:
			return false
		}
	}
	var slot int = -1
	for _, h := range slotHours(u.Frequency) {
		if now.Hour() == h {
			slot = h
		}
	}
	if slot < 0 {
		return false
	}
	if u.LastTriggeredAt == nil {
		return true
	}
	last := u.LastTriggeredAt.In(londonLoc)
	slotStart := time.Date(now.Year(), now.Month(), now.Day(), slot, 0, 0, 0, londonLoc)
	return last.Before(slotStart)
}
