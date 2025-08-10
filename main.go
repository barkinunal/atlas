package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/barkinunal/atlas/internal/twitch"
	"github.com/joho/godotenv"
)

func main() {
	// Load .env file
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}

	cfg := twitch.Config{
		Username: os.Getenv("TWITCH_USERNAME"),
		Token:    os.Getenv("TWITCH_OAUTH_TOKEN"),
		Channel:  os.Getenv("TWITCH_CHANNEL"),
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	msgs, errs, err := twitch.Listen(ctx, cfg)
	if err != nil {
		log.Fatalf("listen init error: %v", err)
	}

	for {
		select {
		case m, ok := <-msgs:
			if !ok {
				return
			}

			fmt.Printf("[%s] %s: %s\n", "twitch", m.User, m.Text)
		case e, ok := <-errs:
			if !ok {
				return
			}
			log.Printf("connection error: %v", e)
		}
	}
}
