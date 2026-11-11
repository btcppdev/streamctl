package handlers

import (
	"net/http"
	"strconv"
	"streamctl/internal/db"
)

type workerJobsView struct {
	Active, Recent, History []db.WorkerJob
	All, HasNext            bool
	Page, Previous, Next    int
	Error                   string
}

func (h *Handler) workerJobsView(r *http.Request) workerJobsView {
	v := workerJobsView{All: r.URL.Query().Get("all") == "1", Page: 1}
	if page, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && page > 0 && page <= 100000 {
		v.Page = page
	}
	v.Previous = v.Page - 1
	v.Next = v.Page + 1
	var err error
	if v.All {
		v.History, err = h.DB.WorkerJobs("history", 26, (v.Page-1)*25)
		if len(v.History) > 25 {
			v.HasNext = true
			v.History = v.History[:25]
		}
	} else {
		v.Active, err = h.DB.WorkerJobs("active", 0, 0)
		if err == nil {
			v.Recent, err = h.DB.WorkerJobs("recent", 10, 0)
		}
	}
	if err != nil {
		v.Error = "Could not load jobs. Please try again."
	}
	return v
}
func (h *Handler) workerJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	// This page includes completed jobs; the dashboard always includes all active jobs.
	q := r.URL.Query()
	q.Set("all", "1")
	r.URL.RawQuery = q.Encode()
	h.render(w, r, "worker_history.html", h.workerJobsView(r))
}
func (h *Handler) workerJobsStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	v := h.workerJobsView(r)
	if v.Error != "" {
		http.Error(w, v.Error, 503)
		return
	}
	h.render(w, r, "worker_jobs.html", v)
}
func (h *Handler) workerState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	// Provider/readiness only; do not run gpuStatus's reconciliation from polling.
	worker := h.gpuWorkerView(r.Context())
	h.render(w, r, "worker_panel.html", map[string]any{"GPUWorker": worker, "GPUConfigured": worker.Configured, "GPUAvailability": h.gpuAvailability(r.Context())})
}
