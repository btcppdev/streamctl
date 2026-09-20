package handlers

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"streamctl/internal/btcppclient"
	"streamctl/internal/db"
)

func TestBroadcastCatalog(t *testing.T) {
	h := &Handler{BTCPP: productionCandidatesStub{
		conferences: []btcppclient.Conference{{Tag: "old", StartsAt: stringPointer("2024-01-01T00:00:00Z")}, {Tag: "undated"}, {Tag: "new", StartsAt: stringPointer("2026-07-22T00:00:00Z")}},
		candidates:  []btcppclient.Candidate{{Title: "Test talk", Recording: &btcppclient.Recording{ID: "recording", FileURI: "new/recordings/edits/talk.mp4"}}},
	}}
	w := httptest.NewRecorder()
	h.broadcastCatalog(w, httptest.NewRequest(http.MethodGet, "/streams/broadcast-catalog", nil))
	var got []broadcastConference
	if w.Code != 200 {
		t.Fatalf("status=%d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Tag != "new" || got[1].Tag != "old" || got[2].Tag != "undated" || got[0].Talks[0].Recording.ID != "recording" {
		t.Fatalf("catalog=%+v", got)
	}
	h.BTCPP = productionCandidatesStub{err: errors.New("unavailable")}
	w = httptest.NewRecorder()
	h.broadcastCatalog(w, httptest.NewRequest(http.MethodGet, "/streams/broadcast-catalog", nil))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("failure status=%d", w.Code)
	}
}

func TestBroadcastPickerTemplatePreservesSavedTarget(t *testing.T) {
	h := &Handler{funcs: template.FuncMap{"contains": func([]int64, int64) bool { return false }}}
	w := httptest.NewRecorder()
	h.render(w, httptest.NewRequest(http.MethodGet, "/streams/edit/1", nil), "stream_form.html", map[string]any{
		"Stream":       &db.Stream{ID: 1, BTCPPRecordingID: "saved-recording", Videos: []string{"video.mp4"}},
		"BitratesJSON": template.JS(`{}`), "DefaultSchedule": "once",
	})
	if w.Code != 200 {
		t.Fatalf("status=%d: %s", w.Code, w.Body.String())
	}
	for _, want := range []string{`value="saved-recording"`, `id="conference-search"`, `id="talk-search"`, `/streams/broadcast-catalog`, `matchingRecordings`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(w.Body.String(), "ZgotmplZ") || strings.Contains(w.Body.String(), "template:") {
		t.Fatalf("invalid template: %s", w.Body.String())
	}
}
