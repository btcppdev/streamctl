package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"streamctl/internal/db"
)

func (h *Handler) validateRecordingTarget(r *http.Request, conference string, metadata *productionEditorMetadata) error {
	if metadata == nil || !metadata.RegisterRecording {
		return nil
	}
	if metadata.RecordingTalkID == "" {
		return fmt.Errorf("Choose the talk to register this recording for.")
	}
	talks, err := h.productionTalks(r.Context(), conference)
	if err != nil {
		return err
	}
	for _, talk := range talks {
		if talk.TalkID == metadata.RecordingTalkID {
			return nil
		}
	}
	return fmt.Errorf("The recording target is not in this conference's current talk list.")
}

// Creation is separate from queueing: all inputs resolve before any draft is
// written, and a batch is committed atomically. Clip imports never read defaults
// for project registration or encoding.
func (h *Handler) productionRenderCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	fail := func(err error) { livestreamResponse(w, 400, map[string]string{"error": err.Error()}) }
	r.Body = http.MaxBytesReader(w, r.Body, maxRenderManifestBytes)
	if err := r.ParseForm(); err != nil {
		fail(err)
		return
	}
	conference := strings.TrimSpace(r.FormValue("conference"))
	name := strings.TrimSpace(r.FormValue("name"))
	if !validProductionConference(conference) {
		fail(fmt.Errorf("Choose a conference."))
		return
	}
	settings := json.RawMessage(r.FormValue("settings"))
	if len(settings) == 0 {
		settings = json.RawMessage(`{}`)
	}
	if err := validateProductionTemplateSettings(settings); err != nil {
		fail(err)
		return
	}
	metadata := &productionEditorMetadata{RegisterRecording: r.FormValue("register_recording") == "true", RecordingTalkID: r.FormValue("recording_talk_id")}
	include := r.FormValue("include_talks") == "true"
	batch := include && r.FormValue("output") == "separate"
	if r.FormValue("output") != "" && r.FormValue("output") != "one" && r.FormValue("output") != "separate" {
		fail(fmt.Errorf("Choose one render or separate renders per talk."))
		return
	}
	type draft struct {
		Name     string
		Segments []json.RawMessage
		Metadata *productionEditorMetadata
	}
	drafts := []draft{}
	if !include {
		if name == "" {
			name = "Untitled render"
		}
		drafts = append(drafts, draft{name, []json.RawMessage{}, metadata})
	} else {
		id, err := strconv.ParseInt(r.FormValue("template_id"), 10, 64)
		if err != nil || id < -1 {
			fail(fmt.Errorf("Choose a talk template or None."))
			return
		}
		recipeName, recipe, err := h.clipRecipe(conference, id)
		if err != nil {
			fail(err)
			return
		}
		if !recipeNeedsTalk(recipe) {
			fail(fmt.Errorf("Choose a template with talk placeholders."))
			return
		}
		talks, err := h.livestreamTalks(r, conference)
		if err != nil {
			fail(err)
			return
		}
		cuts, err := h.DB.ListProductionCuts(conference)
		if err != nil {
			fail(err)
			return
		}
		wanted := map[string]bool{}
		for _, id := range r.Form["talk_ids"] {
			wanted[id] = true
		}
		if len(wanted) == 0 {
			fail(fmt.Errorf("Choose at least one talk."))
			return
		}
		if len(wanted) > 500 {
			fail(fmt.Errorf("Choose at most 500 talks at once."))
			return
		}
		selected := []productionTalkView{}
		for _, talk := range talks {
			if wanted[talk.TalkID] {
				selected = append(selected, talk)
				delete(wanted, talk.TalkID)
			}
		}
		if len(wanted) > 0 {
			fail(fmt.Errorf("A selected talk is no longer available."))
			return
		}
		for _, talk := range selected {
			if talk.StartsAt == nil || livestreamDay(talk) == "" || productionStageLabel(talk.Venue) == "" {
				fail(fmt.Errorf("%s is not scheduled.", talk.Title))
				return
			}
		}

		sort.SliceStable(selected, func(i, j int) bool {
			a, _ := time.Parse(time.RFC3339, *selected[i].StartsAt)
			b, _ := time.Parse(time.RFC3339, *selected[j].StartsAt)
			if !a.Equal(b) {
				return a.Before(b)
			}
			return selected[i].TalkID < selected[j].TalkID
		})
		clips := []json.RawMessage{}
		for _, talk := range selected {
			if talk.StartsAt == nil || !talkAllowsIndividualSelection(talk) {
				fail(fmt.Errorf("%s is not available for selection.", talk.Title))
				return
			}
			if recipeNeedsCuts(recipe) && len(cuts[talk.TalkID]) == 0 {
				fail(fmt.Errorf("%s has no saved cuts.", talk.Title))
				return
			}
			children, err := expandProductionTemplate(recipe, conference, talk.TalkID, talk.SocialCard, cuts[talk.TalkID], talk.Title, productionTalkSchedule(talk.StartsAt, talk.EndsAt))
			if err != nil {
				fail(fmt.Errorf("%s: %w", talk.Title, err))
				return
			}
			clip := makeProductionClip(recipeName, id, recipe, &talk, children)
			raw, err := json.Marshal(clip)
			if err != nil {
				fail(err)
				return
			}
			clips = append(clips, raw)
			if batch {
				target := &productionEditorMetadata{RegisterRecording: metadata.RegisterRecording}
				if target.RegisterRecording {
					target.RecordingTalkID = talk.TalkID
				}
				drafts = append(drafts, draft{talk.Title, []json.RawMessage{raw}, target})
			}
		}
		if !batch {
			if name == "" {
				if len(selected) == 1 {
					name = selected[0].Title
				} else {
					name = conference + " render"
				}
			}
			drafts = append(drafts, draft{name, clips, metadata})
		}
	}
	manifests := []db.ProductionRender{}
	for _, draft := range drafts {
		if !batch {
			if err := h.validateRecordingTarget(r, conference, draft.Metadata); err != nil {
				fail(err)
				return
			}
		}
		if !draft.Metadata.RegisterRecording {
			draft.Metadata.RecordingTalkID = ""
		}
		manifest, err := productionRenderManifestJSON(draft.Name, settings, draft.Segments, draft.Metadata)
		if err != nil {
			fail(err)
			return
		}
		editor := productionTemplateDefinition{Version: 1, Settings: settings, Segments: &draft.Segments, Streamctl: draft.Metadata}
		raw, _ := json.Marshal(editor)
		if _, err := validateProductionRenderEditor(raw); err != nil {
			fail(err)
			return
		}
		manifests = append(manifests, db.ProductionRender{Conference: conference, Name: draft.Name, JSON: manifest})
	}
	ids, err := h.DB.CreateProductionRenderBatch(manifests)
	if err != nil {
		livestreamResponse(w, 500, map[string]string{"error": "Could not create render drafts."})
		return
	}
	url := productionRenderURL(conference, ids[0]) + "&created=1"
	if batch {
		url = "/production/renders?conference=" + conference + "&generated=" + strconv.Itoa(len(ids))
	}
	livestreamResponse(w, 200, map[string]any{"url": url, "created": len(ids)})
}
