package handlers

import (
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStagingAndWorkerTemplatesRender(t *testing.T) {
	tests := []struct {
		name     string
		template string
		data     any
		want     string
	}{

		{
			name:     "production overview",
			template: "production.html",
			data:     productionHomePage{},
			want:     "Production overview",
		},
		{name: "production talk list", template: "production_talks.html", data: productionTalksPage{Conference: "toronto"}, want: "Timestamp talks"},
		{
			name:     "render staging",
			template: "render.html",
			data:     map[string]any{},
			want:     "Prepare a conf-render manifest",
		},
		{
			name:     "worker execution",
			template: "worker.html",
			data: map[string]any{
				"GPUWorker":       gpuWorkerView{},
				"GPUStatus":       gpuStatusView{},
				"GPUAvailability": gpuAvailabilityView{},
				"RenderQueue":     renderJobsView{},
			},
			want: "GPU lifecycle and execution queues",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Handler{funcs: template.FuncMap{}}
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			h.render(recorder, request, tt.template, tt.data)

			response := recorder.Result()
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.StatusCode, body)
			}
			if !strings.Contains(string(body), tt.want) {
				t.Fatalf("body does not contain %q: %s", tt.want, body)
			}
		})
	}
}

func TestLegacyPageRedirects(t *testing.T) {
	tests := []struct {
		legacy string
		want   string
	}{
		{legacy: "/render-jobs", want: "/render"},
	}

	for _, tt := range tests {
		t.Run(tt.legacy, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, tt.legacy, nil)
			redirectTo(tt.want).ServeHTTP(recorder, request)

			response := recorder.Result()
			defer response.Body.Close()
			if response.StatusCode != http.StatusPermanentRedirect {
				t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusPermanentRedirect)
			}
			if location := response.Header.Get("Location"); location != tt.want {
				t.Fatalf("Location = %q, want %q", location, tt.want)
			}
		})
	}
}
