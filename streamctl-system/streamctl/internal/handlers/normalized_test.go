package handlers

import (
	"context"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"streamctl/internal/db"
)

func TestRescheduleImportedRender(t *testing.T) {
	const video = "nairobi/recordings/renders/1/sending-and-receiving-silent-payment-transactions-with-bdk-sp.mp4"
	h := &Handler{Remote: "spaces:btcpp", CacheDir: t.TempDir()}
	for _, tc := range []struct {
		name      string
		existing  *db.Stream
		wantError bool
	}{
		{"imported broadcast", &db.Stream{ID: 1, AutoScheduled: true}, false},
		{"ordinary stream", &db.Stream{ID: 2}, true},
		{"new stream", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{
				"name": {"Silent payments"}, "video_file": {video},
				"schedule_type": {"once"}, "on_calendar": {"2026-09-23 12:00:00"},
				"auto_scheduled": {"true"}, // A submitted flag cannot bypass validation.
			}
			r := httptest.NewRequest("POST", "/streams/update/1", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			s, _, videos, err := h.streamFromForm(r, tc.existing)
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "must be under") {
					t.Fatalf("expected normalization restriction, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !s.AutoScheduled || len(videos) != 1 || videos[0] != video || s.OnCalendar != "2026-09-23 12:00:00" {
				t.Fatalf("reschedule lost playback mode or form data: %+v, %v", s, videos)
			}
		})
	}
}

func TestNormalizedPathAllowsFileDirectlyUnderEdits(t *testing.T) {
	rawPath := "toronto/recordings/edits/03main1545_fork-strategies-from-the-front-lines.mp4"
	want := "toronto/recordings/normalized/03main1545_fork-strategies-from-the-front-lines.mp4"
	got, err := normalizedRecordingPath(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("normalized path = %q, want %q", got, want)
	}
}

func TestValidateNormalizedStreamVideos(t *testing.T) {
	binDir := t.TempDir()
	rclonePath := filepath.Join(binDir, "rclone")
	script := `#!/bin/sh
set -eu
cmd="$1"
target="$2"
case "$cmd:$target" in
  lsl:spaces:btcpp/vienna/recordings/normalized/stage/talk.mp4)
    printf '123 2026-06-25 08:13:04.043805718 talk.mp4\n'
    ;;
  lsl:spaces:btcpp/vienna/recordings/normalized/missing/talk.mp4)
    ;;
  cat:spaces:btcpp/vienna/recordings/normalized/stage/talk.mp4.ready.json)
    cat <<'JSON'
{"raw_path":"vienna/recordings/edits/stage/talk.mp4","normalized_path":"vienna/recordings/normalized/stage/talk.mp4","status":"ready","verified_by":"ffprobe"}
JSON
    ;;
  *)
    echo "unexpected rclone call: $*" >&2
    exit 1
    ;;
esac
`
	if err := os.WriteFile(rclonePath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	h := &Handler{Remote: "spaces:btcpp"}
	if err := h.validateNormalizedStreamVideos(context.Background(), []string{"vienna/recordings/edits/stage/talk.mp4"}); err != nil {
		t.Fatalf("expected verified normalized output to pass: %v", err)
	}

	err := h.validateNormalizedStreamVideos(context.Background(), []string{"vienna/recordings/edits/missing/talk.mp4"})
	if err == nil || !strings.Contains(err.Error(), "not ready for scheduling") {
		t.Fatalf("expected missing normalized output to fail clearly, got %v", err)
	}

	err = h.validateNormalizedStreamVideos(context.Background(), []string{"vienna/recordings/normalized/stage/talk.mp4"})
	if err == nil || !strings.Contains(err.Error(), "schedule the original") {
		t.Fatalf("expected direct normalized path to fail clearly, got %v", err)
	}
}

func TestVerifyNormalizedOutputRejectsMismatchedReadyMarker(t *testing.T) {
	binDir := t.TempDir()
	rclonePath := filepath.Join(binDir, "rclone")
	script := `#!/bin/sh
set -eu
cmd="$1"
target="$2"
case "$cmd:$target" in
  lsl:spaces:btcpp/vienna/recordings/normalized/stage/talk.mp4)
    printf '123 2026-06-25 08:13:04.043805718 talk.mp4\n'
    ;;
  cat:spaces:btcpp/vienna/recordings/normalized/stage/talk.mp4.ready.json)
    cat <<'JSON'
{"raw_path":"vienna/recordings/edits/stage/other.mp4","normalized_path":"vienna/recordings/normalized/stage/talk.mp4","status":"ready"}
JSON
    ;;
  *)
    echo "unexpected rclone call: $*" >&2
    exit 1
    ;;
esac
`
	if err := os.WriteFile(rclonePath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	h := &Handler{Remote: "spaces:btcpp"}
	err := h.verifyNormalizedOutput(context.Background(), "vienna/recordings/edits/stage/talk.mp4")
	if err == nil || !strings.Contains(err.Error(), "raw path mismatch") {
		t.Fatalf("expected ready marker mismatch to fail clearly, got %v", err)
	}
}
