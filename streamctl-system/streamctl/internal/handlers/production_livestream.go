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
	"streamctl/internal/renderdraft"
)

type productionLivestreamDay struct {
	Date   string   `json:"date"`
	Label  string   `json:"label"`
	Stages []string `json:"stages"`
}

type productionLivestreamOmission struct {
	Title  string `json:"title"`
	Reason string `json:"reason"`
}

type livestreamTalkOption struct {
	productionTalkView
	Ready  bool   `json:"ready"`
	Reason string `json:"reason"`
}

func livestreamDay(talk productionTalkView) string {
	if talk.StartsAt == nil {
		return ""
	}
	start, err := time.Parse(time.RFC3339, *talk.StartsAt)
	if err != nil {
		return ""
	}
	return start.Format("2006-01-02")
}

func livestreamChoices(talks []productionTalkView) []productionLivestreamDay {
	days := []productionLivestreamDay{}
	for _, talk := range talks {
		date, stage := livestreamDay(talk), productionStageLabel(talk.Venue)
		if date == "" || stage == "" {
			continue
		}
		index := -1
		for i := range days {
			if days[i].Date == date {
				index = i
				break
			}
		}
		if index < 0 {
			label := talk.DayLabel
			if label == "" {
				label = date
			}
			days = append(days, productionLivestreamDay{Date: date, Label: label + " · " + talk.DateLabel, Stages: []string{}})
			index = len(days) - 1
		}
		found := false
		for _, existing := range days[index].Stages {
			if existing == stage {
				found = true
			}
		}
		if !found {
			days[index].Stages = append(days[index].Stages, stage)
		}
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Date < days[j].Date })
	return days
}

func (h *Handler) livestreamTalks(r *http.Request, conference string) ([]productionTalkView, error) {
	conferences, message := h.productionConferences(r.Context())
	if message != "" {
		return nil, fmt.Errorf("%s", message)
	}
	talks, err := h.productionTalks(r.Context(), conference)
	if err != nil {
		return nil, err
	}
	decorateProductionTalks(talks, productionConferenceStart(conferences, conference))
	return talks, nil
}

func livestreamResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (h *Handler) productionLivestreamOptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	conference := selectedProductionConference(r)
	if !validProductionConference(conference) {
		livestreamResponse(w, http.StatusBadRequest, map[string]string{"error": "Choose a conference."})
		return
	}
	talks, err := h.livestreamTalks(r, conference)
	if err != nil {
		livestreamResponse(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	cuts, err := h.DB.ListProductionCuts(conference)
	if err != nil {
		livestreamResponse(w, 500, map[string]string{"error": err.Error()})
		return
	}
	id, err := strconv.ParseInt(r.URL.Query().Get("template_id"), 10, 64)
	if r.URL.Query().Get("template_id") == "" {
		id, err = 0, nil
	}
	if err != nil || id < 0 {
		livestreamResponse(w, 400, map[string]string{"error": "Choose a valid talk template."})
		return
	}
	_, recipe, err := h.clipRecipe(conference, id)
	if err != nil || !recipeNeedsTalk(recipe) {
		livestreamResponse(w, 400, map[string]string{"error": "Choose a template with talk placeholders."})
		return
	}
	options := []livestreamTalkOption{}
	for _, talk := range talks {
		if livestreamDay(talk) == "" || productionStageLabel(talk.Venue) == "" {
			continue
		}
		option := livestreamTalkOption{productionTalkView: talk, Ready: true}
		switch {
		case !talkAllowsIndividualSelection(talk):
			option.Reason = "Not eligible for video rendering"
		case recipeNeedsCuts(recipe) && len(cuts[talk.TalkID]) == 0:
			option.Reason = "No saved cuts"
		default:
			if _, err := expandProductionTemplate(recipe, conference, talk.TalkID, talk.SocialCard, cuts[talk.TalkID], talk.Title); err != nil {
				option.Reason = err.Error()
			}
		}
		option.Ready = option.Reason == ""
		options = append(options, option)
	}
	livestreamResponse(w, http.StatusOK, map[string]any{"days": livestreamChoices(talks), "talks": options})
}

// Assemble a snapshot: workers receive only ordinary conf-render segments.
func assembleLivestream(talks []productionTalkView, cuts map[string][]db.ProductionCut, conference, day, stage string, recipes ...[]json.RawMessage) (string, []json.RawMessage, []productionLivestreamOmission, error) {
	selected := []productionTalkView{}
	for _, talk := range talks {
		if livestreamDay(talk) != "" && productionStageLabel(talk.Venue) != "" && ((day == "" && stage == "") || (livestreamDay(talk) == day && productionStageLabel(talk.Venue) == stage && stage != "")) {
			selected = append(selected, talk)
		}
	}
	if len(selected) == 0 {
		return "", nil, nil, fmt.Errorf("Choose at least one scheduled talk.")
	}
	sort.Slice(selected, func(i, j int) bool {
		left, _ := time.Parse(time.RFC3339, *selected[i].StartsAt)
		right, _ := time.Parse(time.RFC3339, *selected[j].StartsAt)
		if !left.Equal(right) {
			return left.Before(right)
		}
		return selected[i].TalkID < selected[j].TalkID
	})
	label := selected[0].DayLabel
	if label == "" {
		label = livestreamDay(selected[0])
	}
	name := label + " — " + productionStageLabel(selected[0].Venue) + " livestream"
	for _, talk := range selected[1:] {
		if livestreamDay(talk) != livestreamDay(selected[0]) {
			name = conference + " livestream"
			break
		}
		if productionStageLabel(talk.Venue) != productionStageLabel(selected[0].Venue) {
			name = label + " livestream"
		}
	}
	segments := []json.RawMessage{}
	omitted := []productionLivestreamOmission{}
	recipe := defaultTalkRecipe
	if len(recipes) > 0 {
		recipe = recipes[0]
	}
	for _, talk := range selected {
		reasons := []string{}
		if (!talk.Eligible || talk.RecordingPolicy == "prohibited") && !(day == "" && stage == "" && talkAllowsIndividualSelection(talk)) {
			reasons = append(reasons, "Not eligible for video rendering")
		}
		if recipeNeedsCuts(recipe) && len(cuts[talk.TalkID]) == 0 {
			reasons = append(reasons, "No saved cuts")
		}
		expanded, err := expandProductionTemplate(recipe, conference, talk.TalkID, talk.SocialCard, cuts[talk.TalkID], talk.Title, productionTalkSchedule(talk.StartsAt, talk.EndsAt))
		if err != nil {
			reasons = append(reasons, err.Error())
		}
		if len(reasons) > 0 {
			omitted = append(omitted, productionLivestreamOmission{Title: talk.Title, Reason: strings.Join(reasons, "; ")})
			continue
		}
		clip := makeProductionClip("Card + saved cuts", 0, recipe, &talk, expanded)
		encoded, _ := json.Marshal(clip)
		segments = append(segments, encoded)
	}
	return name, segments, omitted, nil
}

func (h *Handler) productionRenderCreateLivestream(w http.ResponseWriter, r *http.Request) {
	conference := strings.TrimSpace(r.FormValue("conference"))
	fail := func(status int, err error) { livestreamResponse(w, status, map[string]string{"error": err.Error()}) }
	if !validProductionConference(conference) {
		fail(http.StatusBadRequest, fmt.Errorf("Choose a conference."))
		return
	}
	talks, err := h.livestreamTalks(r, conference)
	if err != nil {
		fail(http.StatusBadGateway, err)
		return
	}
	cuts, err := h.DB.ListProductionCuts(conference)
	if err != nil {
		fail(http.StatusInternalServerError, err)
		return
	}
	templateID := int64(0)
	if value := r.FormValue("template_id"); value != "" {
		var parseErr error
		templateID, parseErr = strconv.ParseInt(value, 10, 64)
		if parseErr != nil || templateID < 0 {
			fail(400, fmt.Errorf("Choose a valid template"))
			return
		}
	}
	templateName, recipe, err := h.clipRecipe(conference, templateID)
	if err != nil || !recipeNeedsTalk(recipe) {
		fail(400, fmt.Errorf("Choose a template with talk placeholders"))
		return
	}
	// Explicit selections are validated against the fresh schedule, never trusted
	// from the dialog. Legacy callers without a selection still assemble ready talks.
	if r.FormValue("selection") == "explicit" {
		wanted := map[string]bool{}
		for _, id := range r.Form["talk_ids"] {
			wanted[id] = true
		}
		if len(wanted) == 0 {
			fail(400, fmt.Errorf("Choose at least one talk."))
			return
		}
		selected := []productionTalkView{}
		for _, talk := range talks {
			if !wanted[talk.TalkID] {
				continue
			}
			if livestreamDay(talk) == "" || productionStageLabel(talk.Venue) == "" || !talkAllowsIndividualSelection(talk) {
				fail(400, fmt.Errorf("%s is no longer available for video rendering. Review your selection.", talk.Title))
				return
			}
			if recipeNeedsCuts(recipe) && len(cuts[talk.TalkID]) == 0 {
				fail(400, fmt.Errorf("%s has no saved cuts. Review your selection.", talk.Title))
				return
			}
			if _, err := expandProductionTemplate(recipe, conference, talk.TalkID, talk.SocialCard, cuts[talk.TalkID], talk.Title); err != nil {
				fail(400, err)
				return
			}
			selected = append(selected, talk)
			delete(wanted, talk.TalkID)
		}
		if len(wanted) != 0 {
			fail(400, fmt.Errorf("A selected talk is no longer available."))
			return
		}
		talks = selected
	}
	day, stage := strings.TrimSpace(r.FormValue("day")), strings.TrimSpace(r.FormValue("stage"))
	if r.FormValue("selection") == "explicit" {
		day, stage = "", ""
	}
	name, segments, omitted, err := assembleLivestream(talks, cuts, conference, day, stage, recipe)
	for i, raw := range segments {
		var clip renderdraft.Clip
		_ = json.Unmarshal(raw, &clip)
		clip.Template = &renderdraft.Template{ID: templateID, Name: templateName, Recipe: recipe}
		segments[i], _ = json.Marshal(clip)
	}
	if err != nil {
		fail(http.StatusBadRequest, err)
		return
	}
	if len(segments) == 0 {
		livestreamResponse(w, http.StatusBadRequest, map[string]any{"error": "No talks are ready for this selection.", "omitted": omitted})
		return
	}
	manifest, err := productionRenderManifestJSON(name, json.RawMessage(`{}`), segments)
	if err != nil {
		fail(http.StatusInternalServerError, err)
		return
	}
	id, created, err := h.DB.CreateProductionRender(conference, name, manifest, nil, "")
	if err != nil {
		fail(http.StatusInternalServerError, err)
		return
	}
	if !created {
		fail(http.StatusInternalServerError, fmt.Errorf("Could not create the livestream render."))
		return
	}
	livestreamResponse(w, http.StatusOK, map[string]any{"url": productionRenderURL(conference, id) + "&created=1", "omitted": omitted})
}
