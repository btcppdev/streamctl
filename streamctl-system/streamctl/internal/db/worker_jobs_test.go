package db

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestWorkerJobsKeepAllActiveAndOrderRecentByCompletion(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.Migrate(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		_, err = d.Exec(`INSERT INTO render_job_queue(name,manifest_json,status,created_at,finished_at) VALUES(?, '{}', 'finished', '2026-01-01', ?)`, fmt.Sprintf("render-%d", i), fmt.Sprintf("2026-02-%02d", i+1))
		if err != nil {
			t.Fatal(err)
		}
	}
	// An ancient failure must not hide recent successes, and an old queued job
	// must stay visible regardless of the number of completed jobs.
	for i := 0; i < 15; i++ {
		_, err = d.Exec(`INSERT INTO render_job_queue(name,manifest_json,status,created_at) VALUES(?, '{}', 'queued','2025-01-01')`, fmt.Sprintf("queued-%d", i))
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = d.Exec(`INSERT INTO production_proxy_jobs(source_object_key,proxy_object_key,status,created_at,finished_at) VALUES('conf/old.mp4','conf/old.proxy.mp4','failed','2024-01-01','2024-01-01'),('conf/new.mp4','conf/new.proxy.mp4','finished','2026-01-01','2026-03-01')`)
	if err != nil {
		t.Fatal(err)
	}
	recent, err := d.WorkerJobs("recent", 10, 0)
	if err != nil || len(recent) != 10 || recent[0].Name != "conf/new.mp4" {
		t.Fatalf("recent=%+v err=%v", recent, err)
	}
	for _, j := range recent {
		if j.Status == "queued" || j.Name == "conf/old.mp4" {
			t.Fatalf("unexpected recent job %+v", j)
		}
	}
	active, err := d.WorkerJobs("active", 1, 0)
	if err != nil || len(active) != 15 {
		t.Fatalf("active=%d err=%v", len(active), err)
	}
	first, err := d.WorkerJobs("history", 25, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.WorkerJobs("history", 25, 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 25 || len(second) != 22 {
		t.Fatalf("pages %d/%d", len(first), len(second))
	}
	seen := map[string]bool{}
	for _, j := range append(first, second...) {
		key := fmt.Sprintf("%s:%d", j.Kind, j.ID)
		if seen[key] {
			t.Fatal("duplicate across pages", key)
		}
		seen[key] = true
	}
}
