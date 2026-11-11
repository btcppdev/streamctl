package handlers

import "net/http"

func (h *Handler) worker(w http.ResponseWriter, r *http.Request) {
	worker := h.gpuWorkerView(r.Context())

	jobs := h.workerJobsView(r)
	h.render(w, r, "worker.html", map[string]any{
		"GPUConfigured": worker.Configured, "GPUWorker": worker,
		"GPUAvailability": h.gpuAvailability(r.Context()), "Jobs": jobs,
		"RequeuedProxy": r.URL.Query().Get("requeued_proxy"),
	})
}
