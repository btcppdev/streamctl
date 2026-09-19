package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"streamctl/internal/db"
)

type productionRenderListItem struct {
	db.ProductionRender
	SegmentCount    int
	UpdatedLabel    string
	URL             string
	Status          string
	StatusLabel     string
	BadgeClass      string
	CanQueue        bool
	Edited          bool
	HasFinished     bool
	RecordingStatus string
	RecordingError  string
}

type productionRendersPage struct {
	Nav       productionNavView
	Renders   []productionRenderListItem
	Templates []db.ProductionTemplate
	Generated int
	Skipped   int
	Queued    int
	Deleted   int
	Error     string
}

type productionRenderManifest struct {
	Version  int                   `json:"version"`
	Settings json.RawMessage       `json:"settings"`
	Jobs     []productionRenderJob `json:"jobs"`
}

type productionRenderJob struct {
	ID       string            `json:"id"`
	Segments []json.RawMessage `json:"segments"`
}

func (h *Handler) productionRenders(w http.ResponseWriter, r *http.Request) {
	conference := selectedProductionConference(r)
	rememberProductionConference(w, r, conference)
	page := productionRendersPage{
		Generated: queryInt(r, "generated"), Skipped: queryInt(r, "skipped"),
		Queued: queryInt(r, "queued"), Deleted: queryInt(r, "deleted"),
	}
	page.Nav = productionNavView{Active: "renders", Action: "/production/renders", Conference: conference}
	page.Nav.Conferences, page.Error = h.productionConferences(r.Context())
	if conference == "" {
		h.render(w, r, "production_renders.html", page)
		return
	}
	if !validProductionConference(conference) {
		page.Error = "Invalid conference tag."
		h.renderStatus(w, r, http.StatusBadRequest, "production_renders.html", page)
		return
	}
	items, err := h.DB.ListProductionRenders(conference)
	if err != nil {
		page.Error = err.Error()
	} else {
		states, stateErr := h.DB.ProductionRenderQueueStates(conference)
		if stateErr != nil {
			page.Error = stateErr.Error()
		}
		for _, item := range items {
			var manifest productionRenderManifest
			_ = json.Unmarshal([]byte(item.JSON), &manifest)
			count := 0
			if len(manifest.Jobs) == 1 {
				count = len(manifest.Jobs[0].Segments)
			}
			view := productionRenderListItem{
				ProductionRender: item, SegmentCount: count,
				UpdatedLabel: item.UpdatedAt.Format("2006-01-02 15:04"), URL: productionRenderURL(conference, item.ID),
			}
			decorateProductionRender(&view, states[item.ID])
			if latest := states[item.ID].Latest; latest != nil && latest.Status == "finished" {
				view.RecordingStatus, view.RecordingError, err = h.DB.RecordingRegistrationStatus(latest.ID)
				if err != nil {
					page.Error = err.Error()
				}
			}
			page.Renders = append(page.Renders, view)
		}
	}
	page.Templates, err = h.DB.ListProductionTemplates(conference)
	if err != nil && page.Error == "" {
		page.Error = err.Error()
	}
	h.render(w, r, "production_renders.html", page)
}

func (h *Handler) productionRenderEdit(w http.ResponseWriter, r *http.Request) {
	conference := selectedProductionConference(r)
	rememberProductionConference(w, r, conference)
	id, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("id")), 10, 64)
	if !validProductionConference(conference) || err != nil || id <= 0 {
		http.Error(w, "conference and render ID are required", http.StatusBadRequest)
		return
	}
	item, err := h.DB.ProductionRender(id, conference)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	editor, err := productionRenderEditorDefinition(item.JSON)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	encoded, err := json.Marshal(editor)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	conferences, conferenceError := h.productionConferences(r.Context())
	page := productionTemplateEditPage{
		Nav:    productionNavView{Active: "renders", Action: "/production/renders", Conference: conference, Conferences: conferences},
		ItemID: item.ID, Conference: item.Conference, Name: item.Name, DefinitionJSON: template.JS(encoded), Kind: "render",
		BackURL:    "/production/renders?conference=" + url.QueryEscape(conference),
		SaveAction: "/production/renders/save", DuplicateAction: "/production/renders/duplicate", DeleteAction: "/production/renders/delete",
		Saved: r.URL.Query().Get("saved") == "1", SelectName: r.URL.Query().Get("created") == "1", Error: conferenceError,
	}
	page.CanDelete = true
	h.render(w, r, "production_template_edit.html", page)
}

func (h *Handler) productionRenderCreate(w http.ResponseWriter, r *http.Request) {
	conference, name := strings.TrimSpace(r.FormValue("conference")), strings.TrimSpace(r.FormValue("name"))
	if !validProductionConference(conference) {
		http.Error(w, "conference is required", http.StatusBadRequest)
		return
	}
	if name == "" {
		name = "Untitled render"
	}
	manifest, err := productionRenderManifestJSON(name, json.RawMessage(`{}`), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id, _, err := h.DB.CreateProductionRender(conference, name, manifest, nil, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, productionRenderURL(conference, id)+"&created=1", http.StatusSeeOther)
}

func (h *Handler) productionRenderSave(w http.ResponseWriter, r *http.Request) {
	conference, name := strings.TrimSpace(r.FormValue("conference")), strings.TrimSpace(r.FormValue("name"))
	id, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("id")), 10, 64)
	if !validProductionConference(conference) || name == "" || err != nil || id <= 0 {
		http.Error(w, "render ID, conference, and name are required", http.StatusBadRequest)
		return
	}
	definition, err := validateProductionRenderEditor([]byte(r.FormValue("definition")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	manifest, err := productionRenderManifestJSON(name, definition.Settings, *definition.Segments)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.DB.UpdateProductionRender(id, conference, name, manifest); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, productionRenderURL(conference, id)+"&saved=1", http.StatusSeeOther)
}

func (h *Handler) productionRenderDuplicate(w http.ResponseWriter, r *http.Request) {
	conference := strings.TrimSpace(r.FormValue("conference"))
	id, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("id")), 10, 64)
	if !validProductionConference(conference) || err != nil || id <= 0 {
		http.Error(w, "render ID and conference are required", http.StatusBadRequest)
		return
	}
	item, err := h.DB.ProductionRender(id, conference)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	name := item.Name + " copy"
	editor, err := productionRenderEditorDefinition(item.JSON)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	manifest, err := productionRenderManifestJSON(name, editor.Settings, *editor.Segments)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	copyID, _, err := h.DB.CreateProductionRender(conference, name, manifest, nil, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, productionRenderURL(conference, copyID), http.StatusSeeOther)
}

func (h *Handler) productionRenderDelete(w http.ResponseWriter, r *http.Request) {
	conference := strings.TrimSpace(r.FormValue("conference"))
	id, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("id")), 10, 64)
	if !validProductionConference(conference) || err != nil || id <= 0 {
		http.Error(w, "render ID and conference are required", http.StatusBadRequest)
		return
	}
	if err := h.DB.ArchiveProductionRender(id, conference); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	http.Redirect(w, r, "/production/renders?conference="+url.QueryEscape(conference), http.StatusSeeOther)
}

func (h *Handler) productionRendersDelete(w http.ResponseWriter, r *http.Request) {
	conference := strings.TrimSpace(r.FormValue("conference"))
	ids, err := selectedProductionIDs(r)
	if !validProductionConference(conference) || err != nil {
		http.Error(w, "conference and selected renders are required", http.StatusBadRequest)
		return
	}
	deleted, err := h.DB.ArchiveProductionRenders(conference, ids)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/production/renders?conference=%s&deleted=%d", url.QueryEscape(conference), deleted), http.StatusSeeOther)
}

func (h *Handler) productionRendersQueue(w http.ResponseWriter, r *http.Request) {
	conference := strings.TrimSpace(r.FormValue("conference"))
	ids, err := selectedProductionIDs(r)
	if !validProductionConference(conference) || err != nil {
		http.Error(w, "conference and selected renders are required", http.StatusBadRequest)
		return
	}
	items := make([]db.ProductionRender, 0, len(ids))
	states, err := h.DB.ProductionRenderQueueStates(conference)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, id := range ids {
		item, err := h.DB.ProductionRender(id, conference)
		if err != nil {
			http.Error(w, fmt.Sprintf("render %d was not found", id), http.StatusBadRequest)
			return
		}
		if err := validateRenderManifest([]byte(item.JSON)); err != nil {
			http.Error(w, fmt.Sprintf("%s is not ready: %v", item.Name, err), http.StatusBadRequest)
			return
		}
		if latest := states[id].Latest; latest != nil && (latest.Status == "queued" || latest.Status == "running") {
			http.Error(w, fmt.Sprintf("%s is already queued or running", item.Name), http.StatusConflict)
			return
		}
		items = append(items, item)
	}
	queued := 0
	for _, item := range items {
		if _, err := h.DB.EnqueueProductionRender(item.ID, item.Name, item.JSON); err != nil {
			if errors.Is(err, db.ErrRenderAlreadyActive) {
				http.Error(w, fmt.Sprintf("%s is already queued or running", item.Name), http.StatusConflict)
			} else {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		queued++
	}
	go h.dispatchGPUQueueOnce(context.Background())
	http.Redirect(w, r, fmt.Sprintf("/production/renders?conference=%s&queued=%d", url.QueryEscape(conference), queued), http.StatusSeeOther)
}

func (h *Handler) productionRenderGenerate(w http.ResponseWriter, r *http.Request) {
	conference := strings.TrimSpace(r.FormValue("conference"))
	templateID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("template_id")), 10, 64)
	if !validProductionConference(conference) || err != nil || templateID <= 0 {
		http.Error(w, "conference and template are required", http.StatusBadRequest)
		return
	}
	recipe, err := h.DB.ProductionTemplate(templateID, conference)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
		} else {
			http.Error(w, err.Error(), 500)
		}
		return
	}
	var definition productionTemplateDefinition
	if err := json.Unmarshal([]byte(recipe.JSON), &definition); err != nil || definition.Segments == nil {
		http.Error(w, "template definition is invalid", http.StatusBadRequest)
		return
	}
	talks, err := h.productionTalks(r.Context(), conference)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	cuts, err := h.DB.ListProductionCuts(conference)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	generated, skipped := 0, 0
	// Only the full-talk generator opts into website recording registration.
	// Merely having a talk association is not sufficient (e.g. future clips).
	recordingKind := ""
	for _, raw := range *definition.Segments {
		var segment struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &segment) == nil && segment.Type == "streamctl.talkCuts" {
			recordingKind = "full_talk"
		}
	}
	for _, talk := range talks {
		talkCuts := cuts[talk.TalkID]
		if len(talkCuts) == 0 {
			skipped++
			continue
		}
		segments, err := expandProductionTemplate(*definition.Segments, conference, talk.TalkID, talk.SocialCard, talkCuts)
		if err != nil {
			http.Error(w, fmt.Sprintf("generate %s: %v", talk.Title, err), http.StatusBadRequest)
			return
		}
		manifest, err := productionRenderManifestJSON(talk.Title, definition.Settings, segments)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, created, err := h.DB.CreateProductionRender(conference, talk.Title, manifest, &templateID, talk.TalkID, recordingKind)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if created {
			generated++
		} else {
			skipped++
		}
	}
	location := fmt.Sprintf("/production/renders?conference=%s&generated=%d&skipped=%d", url.QueryEscape(conference), generated, skipped)
	http.Redirect(w, r, location, http.StatusSeeOther)
}

func validateProductionRenderEditor(raw []byte) (productionTemplateDefinition, error) {
	canonical, err := validateProductionTemplate(raw)
	if err != nil {
		return productionTemplateDefinition{}, err
	}
	var definition productionTemplateDefinition
	if err := json.Unmarshal([]byte(canonical), &definition); err != nil {
		return definition, err
	}
	for _, segment := range *definition.Segments {
		var header struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(segment, &header)
		if strings.HasPrefix(header.Type, "streamctl.") {
			return definition, errors.New("render jobs may not contain dynamic template segments")
		}
	}
	return definition, nil
}

func productionRenderEditorDefinition(raw string) (productionTemplateDefinition, error) {
	var manifest productionRenderManifest
	if err := decodeStrictJSON([]byte(raw), &manifest); err != nil {
		return productionTemplateDefinition{}, fmt.Errorf("invalid render manifest: %w", err)
	}
	if manifest.Version != 1 || len(manifest.Jobs) != 1 {
		return productionTemplateDefinition{}, errors.New("render draft must contain exactly one version 1 job")
	}
	segments := manifest.Jobs[0].Segments
	return productionTemplateDefinition{Version: 1, Settings: manifest.Settings, Segments: &segments}, nil
}

func productionRenderManifestJSON(name string, settings json.RawMessage, segments []json.RawMessage) (string, error) {
	if settings == nil {
		settings = json.RawMessage(`{}`)
	}
	manifest := productionRenderManifest{Version: 1, Settings: settings, Jobs: []productionRenderJob{{ID: productionRenderID(name), Segments: segments}}}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	return string(encoded), err
}

func expandProductionTemplate(source []json.RawMessage, conference, talkID, socialCard string, cuts []db.ProductionCut) ([]json.RawMessage, error) {
	var result []json.RawMessage
	for _, raw := range source {
		var header struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return nil, err
		}
		switch header.Type {
		case "streamctl.talkCuts":
			var dynamic productionTemplateTalkCutsSegment
			if err := json.Unmarshal(raw, &dynamic); err != nil {
				return nil, err
			}
			for _, cut := range cuts {
				segment := map[string]any{"type": cut.SourceType, "src": cut.Source, "in": formatProductionTimecode(cut.InMS), "out": formatProductionTimecode(cut.OutMS)}
				if dynamic.Overlay != nil {
					segment["overlay"] = *dynamic.Overlay
				}
				encoded, _ := json.Marshal(segment)
				result = append(result, encoded)
			}
		case "streamctl.talkCard":
			var dynamic productionTemplateTalkCardSegment
			if err := json.Unmarshal(raw, &dynamic); err != nil {
				return nil, err
			}
			card := strings.TrimPrefix(strings.TrimSpace(socialCard), "/")
			if card == "" {
				// This is the canonical key used by the website's 1080p talk-card generator.
				// Keep the fallback until recording candidates expose SocialCard directly.
				card = fmt.Sprintf("%s/talks/%s-1080p.png", conference, talkID)
			}
			segment := map[string]any{"type": "image", "src": card}
			if dynamic.DurationMS != nil {
				segment["durationMs"] = *dynamic.DurationMS
			}
			encoded, _ := json.Marshal(segment)
			result = append(result, encoded)
		default:
			result = append(result, append(json.RawMessage(nil), raw...))
		}
	}
	return result, nil
}

func formatProductionTimecode(ms int64) string {
	hours := ms / 3600000
	ms %= 3600000
	minutes := ms / 60000
	ms %= 60000
	seconds := ms / 1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, minutes, seconds, ms%1000)
}

func productionRenderID(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		} else {
			dash = true
		}
	}
	value := strings.Trim(b.String(), "-._")
	if value == "" {
		return "render"
	}
	return value
}

func productionRenderURL(conference string, id int64) string {
	return "/production/renders/edit?conference=" + url.QueryEscape(conference) + "&id=" + strconv.FormatInt(id, 10)
}

func queryInt(r *http.Request, key string) int {
	value, _ := strconv.Atoi(r.URL.Query().Get(key))
	return value
}

func selectedProductionIDs(r *http.Request) ([]int64, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	if len(r.Form["ids"]) == 0 || len(r.Form["ids"]) > 500 {
		return nil, errors.New("select at least one item")
	}
	seen := map[int64]bool{}
	ids := make([]int64, 0, len(r.Form["ids"]))
	for _, value := range r.Form["ids"] {
		id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || id <= 0 {
			return nil, errors.New("invalid selection")
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func decorateProductionRender(view *productionRenderListItem, state db.ProductionRenderQueueState) {
	view.Edited = false
	view.HasFinished = false
	view.CanQueue = validateRenderManifest([]byte(view.JSON)) == nil
	if state.Latest == nil {
		view.Status, view.StatusLabel, view.BadgeClass = "draft", "Draft", "badge-inactive"
		if !view.CanQueue {
			view.StatusLabel = "Incomplete"
		}
		return
	}
	view.HasFinished = state.HasFinished
	view.Edited = view.JSON != state.Latest.ManifestJSON || view.Name != state.Latest.Name
	active := state.Latest.Status == "queued" || state.Latest.Status == "running"
	view.CanQueue = view.CanQueue && !active
	if active {
		view.Status = state.Latest.Status
		view.StatusLabel = strings.ToUpper(state.Latest.Status[:1]) + state.Latest.Status[1:]
		if view.Edited {
			view.StatusLabel += " · edited"
		}
	} else if view.Edited {
		view.Status, view.StatusLabel = "edited", "Edited"
	} else {
		view.Status = state.Latest.Status
		switch state.Latest.Status {
		case "finished":
			view.StatusLabel = "Complete"
		case "cancelled":
			view.StatusLabel = "Cancelled"
		default:
			view.StatusLabel = strings.ToUpper(state.Latest.Status[:1]) + state.Latest.Status[1:]
		}
	}
	switch view.Status {
	case "queued":
		view.BadgeClass = "badge-queued"
	case "running":
		view.BadgeClass = "badge-now"
	case "finished":
		view.BadgeClass = "badge-active"
	case "failed", "cancelled":
		view.BadgeClass = "badge-failed"
	default:
		view.BadgeClass = "badge-inactive"
	}
}
