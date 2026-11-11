package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"streamctl/internal/db"
	"strings"
	"testing"
)

const streamingProbeFixture = `{"streams":[{"codec_type":"video","codec_name":"h264","width":1920,"height":1080,"pix_fmt":"yuv420p","r_frame_rate":"30/1","time_base":"1/15360","field_order":"progressive"},{"codec_type":"audio","codec_name":"aac","sample_rate":"48000","channels":2,"time_base":"1/48000"}]}`

func installReadinessTools(t *testing.T, fixture string) string {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "probe.json")
	os.WriteFile(data, []byte(fixture), 0600)
	scripts := map[string]string{"rclone": "#!/bin/sh\nprintf 'https://storage.invalid/%s\\n' \"$4\"\n", "ffprobe": "#!/bin/sh\ncat " + shellQuote(data) + "\n"}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return data
}
func TestManualAndImportedRendersUseTheSameReadinessCheck(t *testing.T) {
	data := installReadinessTools(t, streamingProbeFixture)
	h := &Handler{Remote: "spaces:btcpp"}
	for _, existing := range []*db.Stream{nil, {ID: 1, AutoScheduled: true}} {
		form := url.Values{"name": {"Day 3"}, "video_file": {"berlin26/recordings/renders/38/day-3.mp4"}, "schedule_type": {"once"}, "on_calendar": {"2026-11-15 12:00:00"}, "auto_scheduled": {"true"}}
		request := func() *http.Request {
			r := httptest.NewRequest("POST", "/streams/create", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.ParseForm()
			return r
		}
		os.WriteFile(data, []byte(streamingProbeFixture), 0600)
		s, _, videos, err := h.streamFromForm(request(), existing)
		if err != nil {
			t.Fatal(err)
		}
		if len(videos) != 1 || s.AutoScheduled != (existing != nil) {
			t.Fatalf("lost persisted import identity: %+v", s)
		}
		os.WriteFile(data, []byte(strings.ReplaceAll(streamingProbeFixture, "h264", "hevc")), 0600)
		if _, _, _, err := h.streamFromForm(request(), existing); err == nil || !strings.Contains(err.Error(), "H.264") {
			t.Fatalf("accepted incompatible stream: %v", err)
		}
	}
}
func TestLocalSingleFileAlsoRequiresReadiness(t *testing.T) {
	installReadinessTools(t, strings.ReplaceAll(streamingProbeFixture, "aac", "opus"))
	h := &Handler{VideoDir: t.TempDir()}
	if err := h.validateStreamVideos(context.Background(), []string{"talk.mp4"}); err == nil || !strings.Contains(err.Error(), "AAC") {
		t.Fatalf("accepted Opus: %v", err)
	}
}
func TestNormalizationRoutesAreRetired(t *testing.T) {
	h := &Handler{Secret: "test"}
	mux := http.NewServeMux()
	h.Register(mux)
	for _, path := range []string{"/normalize", "/normalize/process", "/normalize/process-selected", "/livestream", "/livestream-files/process", "/worker/normalize/requeue"} {
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "test"})
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusGone {
			t.Fatalf("%s: got %d", path, w.Code)
		}
	}
}
