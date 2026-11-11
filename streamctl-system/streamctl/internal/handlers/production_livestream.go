package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

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
	if err != nil || id < -1 {
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
	livestreamResponse(w, http.StatusOK, map[string]any{"talks": options})
}
