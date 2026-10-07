package renderdraft

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCompilationKeepsLeafContentAndStripsOnlyEditorData(t *testing.T) {
	leaf := json.RawMessage(`{"type":"video","src":"a.mp4","in":"00:01:13.578","audio":{"src":"music.mp3","in":"00:03:41"},"overlay":"logo.png","streamctl":{"talkTitle":"Talk"}}`)
	blank := json.RawMessage(`{"type":"streamctl.clip","name":"Interview","segments":[]}`)
	raw, _ := json.Marshal(Clip{Type: ClipType, Name: "Talk", Segments: []json.RawMessage{leaf}})
	leaves, err := Flatten([]json.RawMessage{blank, raw, leaf}, false)
	if err != nil || len(leaves) != 2 || string(leaves[0]) != string(leaf) {
		t.Fatalf("%s %v", leaves, err)
	}
	clean, err := Flatten([]json.RawMessage{raw}, true)
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]any
	json.Unmarshal(leaf, &before)
	json.Unmarshal(clean[0], &after)
	delete(before, "streamctl")
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("content changed: %s", clean)
	}
}
func TestClipsRejectNestedDynamicAndUnknownChildren(t *testing.T) {
	for _, raw := range []string{
		`{"type":"streamctl.clip","name":"x","segments":[{"type":"streamctl.clip","name":"y","segments":[]}]}`,
		`{"type":"streamctl.clip","name":"x","segments":[{"type":"streamctl.talkCuts"}]}`,
		`{"type":"streamctl.clip","name":"x","segments":[] ,"surprise":true}`,
		`{"type":"streamctl.clip","name":"x","segments":null}`,
	} {
		if _, err := DecodeClip(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := DecodeClip(json.RawMessage(`{"type":"streamctl.clip","name":"Blank interview","segments":[]}`)); err != nil {
		t.Fatal(err)
	}
}
