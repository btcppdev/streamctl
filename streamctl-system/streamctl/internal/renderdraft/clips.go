// Package renderdraft compiles editor-only clips to ordinary conf-render media.
package renderdraft

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const ClipType = "streamctl.clip"

type Talk struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Schedule string `json:"schedule,omitempty"`
}
type Template struct {
	ID     int64             `json:"id"`
	Name   string            `json:"name"`
	Recipe []json.RawMessage `json:"recipe"`
}
type Clip struct {
	Type     string            `json:"type"`
	Name     string            `json:"name"`
	Talk     *Talk             `json:"talk,omitempty"`
	Template *Template         `json:"template,omitempty"`
	Segments []json.RawMessage `json:"segments"`
}

func IsClip(raw json.RawMessage) bool {
	var header struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(raw, &header) == nil && header.Type == ClipType
}
func DecodeClip(raw json.RawMessage) (Clip, error) {
	var clip Clip
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&clip); err != nil {
		return clip, err
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return clip, fmt.Errorf("clip must contain one object")
	}
	if clip.Type != ClipType || strings.TrimSpace(clip.Name) == "" || clip.Segments == nil {
		return clip, fmt.Errorf("clip needs a name and a media children list")
	}
	if clip.Talk != nil && (clip.Talk.ID == "" || clip.Talk.Title == "") {
		return clip, fmt.Errorf("talk identity and title are required")
	}
	if clip.Template != nil && (clip.Template.ID < 0 || clip.Template.Name == "" || len(clip.Template.Recipe) == 0) {
		return clip, fmt.Errorf("template provenance is incomplete")
	}
	for _, child := range clip.Segments {
		var header struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(child, &header); err != nil {
			return clip, err
		}
		if header.Type != "video" && header.Type != "chunkedVideo" && header.Type != "image" {
			return clip, fmt.Errorf("clip children must be resolved media; nested clips are unsupported")
		}
	}
	return clip, nil
}

// Flatten preserves leaf ordering and timing exactly. Metadata is stripped only
// for worker/export boundaries, never from saved editor snapshots.
func Flatten(segments []json.RawMessage, stripMetadata bool) ([]json.RawMessage, error) {
	var result []json.RawMessage
	for i, raw := range segments {
		children := []json.RawMessage{raw}
		if IsClip(raw) {
			clip, err := DecodeClip(raw)
			if err != nil {
				return nil, fmt.Errorf("clip %d: %w", i+1, err)
			}
			children = clip.Segments
		}
		for _, child := range children {
			if stripMetadata {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(child, &fields); err != nil {
					return nil, err
				}
				delete(fields, "streamctl")
				encoded, err := json.Marshal(fields)
				if err != nil {
					return nil, err
				}
				child = encoded
			}
			result = append(result, child)
		}
	}
	return result, nil
}
