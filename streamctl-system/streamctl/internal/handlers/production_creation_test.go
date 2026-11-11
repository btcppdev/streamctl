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

func creationFixture(t *testing.T) (*Handler, []productionTalkView) {
	t.Helper()
	d := productionHandlerTestDB(t)
	talks := []productionTalkView{livestreamCandidate("a", "2026-07-22T10:00:00-04:00", "one"), livestreamCandidate("b", "2026-07-22T11:00:00-04:00", "one"), livestreamCandidate("uncut", "2026-07-23T10:00:00-04:00", "one")}
	for _, talk := range talks[:2] {
		if err := d.ReplaceProductionCuts("toronto", talk.TalkID, []db.ProductionCut{{SourceType: "video", Source: "toronto/recordings/talk.mp4", InMS: 0, OutMS: 1000}}); err != nil {
			t.Fatal(err)
		}
	}
	candidates := []btcppclient.Candidate{}
	for _, talk := range talks {
		candidates = append(candidates, talk.Candidate)
	}
	return &Handler{DB: d, BTCPP: productionCandidatesStub{candidates: candidates}}, talks
}
func createDraftRequest(h *Handler, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/production/renders/create", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.productionRenderCreate(w, r)
	return w
}
func TestUnifiedCreationBlankIgnoresHiddenSelections(t *testing.T) {
	h, _ := creationFixture(t)
	w := createDraftRequest(h, url.Values{"conference": {"toronto"}, "talk_ids": {"uncut"}, "template_id": {"999"}, "output": {"separate"}, "settings": {`{"fps":60}`}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	items, _ := h.DB.ListProductionRenders("toronto")
	def, err := productionRenderEditorDefinition(items[0].JSON)
	if err != nil || len(*def.Segments) != 0 || !strings.Contains(string(def.Settings), "60") || def.Streamctl.RegisterRecording {
		t.Fatalf("blank %+v %v", def, err)
	}
}
func TestUnifiedCreationBatchSnapshotsExplicitTargetsAndStripsMetadata(t *testing.T) {
	h, _ := creationFixture(t)
	form := url.Values{"conference": {"toronto"}, "include_talks": {"true"}, "output": {"separate"}, "template_id": {"-1"}, "talk_ids": {"b", "a"}, "register_recording": {"true"}, "settings": {`{"width":1280,"height":720}`}}
	w := createDraftRequest(h, form)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	items, _ := h.DB.ListProductionRenders("toronto")
	if len(items) != 2 {
		t.Fatal(items)
	}
	for _, item := range items {
		def, err := productionRenderEditorDefinition(item.JSON)
		if err != nil {
			t.Fatal(err)
		}
		if !def.Streamctl.RegisterRecording || def.Streamctl.RecordingTalkID != item.Name {
			t.Fatalf("target %+v", def.Streamctl)
		}
		clip, err := renderdraft.DecodeClip((*def.Segments)[0])
		if err != nil || len(clip.Segments) != 1 || clip.Talk.ID != item.Name {
			t.Fatalf("clip %+v %v", clip, err)
		}
		worker, err := productionWorkerManifest(item.JSON)
		if err != nil || strings.Contains(worker, "streamctl") || !strings.Contains(worker, "1280") {
			t.Fatalf("worker %s %v", worker, err)
		}
		job, err := h.DB.EnqueueProductionRender(item.ID, item.Name, worker)
		if err != nil {
			t.Fatal(err)
		}
		// Subsequent edits cannot redirect a queued registration.
		if err := h.DB.UpdateProductionRender(item.ID, "toronto", item.Name, `{"version":1,"streamctl":{"registerRecording":false},"jobs":[]}`); err != nil {
			t.Fatal(err)
		}
		var target string
		if err := h.DB.QueryRow("SELECT talk_id FROM recording_registrations WHERE queue_id=?", job.ID).Scan(&target); err != nil || target != item.Name {
			t.Fatalf("snapshot %s %v", target, err)
		}
	}
}
func TestUnifiedCreationInvalidBatchCreatesNothing(t *testing.T) {
	h, _ := creationFixture(t)
	w := createDraftRequest(h, url.Values{"conference": {"toronto"}, "include_talks": {"true"}, "output": {"separate"}, "template_id": {"-1"}, "talk_ids": {"a", "uncut"}})
	items, _ := h.DB.ListProductionRenders("toronto")
	if w.Code != 400 || len(items) != 0 {
		t.Fatalf("partial creation %d %+v", w.Code, items)
	}
}
func TestTemplateRegistrationDoesNotEnableRenderRegistration(t *testing.T) {
	h, _ := creationFixture(t)
	definition := `{"version":1,"streamctl":{"registerRecording":true},"settings":{"fps":60},"segments":[{"type":"streamctl.talkCuts","transcribe":true}]}`
	id, err := h.DB.CreateProductionTemplate("toronto", "Talk", definition)
	if err != nil {
		t.Fatal(err)
	}
	w := createDraftRequest(h, url.Values{"conference": {"toronto"}, "include_talks": {"true"}, "output": {"one"}, "template_id": {"-1"}, "talk_ids": {"a", "b"}, "register_recording": {"true"}})
	if w.Code != 400 {
		t.Fatal("target was not required")
	}
	// A disabled registration remains disabled regardless of clip talk metadata.
	w = createDraftRequest(h, url.Values{"conference": {"toronto"}, "include_talks": {"true"}, "output": {"one"}, "template_id": {strconv.FormatInt(id, 10)}, "talk_ids": {"a", "b"}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	items, _ := h.DB.ListProductionRenders("toronto")
	var m productionRenderManifest
	json.Unmarshal([]byte(items[0].JSON), &m)
	if m.Streamctl.RegisterRecording {
		t.Fatal("clips implicitly enabled registration")
	}
}

func TestEditorHelpersLoadWithoutCreatedFlagAndOldCutsURLsAreGone(t *testing.T) {
	h, _ := creationFixture(t)
	h.funcs = template.FuncMap{}
	w := httptest.NewRecorder()
	h.render(w, httptest.NewRequest("GET", "/production/renders/edit", nil), "production_template_edit.html", productionTemplateEditPage{Kind: "render", DefinitionJSON: template.JS(`{"version":1,"settings":{},"segments":[]}`)})
	body := w.Body.String()
	for _, name := range []string{"function populateRenderSettings", "function createRecordingControls"} {
		if !strings.Contains(body, name) || strings.Index(body, name) > strings.Index(body, "const registrationMetadata=") {
			t.Fatalf("missing early helper %s", name)
		}
	}
	h.Secret = "test"
	mux := http.NewServeMux()
	h.Register(mux)
	for _, path := range []string{"/production/timestamp?conference=toronto", "/production/timestamp/cut?conference=toronto", "/production/renders/create-livestream", "/production/renders/generate"} {
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: "streamctl_session", Value: "test"})
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatalf("old URL %s returned %d", path, w.Code)
		}
	}
}

func TestCreationDialogsSeparateSingleAndBatchFields(t *testing.T) {
	h, _ := creationFixture(t)
	h.funcs = template.FuncMap{}
	w := httptest.NewRecorder()
	h.productionRenders(w, httptest.NewRequest("GET", "/production/renders?conference=toronto", nil))
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatal(body)
	}
	extract := func(id string) string {
		t.Helper()
		start := strings.Index(body, `<dialog id="`+id+`"`)
		if start < 0 {
			t.Fatal("missing dialog", id)
		}
		tail := body[start:]
		return tail[:strings.Index(tail, "</dialog>")]
	}
	single, batch := extract("new-render-dialog"), extract("batch-render-dialog")
	if !strings.Contains(single, `name="name"`) || !strings.Contains(single, `type="checkbox" data-role="include-talks"`) {
		t.Fatal("single render fields missing")
	}
	if strings.Contains(batch, `name="name"`) || strings.Contains(batch, `type="checkbox" data-role="include-talks"`) || !strings.Contains(batch, `name="output" value="separate"`) {
		t.Fatal("batch includes single-render fields")
	}
	for _, dialog := range []string{single, batch} {
		if !strings.Contains(dialog, "<summary>Options</summary>") || strings.Contains(dialog, `data-role="options" open`) {
			t.Fatal("options should start collapsed")
		}
	}
}
