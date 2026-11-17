package gpuwatchdog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"streamctl/internal/db"
	"streamctl/internal/runpodclient"
)

type fakeProvider struct {
	pods         []runpodclient.Pod
	deleted      []string
	deleteErr    error
	beforeDelete func()
}

func (p *fakeProvider) ListPods(context.Context) ([]runpodclient.Pod, error) { return p.pods, nil }
func (p *fakeProvider) DeletePod(_ context.Context, id string) error {
	if p.beforeDelete != nil {
		p.beforeDelete()
	}
	p.deleted = append(p.deleted, id)
	return p.deleteErr
}

func setup(t *testing.T) (Config, *db.DB, *fakeProvider, time.Time) {
	t.Helper()
	root := t.TempDir()
	database, err := db.Open(filepath.Join(root, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	cfg := Config{StateFile: filepath.Join(root, "watchdog.json"), PodName: "managed", IdleTimeout: 15 * time.Minute, CompletionTimeout: 2 * time.Hour}
	p := &fakeProvider{pods: []runpodclient.Pod{
		{ID: "our-pod", Name: "managed", State: "RUNNING", CreatedAt: now.Add(-3 * time.Hour).Format(time.RFC3339)},
		{ID: "unrelated", Name: "someone-else", State: "RUNNING", CreatedAt: now.Add(-24 * time.Hour).Format(time.RFC3339)},
	}}
	return cfg, database, p, now
}

func sqlExec(t *testing.T, database *db.DB, query string, args ...any) {
	t.Helper()
	if _, err := database.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestStalledQueuePersistsPauseBeforeDeleteAndRetries(t *testing.T) {
	cfg, database, p, now := setup(t)
	ctx := context.Background()
	sqlExec(t, database, `INSERT INTO render_job_queue (name,manifest_json,status,unit_name) VALUES ('stalled','{}','running','old-attempt'), ('next','{}','queued','')`)
	sqlExec(t, database, `INSERT INTO production_proxy_jobs (source_object_key,proxy_object_key,status) VALUES ('source','proxy','running')`)
	sqlExec(t, database, `INSERT INTO gpu_job_queue (raw_path,status) VALUES ('legacy','running')`)
	// Fresh diagnostics/error timestamps are deliberately not progress.
	sqlExec(t, database, `INSERT INTO gpu_job_logs (unit_name,journal,updated_at) VALUES ('old-attempt','still alive',?)`, now)
	p.beforeDelete = func() {
		if err := Check(cfg.StateFile); err == nil {
			t.Fatal("pod deleted before durable pause")
		}
	}
	p.deleteErr = errors.New("provider unavailable")
	if _, err := RunOnce(ctx, cfg, database, p, now); err == nil {
		t.Fatal("expected deletion failure")
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM render_job_queue WHERE name='stalled'`).Scan(&status); err != nil || status != "running" {
		t.Fatalf("job prematurely failed: %s %v", status, err)
	}
	// Simulate restart and a late success: a latched cutoff must still retry.
	sqlExec(t, database, `INSERT INTO render_job_queue (name,manifest_json,status,finished_at) VALUES ('late','{}','finished',?)`, now)
	p.deleteErr = nil
	if _, err := RunOnce(ctx, cfg, database, p, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.deleted, ",") != "our-pod,our-pod" {
		t.Fatalf("deleted wrong pods: %v", p.deleted)
	}
	for _, table := range []string{"render_job_queue", "production_proxy_jobs", "gpu_job_queue"} {
		var running int
		if err := database.QueryRow("SELECT count(*) FROM " + table + " WHERE status='running'").Scan(&running); err != nil || running != 0 {
			t.Fatalf("running jobs remain: %s %d %v", table, running, err)
		}
	}
	if err := database.QueryRow(`SELECT status FROM render_job_queue WHERE name='next'`).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("queued work changed: %s %v", status, err)
	}
	if err := database.FinishRenderAttempt(1, "old-attempt", "finished", ""); err == nil {
		t.Fatal("late completion overwrote watchdog failure")
	}
	if err := Resume(ctx, cfg, database, p); err == nil {
		t.Fatal("resumed before termination confirmed")
	}
	p.pods = p.pods[1:]
	if err := Resume(ctx, cfg, database, p); err != nil {
		t.Fatal(err)
	}
	if err := Check(cfg.StateFile); err != nil {
		t.Fatal(err)
	}
}

func TestSuccessfulJobsKeepLongBatchRunning(t *testing.T) {
	cfg, database, p, now := setup(t)
	ctx := context.Background()
	sqlExec(t, database, `INSERT INTO render_job_queue (name,manifest_json,status) VALUES ('current','{}','running')`)
	// Preview completions count, too; long healthy batches are not lifetime-capped.
	sqlExec(t, database, `INSERT INTO production_proxy_jobs (source_object_key,proxy_object_key,status,finished_at) VALUES ('source','proxy','finished',?)`, now.Add(-time.Hour))
	if _, err := RunOnce(ctx, cfg, database, p, now); err != nil {
		t.Fatal(err)
	}
	if len(p.deleted) != 0 {
		t.Fatal("killed healthy batch")
	}
	if _, err := RunOnce(ctx, cfg, database, p, now.Add(59*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(p.deleted) != 0 {
		t.Fatal("killed before completion deadline")
	}
	if _, err := RunOnce(ctx, cfg, database, p, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(p.deleted) != 1 {
		t.Fatal("missed completion deadline")
	}
}

func TestIdleDeadlineSurvivesRestartAndIncludesStoppedStorage(t *testing.T) {
	cfg, database, p, now := setup(t)
	p.pods[0].State = "EXITED"
	ctx := context.Background()
	for _, offset := range []time.Duration{0, 14 * time.Minute} {
		if _, err := RunOnce(ctx, cfg, database, p, now.Add(offset)); err != nil {
			t.Fatal(err)
		}
		if len(p.deleted) != 0 {
			t.Fatal("deleted before idle deadline")
		}
	}
	if _, err := RunOnce(ctx, cfg, database, p, now.Add(15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(p.deleted) != 1 {
		t.Fatal("left stopped storage billing")
	}
}

func TestNewWorkAndReplacementHaveFreshCompletionWindow(t *testing.T) {
	cfg, database, p, now := setup(t)
	ctx := context.Background()
	if _, err := RunOnce(ctx, cfg, database, p, now); err != nil {
		t.Fatal(err)
	}
	sqlExec(t, database, `INSERT INTO render_job_queue (name,manifest_json,status) VALUES ('new','{}','queued')`)
	if _, err := RunOnce(ctx, cfg, database, p, now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(p.deleted) != 0 {
		t.Fatal("idle time counted against new work")
	}
	p.pods[0].ID = "replacement"
	p.pods[0].CreatedAt = now.Add(time.Hour).Format(time.RFC3339)
	if _, err := RunOnce(ctx, cfg, database, p, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(p.deleted) != 0 {
		t.Fatal("previous worker deadline applied to replacement")
	}
}

func TestUnreadableStateFailsClosedAndLockHonorsContext(t *testing.T) {
	cfg, database, p, now := setup(t)
	if err := os.WriteFile(cfg.StateFile, []byte("broken JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Check(cfg.StateFile); err == nil {
		t.Fatal("corrupt state permitted creation")
	}
	if _, err := RunOnce(context.Background(), cfg, database, p, now); err == nil {
		t.Fatal("corrupt state silently reset")
	}
	if len(p.deleted) != 0 {
		t.Fatal("deleted with unreadable state")
	}
	unlock, err := Lock(context.Background(), cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if release, err := Lock(ctx, cfg.StateFile); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			release()
		}
		t.Fatalf("lock did not time out: %v", err)
	}
}
