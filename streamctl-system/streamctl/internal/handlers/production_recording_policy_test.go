package handlers

import (
	"encoding/json"
	"errors"
	"html"
	"html/template"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"streamctl/internal/btcppclient"
	"streamctl/internal/db"
	"streamctl/internal/renderdraft"
)

func TestRecordingPoliciesRemainDistinctAndUnknownIsNotConsent(t *testing.T) {
	for _, tc := range []struct{ policy, label, level string }{
		{"full", "Recording allowed", "allowed"},
		{"record", "Recording allowed", "allowed"},
		{"audio_only", "Slides + audio only — no speaker video", "limited"},
		{"prohibited", "Do not record", "prohibited"},
		{"", "Recording preference unverified", "unknown"},
		{"new-policy", "Recording preference unverified", "unknown"},
	} {
		notice := recordingPolicyNotice(tc.policy)
		if notice.Label != tc.label || notice.Level != tc.level {
			t.Fatalf("%s: %+v", tc.policy, notice)
		}
	}
}

func TestRecordingPreferencesAppearInCutterListAndRenderEditor(t *testing.T) {
	database := productionHandlerTestDB(t)
	candidates := []btcppclient.Candidate{
		{TalkID: "limited", Title: "Privacy talk", RecordingPolicy: "audio_only", Eligible: true, Speakers: []btcppclient.Speaker{{Name: "Ada"}}},
		{TalkID: "prohibited", Title: "Private panel", RecordingPolicy: "prohibited", Speakers: []btcppclient.Speaker{{Name: "Grace"}, {Name: "Lin"}}},
	}
	// The snapshot allowed recording; current API preferences must take priority.
	clip := renderdraft.Clip{Type: renderdraft.ClipType, Name: "Panel", Talk: &renderdraft.Talk{ID: "prohibited", Title: "Old title", RecordingPolicy: "full"}, Segments: []json.RawMessage{json.RawMessage(`{"type":"video","src":"berlin26/panel.mp4"}`)}}
	raw, _ := json.Marshal(clip)
	manifest, err := productionRenderManifestJSON("Livestream", json.RawMessage(`{}`), []json.RawMessage{raw, raw})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := productionWorkerManifest(manifest)
	if err != nil || strings.Contains(worker, "recordingPolicy") || strings.Contains(worker, "recordingNotice") {
		t.Fatalf("recording display metadata leaked to worker: %v", err)
	}
	id, _, err := database.CreateProductionRender("berlin26", "Livestream", manifest, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: database, funcs: template.FuncMap{}, BTCPP: productionCandidatesStub{candidates: candidates}}
	check := func(path string, handler func(w *httptest.ResponseRecorder), want string) {
		t.Helper()
		w := httptest.NewRecorder()
		handler(w)
		if w.Code != 200 || !strings.Contains(html.UnescapeString(w.Body.String()), want) {
			t.Fatalf("%s: status %d, missing %q", path, w.Code, want)
		}
	}
	cutPath := "/production/timestamp/cut?conference=berlin26&talk_id=limited"
	check(cutPath, func(w *httptest.ResponseRecorder) { h.productionCut(w, httptest.NewRequest("GET", cutPath, nil)) }, "Privacy talk · Ada · Slides + audio only — no speaker video</strong>")
	cutPath = "/production/timestamp/cut?conference=berlin26&talk_id=prohibited"
	check(cutPath, func(w *httptest.ResponseRecorder) { h.productionCut(w, httptest.NewRequest("GET", cutPath, nil)) }, "Private panel · Grace, Lin · Do not record</strong>")
	listPath := "/production/renders?conference=berlin26"
	check(listPath, func(w *httptest.ResponseRecorder) { h.productionRenders(w, httptest.NewRequest("GET", listPath, nil)) }, "Private panel · Grace, Lin · Do not record</strong>")
	editPath := "/production/renders/edit?conference=berlin26&id=" + strconv.FormatInt(id, 10)
	check(editPath, func(w *httptest.ResponseRecorder) {
		h.productionRenderEdit(w, httptest.NewRequest("GET", editPath, nil))
	}, "Private panel · Grace, Lin · Do not record</strong>")
	item, _ := database.ProductionRender(id, "berlin26")
	current := h.productionRecordingTalks(httptest.NewRequest("GET", "/", nil), "berlin26")
	notices := renderRecordingNotices(item, current)
	if len(notices) != 1 || notices[0].Level != "prohibited" {
		t.Fatalf("current consent/deduplication: %+v", notices)
	}
	h.BTCPP = productionCandidatesStub{err: errors.New("offline")}
	check(editPath, func(w *httptest.ResponseRecorder) {
		h.productionRenderEdit(w, httptest.NewRequest("GET", editPath, nil))
	}, "Old title · Recording preference unverified</strong>")
}

func TestRecordingNoticesCoverLegacyAndUnlinkedFootage(t *testing.T) {
	manifest := `{"version":1,"settings":{},"jobs":[{"id":"talk","segments":[{"type":"video","src":"berlin26/talk.mp4"}]}]}`
	talks := map[string]productionTalkView{"a": {Candidate: btcppclient.Candidate{TalkID: "a", Title: "Talk A", RecordingPolicy: "audio_only"}}}
	notices := renderRecordingNotices(db.ProductionRender{TalkID: "a", JSON: manifest}, talks)
	if len(notices) != 1 || notices[0].Level != "limited" {
		t.Fatalf("legacy linked draft: %+v", notices)
	}
	notices = renderRecordingNotices(db.ProductionRender{JSON: manifest}, talks)
	if len(notices) != 1 || notices[0].Title != "Unlinked footage" || notices[0].Level != "unknown" {
		t.Fatalf("unlinked draft: %+v", notices)
	}
}
