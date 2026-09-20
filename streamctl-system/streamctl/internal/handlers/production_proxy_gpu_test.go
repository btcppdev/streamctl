package handlers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestUnavailableWorkerRequeuesPreviewButStartingWorkerDoesNot(t *testing.T) {
	database := openGPUQueueTestDB(t)
	_, _, err := database.EnqueueProductionProxyJob("event/recordings/source.mp4", "event/recordings/workspace/source.proxy.mp4")
	if err != nil {
		t.Fatal(err)
	}
	job, err := database.ClaimProductionProxyGPUJob("worker")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: database}
	h.reconcileUnavailableProxyQueue(gpuWorkerView{Managed: true, Status: "starting"})
	current, _ := database.ProductionProxyJobBySource(job.Source)
	if current.Status != "running" {
		t.Fatalf("starting worker discarded job: %+v", current)
	}
	h.reconcileUnavailableProxyQueue(gpuWorkerView{Managed: true, Status: "not found"})
	current, _ = database.ProductionProxyJobBySource(job.Source)
	if current.Status != "queued" {
		t.Fatalf("lost worker did not requeue job: %+v", current)
	}
}

func TestPreviewReconciliationKeepsRunningAndUnreachableJobs(t *testing.T) {
	for _, script := range []string{
		"#!/bin/sh\nexit 255\n",
		`#!/bin/sh
case "$*" in
 *status.json*) printf '%s' '{"state":"finished","durationMs":1000}' ;;
 *"systemctl show"*) printf 'LoadState=loaded\nActiveState=active\nResult=success\n' ;;
esac
`,
	} {
		t.Run(script, func(t *testing.T) {
			database := openGPUQueueTestDB(t)
			_, _, err := database.EnqueueProductionProxyJob("event/recordings/source.mp4", "event/recordings/workspace/source.proxy.mp4")
			if err != nil {
				t.Fatal(err)
			}
			job, err := database.ClaimProductionProxyGPUJob("worker")
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			h := &Handler{DB: database}
			h.reconcileProxyJobs(context.Background())
			current, _ := database.ProductionProxyJobBySource(job.Source)
			if current.Status != "running" {
				t.Fatalf("job lost its queue lock: %+v", current)
			}
		})
	}
}

func TestCollectedPreviewWithoutCompletionFailsInsteadOfBlockingQueue(t *testing.T) {
	database := openGPUQueueTestDB(t)
	_, _, err := database.EnqueueProductionProxyJob("event/recordings/source.mp4", "event/recordings/workspace/source.proxy.mp4")
	if err != nil {
		t.Fatal(err)
	}
	job, err := database.ClaimProductionProxyGPUJob("worker")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := `#!/bin/sh
case "$*" in
 *status.json*) printf '%s' '{"state":"running","stage":"Encoding proxy on GPU","progress":30}' ;;
 *"systemctl show"*) printf 'LoadState=not-found\nActiveState=inactive\nResult=success\n' ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := &Handler{DB: database}
	h.reconcileProxyJobs(context.Background())
	current, _ := database.ProductionProxyJobBySource(job.Source)
	if current.Status != "failed" {
		t.Fatalf("lost process blocks the queue: %+v", current)
	}
}

func TestFinishedPreviewRequiresMatchingUploadedMetadata(t *testing.T) {
	for _, source := range []string{"event/recordings/source.mp4", "event/recordings/wrong.mp4"} {
		t.Run(source, func(t *testing.T) {
			database := openGPUQueueTestDB(t)
			_, _, err := database.EnqueueProductionProxyJob("event/recordings/source.mp4", "event/recordings/workspace/source.proxy.mp4")
			if err != nil {
				t.Fatal(err)
			}
			job, err := database.ClaimProductionProxyGPUJob("worker")
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			scripts := map[string]string{
				"ssh": `#!/bin/sh
case "$*" in
 *status.json*) printf '%s' '{"state":"finished","durationMs":1000}' ;;
 *"systemctl show"*) printf 'LoadState=not-found\nActiveState=inactive\nResult=success\n' ;;
esac
`,
				"rclone": `#!/bin/sh
case "$*" in
 *cat*) printf '%s' '{"version":1,"source":{"path":"` + source + `"},"proxy":{"path":"event/recordings/workspace/source.proxy.mp4","durationMs":1000}}' ;;
 *lsf*) printf 'source.proxy.mp4\nsource.proxy.v1.json\n' ;;
esac
`,
			}
			for name, script := range scripts {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			h := &Handler{DB: database, Remote: "test:bucket"}
			h.reconcileProxyJobs(context.Background())
			current, _ := database.ProductionProxyJobBySource(job.Source)
			want := "finished"
			if source != job.Source {
				want = "failed"
			}
			if current.Status != want {
				t.Fatalf("status=%s want=%s: %+v", current.Status, want, current)
			}
		})
	}
}

func TestGPUDispatcherPrioritizesPreviewAndSerializesFollowingWork(t *testing.T) {
	database := openGPUQueueTestDB(t)
	source := "event/recordings/source.mp4"
	_, _, err := database.EnqueueProductionProxyJob(source, "event/recordings/workspace/source.proxy.mp4")
	if err != nil {
		t.Fatal(err)
	}
	normalize, err := database.EnqueueGPUJob("event/recordings/raw.mp4")
	if err != nil {
		t.Fatal(err)
	}
	render, err := database.EnqueueRenderJob("Later render", `{"version":1,"jobs":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	scripts := map[string]string{
		"ssh": `#!/bin/sh
case "$*" in
 *"cat >"*) cat >/dev/null ;;
 *status.json*) printf '%s' '{"state":"running","stage":"Encoding proxy on GPU","progress":30}' ;;
 *"systemctl show"*) printf 'LoadState=loaded\nActiveState=active\nResult=success\n' ;;
esac
`,
		"rclone": "#!/bin/sh\nprintf 'source.mp4\\n'\n",
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := &Handler{DB: database, Remote: "test:bucket", GPUWorkerHost: "worker", GPUWorkerCommand: "/normalize", RenderWorkerCommand: "/render"}
	h.dispatchGPUQueueOnce(context.Background())
	current, _ := database.ProductionProxyJobBySource(source)
	if current.Status != "running" || current.WorkerUnit == "" {
		t.Fatalf("preview was not dispatched first: %+v", current)
	}
	// The recent-job list is empty in this fake worker. Persisted running state
	// must still prevent another job from starting on the next dispatcher pass.
	h.dispatchGPUQueueOnce(context.Background())
	current, _ = database.ProductionProxyJobBySource(source)
	if current.Progress != 30 || current.Attempts != 1 {
		t.Fatalf("running preview was not reconciled: %+v", current)
	}
	normalizeNow, _ := database.GetGPUQueueItemByRawPath(normalize.RawPath)
	renderNow, _ := database.GetRenderQueueItem(render.ID)
	if normalizeNow.Status != "queued" || renderNow.Status != "queued" {
		t.Fatalf("overlapping GPU jobs: normalize=%s render=%s", normalizeNow.Status, renderNow.Status)
	}
}
