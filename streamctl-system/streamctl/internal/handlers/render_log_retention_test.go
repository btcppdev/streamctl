package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func renderLogTestSSH(t *testing.T, state, journal string) (string, string) {
	t.Helper()
	root := t.TempDir()
	logFile, calls := filepath.Join(root, "journal"), filepath.Join(root, "calls")
	if err := os.WriteFile(logFile, []byte(journal), 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
for command do :; done
printf '%s\n' "$command" >> "$TEST_SSH_CALLS"
case "$command" in
 'systemctl show '*) printf '%s\n' "$TEST_JOB_STATE" ;;
 'journalctl '*) cat "$TEST_JOB_JOURNAL" ;;
 'if [ -f '*) printf 'failed\n1\n' ;;
 'rm -rf '*) : ;;
 *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(root, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_SSH_CALLS", calls)
	t.Setenv("TEST_JOB_JOURNAL", logFile)
	t.Setenv("TEST_JOB_STATE", state)
	return logFile, calls
}

func TestRenderCompletionRetainsFullLogsWithoutDashboard(t *testing.T) {
	for _, resultPath := range []string{"terminal unit", "durable result"} {
		t.Run(resultPath, func(t *testing.T) {
			database := openGPUQueueTestDB(t)
			item, err := database.EnqueueRenderJob("Review", `{"version":1,"jobs":[]}`)
			if err != nil {
				t.Fatal(err)
			}
			unit := renderUnitName(item.ID, 1)
			if err := database.MarkRenderQueueRunning(item.ID, unit); err != nil {
				t.Fatal(err)
			}
			journal := "Original FFmpeg error: out of memory\n" + strings.Repeat("progress\n", 350) + "wrapper exited 1\n"
			state := "LoadState=loaded\nActiveState=failed\nResult=exit-code"
			if resultPath == "durable result" {
				state = "LoadState=not-found\nActiveState=inactive\nResult=success"
			}
			_, calls := renderLogTestSSH(t, state, journal)
			h := &Handler{DB: database}
			h.reconcileRenderJobs(context.Background(), "worker", nil, true)
			current, err := database.GetRenderQueueItem(item.ID)
			if err != nil || current.Status != "failed" {
				t.Fatalf("completion: %+v %v", current, err)
			}
			cached, err := database.GetGPUJobLog(unit)
			if err != nil || cached.Journal != strings.TrimSpace(journal) {
				t.Fatalf("full journal not retained: %+v %v", cached, err)
			}
			commands, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(commands), "-n 300") || !strings.Contains(string(commands), "rm -rf") {
				t.Fatalf("unexpected log/cleanup commands: %s", commands)
			}
			// The worker is now unavailable; the existing Logs endpoint must still work.
			w := httptest.NewRecorder()
			h.gpuJobLogs(w, httptest.NewRequest("GET", "/gpu-worker/logs/"+unit+"?format=json", nil))
			var response struct {
				Cached bool
				Job    gpuJobView
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if w.Code != 200 || !response.Cached || response.Job.Journal != strings.TrimSpace(journal) {
				t.Fatalf("retained logs unavailable: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestRenderLogCollectionFailurePreservesAttemptForRetry(t *testing.T) {
	for _, journal := range []string{"", "-- No entries --"} {
		t.Run(journal, func(t *testing.T) {
			database := openGPUQueueTestDB(t)
			item, err := database.EnqueueRenderJob("Review", `{"version":1,"jobs":[]}`)
			if err != nil {
				t.Fatal(err)
			}
			unit := renderUnitName(item.ID, 1)
			if err := database.MarkRenderQueueRunning(item.ID, unit); err != nil {
				t.Fatal(err)
			}
			logFile, calls := renderLogTestSSH(t, "LoadState=loaded\nActiveState=failed\nResult=exit-code", journal)
			h := &Handler{DB: database}
			h.reconcileRenderJobs(context.Background(), "worker", nil, true)
			current, err := database.GetRenderQueueItem(item.ID)
			if err != nil || current.Status != "running" || !strings.Contains(current.LastError, "Waiting to retain worker logs") {
				t.Fatalf("attempt not retained: %+v %v", current, err)
			}
			commands, _ := os.ReadFile(calls)
			if strings.Contains(string(commands), "rm -rf") {
				t.Fatalf("deleted workspace without logs: %s", commands)
			}
			if err := os.WriteFile(logFile, []byte("FFmpeg failure\n"), 0600); err != nil {
				t.Fatal(err)
			}
			h.reconcileRenderJobs(context.Background(), "worker", nil, true)
			current, err = database.GetRenderQueueItem(item.ID)
			if err != nil || current.Status != "failed" {
				t.Fatalf("retry did not finish: %+v %v", current, err)
			}
			cached, err := database.GetGPUJobLog(unit)
			if err != nil || cached.Journal != "FFmpeg failure" {
				t.Fatalf("retry did not retain logs: %+v %v", cached, err)
			}
		})
	}
}

func TestRenderLogPersistenceFailurePreventsCleanup(t *testing.T) {
	database := openGPUQueueTestDB(t)
	item, err := database.EnqueueRenderJob("Review", `{"version":1,"jobs":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	unit := renderUnitName(item.ID, 1)
	if err := database.MarkRenderQueueRunning(item.ID, unit); err != nil {
		t.Fatal(err)
	}
	_, calls := renderLogTestSSH(t, "LoadState=loaded\nActiveState=failed\nResult=exit-code", "FFmpeg error\n")
	if _, err := database.Exec("DROP TABLE gpu_job_logs"); err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: database}
	if h.finishRenderAttemptWithLogs(context.Background(), "worker", item.ID, unit, "failed", "") {
		t.Fatal("completed without durable logs")
	}
	current, err := database.GetRenderQueueItem(item.ID)
	if err != nil || current.Status != "running" {
		t.Fatalf("attempt not preserved: %+v %v", current, err)
	}
	commands, _ := os.ReadFile(calls)
	if strings.Contains(string(commands), "rm -rf") {
		t.Fatal("cleanup ran despite log persistence failure")
	}
}
