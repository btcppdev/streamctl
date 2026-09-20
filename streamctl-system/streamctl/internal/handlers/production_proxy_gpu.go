package handlers

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"streamctl/internal/db"
)

//go:embed worker/prepare-proxy.py
var proxyWorkerScript string

func proxyWorkspace(job db.ProductionProxyJob) string {
	return "/workspace/streamctl-proxy-jobs/" + job.WorkerUnit
}

type proxyWorkerStatus struct {
	State      string `json:"state"`
	Stage      string `json:"stage"`
	Progress   int    `json:"progress"`
	DurationMS int64  `json:"durationMs"`
	Error      string `json:"error"`
}

// The GPU dispatcher owns all claims; the old CPU dispatcher must never run in
// parallel. A persisted attempt is reconciled before any new GPU work starts.
func (h *Handler) dispatchNextProxy(ctx context.Context, host string) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	queue, err := h.DB.ProductionProxyQueue(1)
	if err != nil {
		log.Printf("loading preview queue: %v", err)
		return true
	}
	if queue.Queued == 0 {
		return false
	}
	if out, err := waitForRemoteSSH(ctx, host, 2*time.Minute); err != nil {
		log.Printf("preview worker readiness: %v: %s", err, out)
		return true
	}
	job, err := h.DB.ClaimProductionProxyGPUJob(host)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		log.Printf("claiming GPU preview: %v", err)
		return true
	}
	if err := h.startRemoteProxy(ctx, job); err != nil {
		// SSH can fail after the worker accepted the launch. Only a reliable absence
		// permits marking it failed; otherwise keep it running and reconcile later.
		present, reliable := h.remoteProxyPresence(ctx, job)
		if reliable && !present {
			_ = h.DB.UpdateProductionProxyAttempt(job, "failed", "Failed", 0, 0, err.Error())
		} else {
			_ = h.DB.UpdateProductionProxyAttempt(job, "running", "Checking worker after submission error", 0, 0, err.Error())
		}
		log.Printf("submitting preview %d: %v", job.ID, err)
	}
	return true
}

func (h *Handler) startRemoteProxy(ctx context.Context, job db.ProductionProxyJob) error {
	source, err := h.logicalMediaSource(ctx, job.Source)
	if err != nil {
		return fmt.Errorf("locate source sequence: %w", err)
	}
	chunks := source.Chunks
	if len(chunks) == 0 {
		chunks = []string{source.Path}
	}
	request, err := json.Marshal(map[string]any{
		"source": job.Source, "sourceType": source.SourceType, "chunks": chunks,
		"proxy": job.Proxy, "sidecar": productionProxySidecarObjectKey(job.Proxy),
		"remote": strings.TrimRight(h.Remote, "/"), "height": productionProxyHeight,
		"version": productionProxyArtifactVersion,
	})
	if err != nil {
		return err
	}
	workspace := proxyWorkspace(job)
	stage := "set -eu; umask 077; mkdir -p " + shellQuote(workspace) +
		"; printf '%s' " + shellQuote(base64.StdEncoding.EncodeToString([]byte(proxyWorkerScript))) +
		" | base64 -d > " + shellQuote(workspace+"/prepare-proxy.py") +
		"; cat > " + shellQuote(workspace+"/request.json")
	if out, err := remoteSSHInput(ctx, job.WorkerHost, stage, string(request)); err != nil {
		return fmt.Errorf("stage preview: %w: %s", err, out)
	}
	invocation := "exec python3 " + shellQuote(workspace+"/prepare-proxy.py") + " " + shellQuote(workspace)
	if out, err := remoteSSH(ctx, job.WorkerHost, remoteGPUJobCommand(job.WorkerUnit, "streamctl editing preview", "", invocation)); err != nil {
		return fmt.Errorf("launch preview: %w: %s", err, out)
	}
	return nil
}

func (h *Handler) remoteProxyPresence(ctx context.Context, job db.ProductionProxyJob) (bool, bool) {
	command := "unit=" + shellQuote(job.WorkerUnit) +
		"; if [ -f " + shellQuote(proxyWorkspace(job)+"/status.json") +
		" ] || [ -f \"${STREAMCTL_GPU_JOB_ROOT:-/root/streamctl-gpu-jobs}/$unit/pid\" ]; then printf 'present\\n'; exit 0; fi; " +
		"state=$(systemctl show \"$unit\" --property=LoadState --value 2>/dev/null || true); " +
		"if [ -n \"$state\" ] && [ \"$state\" != not-found ]; then printf 'present\\n'; else printf 'absent\\n'; fi"
	out, err := remoteSSH(ctx, job.WorkerHost, command)
	if err != nil {
		return false, false
	}
	switch strings.TrimSpace(out) {
	case "present":
		return true, true
	case "absent":
		return false, true
	default:
		return false, false
	}
}

func (h *Handler) reconcileProxyJobs(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	jobs, err := h.DB.RunningProductionProxyJobs()
	if err != nil {
		log.Printf("listing running previews: %v", err)
		return
	}
	for _, job := range jobs {
		if job.WorkerUnit == "" {
			continue
		}
		out, readErr := remoteSSH(ctx, job.WorkerHost, "if [ -f "+shellQuote(proxyWorkspace(job)+"/status.json")+
			" ]; then cat "+shellQuote(proxyWorkspace(job)+"/status.json")+"; fi")
		if readErr != nil {
			continue
		} // An unreachable worker is not a failed job.
		var status proxyWorkerStatus
		if strings.TrimSpace(out) != "" {
			if err := json.Unmarshal([]byte(out), &status); err != nil {
				log.Printf("reading preview %d status: %v", job.ID, err)
				continue
			}
		}
		remoteJob := h.gpuJob(ctx, job.WorkerHost, job.WorkerUnit, false)
		if remoteJob.Error != "" && !isTerminalGPUJob(remoteJob) {
			continue
		}
		// Even after the ready marker is uploaded, wait until the process exits so
		// two GPU jobs cannot overlap while the first cleans up its workspace.
		if isBlockingGPUJob(remoteJob) {
			if status.State == "running" {
				_ = h.DB.UpdateProductionProxyAttempt(job, "running", status.Stage, status.Progress, 0, "")
			}
			continue
		}
		if status.State == "finished" && status.DurationMS > 0 {
			metadata, err := h.readProductionProxyMetadata(ctx, job.Proxy)
			if err != nil {
				continue
			} // Retry a transient Spaces read without re-encoding.
			if metadata.Source.Path != job.Source || metadata.Proxy.DurationMS != status.DurationMS {
				status.State, status.Error = "failed", "preview metadata does not match the submitted source"
			} else if present, checked := h.productionProxyArtifactPresent(ctx, job.Proxy); !checked || !present {
				continue
			}
		} else if status.State != "failed" {
			if isTerminalGPUJob(remoteJob) {
				full := h.gpuJob(ctx, job.WorkerHost, job.WorkerUnit, true)
				status.State, status.Error = "failed", firstNonEmptyString(gpuFailureJournalSummary(full.Journal), "worker stopped before completing the preview")
			} else if remoteJob.LoadedState == "not-found" {
				status.State, status.Error = "failed", "worker stopped before completing the preview; requeue to retry"
			} else {
				present, reliable := h.remoteProxyPresence(ctx, job)
				if present || !reliable {
					continue
				}
				status.State, status.Error = "failed", "worker did not retain the submitted preview; requeue to retry"
			}
		}
		if status.State != "finished" && status.State != "failed" {
			continue
		}
		stage, progress := "Failed", 0
		if status.State == "finished" {
			stage, progress = "Complete", 100
		}
		if err := h.DB.UpdateProductionProxyAttempt(job, status.State, stage, progress, status.DurationMS, status.Error); err != nil {
			continue
		}
		_, _ = remoteSSH(ctx, job.WorkerHost, "rm -rf -- "+shellQuote(proxyWorkspace(job)))
		h.destroyManagedGPUAfterTerminalJob(ctx, renderTerminalGPUJob(job.WorkerUnit, status.State))
	}
}

func (h *Handler) reconcileUnavailableProxyQueue(worker gpuWorkerView) {
	if !managedGPUWorkerDefinitelyUnavailable(worker) {
		return
	}
	jobs, err := h.DB.RunningProductionProxyJobs()
	if err != nil {
		log.Printf("listing unavailable previews: %v", err)
		return
	}
	for _, job := range jobs {
		_ = h.DB.UpdateProductionProxyAttempt(job, "queued", "Waiting for GPU worker", 0, 0, "managed worker is unavailable; requeued automatically")
	}
}
