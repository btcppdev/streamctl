package handlers

import (
	"context"
	"os"
	"path/filepath"
	"streamctl/internal/db"
	"strings"
	"testing"
	"time"
)

// Reproduce the production replacement-worker state without contacting a worker.
func TestReplacementWorkerDispatchIgnoresArchivedRunningLog(t *testing.T) {
	for _, archived := range []bool{false, true} {
		name := "empty history"
		if archived {
			name = "running history from old worker"
		}
		t.Run(name, func(t *testing.T) {
			database := openGPUQueueTestDB(t)
			item, err := database.EnqueueRenderJob("Pending talk", `{"version":1,"jobs":[]}`)
			if err != nil {
				t.Fatal(err)
			}
			if archived {
				err = database.UpsertGPUJobLog(db.GPUJobLog{
					UnitName: renderUnitName(item.ID, 1), Host: "old-worker",
					ActiveState: "running", SubState: "running", Journal: "download started",
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\ncase \"$*\" in\n *\"cat >\"*) cat >/dev/null ;;\nesac\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			h := &Handler{DB: database, Remote: "test:bucket", GPUWorkerHost: "new-worker", RenderWorkerCommand: "/render", RenderOutputDir: "/output"}
			h.dispatchGPUQueueOnce(context.Background())
			current, err := database.GetRenderQueueItem(item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Status != "running" {
				t.Fatalf("healthy replacement worker did not dispatch queued talk: status=%s; archivedRunning=%v", current.Status, archived)
			}
		})
	}
}

func TestLiveWorkerJobStillBlocksDispatch(t *testing.T) {
	database := openGPUQueueTestDB(t)
	item, err := database.EnqueueRenderJob("Pending", `{"version":1,"jobs":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := `#!/bin/sh
case "$*" in
 *"systemctl list-units"*) printf 'streamctl-gpu-render-999-attempt-1.service loaded active running\n' ;;
 *"systemctl show"*) printf 'LoadState=loaded\nActiveState=active\nSubState=running\n' ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := &Handler{DB: database, Remote: "test:bucket", GPUWorkerHost: "worker", RenderWorkerCommand: "/render", RenderOutputDir: "/output"}
	h.dispatchGPUQueueOnce(context.Background())
	current, _ := database.GetRenderQueueItem(item.ID)
	if current.Status != "queued" {
		t.Fatal("overlapping dispatch despite a live worker job")
	}
}

func TestSSHCommandsRespectShorterCallerDeadline(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nexec sleep 10\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, input := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		started := time.Now()
		var err error
		if input {
			_, err = remoteSSHInput(ctx, "worker", "cat", "")
		} else {
			_, err = remoteSSH(ctx, "worker", "true")
		}
		cancel()
		if err == nil || time.Since(started) > time.Second {
			t.Fatalf("SSH input=%v did not honor caller deadline: %v", input, err)
		}
	}
}

func TestArchiveFailureDoesNotBlockNextRender(t *testing.T) {
	database := openGPUQueueTestDB(t)
	first, err := database.EnqueueRenderJob("Finished", `{"version":1,"jobs":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	unit := renderUnitName(first.ID, 1)
	if err := database.MarkRenderQueueRunning(first.ID, unit); err != nil {
		t.Fatal(err)
	}
	second, err := database.EnqueueRenderJob("Next", `{"version":1,"jobs":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := `#!/bin/sh
case "$*" in
 *"systemctl show"*) printf 'LoadState=loaded\nActiveState=failed\nResult=exit-code\n' ;;
 *"journalctl"*) exit 1 ;;
 *"cat >"*) cat >/dev/null ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := &Handler{DB: database, Remote: "test:bucket", GPUWorkerHost: "worker", RenderWorkerCommand: "/render", RenderOutputDir: "/output"}
	if !h.finishRenderAttempt(context.Background(), "worker", first.ID, unit, "failed", "") {
		t.Fatal("could not finish confirmed terminal attempt")
	}
	h.retryPendingGPUJobLogs(context.Background())
	h.dispatchGPUQueueOnce(context.Background())
	current, _ := database.GetRenderQueueItem(second.ID)
	if current.Status != "running" {
		t.Fatalf("pending log archive blocked next render: %s", current.Status)
	}
	saved, _ := database.GetGPUJobLog(unit)
	if !strings.HasPrefix(saved.Error, gpuLogPendingPrefix) {
		t.Fatal("failed archive was discarded")
	}
}
