package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	reseed := flag.Bool("reseed", false, "reload events from the CSV export, dropping stale rows that nobody was offered")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("checkin ")

	cfg := LoadConfig()

	store, err := OpenStore(cfg)
	if err != nil {
		log.Fatalf("open store (%s): %v", storeTarget(cfg), err)
	}
	defer store.Close()
	var events EventSource = NewCSVEventSource("events_live.csv", eventsCSV)
	if cfg.EventsFeedURL != "" {
		events = NewHTTPEventSource(cfg.EventsFeedURL, events)
	}
	if err := store.SeedEvents(events, *reseed); err != nil {
		log.Printf("seed events: %v", err)
	}

	cal := NewCalendar()
	var brainClient AnthropicClient
	if cfg.AnthropicAPIKey != "" {
		brainClient = NewAnthropicClient(cfg.AnthropicAPIKey)
	} else {
		log.Printf("anthropic: no key, /sms will use the canned fallback reply")
	}
	brain := NewBrain(store, cal, brainClient, cfg.AnthropicModel, cfg.GCalICSURL)
	srv := NewServer(cfg, store, brain, NewTelephony(cfg), NewVoice(cfg), cal)

	stop := make(chan struct{})
	go NewScheduler(srv, store).Run(stop)
	// Webhooks only cover calls made after the webhook existed, and only the
	// ones that got through. The sync reconciles against the provider's own
	// record so every call ends up on the dashboard.
	go srv.RunTranscriptSync(NewConversationLister(cfg), stop)
	// Sensitive signals expire rather than accumulate; the sweeper deletes
	// them once they are past their retention window.
	go store.RunRetention(stop)

	httpSrv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("listening on :%s (db=%s, model=%s)", cfg.Port, storeTarget(cfg), cfg.AnthropicModel)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	close(stop)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	log.Printf("bye")
}
