package handlers

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"streamctl/internal/btcppclient"
	"streamctl/internal/db"
	"streamctl/internal/renderdraft"
)

func livestreamCandidate(id, start, stage string) productionTalkView {
	return productionTalkView{Candidate: btcppclient.Candidate{TalkID: id, Title: id, Eligible: true, StartsAt: stringPointer(start), Venue: stage, SocialCard: "toronto/talks/" + id + ".png"}}
}

func TestLivestreamCreateEditQueueSnapshot(t *testing.T) {
	database := productionHandlerTestDB(t)
	talk := livestreamCandidate("talk", "2026-07-22T10:00:00-04:00", "one")
	uncut := livestreamCandidate("uncut", "2026-07-22T11:00:00-04:00", "one")
	cut := db.ProductionCut{SourceType: "video", Source: "toronto/recordings/raw/talk.mp4", InMS: 1000, OutMS: 2000}
	if err := database.ReplaceProductionCuts("toronto", "talk", []db.ProductionCut{cut}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: database, funcs: template.FuncMap{}, BTCPP: productionCandidatesStub{conferences: []btcppclient.Conference{{Tag: "toronto", StartsAt: stringPointer("2026-07-22T09:00:00-04:00")}}, candidates: []btcppclient.Candidate{talk.Candidate, uncut.Candidate}}}
	form := url.Values{"conference": {"toronto"}, "include_talks": {"true"}, "template_id": {"0"}, "talk_ids": {"talk"}}
	create := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/production/renders/create", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.productionRenderCreate(w, r)
		return w
	}
	w := create()
	var result struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || result.URL == "" {
		t.Fatalf("status=%d body=%s err=%v", w.Code, w.Body.String(), err)
	}
	items, err := database.ListProductionRenders("toronto")
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	item := items[0]
	if item.TemplateID.Valid || item.TalkID != "" || !strings.Contains(item.JSON, "streamctl.clip") {
		t.Fatalf("unexpected association/dynamic block: %+v", item)
	}
	var kind string
	if err := database.QueryRow("SELECT recording_kind FROM production_renders WHERE id = ?", item.ID).Scan(&kind); err != nil || kind != "" {
		t.Fatalf("kind=%q err=%v", kind, err)
	}
	if err := database.ReplaceProductionCuts("toronto", "talk", nil); err != nil {
		t.Fatal(err)
	}
	saved, err := database.ProductionRender(item.ID, "toronto")
	if err != nil || saved.JSON != item.JSON {
		t.Fatalf("snapshot changed: %+v %v", saved, err)
	}
	w = httptest.NewRecorder()
	h.productionRenderEdit(w, httptest.NewRequest(http.MethodGet, result.URL, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<summary>Options</summary>") {
		t.Fatalf("editor status=%d body=%s", w.Code, w.Body.String())
	}
	queue := url.Values{"conference": {"toronto"}, "ids": {strconv.FormatInt(item.ID, 10)}}
	r := httptest.NewRequest(http.MethodPost, "/production/renders/queue", strings.NewReader(queue.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.productionRendersQueue(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("queue status=%d body=%s", w.Code, w.Body.String())
	}
	// An independent creation produces a fresh snapshot rather than overwriting.
	if err := database.ReplaceProductionCuts("toronto", "talk", []db.ProductionCut{cut}); err != nil {
		t.Fatal(err)
	}
	if w = create(); w.Code != 200 {
		t.Fatalf("second create=%s", w.Body.String())
	}
	items, _ = database.ListProductionRenders("toronto")
	if len(items) != 2 {
		t.Fatalf("renders=%+v", items)
	}
}

func TestLivestreamOptionsAndDialog(t *testing.T) {
	database := productionHandlerTestDB(t)
	h := &Handler{DB: database, funcs: template.FuncMap{}, BTCPP: productionCandidatesStub{candidates: []btcppclient.Candidate{livestreamCandidate("talk", "2026-07-22T10:00:00-04:00", "one").Candidate, {TalkID: "unscheduled", Venue: "two"}}}}
	w := httptest.NewRecorder()
	h.productionLivestreamOptions(w, httptest.NewRequest(http.MethodGet, "/production/renders/create-options?conference=toronto", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"stageLabel":"Main"`) || strings.Contains(w.Body.String(), "Talks") {
		t.Fatalf("options=%s", w.Body.String())
	}
	w = httptest.NewRecorder()
	h.productionRenders(w, httptest.NewRequest(http.MethodGet, "/production/renders?conference=toronto", nil))
	for _, want := range []string{"+ New render", "new-render-dialog", "batch-render-dialog", "data-role=\"talks\"", "Select ready talks", "include_talks", "showModal()", "livestreamTalkGroups"} {
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("missing %s status=%d body=%s", want, w.Code, w.Body.String())
		}
	}
	mux := http.NewServeMux()
	h.Register(mux)
	for _, path := range []string{"/production/renders/create-options?conference=toronto", "/production/renders/create"} {
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/login") {
			t.Fatalf("unprotected %s: %d", path, w.Code)
		}
	}
}

func TestLivestreamMutationRequiresCSRFAndPOST(t *testing.T) {
	h := &Handler{DB: productionHandlerTestDB(t), Secret: "test-secret"}
	mux := http.NewServeMux()
	h.Register(mux)
	for _, tc := range []struct {
		method, token string
		status        int
	}{
		{http.MethodGet, "", http.StatusMethodNotAllowed},
		{http.MethodPost, "wrong", http.StatusForbidden},
	} {
		form := url.Values{"conference": {"toronto"}, "csrf_token": {tc.token}}
		r := httptest.NewRequest(tc.method, "/production/renders/create", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: h.Secret})
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s status=%d body=%s", tc.method, w.Code, w.Body.String())
		}
	}
}

func TestLivestreamRejectsUnavailableAndStaleExplicitSelections(t *testing.T) {
	database := productionHandlerTestDB(t)
	ready := livestreamCandidate("ready", "2026-07-22T10:00:00-04:00", "one")
	banned := livestreamCandidate("banned", "2026-07-22T11:00:00-04:00", "one")
	banned.Eligible = false
	banned.RecordingPolicy = "prohibited"
	banned.Reasons = []string{"another_eligibility_failure"}
	cut := db.ProductionCut{SourceType: "video", Source: "toronto/recordings/talk.mp4", InMS: 0, OutMS: 10000}
	cuts := map[string][]db.ProductionCut{"ready": {cut}, "banned": {cut}}
	for id, items := range cuts {
		if err := database.ReplaceProductionCuts("toronto", id, items); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handler{DB: database, BTCPP: productionCandidatesStub{candidates: []btcppclient.Candidate{ready.Candidate, banned.Candidate}}}
	for _, id := range []string{"banned", "missing", ""} {
		form := url.Values{"conference": {"toronto"}, "include_talks": {"true"}, "template_id": {"0"}}
		if id != "" {
			form.Set("talk_ids", id)
		}
		r := httptest.NewRequest("POST", "/production/renders/create", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.productionRenderCreate(w, r)
		if w.Code != 400 {
			t.Fatalf("%s accepted: %s", id, w.Body.String())
		}
	}
	drafts, _ := database.ListProductionRenders("toronto")
	if len(drafts) != 0 {
		t.Fatal("failed selection created a draft")
	}
}
func TestTalkCutsInheritTranscriptionWithoutTranscribingCards(t *testing.T) {
	for _, value := range []string{"true", "false"} {
		recipe := []json.RawMessage{json.RawMessage(`{"type":"streamctl.talkCard"}`), json.RawMessage(`{"type":"streamctl.talkCuts","transcribe":` + value + `,"overlay":"toronto/recordings/overlay.png"}`)}
		if err := validateProductionTemplateSegment(recipe[1]); err != nil {
			t.Fatal(err)
		}
		segments, err := expandProductionTemplate(recipe, "toronto", "a", "toronto/talks/a.png", []db.ProductionCut{{SourceType: "video", Source: "toronto/recordings/talk.mp4", InMS: 0, OutMS: 1000}})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(segments[0]), "transcribe") || !strings.Contains(string(segments[1]), `"transcribe":`+value) || !strings.Contains(string(segments[1]), "overlay.png") {
			t.Fatalf("segments %s", segments)
		}
	}
}

func TestLivestreamExplicitSelectionCanSpanDaysAndStages(t *testing.T) {
	d := productionHandlerTestDB(t)
	a := livestreamCandidate("a", "2026-07-22T10:00:00-04:00", "one")
	b := livestreamCandidate("b", "2026-07-22T11:00:00-04:00", "two")
	c := livestreamCandidate("c", "2026-07-23T10:00:00-04:00", "one")
	cut := db.ProductionCut{SourceType: "video", Source: "toronto/recordings/talk.mp4", InMS: 0, OutMS: 10000}
	for _, id := range []string{"a", "b", "c"} {
		if err := d.ReplaceProductionCuts("toronto", id, []db.ProductionCut{cut}); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handler{DB: d, BTCPP: productionCandidatesStub{candidates: []btcppclient.Candidate{c.Candidate, b.Candidate, a.Candidate}}}
	form := url.Values{"conference": {"toronto"}, "include_talks": {"true"}, "template_id": {"0"}, "talk_ids": {"c", "a", "b"}}
	r := httptest.NewRequest("POST", "/production/renders/create", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.productionRenderCreate(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	drafts, err := d.ListProductionRenders("toronto")
	if err != nil || len(drafts) != 1 {
		t.Fatalf("drafts %v %v", drafts, err)
	}
	var manifest productionRenderManifest
	if err := json.Unmarshal([]byte(drafts[0].JSON), &manifest); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"a", "b", "c"} {
		clip, err := renderdraft.DecodeClip(manifest.Jobs[0].Segments[i])
		if err != nil || clip.Talk.ID != want {
			t.Fatalf("clip %d=%+v %v", i, clip, err)
		}
	}
}

func TestRestrictedTalkRequiresIndividualSelection(t *testing.T) {
	d := productionHandlerTestDB(t)
	talk := livestreamCandidate("restricted", "2026-07-22T10:00:00-04:00", "one")
	talk.Eligible = false
	talk.RecordingPolicy = "prohibited"
	talk.Reasons = []string{"speaker_recording_consent_prohibits_recording"}
	cut := db.ProductionCut{SourceType: "video", Source: "toronto/recordings/talk.mp4", InMS: 0, OutMS: 10000}
	if err := d.ReplaceProductionCuts("toronto", talk.TalkID, []db.ProductionCut{cut}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: d, BTCPP: productionCandidatesStub{candidates: []btcppclient.Candidate{talk.Candidate}}}
	w := httptest.NewRecorder()
	h.productionLivestreamOptions(w, httptest.NewRequest("GET", "/production/renders/create-options?conference=toronto", nil))
	if !strings.Contains(w.Body.String(), `"manualOnly":true`) || !strings.Contains(w.Body.String(), `"ready":true`) {
		t.Fatal(w.Body.String())
	}
	for _, explicit := range []bool{false, true} {
		form := url.Values{"conference": {"toronto"}, "include_talks": {"true"}, "template_id": {"0"}}
		if explicit {
			form.Add("talk_ids", "restricted")
		}
		r := httptest.NewRequest("POST", "/production/renders/create", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w = httptest.NewRecorder()
		h.productionRenderCreate(w, r)
		want := 400
		if explicit {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("explicit=%v: %d %s", explicit, w.Code, w.Body.String())
		}
	}
	drafts, _ := d.ListProductionRenders("toronto")
	if len(drafts) != 1 || !strings.Contains(drafts[0].JSON, `"recordingPolicy": "prohibited"`) {
		t.Fatalf("restricted metadata lost: %+v", drafts)
	}
}
