package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"streamctl/internal/db"
	"streamctl/internal/gpuwatchdog"
	"streamctl/internal/runpodclient"
)

func runGPUWatchdog(args []string) error {
	fs := flag.NewFlagSet("gpu-watchdog", flag.ContinueOnError)
	dbPath := fs.String("db", "/var/lib/streamctl/streamctl.db", "controller database")
	state := fs.String("state-file", "/var/lib/streamctl/gpu-watchdog.json", "persistent watchdog state")
	tokenFile := fs.String("runpod-token-file", "/var/lib/streamctl/runpod-token", "RunPod API token file")
	name := fs.String("runpod-pod-name", "streamctl-gpu-worker", "managed pod name")
	idle := fs.Duration("idle-timeout", 15*time.Minute, "maximum time with no open jobs")
	completion := fs.Duration("completion-timeout", 4*time.Hour, "maximum time without a successful job")
	resume := fs.Bool("resume", false, "explicitly resume a queue paused by the watchdog")
	if err := fs.Parse(args); err != nil {
		return err
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(token)) == "" {
		return fmt.Errorf("empty RunPod token")
	}
	// Never create a missing database or run migrations from the timer.
	if _, err := os.Stat(*dbPath); err != nil {
		return err
	}
	database, err := db.Open(*dbPath)
	if err != nil {
		return err
	}
	defer database.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	cfg := gpuwatchdog.Config{StateFile: *state, PodName: *name, IdleTimeout: *idle, CompletionTimeout: *completion}
	client := runpodclient.New(string(token))
	if *resume {
		return gpuwatchdog.Resume(ctx, cfg, database, client)
	}
	message, err := gpuwatchdog.RunOnce(ctx, cfg, database, client, time.Now().UTC())
	if message != "" {
		fmt.Println(message)
	}
	return err
}
