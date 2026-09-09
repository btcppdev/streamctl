package db

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestProductionProxyQueueLifecycleAndRetry(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	source := "toronto/recordings/raw/mix/toronto_01main_100431_0000.mp4"
	proxy := "toronto/recordings/workspace/mix/toronto_01main_100431.proxy.mp4"
	job, queued, err := database.EnqueueProductionProxyJob(source, proxy)
	if err != nil || !queued || job.Status != "queued" {
		t.Fatalf("enqueue job=%+v queued=%v err=%v", job, queued, err)
	}
	claimed, err := database.ClaimProductionProxyJob()
	if err != nil || claimed.ID != job.ID || claimed.Status != "running" || claimed.Attempts != 1 {
		t.Fatalf("claim job=%+v err=%v", claimed, err)
	}
	if err := database.FailProductionProxyJob(job.ID, errors.New("test failure")); err != nil {
		t.Fatal(err)
	}
	failed, queued, err := database.EnqueueProductionProxyJob(source, proxy)
	if err != nil || !queued || failed.Status != "queued" {
		t.Fatalf("retry job=%+v queued=%v err=%v", failed, queued, err)
	}
	claimed, err = database.ClaimProductionProxyJob()
	if err != nil {
		t.Fatal(err)
	}
	if err := database.FailProductionProxyJob(claimed.ID, errors.New("test failure again")); err != nil {
		t.Fatal(err)
	}
	if err := database.RetryProductionProxyJob(claimed.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err = database.ClaimProductionProxyJob()
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateProductionProxyJobProgress(claimed.ID, "Encoding proxy", 42); err != nil {
		t.Fatal(err)
	}
	running, err := database.ProductionProxyQueue(10)
	if err != nil || running.Running != 1 || len(running.Items) != 1 || running.Items[0].Progress != 42 || running.Items[0].Stage != "Encoding proxy" {
		t.Fatalf("running queue=%+v err=%v", running, err)
	}
	if err := database.FinishProductionProxyJob(claimed.ID, 930123); err != nil {
		t.Fatal(err)
	}
	finished, queued, err := database.EnqueueProductionProxyJob(source, proxy)
	if err != nil || !queued || finished.Status != "queued" || finished.DurationMS != 0 {
		t.Fatalf("requeued finished job=%+v queued=%v err=%v", finished, queued, err)
	}
	queue, err := database.ProductionProxyQueue(10)
	if err != nil || queue.Queued != 1 || queue.Finished != 0 || len(queue.Items) != 1 {
		t.Fatalf("finished queue=%+v err=%v", queue, err)
	}
}

func TestProductionProxyCountsAreConferenceScoped(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	for i, conference := range []string{"toronto", "toronto", "nairobi"} {
		source := conference + "/recordings/raw/mix/source" + string(rune('a'+i)) + ".mp4"
		if _, _, err := database.EnqueueProductionProxyJob(source, conference+"/recordings/workspace/test.proxy.mp4"); err != nil {
			t.Fatal(err)
		}
	}
	counts, err := database.ProductionProxyCounts("toronto")
	if err != nil || counts.Queued != 2 || counts.Running != 0 || counts.Finished != 0 || counts.Failed != 0 {
		t.Fatalf("counts=%+v err=%v", counts, err)
	}
}
