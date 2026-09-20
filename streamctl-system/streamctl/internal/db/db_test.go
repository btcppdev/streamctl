package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestStreamPersistsBTCPPRecordingID(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	id, err := database.CreateStream(&Stream{
		Name: "A talk", ScheduleType: "once", OnCalendar: "2026-08-22 10:00:00 UTC",
		BTCPPRecordingID: "recording-1", Enabled: true,
	}, nil, []string{"talk.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := database.GetStream(id)
	if err != nil {
		t.Fatal(err)
	}
	if stream.BTCPPRecordingID != "recording-1" {
		t.Fatalf("BTCPPRecordingID=%q", stream.BTCPPRecordingID)
	}
}

func TestGPUQueueTracksAttemptsAndErrors(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	item, err := database.EnqueueGPUJob("vienna/recordings/edits/talk.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if item.AttemptCount != 0 || item.LastAttemptAt != nil || item.LastError != "" {
		t.Fatalf("new queue item has unexpected attempt metadata: %#v", item)
	}

	if err := database.MarkGPUQueueRunning(item.ID, "streamctl-gpu-talk.service"); err != nil {
		t.Fatal(err)
	}
	item, err = database.GetGPUQueueItemByRawPath("vienna/recordings/edits/talk.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != "running" || item.AttemptCount != 1 || item.LastAttemptAt == nil || item.LastError != "" {
		t.Fatalf("running queue item has unexpected metadata: %#v", item)
	}

	if err := database.MarkGPUQueueFinished(item.RawPath, "failed", "ssh connection refused"); err != nil {
		t.Fatal(err)
	}
	item, err = database.GetGPUQueueItemByRawPath("vienna/recordings/edits/talk.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != "failed" || item.LastError != "ssh connection refused" {
		t.Fatalf("failed queue item did not retain error: %#v", item)
	}

	item, err = database.EnqueueGPUJob("vienna/recordings/edits/talk.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != "queued" || item.AttemptCount != 1 || item.LastError != "ssh connection refused" {
		t.Fatalf("requeued item should preserve last attempt context: %#v", item)
	}
}

func TestGPUQueueMetrics(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	queued, err := database.EnqueueGPUJob("queued.mp4")
	if err != nil {
		t.Fatal(err)
	}
	running, err := database.EnqueueGPUJob("running.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.MarkGPUQueueRunning(running.ID, "streamctl-gpu-running.service"); err != nil {
		t.Fatal(err)
	}

	metrics, err := database.GPUQueueMetrics()
	if err != nil {
		t.Fatal(err)
	}
	if metrics.Counts["queued"] != 1 || metrics.Counts["running"] != 1 || metrics.Attempts != 1 {
		t.Fatalf("unexpected queue metrics: %#v", metrics)
	}
	if metrics.OldestQueuedAt == nil || time.Since(*metrics.OldestQueuedAt) < 0 {
		t.Fatalf("missing oldest queued timestamp for item %#v: %#v", queued, metrics)
	}
}

func TestRenderQueueLifecycleAndRetry(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	item, err := database.EnqueueRenderJob("opening titles", `{"project":{"name":"opening"},"timeline":{"duration_frames":48,"fps":24},"output":{"path":"/output/opening.mp4","width":1920,"height":1080,"format":"mp4","codec":"h264"},"scenes":[{"id":"one","start_frame":0,"end_frame":48,"background":{"color":"#000000"},"layers":[]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != "queued" || item.AttemptCount != 0 {
		t.Fatalf("unexpected new item: %+v", item)
	}

	if err := database.MarkRenderQueueRunning(item.ID, "streamctl-render-opening-1.service"); err != nil {
		t.Fatal(err)
	}
	running, err := database.GetRenderQueueItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if running.Status != "running" || running.AttemptCount != 1 || running.UnitName == "" || running.StartedAt == nil {
		t.Fatalf("unexpected running item: %+v", running)
	}

	if err := database.MarkRenderQueueFinished(item.ID, "failed", "renderer exited 1"); err != nil {
		t.Fatal(err)
	}
	if err := database.ResetRenderJobForRetry(item.ID, "manual retry"); err != nil {
		t.Fatal(err)
	}
	retried, err := database.NextQueuedRenderJob()
	if err != nil {
		t.Fatal(err)
	}
	if retried.ID != item.ID || retried.Status != "queued" || retried.UnitName != "" || retried.AttemptCount != 1 {
		t.Fatalf("unexpected retried item: %+v", retried)
	}

	counts, err := database.RenderQueueStatusCounts()
	if err != nil {
		t.Fatal(err)
	}
	if counts["queued"] != 1 {
		t.Fatalf("unexpected counts: %#v", counts)
	}
}

func TestRenderQueueCancellationAndSubmissionError(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	queued, err := database.EnqueueRenderJob("queued", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CancelRenderJob(queued.ID, "cancelled by user"); err != nil {
		t.Fatal(err)
	}
	cancelled, err := database.GetRenderQueueItem(queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != "cancelled" || cancelled.FinishedAt == nil || cancelled.LastError != "cancelled by user" {
		t.Fatalf("unexpected cancelled item: %+v", cancelled)
	}

	running, err := database.EnqueueRenderJob("running", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.MarkRenderQueueRunning(running.ID, "streamctl-gpu-render-2-attempt-1.service"); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRenderQueueError(running.ID, "submission response lost"); err != nil {
		t.Fatal(err)
	}
	running, err = database.GetRenderQueueItem(running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if running.Status != "running" || running.LastError != "submission response lost" {
		t.Fatalf("ambiguous submission should remain running: %+v", running)
	}
}

func TestMarkGPUQueueFinishedReportsNoOpenRow(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	item, err := database.EnqueueGPUJob("vienna/recordings/edits/talk.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.MarkGPUQueueRunning(item.ID, "streamctl-gpu-talk.service"); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkGPUQueueFinished(item.RawPath, "finished", ""); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkGPUQueueFinished(item.RawPath, "failed", "duplicate terminal event"); err != sql.ErrNoRows {
		t.Fatalf("expected sql.ErrNoRows for duplicate terminal update, got %v", err)
	}
}

func TestResolveGPUQueueFailure(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	item, err := database.EnqueueGPUJob("failed.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.MarkGPUQueueRunning(item.ID, "streamctl-gpu-failed.service"); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkGPUQueueFinished(item.RawPath, "failed", "worker exited"); err != nil {
		t.Fatal(err)
	}
	if err := database.ResolveGPUQueueFailure(item.RawPath, "verified output"); err != nil {
		t.Fatal(err)
	}
	resolved, err := database.GetGPUQueueItemByRawPath(item.RawPath)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != "finished" || resolved.LastError != "verified output" {
		t.Fatalf("unexpected resolved queue item: %#v", resolved)
	}
}

func TestUpsertGPUJobLogKeepsExistingDetailWhenRefreshIsSparse(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	unit := "streamctl-gpu-talk.service"
	if err := database.UpsertGPUJobLog(GPUJobLog{
		UnitName:    unit,
		RawPath:     "vienna/recordings/edits/talk.mp4",
		Host:        "ssh://root@example",
		Description: "streamctl GPU transcode vienna/recordings/edits/talk.mp4",
		ActiveState: "failed",
		SubState:    "failed",
		Result:      "exit-code",
		Journal:     "rclone: object not found",
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertGPUJobLog(GPUJobLog{
		UnitName: unit,
		Error:    "remote job metadata: connection refused",
	}); err != nil {
		t.Fatal(err)
	}

	job, err := database.GetGPUJobLog(unit)
	if err != nil {
		t.Fatal(err)
	}
	if job.RawPath != "vienna/recordings/edits/talk.mp4" || job.Result != "exit-code" || job.Journal != "rclone: object not found" {
		t.Fatalf("sparse refresh should preserve previous job detail: %#v", job)
	}
	if job.Error != "remote job metadata: connection refused" {
		t.Fatalf("sparse refresh should record new error: %#v", job)
	}
}

func TestResetGPUJobForRetryHandlesQueuedJobs(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	item, err := database.EnqueueGPUJob("vienna/recordings/edits/talks/stuck.mov")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ResetGPUJobForRetry(item.RawPath, "manual retry"); err != nil {
		t.Fatal(err)
	}
	item, err = database.GetGPUQueueItemByRawPath(item.RawPath)
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != "queued" || item.UnitName != "" || item.LastError != "manual retry" || item.AttemptCount != 0 {
		t.Fatalf("queued retry reset produced unexpected item: %#v", item)
	}
}

func TestVisibleGPUQueueItemsExcludeFinished(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	finished, err := database.EnqueueGPUJob("vienna/recordings/edits/finished.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.MarkGPUQueueFinished(finished.RawPath, "finished", ""); err != nil {
		t.Fatal(err)
	}
	failed, err := database.EnqueueGPUJob("vienna/recordings/edits/failed.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.MarkGPUQueueFinished(failed.RawPath, "failed", "boom"); err != nil {
		t.Fatal(err)
	}

	visible, err := database.ListVisibleGPUQueueItems(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 || visible[0].RawPath != failed.RawPath {
		t.Fatalf("visible items should include failed but not finished: %#v", visible)
	}
	failedItems, err := database.ListFailedGPUQueueItems(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(failedItems) != 1 || failedItems[0].RawPath != failed.RawPath {
		t.Fatalf("failed items mismatch: %#v", failedItems)
	}
}

func TestVisibleRenderQueueItemsIncludeFinishedHistory(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	finished, err := database.EnqueueRenderJob("finished", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.MarkRenderQueueRunning(finished.ID, "finished.service"); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkRenderQueueFinished(finished.ID, "finished", ""); err != nil {
		t.Fatal(err)
	}
	failed, err := database.EnqueueRenderJob("failed", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.MarkRenderQueueRunning(failed.ID, "failed.service"); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkRenderQueueFinished(failed.ID, "failed", "boom"); err != nil {
		t.Fatal(err)
	}

	visible, err := database.ListVisibleRenderQueueItems(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 2 || visible[0].ID != failed.ID || visible[1].ID != finished.ID {
		t.Fatalf("visible items should include failures and finished history: %#v", visible)
	}
}

func TestStreamConferenceBroadcastTargetPersists(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	s := &Stream{Name: "Day 3", ScheduleType: "once", OnCalendar: "2026-09-20 12:00:00 UTC", BTCPPConference: "toronto", Enabled: true}
	id, err := database.CreateStream(s, nil, []string{"day3.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := database.GetStream(id)
	if err != nil || got.BTCPPConference != "toronto" || got.BTCPPRecordingID != "" {
		t.Fatalf("stream=%+v err=%v", got, err)
	}
	got.BTCPPConference = ""
	got.BTCPPRecordingID = "recording-1"
	if err := database.UpdateStream(got, nil, []string{"talk.mp4"}); err != nil {
		t.Fatal(err)
	}
	all, err := database.ListStreams()
	if err != nil || len(all) != 1 || all[0].BTCPPConference != "" || all[0].BTCPPRecordingID != "recording-1" {
		t.Fatalf("streams=%+v err=%v", all, err)
	}
}
