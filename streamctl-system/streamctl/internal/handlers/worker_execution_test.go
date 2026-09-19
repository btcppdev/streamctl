package handlers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type podCreationTransport func(*http.Request) (*http.Response, error)

func (f podCreationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestConcurrentWorkerCreationReconcilesLostResponse(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{"token": "test-token", "key.pub": "ssh-ed25519 test", "rclone": "[test]\ntype=local\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handler{RunPodTokenFile: filepath.Join(root, "token"), GPUWorkerSSHKey: filepath.Join(root, "key"), RcloneConfig: filepath.Join(root, "rclone"), RunPodPodName: "test-worker"}
	var mu sync.Mutex
	created, posts := false, 0
	previous := http.DefaultTransport
	http.DefaultTransport = podCreationTransport(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			created = true
			posts++
			return nil, errors.New("response lost after creation")
		}
		body := "[]"
		if created {
			body = `[{"id":"existing","name":"test-worker"}]`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	var calls sync.WaitGroup
	errorsSeen := make(chan error, 8)
	for i := 0; i < 8; i++ {
		calls.Add(1)
		go func() {
			defer calls.Done()
			errorsSeen <- h.ensureRunPodWorker(context.Background(), "test-gpu")
		}()
	}
	calls.Wait()
	close(errorsSeen)
	failed := 0
	for err := range errorsSeen {
		if err != nil {
			failed++
		}
	}
	if posts != 1 || failed != 1 {
		t.Fatalf("posts=%d errors=%d; want one ambiguous creation, then reconciliation", posts, failed)
	}
}

func TestOldRenderAttemptCannotFinishNewAttempt(t *testing.T) {
	database := openGPUQueueTestDB(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	item, err := database.EnqueueRenderJob("Review", `{"version":1,"jobs":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	oldUnit, newUnit := renderUnitName(item.ID, 1), renderUnitName(item.ID, 2)
	for _, err := range []error{
		database.MarkRenderQueueRunning(item.ID, oldUnit),
		database.MarkRenderQueueFinished(item.ID, "failed", "old failure"),
		database.ResetRenderJobForRetry(item.ID, "retry"),
		database.MarkRenderQueueRunning(item.ID, newUnit),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	h := &Handler{DB: database}
	h.reconcileRenderJobs(context.Background(), "unused", []gpuJobView{{UnitName: oldUnit, ActiveState: "failed", Result: "exit-code"}}, true)
	current, err := database.GetRenderQueueItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "running" {
		t.Fatalf("old attempt changed current attempt to %s", current.Status)
	}
	if err := database.FinishRenderAttempt(item.ID, oldUnit, "finished", ""); err == nil {
		t.Fatal("stale completion update succeeded")
	}
	if err := database.FinishRenderAttempt(item.ID, newUnit, "finished", ""); err != nil {
		t.Fatal(err)
	}
}

func TestMissingSystemdUnitIsNotACompletedRender(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nprintf 'LoadState=not-found\\nActiveState=inactive\\nResult=success\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	job := (&Handler{}).gpuJob(context.Background(), "unused", renderUnitName(1, 1), false)
	if job.LoadedState != "not-found" || isTerminalGPUJob(job) {
		t.Fatalf("missing unit interpreted as completed: %+v", job)
	}
}

func TestWorkerStartupAndExecutionAreBounded(t *testing.T) {
	setup := boundedWorkerSetup("echo 'renderer check'")
	for _, want := range []string{"20m", ".streamctl-worker-setup-failed", "streamctl-worker-setup.log", "sshd -D"} {
		if !strings.Contains(setup, want) {
			t.Errorf("setup missing %q", want)
		}
	}
	launch := remoteRenderLaunchCommand(renderUnitName(1, 1), "run-render")
	for _, want := range []string{"RuntimeMaxSec=48h", "timeout --kill-after=30s 48h"} {
		if !strings.Contains(launch, want) {
			t.Errorf("launch missing %q", want)
		}
	}
}

// macOS lacks the Linux utility names, but supports the same flock and session
// primitives. These tiny test adapters exercise the real launcher on both OSes.
func containerTestTools(t *testing.T) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required for container launcher tests")
	}
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("GNU timeout required")
	}
	bin := t.TempDir()
	for name, script := range map[string]string{
		"systemctl": "#!/bin/sh\nexit 1\n",
		"flock":     "#!" + python + "\nimport fcntl,sys\ntry: fcntl.flock(int(sys.argv[-1]),fcntl.LOCK_EX|fcntl.LOCK_NB)\nexcept BlockingIOError: sys.exit(1)\n",
		"setsid":    "#!" + python + "\nimport os,sys\nos.setsid()\nos.execvp(sys.argv[1],sys.argv[1:])\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestContainerRenderLifecycle(t *testing.T) {
	containerTestTools(t)
	root := t.TempDir()
	t.Setenv("STREAMCTL_GPU_JOB_ROOT", root)
	unit := renderUnitName(42, 1)
	count := filepath.Join(root, "starts")
	run := func(script string) {
		t.Helper()
		if out, err := exec.Command("bash", "-c", script).CombinedOutput(); err != nil {
			t.Fatalf("command: %v: %s", err, out)
		}
	}
	launch := remoteRenderLaunchCommand(unit, "printf 'started\\n' >> "+shellQuote(count)+"; sleep 0.2")
	run(launch)
	waitForGPUStartCount(t, count, 1)
	run(launch)
	deadline := time.Now().Add(5 * time.Second)
	for {
		result, _ := os.ReadFile(filepath.Join(root, unit, "result"))
		if strings.TrimSpace(string(result)) == "success" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not finish: %s", result)
		}
		time.Sleep(20 * time.Millisecond)
	}
	run(launch) // Completed attempts are idempotent too.
	data, _ := os.ReadFile(count)
	if strings.Count(string(data), "started") != 1 {
		t.Fatalf("duplicate execution: %s", data)
	}
	second := renderUnitName(42, 2)
	run(remoteRenderLaunchCommand(second, "printf 'started\\n' >> "+shellQuote(count)+"; sleep 60"))
	t.Cleanup(func() { exec.Command("bash", "-c", remoteGPUStopCommand(second)).Run() })
	waitForGPUStartCount(t, count, 2)
	run(remoteGPUStopCommand(second))
	result, _ := os.ReadFile(filepath.Join(root, second, "result"))
	if strings.TrimSpace(string(result)) != "cancelled" {
		t.Fatalf("cancel: %s", result)
	}
}

func TestContainerTimeoutStopsDescendants(t *testing.T) {
	containerTestTools(t)
	root := t.TempDir()
	t.Setenv("STREAMCTL_GPU_JOB_ROOT", root)
	escaped := filepath.Join(root, "escaped")
	unit := renderUnitName(43, 1)
	launch := remoteRenderLaunchCommand(unit, "sleep 0.8; printf 'escaped' > "+shellQuote(escaped))
	// Exercise the production launcher with a short deadline, not a 48-hour wait.
	launch = strings.Replace(launch, " 48h ", " 0.2s ", 1)
	if out, err := exec.Command("bash", "-c", launch).CombinedOutput(); err != nil {
		t.Fatalf("launch: %v: %s", err, out)
	}
	t.Cleanup(func() { exec.Command("bash", "-c", remoteGPUStopCommand(unit)).Run() })
	time.Sleep(time.Second)
	if _, err := os.Stat(escaped); !os.IsNotExist(err) {
		t.Fatalf("render subprocess escaped timeout: %v", err)
	}
}
