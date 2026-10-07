package handlers

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"streamctl/internal/btcppclient"
)

func TestValidateProductionTemplate(t *testing.T) {
	valid := `{
		"version": 1,
		"settings": {"width": 1920, "videoEncoder": "auto", "videoBitrate": "6800k"},
		"segments": [
			{"type":"image","src":"toronto/recordings/assets/card.png","durationMs":4000},
			{"type":"video","src":"toronto/recordings/assets/intro.mp4","in":"00:00:01.000","out":"00:00:03.000","overlay":"toronto/recordings/assets/bug.png","transcribe":false},
			{"type":"streamctl.talkCard","durationMs":5000},
			{"type":"streamctl.talkCuts","overlay":"toronto/recordings/assets/bug.png"}
		]
	}`
	canonical, err := validateProductionTemplate([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"version": 1`, `"type": "streamctl.talkCard"`, `"type": "streamctl.talkCuts"`, `"src": "toronto/recordings/assets/intro.mp4"`} {
		if !strings.Contains(canonical, want) {
			t.Fatalf("canonical template omitted %q: %s", want, canonical)
		}
	}
}

func TestValidateProductionTemplateRejectsInvalidDefinitions(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"missing settings", `{"version":1,"segments":[]}`},
		{"missing segments", `{"version":1,"settings":{}}`},
		{"unknown setting", `{"version":1,"settings":{"wat":1},"segments":[]}`},
		{"unknown static field", `{"version":1,"settings":{},"segments":[{"type":"image","src":"toronto/recordings/card.png","wat":1}]}`},
		{"missing source", `{"version":1,"settings":{},"segments":[{"type":"video"}]}`},
		{"absolute source", `{"version":1,"settings":{},"segments":[{"type":"video","src":"/recordings/intro.mp4"}]}`},
		{"video with image source", `{"version":1,"settings":{},"segments":[{"type":"video","src":"toronto/recordings/card.png"}]}`},
		{"image with video source", `{"version":1,"settings":{},"segments":[{"type":"image","src":"toronto/recordings/intro.mp4"}]}`},
		{"video overlay", `{"version":1,"settings":{},"segments":[{"type":"video","src":"toronto/recordings/intro.mp4","overlay":"toronto/recordings/bug.mp4"}]}`},
		{"video used as audio", `{"version":1,"settings":{},"segments":[{"type":"video","src":"toronto/recordings/intro.mp4","audio":{"src":"toronto/recordings/music.mp4"}}]}`},
		{"dynamic source", `{"version":1,"settings":{},"segments":[{"type":"streamctl.talkCuts","src":"toronto/recordings/raw.mp4"}]}`},
		{"dynamic transcription", `{"version":1,"settings":{},"segments":[{"type":"streamctl.talkCuts","transcribe":true}]}`},
		{"talk card source", `{"version":1,"settings":{},"segments":[{"type":"streamctl.talkCard","src":"toronto/card.png"}]}`},
		{"talk card duration", `{"version":1,"settings":{},"segments":[{"type":"streamctl.talkCard","durationMs":0}]}`},
		{"bad window", `{"version":1,"settings":{},"segments":[{"type":"video","src":"toronto/recordings/raw.mp4","in":"00:00:05.000","out":"00:00:04.000"}]}`},
		{"unknown type", `{"version":1,"settings":{},"segments":[{"type":"streamctl.magic"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateProductionTemplate([]byte(test.raw)); err == nil {
				t.Fatalf("accepted %s", test.raw)
			}
		})
	}
}

func TestValidateProductionTemplateAllowsAnyBucketObject(t *testing.T) {
	for _, source := range []string{"nairobi/recordings/assets/intro.mp4", "shared.mp4"} {
		raw := `{"version":1,"settings":{},"segments":[{"type":"video","src":"` + source + `"}]}`
		if _, err := validateProductionTemplate([]byte(raw)); err != nil {
			t.Fatalf("source %q: %v", source, err)
		}
	}
}

func TestProductionTemplatesListAndEditor(t *testing.T) {
	database := productionHandlerTestDB(t)
	id, err := database.CreateProductionTemplate("toronto", "Standard talk", `{"version":1,"settings":{},"segments":[{"type":"streamctl.talkCuts"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: database, funcs: template.FuncMap{}, BTCPP: productionCandidatesStub{conferences: []btcppclient.Conference{{Tag: "toronto", Description: "Toronto++"}}}}
	response := httptest.NewRecorder()
	h.productionTemplates(response, httptest.NewRequest(http.MethodGet, "/production/templates?conference=toronto", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, want := range []string{"Templates", "+ New template", "production-quick-create", "Standard talk", "1 segment", "/production/templates/edit?conference=toronto", ">Talks</a>", "Select all", "selection-actions", ">Delete</button>", "/production/templates/delete-selected"} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("template list omitted %q: %s", want, response.Body.String())
		}
	}

	response = httptest.NewRecorder()
	editURL := productionTemplateURL("toronto", id)
	h.productionTemplateEdit(response, httptest.NewRequest(http.MethodGet, editURL, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, want := range []string{"Standard talk", "Render settings", "+ Add segment", "A video file or numbered sequence", "The current talk’s saved 1080p social card", "The saved source ranges for each talk", "streamctl.talkCard", "streamctl.talkCuts", "Choose media", "Duplicate", "Delete", ">Save</button>", "form=\"template-form\"", "template-editor-head", "min-height:66px", "template-editor-title", "template-actions", "position:sticky", "position:fixed", "safe-area-inset-bottom", "Drag to reorder", "template-workspace", "grid-template-columns:minmax(0,2fr) minmax(360px,1fr)", "template-side", "add.onclick=openSegmentDialog", "segment.type=file.sourceType", "createProductionMediaBrowser", "createProductionMediaPreview", "onMark:markPreview", "previewSelected(atMs)", "selectSegment(index,value)", "markPreview", "media-preview-mark-in", "media-preview-fine-row", "preview.setMarks", "Out must be after In", "allowBucketRoot:true", "kind!=='video'||file.proxyStatus==='finished'"} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("template editor omitted %q: %s", want, response.Body.String())
		}
	}
	if strings.Contains(response.Body.String(), `id="live-preview-mode"`) || strings.Contains(response.Body.String(), `<div class="project-preview"`) {
		t.Fatal("template editor exposed the render-only project player")
	}
	if strings.Contains(response.Body.String(), "template-preview-actions") {
		t.Fatalf("template editor retained controls outside the shared preview card: %s", response.Body.String())
	}
	if strings.Index(response.Body.String(), "template-actions") > strings.Index(response.Body.String(), "template-editor\"") {
		t.Fatalf("template actions are not in the sticky editor header: %s", response.Body.String())
	}
}

func TestProductionTemplateCreateUsesEditableDefaultName(t *testing.T) {
	database := productionHandlerTestDB(t)
	h := &Handler{DB: database}
	form := url.Values{"conference": {"toronto"}}
	request := httptest.NewRequest(http.MethodPost, "/production/templates/create", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	h.productionTemplateCreate(response, request)
	if response.Code != http.StatusSeeOther || !strings.Contains(response.Header().Get("Location"), "created=1") {
		t.Fatalf("status=%d location=%q body=%s", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	items, err := database.ListProductionTemplates("toronto")
	if err != nil || len(items) != 1 || items[0].Name != "Untitled template" {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	h.funcs = template.FuncMap{}
	h.BTCPP = productionCandidatesStub{}
	response = httptest.NewRecorder()
	h.productionTemplateEdit(response, httptest.NewRequest(http.MethodGet, productionTemplateURL("toronto", items[0].ID)+"&created=1", nil))
	if !strings.Contains(response.Body.String(), "newItemName.select()") {
		t.Fatalf("new template name was not selected: %s", response.Body.String())
	}
}

func TestProductionTemplateSaveValidatesAndPersists(t *testing.T) {
	database := productionHandlerTestDB(t)
	id, err := database.CreateProductionTemplate("toronto", "Draft", defaultProductionTemplateJSON)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: database}
	definition := `{"version":1,"settings":{},"segments":[{"type":"streamctl.talkCuts","overlay":"toronto/recordings/assets/bug.png"}]}`
	form := url.Values{"conference": {"toronto"}, "id": {"1"}, "name": {"Standard talk"}, "template": {definition}}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/production/templates/save", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.productionTemplateSave(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	item, err := database.ProductionTemplate(id, "toronto")
	if err != nil || item.Name != "Standard talk" || !strings.Contains(item.JSON, "streamctl.talkCuts") {
		t.Fatalf("template=%+v err=%v", item, err)
	}
}
