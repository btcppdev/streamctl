// Package gpuwatchdog bounds paid worker time independently of the controller.
package gpuwatchdog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"streamctl/internal/db"
	"streamctl/internal/runpodclient"
)

type Provider interface {
	ListPods(context.Context) ([]runpodclient.Pod, error)
	DeletePod(context.Context, string) error
}

type Config struct {
	StateFile         string
	PodName           string
	IdleTimeout       time.Duration
	CompletionTimeout time.Duration
}

type observation struct {
	LastCompletion time.Time `json:"last_completion"`
	IdleSince      time.Time `json:"idle_since,omitempty"`
}

type State struct {
	PauseReason string                 `json:"pause_reason,omitempty"`
	PausedAt    time.Time              `json:"paused_at,omitempty"`
	Pods        map[string]observation `json:"pods"`
}

// Lock serializes dispatch/provisioning with the independent watchdog process.
// The lock file is never replaced; the adjacent state file is replaced atomically.
func Lock(ctx context.Context, path string) (func(), error) {
	if path == "" {
		return func() {}, nil
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func Read(path string) (State, error) {
	s := State{Pods: make(map[string]observation)}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("read GPU watchdog state: %w", err)
	}
	if s.Pods == nil {
		s.Pods = make(map[string]observation)
	}
	return s, nil
}

func Check(path string) error {
	s, err := Read(path)
	if err != nil {
		return err
	}
	if s.PauseReason != "" {
		return fmt.Errorf("GPU queue paused by spending watchdog: %s; review the worker and resume explicitly", s.PauseReason)
	}
	return nil
}

func save(path string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".gpu-watchdog-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// RunOnce persists a pause before requesting termination. Failed API calls are
// retried next time, even if jobs subsequently complete. Logging/heartbeats and
// failed attempts cannot extend the completion deadline.
func RunOnce(ctx context.Context, cfg Config, database *db.DB, provider Provider, now time.Time) (string, error) {
	if cfg.StateFile == "" || cfg.PodName == "" || cfg.IdleTimeout <= 0 || cfg.CompletionTimeout <= 0 {
		return "", errors.New("GPU watchdog requires state file, pod name, and positive timeouts")
	}
	unlock, err := Lock(ctx, cfg.StateFile)
	if err != nil {
		return "", err
	}
	defer unlock()
	s, err := Read(cfg.StateFile)
	if err != nil {
		return "", err
	}
	pods, err := provider.ListPods(ctx)
	if err != nil {
		return "", err
	}
	var managed []runpodclient.Pod
	for _, p := range pods {
		if p.Name == cfg.PodName && p.State != "TERMINATED" {
			managed = append(managed, p)
		}
	}
	if s.PauseReason == "" {
		activity, err := database.GPUWatchdogActivity(ctx)
		if err != nil {
			return "", err
		}
		observed := make(map[string]observation)
		for _, p := range managed {
			o, known := s.Pods[p.ID]
			if !known {
				o.LastCompletion, err = time.Parse(time.RFC3339Nano, p.CreatedAt)
				if err != nil || o.LastCompletion.After(now) {
					o.LastCompletion = now
				}
			}
			if activity.LastSuccess.After(o.LastCompletion) && !activity.LastSuccess.After(now) {
				o.LastCompletion = activity.LastSuccess
			}
			if activity.Open == 0 {
				if o.IdleSince.IsZero() {
					o.IdleSince = now
				}
				if now.Sub(o.IdleSince) >= cfg.IdleTimeout {
					s.PauseReason = fmt.Sprintf("worker %s had no queued or running jobs for %s", p.ID, cfg.IdleTimeout)
				}
			} else {
				// New work after a brief idle period gets a fresh completion window.
				if !o.IdleSince.IsZero() {
					o.LastCompletion = now
				}
				o.IdleSince = time.Time{}
				if now.Sub(o.LastCompletion) >= cfg.CompletionTimeout {
					s.PauseReason = fmt.Sprintf("worker %s had no successful job completion for %s", p.ID, cfg.CompletionTimeout)
				}
			}
			observed[p.ID] = o
		}
		s.Pods = observed
		if s.PauseReason != "" {
			s.PausedAt = now
		}
		if err := save(cfg.StateFile, s); err != nil {
			return "", err
		}
	}
	if s.PauseReason == "" {
		return "GPU watchdog: worker within limits", nil
	}
	// Include stopped pods: their local storage still costs money. Only this
	// controller's exact pod name is in scope; unrelated account pods are untouched.
	var failures []error
	for _, p := range managed {
		if err := provider.DeletePod(ctx, p.ID); err != nil {
			failures = append(failures, fmt.Errorf("terminate %s: %w", p.ID, err))
		}
	}
	if len(failures) > 0 {
		return s.PauseReason, errors.Join(failures...)
	}
	// Do not mark a job failed until termination has succeeded. Remaining queued
	// work stays queued, and the pause prevents automatic paid replacements.
	if err := database.FailWatchdogGPUJobs(ctx, s.PauseReason); err != nil {
		return s.PauseReason, err
	}
	return "GPU watchdog paused queue and terminated managed workers: " + s.PauseReason, nil
}

// Resume requires confirmed termination before allowing new paid work.
func Resume(ctx context.Context, cfg Config, database *db.DB, provider Provider) error {
	unlock, err := Lock(ctx, cfg.StateFile)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := Read(cfg.StateFile)
	if err != nil {
		return err
	}
	if s.PauseReason == "" {
		return nil
	}
	pods, err := provider.ListPods(ctx)
	if err != nil {
		return err
	}
	for _, p := range pods {
		if p.Name == cfg.PodName && p.State != "TERMINATED" {
			return fmt.Errorf("worker %s still exists; wait for watchdog termination before resuming", p.ID)
		}
	}
	if err := database.FailWatchdogGPUJobs(ctx, s.PauseReason); err != nil {
		return err
	}
	return save(cfg.StateFile, State{Pods: make(map[string]observation)})
}
