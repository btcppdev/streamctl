package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"streamctl/internal/db"
	"streamctl/internal/renderdraft"
)

type productionRecordingNotice struct {
	Title  string `json:"title"`
	Label  string `json:"label"`
	Detail string `json:"detail"`
	Level  string `json:"level"`
}

func recordingPolicyNotice(policy string) productionRecordingNotice {
	switch policy {
	case "full", "record":
		return productionRecordingNotice{Label: "Recording allowed", Detail: "Speaker video, slides and audio permitted.", Level: "allowed"}
	case "audio_only":
		return productionRecordingNotice{Label: "Slides + audio only — no speaker video", Detail: "Use slides and audio only. Exclude all speaker camera footage.", Level: "limited"}
	case "prohibited":
		return productionRecordingNotice{Label: "Do not record", Detail: "Speaker consent prohibits recording. Exclude this talk from recordings and renders.", Level: "prohibited"}
	default:
		return productionRecordingNotice{Label: "Recording preference unverified", Detail: "Confirm speaker consent before using this talk in a recording or render.", Level: "unknown"}
	}
}

func talkRecordingNotice(talk productionTalkView) productionRecordingNotice {
	notice := recordingPolicyNotice(talk.RecordingPolicy)
	names := []string{}
	for _, speaker := range talk.Speakers {
		if speaker.Name != "" {
			names = append(names, speaker.Name)
		}
	}
	notice.Title = talk.Title
	if len(names) > 0 {
		notice.Title += " · " + strings.Join(names, ", ")
	}
	if len(names) > 1 {
		notice.Detail += " Applies to the whole talk using the strictest speaker preference."
	}
	return notice
}

// Consult current consent when opening a saved draft, including all talk clips
// in livestreams. An API failure or missing talk is never treated as consent.
func (h *Handler) productionRecordingTalks(r *http.Request, conference string) map[string]productionTalkView {
	talks, err := h.productionTalks(r.Context(), conference)
	result := map[string]productionTalkView{}
	if err == nil {
		for _, talk := range talks {
			result[talk.TalkID] = talk
		}
	}
	return result
}

func renderRecordingNotices(item db.ProductionRender, talks map[string]productionTalkView) []productionRecordingNotice {
	type identity struct{ id, title string }
	identities := []identity{}
	if item.TalkID != "" {
		identities = append(identities, identity{item.TalkID, item.Name})
	}
	var manifest productionRenderManifest
	unlinkedVideo := false
	if json.Unmarshal([]byte(item.JSON), &manifest) == nil {
		for _, job := range manifest.Jobs {
			for _, raw := range job.Segments {
				if renderdraft.IsClip(raw) {
					clip, err := renderdraft.DecodeClip(raw)
					if err == nil && clip.Talk != nil {
						identities = append(identities, identity{clip.Talk.ID, clip.Talk.Title})
					} else if err == nil {
						for _, child := range clip.Segments {
							if recordingSegmentHasVideo(child) {
								unlinkedVideo = true
							}
						}
					}
				} else if item.TalkID == "" && recordingSegmentHasVideo(raw) {
					unlinkedVideo = true
				}
			}
		}
	}
	notices := []productionRecordingNotice{}
	seen := map[string]bool{}
	for _, identity := range identities {
		if seen[identity.id] {
			continue
		}
		seen[identity.id] = true
		notice := recordingPolicyNotice("")
		notice.Title = identity.title
		if talk, ok := talks[identity.id]; ok {
			notice = talkRecordingNotice(talk)
		}
		notices = append(notices, notice)
	}
	if unlinkedVideo {
		notice := recordingPolicyNotice("")
		notice.Title = "Unlinked footage"
		notice.Detail = "Some footage is not linked to a talk. Verify its speakers’ recording preferences before rendering."
		notices = append(notices, notice)
	}
	return notices
}

func recordingSegmentHasVideo(raw json.RawMessage) bool {
	var header struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(raw, &header) == nil && (header.Type == "video" || header.Type == "chunkedVideo")
}
