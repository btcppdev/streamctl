package handlers

import (
	"context"
	"log"
	"net/http"
	"streamctl/internal/gpuwatchdog"
)

func (h *Handler) gpuWorkerResume(w http.ResponseWriter, r *http.Request) {
	if h.GPUWatchdogStateFile == "" || !h.hasRunPodToken() {
		http.Error(w, "RunPod spending watchdog is not configured", http.StatusBadRequest)
		return
	}
	client, err := h.runpodClient()
	if err == nil {
		err = gpuwatchdog.Resume(r.Context(), gpuwatchdog.Config{StateFile: h.GPUWatchdogStateFile, PodName: h.runpodPodName()}, h.DB, client)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	log.Printf("GPU spending watchdog resumed explicitly by operator")
	go h.dispatchGPUQueueOnce(context.Background())
	http.Redirect(w, r, "/worker", http.StatusSeeOther)
}

func (h *Handler) worker(w http.ResponseWriter, r *http.Request) {
	worker := h.gpuWorkerView(r.Context())

	jobs := h.workerJobsView(r)
	h.render(w, r, "worker.html", map[string]any{
		"GPUConfigured": worker.Configured, "GPUWorker": worker,
		"GPUAvailability": h.gpuAvailability(r.Context()), "Jobs": jobs,
		"RequeuedProxy": r.URL.Query().Get("requeued_proxy"),
	})
}
