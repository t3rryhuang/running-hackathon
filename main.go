package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("checkin ")

	cfg := LoadConfig()

	store, err := OpenStore(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("open store %s: %v", cfg.DatabasePath, err)
	}
	defer store.Close()
	if err := store.SeedEvents(eventsCSV); err != nil {
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
	tel := NewTelephony(cfg)
	srv := NewServer(cfg, store, brain, tel, cal)

	stop := make(chan struct{})
	go NewScheduler(srv, store).Run(stop)

	httpSrv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("listening on :%s (db=%s, model=%s)", cfg.Port, cfg.DatabasePath, cfg.AnthropicModel)
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
