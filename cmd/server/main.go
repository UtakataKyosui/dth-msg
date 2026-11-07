package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/UtakataKyosui/dth-msg/internal/monitor"
)

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func number(key, fallback string) int {
	n, err := strconv.Atoi(env(key, fallback))
	if err != nil {
		log.Fatalf("%s must be an integer", key)
	}
	return n
}

func main() {
	c := monitor.Config{
		AdminToken: os.Getenv("ADMIN_TOKEN"), ClientID: os.Getenv("GOOGLE_CLIENT_ID"),
		ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"), RefreshToken: os.Getenv("GOOGLE_REFRESH_TOKEN"),
		DeviceName: os.Getenv("FITBIT_DEVICE_NAME"), MissingHours: number("MISSING_HOURS", "24"),
		SyncMinutes: number("MAX_SYNC_AGE_MINUTES", "120"), PollMinutes: number("MAX_POLL_GAP_MINUTES", "15"),
		PublicURL: os.Getenv("PUBLIC_URL"), ClerkPublishableKey: os.Getenv("CLERK_PUBLISHABLE_KEY"),
		ClerkJWTKey: os.Getenv("CLERK_JWT_KEY"), ClerkSecretKey: os.Getenv("CLERK_SECRET_KEY"), OwnerUserID: os.Getenv("OWNER_CLERK_USER_ID"),
		DiscordClientID: os.Getenv("DISCORD_CLIENT_ID"), DiscordClientSecret: os.Getenv("DISCORD_CLIENT_SECRET"), EnableX: env("ENABLE_X", "false") == "true",
	}
	if err := json.Unmarshal([]byte(env("SNS_TARGETS_JSON", "[]")), &c.Targets); err != nil {
		log.Fatal("SNS_TARGETS_JSON must be a target array")
	}
	s, err := monitor.NewService(c, monitor.FileStore{Path: env("STATE_FILE", "state.json")})
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{
		Addr: env("LISTEN_ADDR", "127.0.0.1:8080"), Handler: s,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 2 * time.Minute, IdleTimeout: 60 * time.Second,
	}
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			if err := s.Check(ctx); err != nil && ctx.Err() == nil {
				log.Print("monitor check failed; inspect authenticated /status and upstream configuration")
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("listening on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
