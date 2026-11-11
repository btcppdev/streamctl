package handlers

import (
	"log"
	"net/http"
)

func (h *Handler) worker(w http.ResponseWriter, r *http.Request) {
	worker := h.gpuWorkerView(r.Context())
	status := h.gpuStatus(r.Context(), worker)
	renderQueue := h.renderQueueView()
	proxyQueue, proxyQueueErr := h.DB.ProductionProxyQueue(100)
	if proxyQueueErr != nil {
		log.Printf("loading production proxy queue failed: %v", proxyQueueErr)
	}
	h.render(w, r, "worker.html", map[string]any{
		"GPUConfigured": worker.Configured, "GPUWorker": worker, "GPUStatus": status,
		"GPUAvailability": h.gpuAvailability(r.Context()), "RenderQueue": renderQueue,
		"ProxyQueue": proxyQueue, "ProxyQueueError": proxyQueueErr,
		"QueuedCount": renderQueue.Queued + proxyQueue.Queued, "RunningCount": renderQueue.Running + proxyQueue.Running,
		"QueueNeedsWorker": renderQueue.Queued+proxyQueue.Queued > 0 && worker.Managed && worker.Status != "active",
		"RequeuedProxy":    r.URL.Query().Get("requeued_proxy"),
	})
}
