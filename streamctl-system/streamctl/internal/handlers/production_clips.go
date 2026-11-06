package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"streamctl/internal/renderdraft"
)

var defaultTalkRecipe = []json.RawMessage{json.RawMessage(`{"type":"streamctl.talkCard"}`), json.RawMessage(`{"type":"streamctl.talkCuts"}`)}

func recipeNeedsTalk(recipe []json.RawMessage) bool {
	for _, raw := range recipe {
		var h struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &h)
		if h.Type == "streamctl.talkCard" || h.Type == "streamctl.talkCuts" {
			return true
		}
	}
	return false
}
func recipeNeedsCuts(recipe []json.RawMessage) bool {
	for _, raw := range recipe {
		var header struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &header)
		if header.Type == "streamctl.talkCuts" {
			return true
		}
	}
	return false
}
func makeProductionClip(name string, templateID int64, recipe []json.RawMessage, talk *productionTalkView, children []json.RawMessage) renderdraft.Clip {
	clip := renderdraft.Clip{Type: renderdraft.ClipType, Name: name, Segments: children, Template: &renderdraft.Template{ID: templateID, Name: name, Recipe: recipe}}
	if talk != nil {
		clip.Name = talk.Title
		clip.Talk = &renderdraft.Talk{ID: talk.TalkID, Title: talk.Title, Schedule: productionTalkSchedule(talk.StartsAt, talk.EndsAt)}
	}
	return clip
}
func (h *Handler) clipRecipe(conference string, id int64) (string, []json.RawMessage, error) {
	if id == 0 {
		return "Card + saved cuts", defaultTalkRecipe, nil
	}
	template, err := h.DB.ProductionTemplate(id, conference)
	if err != nil {
		return "", nil, fmt.Errorf("The originating template is missing. Choose another template to insert a new clip.")
	}
	var definition productionTemplateDefinition
	if err := json.Unmarshal([]byte(template.JSON), &definition); err != nil || definition.Segments == nil || len(*definition.Segments) == 0 {
		return "", nil, fmt.Errorf("Template has no usable segments")
	}
	return template.Name, *definition.Segments, nil
}
func (h *Handler) productionTemplateOptions(w http.ResponseWriter, r *http.Request) {
	conference := selectedProductionConference(r)
	if !validProductionConference(conference) {
		livestreamResponse(w, 400, map[string]string{"error": "Choose a conference."})
		return
	}
	templates, err := h.DB.ListProductionTemplates(conference)
	if err != nil {
		livestreamResponse(w, 500, map[string]string{"error": err.Error()})
		return
	}
	type option struct {
		ID        int64  `json:"id"`
		Name      string `json:"name"`
		NeedsTalk bool   `json:"needsTalk"`
		Count     int    `json:"count"`
	}
	options := []option{{0, "Card + saved cuts", true, 2}}
	for _, template := range templates {
		var def productionTemplateDefinition
		if json.Unmarshal([]byte(template.JSON), &def) == nil && def.Segments != nil && len(*def.Segments) > 0 {
			options = append(options, option{template.ID, template.Name, recipeNeedsTalk(*def.Segments), len(*def.Segments)})
		}
	}
	talks, err := h.productionTalks(r.Context(), conference)
	response := map[string]any{"templates": options, "talks": talks}
	if err != nil {
		// Static templates remain usable when the website schedule is unavailable.
		response["talks"] = []productionTalkView{}
		response["talksError"] = "Talks could not be loaded. Close and reopen this dialog to retry."
	}
	livestreamResponse(w, 200, response)
}
func (h *Handler) productionExpandTemplate(w http.ResponseWriter, r *http.Request) {
	fail := func(err error) { livestreamResponse(w, 400, map[string]string{"error": err.Error()}) }
	conference := r.FormValue("conference")
	id, err := strconv.ParseInt(r.FormValue("template_id"), 10, 64)
	if !validProductionConference(conference) || err != nil || id < 0 {
		fail(fmt.Errorf("Choose a conference and template."))
		return
	}
	name, recipe, err := h.clipRecipe(conference, id)
	if err != nil {
		fail(err)
		return
	}
	for _, segment := range recipe {
		if err := validateProductionTemplateSegment(segment); err != nil {
			fail(err)
			return
		}
	}
	clips := []renderdraft.Clip{}
	if !recipeNeedsTalk(recipe) {
		clips = append(clips, makeProductionClip(name, id, recipe, nil, recipe))
	} else {
		talks, err := h.productionTalks(r.Context(), conference)
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
		selected := []productionTalkView{}
		for _, talk := range talks {
			if wanted[talk.TalkID] {
				if !talk.Eligible {
					fail(fmt.Errorf("%s is not eligible for video rendering", talk.Title))
					return
				}
				selected = append(selected, talk)
				delete(wanted, talk.TalkID)
			}
		}
		if len(wanted) > 0 {
			fail(fmt.Errorf("A selected talk is no longer available."))
			return
		}
		sort.SliceStable(selected, func(i, j int) bool {
			left, right := "", ""
			if selected[i].StartsAt != nil {
				left = *selected[i].StartsAt
			}
			if selected[j].StartsAt != nil {
				right = *selected[j].StartsAt
			}
			a, ae := time.Parse(time.RFC3339, left)
			b, be := time.Parse(time.RFC3339, right)
			if ae == nil && be == nil && !a.Equal(b) {
				return a.Before(b)
			}
			if ae == nil && be != nil {
				return true
			}
			if ae != nil && be == nil {
				return false
			}
			return selected[i].TalkID < selected[j].TalkID
		})
		cuts, err := h.DB.ListProductionCuts(conference)
		if err != nil {
			fail(err)
			return
		}
		for _, talk := range selected {
			for _, raw := range recipe {
				var header struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal(raw, &header)
				if header.Type == "streamctl.talkCuts" && len(cuts[talk.TalkID]) == 0 {
					fail(fmt.Errorf("%s has no saved cuts", talk.Title))
					return
				}
			}
			children, err := expandProductionTemplate(recipe, conference, talk.TalkID, talk.SocialCard, cuts[talk.TalkID], talk.Title, productionTalkSchedule(talk.StartsAt, talk.EndsAt))
			if err != nil {
				fail(err)
				return
			}
			clip := makeProductionClip(name, id, recipe, &talk, children)
			raw, _ := json.Marshal(clip)
			if _, err := renderdraft.DecodeClip(raw); err != nil {
				fail(err)
				return
			}
			clips = append(clips, clip)
		}
	}
	livestreamResponse(w, 200, map[string]any{"clips": clips})
}
