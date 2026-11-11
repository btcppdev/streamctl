package handlers

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"streamctl/internal/db"
	"strings"
	"testing"
)

func TestWorkerDashboardHistoryAndPolling(t *testing.T) {
	d := productionHandlerTestDB(t)
	h := &Handler{DB: d, Secret: "test", funcs: template.FuncMap{}}
	for i := 0; i < 27; i++ {
		if _, err := d.EnqueueRenderJob("Render <test>", `{"version":1,"jobs":[]}`); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		path string
		call http.HandlerFunc
		want string
	}{
		{"/worker", h.worker, "Recent jobs"},
		{"/worker/jobs", h.workerJobs, "Older →"},
		{"/worker/jobs?page=2", h.workerJobs, "Page 2"},
		{"/worker/jobs/status", h.workerJobsStatus, "In progress"},
		{"/worker/jobs/status?all=1&page=2", h.workerJobsStatus, "Page 2"},
	} {
		w := httptest.NewRecorder()
		tc.call(w, httptest.NewRequest("GET", tc.path, nil))
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, tc.want) || strings.Contains(body, "template:") {
			t.Fatalf("%s: %d %s", tc.path, w.Code, body)
		}
		if strings.Contains(body, "<test>") {
			t.Fatal("job name not escaped")
		}
	}
	mux := http.NewServeMux()
	h.Register(mux)
	for _, p := range []string{"/worker/jobs", "/worker/jobs/status", "/worker/state", "/gpu-worker/logs/streamctl-gpu-render-1?format=json"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != 303 {
			t.Fatalf("unprotected %s: %d", p, w.Code)
		}
	}
}
func TestJobLogsClearlyIdentifyCachedOutput(t *testing.T) {
	d := productionHandlerTestDB(t)
	unit := "streamctl-gpu-render-1"
	if err := d.UpsertGPUJobLog(db.GPUJobLog{UnitName: unit, Host: "retired-worker", Journal: "saved <output>", ActiveState: "inactive", Result: "success"}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: d, funcs: template.FuncMap{}}
	w := httptest.NewRecorder()
	h.gpuJobLogs(w, httptest.NewRequest("GET", "/gpu-worker/logs/"+unit+"?format=json", nil))
	var result struct {
		Cached  bool
		Warning string
		Job     gpuJobView
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || !result.Cached || result.Warning == "" || result.Job.Journal != "saved <output>" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cached logs %d %+v", w.Code, result)
	}
	w = httptest.NewRecorder()
	h.gpuJobLogs(w, httptest.NewRequest("GET", "/gpu-worker/logs/"+unit, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "saved &lt;output&gt;") {
		t.Fatal(w.Body.String())
	}
}
