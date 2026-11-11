package handlers

import (
	"bytes"
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
	cutPath := "/production/cuts/edit?conference=berlin26&talk_id=limited"
	check(cutPath, func(w *httptest.ResponseRecorder) { h.productionCut(w, httptest.NewRequest("GET", cutPath, nil)) }, "Privacy talk · Ada · Slides + audio only — no speaker video</strong>")
	cutPath = "/production/cuts/edit?conference=berlin26&talk_id=prohibited"
	check(cutPath, func(w *httptest.ResponseRecorder) { h.productionCut(w, httptest.NewRequest("GET", cutPath, nil)) }, "Private panel · Grace, Lin · Do not record</strong>")
	listPath := "/production/renders?conference=berlin26"
	check(listPath, func(w *httptest.ResponseRecorder) { h.productionRenders(w, httptest.NewRequest("GET", listPath, nil)) }, ">Do not record</span>")
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
	w := httptest.NewRecorder()
	h.productionRenderEdit(w, httptest.NewRequest("GET", editPath, nil))
	if w.Code != 200 || strings.Contains(w.Body.String(), `class="recording-policy recording-policy-`) {
		t.Fatal("unverified preferences should not produce an editor banner")
	}
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

func TestRecordingListTagsAreCompactAndOnlyShowRestrictions(t *testing.T) {
	raw, err := templateFS.ReadFile("templates/production_recording_policy.html")
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := template.New("policies").Funcs(template.FuncMap{"recordingPolicyCatalog": func() any { return nil }}).Parse(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, policy := range []string{"full", "record", "", "new-policy"} {
		for _, name := range []string{"productionRecordingTag", "productionRecordingNotice"} {
			var out bytes.Buffer
			if err := tmpl.ExecuteTemplate(&out, name, recordingPolicyNotice(policy)); err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(out.String()) != "" {
				t.Fatalf("%s rendered for %q: %s", name, policy, &out)
			}
		}
	}
	var out bytes.Buffer
	notices := []productionRecordingNotice{recordingPolicyNotice("audio_only"), recordingPolicyNotice("prohibited"), recordingPolicyNotice("audio_only"), recordingPolicyNotice("full"), recordingPolicyNotice("")}
	if err := tmpl.ExecuteTemplate(&out, "productionRecordingTags", notices); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{">Slides only</span>", ">Do not record</span>"} {
		if strings.Count(out.String(), label) != 1 {
			t.Fatalf("expected one compact %s tag: %s", label, &out)
		}
	}
	if strings.Contains(out.String(), "<div") || strings.Contains(out.String(), "Exclude") || strings.Contains(out.String(), "Recording allowed") {
		t.Fatalf("list tags contain banner text: %s", &out)
	}
}
