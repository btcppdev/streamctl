package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"streamctl/internal/db"
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
 'rm -rf '*) [ "${TEST_CLEANUP_FAIL:-0}" = 0 ] ;;
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
			h := &Handler{DB: database, GPUWorkerHost: "worker"}
			h.reconcileRenderJobs(context.Background(), "worker", nil, true)
			current, err := database.GetRenderQueueItem(item.ID)
			if err != nil || current.Status != "failed" {
				t.Fatalf("completion: %+v %v", current, err)
			}
			h.retryPendingGPUJobLogs(context.Background())
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
			before, _ := os.ReadFile(calls)
			h.retryPendingGPUJobLogs(context.Background())
			after, _ := os.ReadFile(calls)
			if string(before) != string(after) {
				t.Fatal("completed archive was collected again")
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

func TestRenderLogCollectionFailureReleasesQueueAndPreservesEvidence(t *testing.T) {
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
			h := &Handler{DB: database, GPUWorkerHost: "worker"}
			h.reconcileRenderJobs(context.Background(), "worker", nil, true)
			current, err := database.GetRenderQueueItem(item.ID)
			if err != nil || current.Status != "failed" {
				t.Fatalf("terminal attempt blocked execution: %+v %v", current, err)
			}
			h.retryPendingGPUJobLogs(context.Background())
			pending, _ := database.GetGPUJobLog(unit)
			if !strings.HasPrefix(pending.Error, gpuLogPendingPrefix) {
				t.Fatal("missing pending archive")
			}
			commands, _ := os.ReadFile(calls)
			if strings.Contains(string(commands), "rm -rf") {
				t.Fatalf("deleted workspace without logs: %s", commands)
			}
			if err := os.WriteFile(logFile, []byte("FFmpeg failure\n"), 0600); err != nil {
				t.Fatal(err)
			}
			h.retryPendingGPUJobLogs(context.Background())
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
	h := &Handler{DB: database, GPUWorkerHost: "worker"}
	if h.finishRenderAttempt(context.Background(), "worker", item.ID, unit, "failed", "") {
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

func TestArchiveCleanupRetryDoesNotReadFullLogAgain(t *testing.T) {
	database := openGPUQueueTestDB(t)
	item, err := database.EnqueueRenderJob("Review", `{"version":1,"jobs":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	unit := renderUnitName(item.ID, 1)
	if err := database.MarkRenderQueueRunning(item.ID, unit); err != nil {
		t.Fatal(err)
	}
	_, calls := renderLogTestSSH(t, "LoadState=loaded\nActiveState=inactive\nResult=success", "complete output\n")
	h := &Handler{DB: database, GPUWorkerHost: "worker", GPUPreserveWorker: true}
	if !h.finishRenderAttempt(context.Background(), "worker", item.ID, unit, "finished", "") {
		t.Fatal("completion blocked")
	}
	t.Setenv("TEST_CLEANUP_FAIL", "1")
	h.retryPendingGPUJobLogs(context.Background())
	saved, _ := database.GetGPUJobLog(unit)
	if saved.Journal != "complete output" || !strings.HasPrefix(saved.Error, gpuLogCleanupPrefix) {
		t.Fatalf("cleanup not retained for retry: %+v", saved)
	}
	before, _ := os.ReadFile(calls)
	t.Setenv("TEST_CLEANUP_FAIL", "0")
	h.retryPendingGPUJobLogs(context.Background())
	after, _ := os.ReadFile(calls)
	if strings.Count(string(before), "journalctl") != strings.Count(string(after), "journalctl") {
		t.Fatal("cleanup retry re-read full journal")
	}
	saved, _ = database.GetGPUJobLog(unit)
	if saved.Error != "" {
		t.Fatalf("cleanup still pending: %s", saved.Error)
	}
}

func TestRenderDiagnosticModeKeepsFailedWorkspaceAndWorker(t *testing.T) {
	database := openGPUQueueTestDB(t)
	item, err := database.EnqueueRenderJob("Diagnostic retry", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	unit := renderUnitName(item.ID, 1)
	if err := database.MarkRenderQueueRunning(item.ID, unit); err != nil {
		t.Fatal(err)
	}
	_, calls := renderLogTestSSH(t, "LoadState=loaded\nActiveState=failed\nResult=exit-code", "FFmpeg exit -9\nOOM counter increased\n")
	h := &Handler{DB: database, GPUWorkerHost: "worker", GPUPreserveWorker: true, GPUDestroyAfterJob: true}
	if !h.finishRenderAttempt(context.Background(), "worker", item.ID, unit, "failed", "") {
		t.Fatal("diagnostic failure did not finish")
	}
	h.retryPendingGPUJobLogs(context.Background())
	commands, _ := os.ReadFile(calls)
	if strings.Contains(string(commands), "rm -rf") {
		t.Fatalf("diagnostic workspace was deleted: %s", commands)
	}
	current, _ := database.GetRenderQueueItem(item.ID)
	if current.Status != "failed" {
		t.Fatalf("unexpected queue state: %+v", current)
	}
	if saved, err := database.GetGPUJobLog(unit); err != nil || !strings.Contains(saved.Journal, "OOM counter increased") || saved.Error != "" {
		t.Fatalf("diagnostic log not retained: %+v %v", saved, err)
	}
	before, _ := os.ReadFile(calls)
	h.retryPendingGPUJobLogs(context.Background())
	after, _ := os.ReadFile(calls)
	if string(before) != string(after) {
		t.Fatal("preserved workspace caused repeated archival")
	}
	// Preservation takes precedence even when another old/successful job
	// triggers cleanup from the dashboard. No database/provider access is needed.
	h = &Handler{GPUPreserveWorker: true, GPUDestroyAfterJob: true, DOTokenFile: "missing-token"}
	h.destroyManagedGPUAfterTerminalJob(context.Background(), gpuJobView{ActiveState: "inactive", Result: "success"})
}

func TestCachedRunningRenderDoesNotBlockReplacementWorker(t *testing.T) {
	database := openGPUQueueTestDB(t)
	if err := database.UpsertGPUJobLog(db.GPUJobLog{
		UnitName: renderUnitName(42, 1), Host: "old-worker", ActiveState: "running", Journal: "last resource sample",
	}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: database}
	status := h.appendCachedGPUJobs(gpuStatusView{Host: "new-worker", Available: true})
	if len(status.Jobs) != 1 || !status.Jobs[0].Cached || isBlockingGPUJob(status.Jobs[0]) || isTerminalGPUJob(status.Jobs[0]) {
		t.Fatalf("cached snapshot affected live scheduling: %+v", status)
	}
	if !isBlockingGPUJob(gpuJobView{ActiveState: "running"}) {
		t.Fatal("live running job no longer blocks dispatch")
	}
}

func TestRenderReconciliationRefreshesCachedState(t *testing.T) {
	database := openGPUQueueTestDB(t)
	item, err := database.EnqueueRenderJob("Review", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	unit := renderUnitName(item.ID, 1)
	if err := database.MarkRenderQueueRunning(item.ID, unit); err != nil {
		t.Fatal(err)
	}
	_, calls := renderLogTestSSH(t, "LoadState=loaded\nActiveState=running\nResult=", "still rendering")
	h := &Handler{DB: database}
	h.reconcileRenderJobs(context.Background(), "worker", []gpuJobView{{
		UnitName: unit, ActiveState: "failed", Cached: true,
	}}, true)
	current, _ := database.GetRenderQueueItem(item.ID)
	commands, _ := os.ReadFile(calls)
	if current.Status != "running" || !strings.Contains(string(commands), "systemctl show") {
		t.Fatalf("cached status trusted without live verification: %+v %s", current, commands)
	}
}
