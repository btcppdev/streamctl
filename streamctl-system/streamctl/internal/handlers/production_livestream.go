package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"streamctl/internal/db"
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
	livestreamResponse(w, http.StatusOK, map[string]any{"days": livestreamChoices(talks)})
}

// Assemble a snapshot: workers receive only ordinary conf-render segments.
func assembleLivestream(talks []productionTalkView, cuts map[string][]db.ProductionCut, conference, day, stage string) (string, []json.RawMessage, []productionLivestreamOmission, error) {
	selected := []productionTalkView{}
	for _, talk := range talks {
		if livestreamDay(talk) == day && productionStageLabel(talk.Venue) == stage && stage != "" {
			selected = append(selected, talk)
		}
	}
	if len(selected) == 0 {
		return "", nil, nil, fmt.Errorf("Choose a day and stage from the current schedule.")
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
		label = day
	}
	name := label + " — " + stage + " livestream"
	segments := []json.RawMessage{}
	omitted := []productionLivestreamOmission{}
	recipe := []json.RawMessage{json.RawMessage(`{"type":"streamctl.talkCard"}`), json.RawMessage(`{"type":"streamctl.talkCuts"}`)}
	for _, talk := range selected {
		reasons := []string{}
		if len(cuts[talk.TalkID]) == 0 {
			reasons = append(reasons, "No saved cuts")
		}
		expanded, err := expandProductionTemplate(recipe, conference, talk.TalkID, talk.SocialCard, cuts[talk.TalkID])
		if err != nil {
			reasons = append(reasons, "Missing or invalid saved title card")
		}
		if len(reasons) > 0 {
			omitted = append(omitted, productionLivestreamOmission{Title: talk.Title, Reason: strings.Join(reasons, "; ")})
			continue
		}
		segments = append(segments, expanded...)
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
	name, segments, omitted, err := assembleLivestream(talks, cuts, conference, strings.TrimSpace(r.FormValue("day")), strings.TrimSpace(r.FormValue("stage")))
	if err != nil {
		fail(http.StatusBadRequest, err)
		return
	}
	if len(segments) == 0 {
		livestreamResponse(w, http.StatusBadRequest, map[string]any{"error": "No talks are ready for this day and stage.", "omitted": omitted})
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
