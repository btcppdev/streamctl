package handlers

import (
	"log"
	"net/http"
)

func (h *Handler) worker(w http.ResponseWriter, r *http.Request) {
	worker := h.gpuWorkerView(r.Context())
	status := h.gpuStatus(r.Context(), worker)
	openQueue, err := h.DB.ListOpenGPUQueueItems(1000)
	if err != nil {
		log.Printf("listing GPU queue failed: %v", err)
	}
	openQueue = h.reconcileStaleGPUQueue(worker, status.Jobs, openQueue)
	nowProcessing := currentNormalizationGPUJob(status.Jobs)
	normalizeQueue, queueErr := h.gpuQueueDashboard(status.Jobs, nowProcessing)
	if queueErr != nil {
		log.Printf("loading GPU queue dashboard failed: %v", queueErr)
	}
	queuedCount, runningCount := countGPUQueueStates(openQueue)
	renderQueue := h.renderQueueView()
	proxyQueue, proxyQueueErr := h.DB.ProductionProxyQueue(100)
	if proxyQueueErr != nil {
		log.Printf("loading production proxy queue failed: %v", proxyQueueErr)
	}
	h.render(w, r, "worker.html", map[string]any{
		"GPUConfigured": worker.Configured, "GPUWorker": worker, "GPUStatus": status,
		"GPUAvailability": h.gpuAvailability(r.Context()), "NormalizeQueue": normalizeQueue, "RenderQueue": renderQueue,
		"ProxyQueue": proxyQueue, "ProxyQueueError": proxyQueueErr,
		"QueuedCount": queuedCount + renderQueue.Queued + proxyQueue.Queued, "RunningCount": runningCount + renderQueue.Running + proxyQueue.Running,
		"QueueNeedsWorker": queuedCount+renderQueue.Queued+proxyQueue.Queued > 0 && worker.Managed && worker.Status != "active",
		"Requeued":         r.URL.Query().Get("requeued"), "RequeuedStale": r.URL.Query().Get("requeued_stale"),
		"RequeuedProxy":   r.URL.Query().Get("requeued_proxy"),
		"ClearedFailures": r.URL.Query().Get("cleared_failures"),
	})
}
