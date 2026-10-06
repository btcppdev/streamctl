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
)

func TestProductionRenderEditorRejectsDynamicSegments(t *testing.T) {
	if _, err := validateProductionRenderEditor([]byte(`{"version":1,"settings":{},"segments":[{"type":"streamctl.talkCuts"}]}`)); err == nil {
		t.Fatal("accepted dynamic segment in render")
	}
	if _, err := validateProductionRenderEditor([]byte(`{"version":1,"settings":{},"segments":[{"type":"image","src":"toronto/card.png"}]}`)); err != nil {
		t.Fatal(err)
	}
}

func TestDecorateProductionRenderLifecycle(t *testing.T) {
	manifest := `{"version":1,"settings":{},"jobs":[{"id":"talk","segments":[{"type":"video","src":"toronto/recordings/raw/talk.mp4"}]}]}`
	view := productionRenderListItem{ProductionRender: db.ProductionRender{Name: "Talk", JSON: manifest}}
	decorateProductionRender(&view, db.ProductionRenderQueueState{})
	if view.Status != "draft" || !view.CanQueue {
		t.Fatalf("draft=%+v", view)
	}
	queued := db.RenderJobQueueItem{Name: "Talk", ManifestJSON: manifest, Status: "queued"}
	decorateProductionRender(&view, db.ProductionRenderQueueState{Latest: &queued})
	if view.Status != "queued" || view.CanQueue {
		t.Fatalf("queued=%+v", view)
	}
	finished := db.RenderJobQueueItem{Name: "Talk", ManifestJSON: manifest, Status: "finished"}
	decorateProductionRender(&view, db.ProductionRenderQueueState{Latest: &finished, HasFinished: true})
	if view.Status != "finished" || view.StatusLabel != "Complete" || !view.CanQueue || !view.HasFinished {
		t.Fatalf("finished=%+v", view)
	}
	view.JSON = strings.Replace(manifest, "talk.mp4", "talk-v2.mp4", 1)
	decorateProductionRender(&view, db.ProductionRenderQueueState{Latest: &finished, HasFinished: true})
	if view.Status != "edited" || view.StatusLabel != "Edited" || !view.CanQueue {
		t.Fatalf("edited=%+v", view)
	}
}

func TestExpandProductionTemplate(t *testing.T) {
	source := []json.RawMessage{
		json.RawMessage(`{"type":"streamctl.talkCard","durationMs":5000}`),
		json.RawMessage(`{"type":"streamctl.talkCuts","overlay":"toronto/recordings/assets/bug.png"}`),
	}
	cuts := []db.ProductionCut{{Source: "toronto/recordings/raw/mix/main_0000.mp4", SourceType: "chunkedVideo", InMS: 1001, OutMS: 2002}}
	segments, err := expandProductionTemplate(source, "toronto", "talk-1", "/toronto/talks/custom.png", cuts)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(segments)
	for _, want := range []string{"toronto/talks/custom.png", "00:00:01.001", "00:00:02.002", "chunkedVideo", "bug.png"} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("expanded segments omitted %q: %s", want, encoded)
		}
	}
}

func TestTalkCardUsesSavedAPIPath(t *testing.T) {
	for _, tc := range []struct{ name, card, want string }{
		{"legacy ID", "/berlin25/talks/legacy-id-1080p.png", "berlin25/talks/legacy-id-1080p.png"},
		{"object key", "berlin25/talks/custom.png", "berlin25/talks/custom.png"},
		{"Spaces URL", "https://btcpp.nyc3.digitaloceanspaces.com/berlin25/talks/legacy-id-1080p.png", "berlin25/talks/legacy-id-1080p.png"},
		{"Spaces CDN URL", "https://btcpp.nyc3.cdn.digitaloceanspaces.com/berlin25/talks/legacy-id-1080p.png", "berlin25/talks/legacy-id-1080p.png"},
		{"escaped URL path", "https://btcpp.nyc3.digitaloceanspaces.com/berlin25/talks/custom%20card.png", "berlin25/talks/custom card.png"},
		{"missing", "", ""},
		{"whitespace", "  ", ""},
		{"traversal", "berlin25/../private.png", ""},
		{"URL", "https://example.com/card.png", ""},
		{"other bucket", "https://other.nyc3.digitaloceanspaces.com/berlin25/talks/card.png", ""},
		{"host suffix", "https://btcpp.nyc3.digitaloceanspaces.com.example.com/berlin25/talks/card.png", ""},
		{"insecure URL", "http://btcpp.nyc3.digitaloceanspaces.com/berlin25/talks/card.png", ""},
		{"URL credentials", "https://user@btcpp.nyc3.digitaloceanspaces.com/berlin25/talks/card.png", ""},
		{"URL query", "https://btcpp.nyc3.digitaloceanspaces.com/berlin25/talks/card.png?download=1", ""},
		{"URL fragment", "https://btcpp.nyc3.digitaloceanspaces.com/berlin25/talks/card.png#fragment", ""},
		{"escaped traversal", "https://btcpp.nyc3.digitaloceanspaces.com/berlin25/%2e%2e/card.png", ""},
		{"malformed URL", "https://btcpp.nyc3.digitaloceanspaces.com/berlin25/talks/%zz.png", ""},
		{"protocol relative URL", "//btcpp.nyc3.digitaloceanspaces.com/berlin25/talks/card.png", ""},
		{"other conference", "/toronto/talks/card.png", ""},
		{"backslash", `berlin25/talks\card.png`, ""},
		{"not image", "berlin25/talks/video.mp4", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			segments, err := expandProductionTemplate([]json.RawMessage{json.RawMessage(`{"type":"streamctl.talkCard","durationMs":5000}`)}, "berlin25", "new-talk-id", tc.card, nil)
			if tc.want == "" {
				if err == nil {
					t.Fatal("expected an error, not a guessed filename")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Type       string `json:"type"`
				Src        string `json:"src"`
				DurationMS int    `json:"durationMs"`
			}
			if err := json.Unmarshal(segments[0], &result); err != nil {
				t.Fatal(err)
			}
			if result.Type != "image" || result.Src != tc.want || result.DurationMS != 5000 {
				t.Fatalf("resolved card: %+v", result)
			}
		})
	}
}

func TestProductionRendersGenerateAndEdit(t *testing.T) {
	database := productionHandlerTestDB(t)
	templateID, err := database.CreateProductionTemplate("toronto", "Standard talk", `{"version":1,"settings":{},"segments":[{"type":"streamctl.talkCard","durationMs":5000},{"type":"streamctl.talkCuts"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ReplaceProductionCuts("toronto", "talk-1", []db.ProductionCut{{Source: "toronto/recordings/raw/mix/main.mp4", SourceType: "video", InMS: 1000, OutMS: 2000}}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: database, funcs: template.FuncMap{}, BTCPP: productionCandidatesStub{
		conferences: []btcppclient.Conference{{Tag: "toronto", Description: "Toronto++"}},
		candidates:  []btcppclient.Candidate{{TalkID: "talk-1", Title: "A Great Talk", Venue: "one", SocialCard: "https://btcpp.nyc3.digitaloceanspaces.com/toronto/talks/legacy-id-1080p.png"}, {TalkID: "talk-2", Title: "Uncut"}},
	}}
	form := url.Values{"conference": {"toronto"}, "template_id": {strconv.FormatInt(templateID, 10)}}
	request := httptest.NewRequest(http.MethodPost, "/production/renders/generate", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	h.productionRenderGenerate(response, request)
	if response.Code != http.StatusSeeOther || !strings.Contains(response.Header().Get("Location"), "generated=1") {
		t.Fatalf("status=%d location=%q body=%s", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	items, err := database.ListProductionRenders("toronto")
	if err != nil || len(items) != 1 || strings.Contains(items[0].JSON, "streamctl.") || !strings.Contains(items[0].JSON, `"id": "a-great-talk"`) {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	if !strings.Contains(items[0].JSON, `"src": "toronto/talks/legacy-id-1080p.png"`) {
		t.Fatalf("render did not preserve the saved card's object key: %s", items[0].JSON)
	}
	response = httptest.NewRecorder()
	h.productionRenders(response, httptest.NewRequest(http.MethodGet, "/production/renders?conference=toronto", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	for _, want := range []string{"Renders", "+ New render", "production-new-menu", "From template", ">Blank<", "Standard talk", "A Great Talk", "/production/renders/edit?conference=toronto&amp;id=", "Select all", "selection-actions", ">Queue</button>", ">Delete</button>", "Draft"} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("render list omitted %q: %s", want, response.Body.String())
		}
	}

	response = httptest.NewRecorder()
	h.productionRenderEdit(response, httptest.NewRequest(http.MethodGet, productionRenderURL("toronto", items[0].ID), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{"A Great Talk", "Back to renders", "Render settings", "+ Add segment", "Choose media", "Duplicate", "Delete", ">Save</button>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("render editor omitted %q: %s", want, body)
		}
	}
	if strings.Contains(body, "The saved source ranges for each talk") || strings.Contains(body, "The current talk’s saved 1080p social card") || strings.Contains(body, "data-add=\"streamctl.") {
		t.Fatalf("render editor exposed dynamic segments: %s", body)
	}

	queueForm := url.Values{"conference": {"toronto"}, "ids": {strconv.FormatInt(items[0].ID, 10)}}
	request = httptest.NewRequest(http.MethodPost, "/production/renders/queue", strings.NewReader(queueForm.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response = httptest.NewRecorder()
	h.productionRendersQueue(response, request)
	if response.Code != http.StatusSeeOther || !strings.Contains(response.Header().Get("Location"), "queued=1") {
		t.Fatalf("queue status=%d location=%q body=%s", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	states, err := database.ProductionRenderQueueStates("toronto")
	if err != nil || states[items[0].ID].Latest == nil || states[items[0].ID].Latest.Status != "queued" {
		t.Fatalf("states=%+v err=%v", states, err)
	}
	job := states[items[0].ID].Latest
	if err := database.MarkRenderQueueRunning(job.ID, "render.service"); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkRenderQueueFinished(job.ID, "finished", ""); err != nil {
		t.Fatal(err)
	}
	editedManifest := strings.Replace(items[0].JSON, "00:00:02.000", "00:00:03.000", 1)
	if err := database.UpdateProductionRender(items[0].ID, "toronto", items[0].Name, editedManifest); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	h.productionRenders(response, httptest.NewRequest(http.MethodGet, "/production/renders?conference=toronto", nil))
	body = response.Body.String()
	for _, want := range []string{"Edited", `data-finished="true"`, `data-delete="true"`, `data-queue="true"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("edited render list omitted %q: %s", want, body)
		}
	}
	response = httptest.NewRecorder()
	h.productionRenderEdit(response, httptest.NewRequest(http.MethodGet, productionRenderURL("toronto", items[0].ID), nil))
	if !strings.Contains(response.Body.String(), "Delete this render") {
		t.Fatalf("sent render editor omitted delete action: %s", response.Body.String())
	}
}

func TestProductionRenderGenerateRejectsUnavailableTalkCard(t *testing.T) {
	for _, card := range []string{"", "https://example.com/toronto/talks/card.png"} {
		t.Run(card, func(t *testing.T) {
			database := productionHandlerTestDB(t)
			templateID, err := database.CreateProductionTemplate("toronto", "Standard talk", `{"version":1,"settings":{},"segments":[{"type":"streamctl.talkCard"},{"type":"streamctl.talkCuts"}]}`)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.ReplaceProductionCuts("toronto", "talk-1", []db.ProductionCut{{Source: "toronto/recordings/raw/talk.mp4", SourceType: "video", InMS: 0, OutMS: 1000}}); err != nil {
				t.Fatal(err)
			}
			h := &Handler{DB: database, BTCPP: productionCandidatesStub{
				candidates: []btcppclient.Candidate{{TalkID: "talk-1", Title: "A Great Talk", SocialCard: card}},
			}}
			form := url.Values{"conference": {"toronto"}, "template_id": {strconv.FormatInt(templateID, 10)}}
			request := httptest.NewRequest(http.MethodPost, "/production/renders/generate", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			response := httptest.NewRecorder()
			h.productionRenderGenerate(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "A Great Talk") || !strings.Contains(response.Body.String(), "social card") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			items, err := database.ListProductionRenders("toronto")
			if err != nil || len(items) != 0 {
				t.Fatalf("invalid card created a render: items=%+v err=%v", items, err)
			}
		})
	}
}

func TestProductionRenderCreateUsesEditableDefaultName(t *testing.T) {
	database := productionHandlerTestDB(t)
	h := &Handler{DB: database}
	form := url.Values{"conference": {"toronto"}}
	request := httptest.NewRequest(http.MethodPost, "/production/renders/create", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	h.productionRenderCreate(response, request)
	if response.Code != http.StatusSeeOther || !strings.Contains(response.Header().Get("Location"), "created=1") {
		t.Fatalf("status=%d location=%q body=%s", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	items, err := database.ListProductionRenders("toronto")
	if err != nil || len(items) != 1 || items[0].Name != "Untitled render" {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	h.funcs = template.FuncMap{}
	h.BTCPP = productionCandidatesStub{}
	response = httptest.NewRecorder()
	h.productionRenderEdit(response, httptest.NewRequest(http.MethodGet, productionRenderURL("toronto", items[0].ID)+"&created=1", nil))
	if !strings.Contains(response.Body.String(), "newItemName.select()") {
		t.Fatalf("new render name was not selected: %s", response.Body.String())
	}
}
