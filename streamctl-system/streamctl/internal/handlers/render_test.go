package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"streamctl/internal/db"
)

func containsStringValue(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestValidateRenderManifest(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr bool
	}{
		{
			name:    "valid",
			payload: `{"version":1,"jobs":[{"id":"intro","segments":[{"type":"video","src":"dev26/recordings/source.mp4","overlay":"dev26/assets/title.png","audio":{"src":"dev26/audio/audio.wav"}}]}]}`,
		},
		{name: "missing jobs", payload: `{"version":1,"jobs":[]}`, wantErr: true},
		{name: "duplicate IDs", payload: `{"version":1,"jobs":[{"id":"same","segments":[{"src":"dev26/a"}]},{"id":"same","segments":[{"src":"dev26/b"}]}]}`, wantErr: true},
		{name: "absolute source", payload: `{"version":1,"jobs":[{"id":"intro","segments":[{"src":"/source.mp4"}]}]}`, wantErr: true},
		{name: "source without conference", payload: `{"version":1,"jobs":[{"id":"intro","segments":[{"src":"source.mp4"}]}]}`, wantErr: true},
		{name: "source traversal", payload: `{"version":1,"jobs":[{"id":"intro","segments":[{"src":"dev26/../source.mp4"}]}]}`, wantErr: true},
		{name: "mixed conferences", payload: `{"version":1,"jobs":[{"id":"intro","segments":[{"src":"dev26/source.mp4","overlay":"aus25/title.png"}]}]}`, wantErr: true},
		{name: "unsupported version", payload: `{"version":2,"jobs":[{"id":"intro","segments":[{"src":"dev26/source.mp4"}]}]}`, wantErr: true},
		{name: "trailing JSON", payload: `{"version":1,"jobs":[{"id":"intro","segments":[{"src":"dev26/source.mp4"}]}]} {}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRenderManifest([]byte(tt.payload))
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateRenderManifest() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRenderJobIDFromUnit(t *testing.T) {
	for _, unit := range []string{"streamctl-gpu-render-42.service", "streamctl-gpu-render-42-attempt-3.service"} {
		if id, ok := renderJobIDFromUnit(unit); !ok || id != 42 {
			t.Fatalf("renderJobIDFromUnit(%q) = (%d, %v), want (42, true)", unit, id, ok)
		}
	}
	for _, unit := range []string{"streamctl-gpu-foo.service", "streamctl-gpu-render-x.service", "streamctl-gpu-render-0.service", "streamctl-gpu-render-42-attempt-0.service", "streamctl-gpu-render-42-attempt-x.service"} {
		if _, ok := renderJobIDFromUnit(unit); ok {
			t.Fatalf("renderJobIDFromUnit(%q) unexpectedly succeeded", unit)
		}
	}
}

func TestRenderAttemptUnitNamesAreStableAndDistinct(t *testing.T) {
	first := renderUnitName(42, 1)
	if got := renderUnitName(42, 1); got != first {
		t.Fatalf("same attempt changed unit name: %q != %q", got, first)
	}
	if second := renderUnitName(42, 2); second == first {
		t.Fatalf("different attempts share unit name %q", first)
	}
	command := remoteRenderLaunchCommand(first, "run-render")
	for _, want := range []string{"systemctl show", "LoadState", "systemd-run", "run-render"} {
		if !strings.Contains(command, want) {
			t.Fatalf("launch command does not contain %q: %s", want, command)
		}
	}
}

func TestCancelQueuedRenderJob(t *testing.T) {
	database := openGPUQueueTestDB(t)
	item, err := database.EnqueueRenderJob("keynote", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: database}
	request := httptest.NewRequest(http.MethodPost, "/worker/render/cancel/"+strconv.FormatInt(item.ID, 10), nil)
	recorder := httptest.NewRecorder()

	h.renderJobCancel(recorder, request)

	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	got, err := database.GetRenderQueueItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "cancelled" || got.LastError != "cancelled by user" {
		t.Fatalf("unexpected cancelled item: %+v", got)
	}
}

func TestRenderOutputPrefix(t *testing.T) {
	raw := `{"version":1,"jobs":[{"id":"intro","segments":[{"type":"video","src":"dev26/recordings/source.mp4"}]}]}`
	if got, want := renderOutputPrefix(42, raw), "dev26/recordings/renders/42/"; got != want {
		t.Fatalf("renderOutputPrefix() = %q, want %q", got, want)
	}
}

func TestRenderWorkerScriptRoundTrip(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required")
	}
	root := t.TempDir()
	remoteRoot := filepath.Join(root, "remote")
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(filepath.Join(remoteRoot, "dev26", "recordings"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remoteRoot, "dev26", "recordings", "source.mp4"), []byte("source"), 0644); err != nil {
		t.Fatal(err)
	}
	rclone := `#!/usr/bin/env bash
set -euo pipefail
command="$1"
src="${@: -2:1}"
dst="${@: -1}"
local_path() { case "$1" in spaces:test/*) printf '%s/%s' "$REMOTE_ROOT" "${1#spaces:test/}" ;; *) printf '%s' "$1" ;; esac; }
src="$(local_path "$src")"
dst="$(local_path "$dst")"
case "$command" in
  lsjson) printf '[{"Path":"source.mp4","Size":6,"ModTime":"2026-09-18T00:00:00Z","Hashes":{}}]\n' ;;
  delete) while IFS= read -r key; do rm -f "$REMOTE_ROOT/$key"; done < "$4" ;;
  copyto) mkdir -p "$(dirname "$dst")"; cp "$src" "$dst" ;;
  copy) mkdir -p "$dst"; cp -R "$src"/. "$dst" ;;
  *) echo "unsupported fake rclone command: $command" >&2; exit 2 ;;
esac
`
	confRender := `#!/usr/bin/env bash
set -euo pipefail
if [ "$1" = validate ]; then exit 0; fi
if [ "$1" != render ]; then exit 2; fi
shift
output=""
while [ $# -gt 0 ]; do
  if [ "$1" = --output ]; then output="$2"; shift 2; else shift; fi
done
mkdir -p "$output"
printf 'rendered' > "$output/intro.mp4"
printf 'readable subtitles' > "$output/intro.subs.srt"
printf 'word subtitles' > "$output/intro.words.srt"
`
	for name, body := range map[string]string{"rclone": rclone, "conf-render": confRender} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	manifest := filepath.Join(root, "manifest.json")
	if err := os.WriteFile(manifest, []byte(`{"version":1,"jobs":[{"id":"intro","segments":[{"type":"video","src":"dev26/recordings/source.mp4","transcribe":true}]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "render-from-spaces.py")
	if err := os.WriteFile(script, []byte(renderWorkerScript()), 0755); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "output", "42")
	cmd := exec.Command(python, script, manifest, output, filepath.Join(root, "work"))
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"REMOTE_ROOT="+remoteRoot,
		"SPACES_REMOTE=spaces:test",
		"STREAMCTL_INPUT_CACHE="+filepath.Join(root, "cache"),
		"CONF_RENDER_COMMAND="+filepath.Join(binDir, "conf-render"),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render worker failed: %v\n%s", err, out)
	}
	for _, name := range []string{"intro.mp4", "intro.subs.srt", "intro.words.srt", "intro.manifest.json", "ready.json"} {
		path := filepath.Join(remoteRoot, "dev26", "recordings", "renders", "42", name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("uploaded %s: %v", name, err)
		}
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("uploaded output was not cleaned up: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "work")); !os.IsNotExist(err) {
		t.Fatalf("uploaded work was not cleaned up: %v", err)
	}
	manifestData, err := os.ReadFile(filepath.Join(remoteRoot, "dev26", "recordings", "renders", "42", "intro.manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var uploadedManifest struct {
		Jobs []struct {
			Segments []struct {
				Src string `json:"src"`
			} `json:"segments"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(manifestData, &uploadedManifest); err != nil {
		t.Fatal(err)
	}
	if len(uploadedManifest.Jobs) != 1 || len(uploadedManifest.Jobs[0].Segments) != 1 || uploadedManifest.Jobs[0].Segments[0].Src != "dev26/recordings/source.mp4" {
		t.Fatalf("uploaded manifest was localized instead of preserving object keys: %s", manifestData)
	}
	readyData, err := os.ReadFile(filepath.Join(remoteRoot, "dev26", "recordings", "renders", "42", "ready.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ready struct {
		Jobs []struct {
			ID        string `json:"id"`
			Video     string `json:"video"`
			Manifest  string `json:"manifest"`
			Subtitles *struct {
				Readable string `json:"readable"`
				Words    string `json:"words"`
			} `json:"subtitles"`
		} `json:"jobs"`
		Files []string `json:"files"`
	}
	if err := json.Unmarshal(readyData, &ready); err != nil {
		t.Fatal(err)
	}
	if len(ready.Jobs) != 1 || ready.Jobs[0].Manifest != "intro.manifest.json" || ready.Jobs[0].Subtitles == nil {
		t.Fatalf("unexpected ready metadata: %s", readyData)
	}
	for _, want := range []string{"intro.manifest.json", "intro.mp4", "intro.subs.srt", "intro.words.srt"} {
		if !containsStringValue(ready.Files, want) {
			t.Fatalf("ready file list does not contain %q: %s", want, readyData)
		}
	}
}

func TestShouldRequeueMissingRenderJob(t *testing.T) {
	now := time.Now()
	old := now.Add(-renderSubmissionStaleAfter)
	recent := now.Add(-renderSubmissionStaleAfter / 2)

	if !shouldRequeueMissingRenderJob(db.RenderJobQueueItem{Status: "running", StartedAt: &old}, true, now) {
		t.Fatal("stale missing render was not selected for requeue")
	}
	for _, item := range []db.RenderJobQueueItem{
		{Status: "running", StartedAt: &recent},
		{Status: "queued", StartedAt: &old},
		{Status: "running"},
	} {
		if shouldRequeueMissingRenderJob(item, true, now) {
			t.Fatalf("render unexpectedly selected for requeue: %#v", item)
		}
	}
	if shouldRequeueMissingRenderJob(db.RenderJobQueueItem{Status: "running", StartedAt: &old}, false, now) {
		t.Fatal("unreliable worker state selected a render for requeue")
	}
}

func TestReconcileUnavailableRenderQueue(t *testing.T) {
	database := openGPUQueueTestDB(t)
	item, err := database.EnqueueRenderJob("keynote", `{"version":1,"jobs":[{"id":"keynote","segments":[{"src":"/data/keynote.mp4"}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.MarkRenderQueueRunning(item.ID, renderUnitName(item.ID, 1)); err != nil {
		t.Fatal(err)
	}

	h := &Handler{DB: database}
	h.reconcileUnavailableRenderQueue(gpuWorkerView{Managed: true, Status: "not found"})

	got, err := database.GetRenderQueueItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "queued" || got.UnitName != "" {
		t.Fatalf("missing managed worker did not requeue render: %#v", got)
	}
}

func TestReconcileUnavailableRenderQueueLeavesStartingWorkerAlone(t *testing.T) {
	database := openGPUQueueTestDB(t)
	item, err := database.EnqueueRenderJob("keynote", `{"version":1,"jobs":[{"id":"keynote","segments":[{"src":"/data/keynote.mp4"}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.MarkRenderQueueRunning(item.ID, renderUnitName(item.ID, 1)); err != nil {
		t.Fatal(err)
	}

	h := &Handler{DB: database}
	h.reconcileUnavailableRenderQueue(gpuWorkerView{Managed: true, Status: "starting"})

	got, err := database.GetRenderQueueItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" {
		t.Fatalf("starting worker requeued render: %#v", got)
	}
}
