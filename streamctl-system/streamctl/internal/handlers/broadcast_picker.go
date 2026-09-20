package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"streamctl/internal/btcppclient"
)

type broadcastConference struct {
	btcppclient.Conference
	Talks []btcppclient.Candidate `json:"talks"`
}

// The catalog stays behind streamctl authentication; the API token never reaches
// the browser. Limit concurrent requests when loading recordings across events.
func (h *Handler) broadcastCatalog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if h.BTCPP == nil {
		http.Error(w, "bitcoin++ API is not configured.", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	conferences, message := h.productionConferences(ctx)
	if message != "" {
		http.Error(w, message, http.StatusBadGateway)
		return
	}
	catalog := make([]broadcastConference, len(conferences))
	errs := make([]error, len(conferences))
	slots := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, conf := range conferences {
		catalog[i].Conference = conf.Conference
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			defer func() { <-slots }()
			catalog[i].Talks, errs[i] = h.BTCPP.RecordingCandidates(ctx, catalog[i].Tag)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			http.Error(w, "Could not load bitcoin++ talks. Retry to refresh the selectors; existing selections have been kept.", http.StatusBadGateway)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(catalog)
}
