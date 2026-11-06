package handlers

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/http/httptest"
	"net/url"
	"strconv"
	"streamctl/internal/btcppclient"
	"streamctl/internal/db"
	"streamctl/internal/renderdraft"
	"strings"
	"testing"
)

func TestTemplateClipExpansionIsAtomicOrderedAndDoesNotSave(t *testing.T) {
	database := productionHandlerTestDB(t)
	a := livestreamCandidate("a", "2026-10-01T12:00:00+02:00", "Main")
	a.Eligible = true
	a.SocialCard = "berlin26/talks/a.png"
	b := livestreamCandidate("b", "2026-10-01T09:00:00Z", "Main")
	b.Eligible = true
	b.SocialCard = "berlin26/talks/b.png"
	h := &Handler{DB: database, funcs: template.FuncMap{}, BTCPP: productionCandidatesStub{candidates: []btcppclient.Candidate{a.Candidate, b.Candidate}}}
	cut := db.ProductionCut{SourceType: "video", Source: "berlin26/recordings/talk.mp4", InMS: 1000, OutMS: 2000}
	for _, id := range []string{"a", "b"} {
		if err := database.ReplaceProductionCuts("berlin26", id, []db.ProductionCut{cut}); err != nil {
			t.Fatal(err)
		}
	}
	expand := func(id string, talks ...string) *httptest.ResponseRecorder {
		form := url.Values{"conference": {"berlin26"}, "template_id": {id}, "talk_ids": talks}
		r := httptest.NewRequest("POST", "/production/renders/expand-template", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.productionExpandTemplate(w, r)
		return w
	}
	w := expand("0", "a", "b")
	var result struct {
		Clips []renderdraft.Clip `json:"clips"`
	}
	json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 200 || len(result.Clips) != 2 || result.Clips[0].Talk.ID != "b" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if items, _ := database.ListProductionRenders("berlin26"); len(items) != 0 {
		t.Fatal("expansion saved a project")
	}
	saved := string(result.Clips[0].Segments[1])
	database.ReplaceProductionCuts("berlin26", "b", nil)
	if string(result.Clips[0].Segments[1]) != saved {
		t.Fatal("snapshot changed")
	}
	if w = expand("0", "a", "b"); w.Code != 400 || strings.Contains(w.Body.String(), `"clips"`) {
		t.Fatalf("partial result: %s", w.Body)
	}
	if w = expand("0", "missing"); w.Code != 400 {
		t.Fatal("accepted missing talk")
	}
	if w = expand("9999", "a"); w.Code != 400 {
		t.Fatal("accepted missing template")
	}
	id, err := database.CreateProductionTemplate("berlin26", "Static", `{"version":1,"settings":{},"segments":[{"type":"image","src":"berlin26/card.png","durationMs":4000}]}`)
	if err != nil {
		t.Fatal(err)
	}
	w = expand(strconv.FormatInt(id, 10))
	result.Clips = nil
	json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 200 || len(result.Clips) != 1 || result.Clips[0].Name != "Static" || result.Clips[0].Talk != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}
func TestClipWorkerCompilationMatchesLegacyAndRejectsInvalidProvenance(t *testing.T) {
	leaf := json.RawMessage(`{"type":"video","src":"berlin26/talk.mp4","in":"00:00:01","out":"00:00:03","streamctl":{"talkTitle":"Talk"}}`)
	clip := renderdraft.Clip{Type: renderdraft.ClipType, Name: "Talk", Segments: []json.RawMessage{leaf}}
	raw, _ := json.Marshal(clip)
	old, _ := productionRenderManifestJSON("same", json.RawMessage(`{}`), []json.RawMessage{leaf})
	grouped, _ := productionRenderManifestJSON("same", json.RawMessage(`{}`), []json.RawMessage{raw})
	a, err := productionWorkerManifest(old)
	if err != nil {
		t.Fatal(err)
	}
	b, err := productionWorkerManifest(grouped)
	if err != nil || a != b {
		t.Fatalf("compilation changed: %v\n%s\n%s", err, a, b)
	}
	clip.Template = &renderdraft.Template{ID: 1, Name: "Bad", Recipe: []json.RawMessage{json.RawMessage(`{"type":"streamctl.talkCuts","unknown":true}`)}}
	raw, _ = json.Marshal(clip)
	grouped, _ = productionRenderManifestJSON("same", json.RawMessage(`{}`), []json.RawMessage{raw})
	if _, err := productionWorkerManifest(grouped); err == nil {
		t.Fatal("unknown provenance accepted")
	}
	clip.Template = nil
	clip.Segments = []json.RawMessage{}
	raw, _ = json.Marshal(clip)
	segments := []json.RawMessage{raw}
	definition, _ := json.Marshal(productionTemplateDefinition{Version: 1, Settings: json.RawMessage(`{}`), Segments: &segments})
	if _, err := validateProductionRenderEditor(definition); err != nil {
		t.Fatalf("blank draft: %v", err)
	}
}

func TestTemplateOptionsKeepStaticTemplatesAvailableWithoutTalks(t *testing.T) {
	database := productionHandlerTestDB(t)
	id, err := database.CreateProductionTemplate("berlin26", "Sponsors", `{"version":1,"settings":{},"segments":[{"type":"image","src":"berlin26/sponsors.png"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: database, BTCPP: productionCandidatesStub{err: errors.New("website unavailable")}}
	w := httptest.NewRecorder()
	h.productionTemplateOptions(w, httptest.NewRequest("GET", "/production/renders/template-options?conference=berlin26", nil))
	var result struct {
		Templates []struct {
			ID        int64 `json:"id"`
			NeedsTalk bool  `json:"needsTalk"`
		} `json:"templates"`
		Talks      []productionTalkView `json:"talks"`
		TalksError string               `json:"talksError"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || result.TalksError == "" || result.Talks == nil || len(result.Talks) != 0 {
		t.Fatalf("options unavailable: %d %s", w.Code, w.Body)
	}
	found := false
	for _, option := range result.Templates {
		if option.ID == id && !option.NeedsTalk {
			found = true
		}
	}
	if !found {
		t.Fatalf("static template missing: %s", w.Body)
	}
	for _, templateID := range []int64{id, 0} {
		form := url.Values{"conference": {"berlin26"}, "template_id": {strconv.FormatInt(templateID, 10)}}
		r := httptest.NewRequest("POST", "/production/renders/expand-template", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.productionExpandTemplate(w, r)
		want := 200
		if templateID == 0 {
			want = 400
		}
		if w.Code != want {
			t.Fatalf("template %d: %d %s", templateID, w.Code, w.Body)
		}
	}
}
