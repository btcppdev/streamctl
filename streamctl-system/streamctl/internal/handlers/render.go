package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"streamctl/internal/db"
)

const maxRenderManifestBytes = 2 << 20

const renderSubmissionStaleAfter = 2 * time.Minute

type renderManifestEnvelope struct {
	Version  int             `json:"version"`
	Jobs     []renderJobStub `json:"jobs"`
	Settings json.RawMessage `json:"settings"`
}

type renderJobStub struct {
	ID       string              `json:"id"`
	Segments []renderSegmentStub `json:"segments"`
}

type renderSegmentStub struct {
	Type    string `json:"type"`
	Src     string `json:"src"`
	Overlay string `json:"overlay"`
	Audio   *struct {
		Src string `json:"src"`
	} `json:"audio"`
}

type renderQueueItemView struct {
	ID           int64
	Name         string
	UnitName     string
	Status       string
	AttemptCount int
	LastError    string
	CreatedAt    string
	StartedAt    string
	FinishedAt   string
	OutputPrefix string
}

type renderJobsView struct {
	Items     []renderQueueItemView
	Queued    int
	Running   int
	Failed    int
	Finished  int
	Cancelled int
	Error     string
}

func validateRenderManifest(raw []byte) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return errors.New("manifest is required")
	}
	if len(raw) > maxRenderManifestBytes {
		return fmt.Errorf("manifest exceeds %d bytes", maxRenderManifestBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var manifest renderManifestEnvelope
	if err := dec.Decode(&manifest); err != nil {
		return fmt.Errorf("invalid manifest JSON: %w", err)
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return errors.New("manifest must contain one JSON object")
	}
	if manifest.Version != 1 {
		return fmt.Errorf("unsupported manifest version %d", manifest.Version)
	}
	if len(manifest.Jobs) == 0 {
		return errors.New("manifest must contain at least one job")
	}
	seen := map[string]bool{}
	conference := ""
	for _, job := range manifest.Jobs {
		id := strings.TrimSpace(job.ID)
		if id == "" {
			return errors.New("every manifest job must have an id")
		}
		if seen[id] {
			return fmt.Errorf("duplicate manifest job id %q", id)
		}
		seen[id] = true
		if len(job.Segments) == 0 {
			return fmt.Errorf("manifest job %q must contain at least one segment", id)
		}
		for _, segment := range job.Segments {
			switch segment.Type {
			case "image", "video", "chunkedVideo":
			default:
				return fmt.Errorf("every segment in manifest job %q must have type image, video, or chunkedVideo", id)
			}
			if strings.TrimSpace(segment.Src) == "" {
				return fmt.Errorf("every segment in manifest job %q must have a source object key", id)
			}
			sources := []string{segment.Src, segment.Overlay}
			if segment.Audio != nil {
				sources = append(sources, segment.Audio.Src)
			}
			for _, source := range sources {
				if source == "" {
					continue
				}
				clean, err := validateRenderObjectKey(source)
				if err != nil {
					return err
				}
				currentConference := strings.SplitN(clean, "/", 2)[0]
				if conference == "" {
					conference = currentConference
				} else if currentConference != conference {
					return fmt.Errorf("all render sources must belong to one conference; found %q and %q", conference, currentConference)
				}
			}
		}
	}
	return nil
}

func validateRenderObjectKey(source string) (string, error) {
	if source != strings.TrimSpace(source) || strings.ContainsAny(source, "\r\n\x00") {
		return "", fmt.Errorf("manifest source %q is not a valid object key", source)
	}
	clean := path.Clean(source)
	if source == "" || strings.HasPrefix(source, "/") || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != source || !strings.Contains(clean, "/") {
		return "", fmt.Errorf("manifest source %q must be a relative <conference>/... object key", source)
	}
	return clean, nil
}

func (h *Handler) renderJobs(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "render.html", map[string]any{"Error": r.URL.Query().Get("err")})
}

func (h *Handler) renderQueueView() renderJobsView {
	items, err := h.DB.ListVisibleRenderQueueItems(50)
	view := renderJobsView{}
	if err != nil {
		view.Error = err.Error()
	} else {
		for _, item := range items {
			view.Items = append(view.Items, renderQueueItemView{
				ID: item.ID, Name: item.Name, UnitName: item.UnitName, Status: item.Status,
				AttemptCount: item.AttemptCount, LastError: item.LastError,
				CreatedAt: item.CreatedAt.Format("2006-01-02 15:04"),
				StartedAt: formatOptionalTime(item.StartedAt), FinishedAt: formatOptionalTime(item.FinishedAt),
				OutputPrefix: renderOutputPrefix(item.ID, item.ManifestJSON),
			})
		}
	}
	if counts, countErr := h.DB.RenderQueueStatusCounts(); countErr != nil {
		if view.Error == "" {
			view.Error = countErr.Error()
		}
	} else {
		view.Queued, view.Running, view.Failed = counts["queued"], counts["running"], counts["failed"]
		view.Finished, view.Cancelled = counts["finished"], counts["cancelled"]
	}
	return view
}

func renderOutputPrefix(id int64, raw string) string {
	var manifest renderManifestEnvelope
	if json.Unmarshal([]byte(raw), &manifest) != nil {
		return ""
	}
	for _, job := range manifest.Jobs {
		for _, segment := range job.Segments {
			clean, err := validateRenderObjectKey(segment.Src)
			if err == nil {
				conference := strings.SplitN(clean, "/", 2)[0]
				return fmt.Sprintf("%s/recordings/renders/%d/", conference, id)
			}
		}
	}
	return ""
}

func (h *Handler) renderJobCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRenderManifestBytes+64*1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	raw := []byte(r.FormValue("manifest"))
	if err := validateRenderManifest(raw); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = "conf-render job"
	}
	if _, err := h.DB.EnqueueRenderJob(name, string(raw)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	go h.dispatchGPUQueueOnce(context.Background())
	http.Redirect(w, r, "/render", http.StatusSeeOther)
}

func (h *Handler) renderJobRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idText := strings.TrimPrefix(r.URL.Path, "/worker/render/retry/")
	if idText == r.URL.Path {
		idText = strings.TrimPrefix(r.URL.Path, "/render-jobs/retry/")
	}
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	if err := h.DB.ResetRenderJobForRetry(id, "manual retry"); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	go h.dispatchGPUQueueOnce(context.Background())
	http.Redirect(w, r, "/worker", http.StatusSeeOther)
}

func renderJobIDFromPath(requestPath string, prefixes ...string) (int64, bool) {
	for _, prefix := range prefixes {
		if value, ok := strings.CutPrefix(requestPath, prefix); ok {
			id, err := strconv.ParseInt(value, 10, 64)
			return id, err == nil && id > 0
		}
	}
	return 0, false
}

func (h *Handler) renderJobCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, ok := renderJobIDFromPath(r.URL.Path, "/worker/render/cancel/", "/render-jobs/cancel/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	item, err := h.DB.GetRenderQueueItem(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if item.Status == "running" {
		host, err := h.gpuWorkerSSHHost(r.Context())
		if err != nil {
			http.Error(w, "stopping render: "+err.Error(), http.StatusBadGateway)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if out, err := h.stopRemoteRender(ctx, host, item.UnitName, item.ID); err != nil {
			http.Error(w, strings.TrimSpace(fmt.Sprintf("stopping render: %v: %s", err, out)), http.StatusBadGateway)
			return
		}
	}
	if err := h.DB.CancelRenderJob(id, "cancelled by user"); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	go h.dispatchGPUQueueOnce(context.Background())
	http.Redirect(w, r, "/worker", http.StatusSeeOther)
}

func renderUnitName(id int64, attempt int) string {
	return fmt.Sprintf("streamctl-gpu-render-%d-attempt-%d.service", id, attempt)
}
func renderWorkspace(id int64) string { return fmt.Sprintf("/workspace/streamctl-render-jobs/%d", id) }

func renderAttemptWorkspace(id int64, unit string) string { return renderWorkspace(id) + "/" + unit }

func remoteSSHInput(ctx context.Context, host, remoteCommand, input string) (string, error) {
	cmd := exec.CommandContext(ctx, "ssh", sshArgs(host, remoteCommand)...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func remoteRenderLaunchCommand(unit, renderCommand string) string {
	launch := remoteGPUJobCommand(unit, "streamctl render", "", renderCommand)
	return "unit=" + shellQuote(unit) + "; state=$(systemctl show \"$unit\" --property=LoadState --value 2>/dev/null || true); if { [ -n \"$state\" ] && [ \"$state\" != not-found ]; } || [ -s \"${STREAMCTL_GPU_JOB_ROOT:-/root/streamctl-gpu-jobs}/$unit/result\" ]; then printf '%s\\n' \"$unit\"; exit 0; fi; " + launch
}

func remoteRenderPresenceCommand(unit string, id int64) string {
	workspace := renderAttemptWorkspace(id, unit)
	return "unit=" + shellQuote(unit) + "; if [ -f " + shellQuote(workspace+"/result") + " ] || [ -f \"${STREAMCTL_GPU_JOB_ROOT:-/root/streamctl-gpu-jobs}/$unit/pid\" ]; then printf 'present\\n'; exit 0; fi; state=$(systemctl show \"$unit\" --property=LoadState --value 2>/dev/null || true); if [ -n \"$state\" ] && [ \"$state\" != not-found ]; then printf 'present\\n'; else printf 'absent\\n'; fi"
}

func (h *Handler) remoteRenderPresence(ctx context.Context, host, unit string, id int64) (present, reliable bool) {
	out, err := remoteSSH(ctx, host, remoteRenderPresenceCommand(unit, id))
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

func (h *Handler) stopRemoteRender(ctx context.Context, host, unit string, id int64) (string, error) {
	return remoteSSH(ctx, host, remoteGPUStopCommand(unit)+"; "+h.renderAttemptCleanupCommand(id, unit))
}

func (h *Handler) renderAttemptCleanupCommand(id int64, unit string) string {
	command := "rm -rf -- " + shellQuote(renderAttemptWorkspace(id, unit))
	outputRoot := strings.TrimRight(strings.TrimSpace(h.RenderOutputDir), "/")
	if outputRoot != "" && strings.HasPrefix(outputRoot, "/") {
		command += " " + shellQuote(outputRoot+"/"+strconv.FormatInt(id, 10)+"/"+unit)
	}
	return command
}

func (h *Handler) startRemoteRender(ctx context.Context, host string, item db.RenderJobQueueItem) (string, string, error) {
	unit := renderUnitName(item.ID, item.AttemptCount+1)
	workspace := renderAttemptWorkspace(item.ID, unit)
	command := strings.TrimSpace(h.RenderWorkerCommand)
	if command == "" {
		return unit, "", errors.New("render worker command is not configured")
	}
	outputRoot := strings.TrimRight(strings.TrimSpace(h.RenderOutputDir), "/")
	if outputRoot == "" || !strings.HasPrefix(outputRoot, "/") {
		return unit, "", errors.New("render output directory must be an absolute worker path")
	}
	remoteName := strings.TrimSpace(h.Remote)
	if remoteName == "" {
		return unit, "", errors.New("Spaces remote is not configured")
	}
	stage := "umask 077; mkdir -p " + shellQuote(workspace) + "; rm -f " + shellQuote(workspace+"/result") + " " + shellQuote(workspace+"/exit-code") + "; cat > " + shellQuote(workspace+"/manifest.json")
	if out, err := remoteSSHInput(ctx, host, stage, item.ManifestJSON); err != nil {
		return unit, out, fmt.Errorf("stage manifest: %w", err)
	}
	manifest := workspace + "/manifest.json"
	output := outputRoot + "/" + strconv.FormatInt(item.ID, 10) + "/" + unit
	renderInvocation := "env RCLONE_CONFIG=/root/rclone.conf STREAMCTL_RENDER_QUEUE_ID=" + strconv.FormatInt(item.ID, 10) + " SPACES_REMOTE=" + shellQuote(remoteName) + " " + shellQuote(command) + " " + shellQuote(manifest) + " " + shellQuote(output) + " " + shellQuote(workspace+"/work")
	renderCommand := "set +e; mkdir -p " + shellQuote(output) + "; " + renderInvocation + "; rc=$?; printf '%s\\n' \"$rc\" > " + shellQuote(workspace+"/exit-code") + "; if [ \"$rc\" -eq 0 ]; then printf 'success\\n' > " + shellQuote(workspace+"/result") + "; else printf 'failed\\n' > " + shellQuote(workspace+"/result") + "; fi; exit \"$rc\""
	remote := remoteRenderLaunchCommand(unit, renderCommand)
	out, err := remoteSSH(ctx, host, remote)
	return unit, out, err
}

func (h *Handler) dispatchNextRender(ctx context.Context, host string) bool {
	item, err := h.DB.NextQueuedRenderJob()
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("loading next queued render job failed: %v", err)
		}
		return false
	}
	if out, err := waitForRemoteSSH(ctx, host, 2*time.Minute); err != nil {
		log.Printf("GPU worker SSH readiness failed for queued render %d: %v: %s", item.ID, err, strings.TrimSpace(out))
		return true
	}
	unit := renderUnitName(item.ID, item.AttemptCount+1)
	if err := h.DB.MarkRenderQueueRunning(item.ID, unit); err != nil {
		log.Printf("claiming render job %d failed: %v", item.ID, err)
		return true
	}
	unit, out, err := h.startRemoteRender(ctx, host, *item)
	if err != nil {
		errText := strings.TrimSpace(fmt.Sprintf("%v: %s", err, out))
		present, reliable := h.remoteRenderPresence(ctx, host, unit, item.ID)
		if reliable && !present {
			log.Printf("render job %d submission failed: %s", item.ID, errText)
			_ = h.DB.FinishRenderAttempt(item.ID, unit, "failed", errText)
			_, _ = remoteSSH(ctx, host, h.renderAttemptCleanupCommand(item.ID, unit))
			return true
		}
		log.Printf("render job %d submission outcome is ambiguous; monitoring idempotent unit %s: %s", item.ID, unit, errText)
		_ = h.DB.UpdateRenderQueueError(item.ID, "submission response lost; reconciling worker state: "+errText)
		go h.monitorRenderJob(item.ID, unit, host)
		return true
	}
	go h.monitorRenderJob(item.ID, unit, host)
	return true
}

func renderJobIDFromUnit(unit string) (int64, bool) {
	value := strings.TrimSuffix(strings.TrimPrefix(unit, "streamctl-gpu-render-"), ".service")
	if value == unit {
		return 0, false
	}
	if idText, attemptText, ok := strings.Cut(value, "-attempt-"); ok {
		attempt, attemptErr := strconv.Atoi(attemptText)
		id, idErr := strconv.ParseInt(idText, 10, 64)
		return id, idErr == nil && id > 0 && attemptErr == nil && attempt > 0
	}
	id, err := strconv.ParseInt(value, 10, 64)
	return id, err == nil && id > 0
}

func (h *Handler) reconcileRenderJobs(ctx context.Context, host string, jobs []gpuJobView, workerStateReliable bool) {
	// Dashboard history is deliberately limited. Always inspect the current
	// attempt as well, including after a controller restart or worker timeout.
	listed := make(map[string]bool)
	for _, job := range jobs {
		listed[job.UnitName] = true
	}
	if items, err := h.DB.ListOpenRenderQueueItems(1000); err == nil {
		for _, item := range items {
			if item.Status == "running" && !listed[item.UnitName] {
				jobs = append(jobs, h.gpuJob(ctx, host, item.UnitName, false))
			}
		}
	}
	seen := make(map[int64]bool)
	for _, job := range jobs {
		id, ok := renderJobIDFromUnit(job.UnitName)
		if !ok {
			continue
		}
		item, err := h.DB.GetRenderQueueItem(id)
		if err != nil || item.Status != "running" || item.UnitName != job.UnitName {
			continue
		}
		seen[id] = isBlockingGPUJob(job) || isTerminalGPUJob(job)
		if !isTerminalGPUJob(job) {
			continue
		}
		status, message := "finished", ""
		if job.ActiveState == "failed" || (job.Result != "" && job.Result != "success") {
			status = "failed"
			full := h.gpuJob(ctx, host, job.UnitName, true)
			message = firstNonEmptyString(gpuFailureJournalSummary(full.Journal), full.Error, full.Result)
		}
		if err := h.DB.FinishRenderAttempt(id, item.UnitName, status, message); err != nil {
			log.Printf("reconciling render job %d failed: %v", id, err)
			continue
		}
		_, _ = remoteSSH(ctx, host, h.renderAttemptCleanupCommand(id, item.UnitName))
		h.destroyManagedGPUAfterTerminalJob(ctx, job)
	}
	running, err := h.DB.ListOpenRenderQueueItems(1000)
	if err != nil {
		log.Printf("listing running render jobs for reconciliation failed: %v", err)
		return
	}
	for _, item := range running {
		if item.Status != "running" || seen[item.ID] {
			continue
		}
		status, exitCode, found := h.remoteRenderResult(ctx, host, item.ID, item.UnitName)
		if !found {
			if shouldRequeueMissingRenderJob(item, workerStateReliable, time.Now()) {
				// The dashboard's recent-job list is capped; absence from it is
				// not evidence that this attempt disappeared from the worker.
				present, reliable := h.remoteRenderPresence(ctx, host, item.UnitName, item.ID)
				if present || !reliable {
					continue
				}
				if err := h.DB.RequeueRunningRenderJob(item.ID, "worker did not retain the submitted render; requeued automatically"); err != nil {
					log.Printf("requeueing missing render job %d failed: %v", item.ID, err)
				}
			}
			continue
		}
		message := ""
		if status != "finished" {
			message = "conf-render exited with status " + exitCode
		}
		if err := h.DB.FinishRenderAttempt(item.ID, item.UnitName, status, message); err != nil {
			log.Printf("reconciling durable render result %d failed: %v", item.ID, err)
			continue
		}
		_, _ = remoteSSH(ctx, host, h.renderAttemptCleanupCommand(item.ID, item.UnitName))
		h.destroyManagedGPUAfterTerminalJob(ctx, renderTerminalGPUJob(item.UnitName, status))
	}
}

func renderTerminalGPUJob(unit, status string) gpuJobView {
	job := gpuJobView{UnitName: unit, ActiveState: "inactive", Result: "success"}
	if status != "finished" {
		job.ActiveState = "failed"
		job.Result = "exit-code"
	}
	return job
}

func shouldRequeueMissingRenderJob(item db.RenderJobQueueItem, workerStateReliable bool, now time.Time) bool {
	return workerStateReliable && item.Status == "running" && item.StartedAt != nil && now.Sub(*item.StartedAt) >= renderSubmissionStaleAfter
}

func (h *Handler) reconcileUnavailableRenderQueue(worker gpuWorkerView) {
	if !managedGPUWorkerDefinitelyUnavailable(worker) {
		return
	}
	items, err := h.DB.ListOpenRenderQueueItems(1000)
	if err != nil {
		log.Printf("listing render jobs for unavailable worker reconciliation failed: %v", err)
		return
	}
	for _, item := range items {
		if item.Status != "running" {
			continue
		}
		if err := h.DB.RequeueRunningRenderJob(item.ID, "managed worker is unavailable; requeued automatically"); err != nil {
			log.Printf("requeueing render job %d after worker loss failed: %v", item.ID, err)
		}
	}
}

func (h *Handler) remoteRenderResult(ctx context.Context, host string, id int64, unit string) (string, string, bool) {
	workspace := renderAttemptWorkspace(id, unit)
	out, err := remoteSSH(ctx, host, "if [ -f "+shellQuote(workspace+"/result")+" ]; then cat "+shellQuote(workspace+"/result")+"; cat "+shellQuote(workspace+"/exit-code")+" 2>/dev/null || true; fi")
	if err != nil {
		return "", "", false
	}
	lines := strings.Fields(out)
	if len(lines) == 0 {
		return "", "", false
	}
	exitCode := "unknown"
	if len(lines) > 1 {
		exitCode = lines[1]
	}
	if lines[0] == "success" {
		return "finished", exitCode, true
	}
	return "failed", exitCode, true
}

func (h *Handler) monitorRenderJob(id int64, unit, host string) {
	ctx, cancel := context.WithTimeout(context.Background(), 48*time.Hour)
	defer cancel()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			item, err := h.DB.GetRenderQueueItem(id)
			if err != nil || item.Status != "running" || item.UnitName != unit {
				return
			}
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
			out, stopErr := h.stopRemoteRender(cleanupCtx, host, unit, id)
			cleanupCancel()
			message := "render monitor timed out"
			if stopErr != nil {
				message += "; stopping remote job failed: " + strings.TrimSpace(fmt.Sprintf("%v: %s", stopErr, out))
			}
			if err := h.DB.FinishRenderAttempt(id, unit, "failed", message); err != nil {
				return
			}
			h.destroyManagedGPUAfterTerminalJob(context.Background(), renderTerminalGPUJob(unit, "failed"))
			go h.dispatchGPUQueueOnce(context.Background())
			return
		case <-ticker.C:
			if item, err := h.DB.GetRenderQueueItem(id); err != nil || item.Status != "running" || item.UnitName != unit {
				return
			}
			if status, exitCode, found := h.remoteRenderResult(ctx, host, id, unit); found {
				message := ""
				if status != "finished" {
					message = "conf-render exited with status " + exitCode
				}
				if err := h.DB.FinishRenderAttempt(id, unit, status, message); err != nil {
					return
				}
				_, _ = remoteSSH(ctx, host, h.renderAttemptCleanupCommand(id, unit))
				h.destroyManagedGPUAfterTerminalJob(ctx, renderTerminalGPUJob(unit, status))
				go h.dispatchGPUQueueOnce(context.Background())
				return
			}
			job := h.gpuJob(ctx, host, unit, false)
			if !isTerminalGPUJob(job) {
				continue
			}
			status, message := "finished", ""
			if job.ActiveState == "failed" || (job.Result != "" && job.Result != "success") {
				status = "failed"
				full := h.gpuJob(ctx, host, unit, true)
				message = firstNonEmptyString(gpuFailureJournalSummary(full.Journal), full.Error, full.Result)
			}
			if err := h.DB.FinishRenderAttempt(id, unit, status, message); err != nil {
				return
			}
			_, _ = remoteSSH(ctx, host, h.renderAttemptCleanupCommand(id, unit))
			h.destroyManagedGPUAfterTerminalJob(ctx, job)
			go h.dispatchGPUQueueOnce(context.Background())
			return
		}
	}
}
