package systemd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"streamctl/internal/db"
)

func TestNormalizedRemoteAndCachePaths(t *testing.T) {
	m := &Manager{
		CacheDir:  "/var/lib/streamctl/cache",
		Remote:    "spaces:btcpp",
		Normalize: true,
	}
	source := "vienna/recordings/edits/stage-two/talk.mp4"

	if got, want := m.normalizedRemoteClipPath(source), "vienna/recordings/normalized/stage-two/talk.mp4"; got != want {
		t.Fatalf("normalized remote path = %q, want %q", got, want)
	}
	if got, want := m.localClipPath(&db.Stream{}, source), "/var/lib/streamctl/cache/normalized/vienna/recordings/edits/stage-two/talk.mp4"; got != want {
		t.Fatalf("local normalized cache path = %q, want %q", got, want)
	}
	paths := m.remoteCachePaths(&db.Stream{}, source)
	if len(paths) != 2 {
		t.Fatalf("expected raw and normalized cleanup paths, got %#v", paths)
	}
	if paths[0] != "/var/lib/streamctl/cache/vienna/recordings/edits/stage-two/talk.mp4" {
		t.Fatalf("raw cleanup path = %q", paths[0])
	}
	if paths[1] != "/var/lib/streamctl/cache/normalized/vienna/recordings/edits/stage-two/talk.mp4" {
		t.Fatalf("normalized cleanup path = %q", paths[1])
	}
}

func TestPrefetchScriptPrefersPreprocessedNormalizedObject(t *testing.T) {
	m := &Manager{
		CacheDir:  "/var/lib/streamctl/cache",
		Remote:    "spaces:btcpp",
		Normalize: true,
	}
	stream := &db.Stream{
		ID:     42,
		Name:   "test",
		Videos: []string{"vienna/recordings/edits/stage-two/talk.mp4"},
	}

	script := m.renderPrefetchScript(stream)
	for _, want := range []string{
		"spaces:btcpp/vienna/recordings/normalized/stage-two/talk.mp4.ready.json",
		"spaces:btcpp/vienna/recordings/normalized/stage-two/talk.mp4",
		"prefetch: fetching preprocessed normalized Spaces object vienna/recordings/normalized/stage-two/talk.mp4",
		"prefetch: preprocessed normalized Spaces object failed verification; falling back to raw vienna/recordings/edits/stage-two/talk.mp4",
		"/var/lib/streamctl/cache/normalized/vienna/recordings/edits/stage-two/talk.mp4",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("prefetch script missing %q\n%s", want, script)
		}
	}
}

func TestAutoScheduledRenderSkipsNormalization(t *testing.T) {
	m := &Manager{CacheDir: "/cache", Remote: "spaces:btcpp", Normalize: true, CleanupCache: true}
	s := &db.Stream{ID: 42, AutoScheduled: true, Videos: []string{"dev26/recordings/renders/talk.mp4"}}
	prefetch := m.renderPrefetchScript(s)
	for _, want := range []string{"rclone copyto 'spaces:btcpp/dev26/recordings/renders/talk.mp4' '/cache/dev26/recordings/renders/talk.mp4'", "ffprobe -v error -show_streams '/cache/dev26/recordings/renders/talk.mp4'"} {
		if !strings.Contains(prefetch, want) {
			t.Fatalf("prefetch missing %q:\n%s", want, prefetch)
		}
	}
	for _, forbidden := range []string{"ffmpeg", ".ready.json", "/normalized/", "should_use_raw_fallback"} {
		if strings.Contains(prefetch, forbidden) {
			t.Fatalf("auto-scheduled prefetch contains %q", forbidden)
		}
	}
	for name, script := range map[string]string{"playlist": m.renderPlaylist(s), "probe": m.renderProbeScript(s), "cleanup": m.renderCleanupScript(s)} {
		if !strings.Contains(script, "/cache/dev26/recordings/renders/talk.mp4") || strings.Contains(script, "/normalized/") {
			t.Fatalf("%s uses wrong file:\n%s", name, script)
		}
	}
	if !strings.Contains(m.renderRunScript(s), " -c copy ") {
		t.Fatal("playback no longer uses stream copy")
	}
	// The shared manager must still normalize ordinary jobs.
	s.AutoScheduled = false
	if !strings.Contains(m.renderPrefetchScript(s), "ffmpeg") || !strings.Contains(m.renderPlaylist(s), "/normalized/") {
		t.Fatal("ordinary jobs stopped normalizing")
	}
}

func TestRunScriptReportsBTCPPBroadcastLifecycle(t *testing.T) {
	m := &Manager{
		HLSDir: "/var/lib/streamctl/hls", PublicBaseURL: "https://stream.btcpp.dev",
		BTCPPAPIBase: "https://btcpp.dev", BTCPPTokenFile: "/var/lib/streamctl/btcpp-api-token",
		SelfPath: "/run/current-system/sw/bin/cmd",
	}
	stream := &db.Stream{ID: 7, Name: "A talk", Videos: []string{"talk.mp4"}, BTCPPRecordingID: "recording-1"}
	script := m.renderRunScript(stream)
	for _, want := range []string{
		"btcpp-broadcast", "-recording-id 'recording-1'", "-state 'live'",
		"sleep 45", "-state 'ended'", "-state 'failed'",
		"https://stream.btcpp.dev/live/stream-7/index.m3u8",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("run script missing %q\n%s", want, script)
		}
	}
}

func TestBroadcastUsesServiceCredential(t *testing.T) {
	dir := t.TempDir()
	client := filepath.Join(dir, "fake client")
	if err := os.WriteFile(client, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		RunUser: "streamctl", VideoDir: "/videos", CacheDir: "/cache", HLSDir: "/hls",
		PublicBaseURL: "https://stream.btcpp.dev", BTCPPAPIBase: "https://btcpp.dev",
		BTCPPTokenFile: "/root/private token", SelfPath: client,
	}
	s := &db.Stream{ID: 7, BTCPPRecordingID: "recording-1"}
	unit := m.renderService(s)
	if !strings.Contains(unit, "LoadCredential=btcpp-api-token:/root/private token\n") {
		t.Fatalf("service does not load the root-owned token: %s", unit)
	}
	if strings.Contains(unit, "ReadOnlyPaths=/videos /root/private token") {
		t.Fatal("service exposes the original token path instead of a credential")
	}
	credentialsDir := filepath.Join(dir, "service credentials")
	cmd := exec.Command("sh", "-c", m.renderBTCPPBroadcastCommand(s, "live"))
	cmd.Env = append(os.Environ(), "CREDENTIALS_DIRECTORY="+credentialsDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run callback: %v: %s", err, output)
	}
	if !strings.Contains(string(output), "-token-file\n"+credentialsDir+"/btcpp-api-token\n") {
		t.Fatalf("callback did not expand the service credential path correctly: %s", output)
	}
	s.BTCPPRecordingID = ""
	if strings.Contains(m.renderService(s), "LoadCredential=") || m.renderBTCPPBroadcastCommand(s, "live") != "" {
		t.Fatal("unlinked streams should neither load credentials nor send callbacks")
	}
}

func TestStreamServiceEnablesIPAccounting(t *testing.T) {
	m := &Manager{RunUser: "streamctl", VideoDir: "/videos", CacheDir: "/cache", HLSDir: "/hls"}
	unit := m.renderService(&db.Stream{ID: 7, Name: "A talk"})
	if !strings.Contains(unit, "IPAccounting=true") {
		t.Fatalf("stream service did not enable network accounting:\n%s", unit)
	}
}

func TestCredentialPathIsLiteralWithEscapedSpecifiers(t *testing.T) {
	m := &Manager{
		PublicBaseURL: "https://stream.example", BTCPPAPIBase: "https://btcpp.dev",
		BTCPPTokenFile: `/root/private tokens/100% "literal"\token`,
	}
	unit := m.renderService(&db.Stream{ID: 7, BTCPPRecordingID: "recording-1"})
	want := "LoadCredential=btcpp-api-token:/root/private tokens/100%% \"literal\"\\token\n"
	if !strings.Contains(unit, want) {
		t.Fatalf("credential path was quoted or escaped as argv:\n%s", unit)
	}
}

func TestParseStreamRuntime(t *testing.T) {
	runtime := parseStreamRuntime("ActiveState=active\nResult=success\nIPIngressBytes=123\nIPEgressBytes=456\n")
	if !runtime.Active || runtime.Failed || runtime.Result != "success" || runtime.IngressBytes != 123 || runtime.EgressBytes != 456 {
		t.Fatalf("unexpected runtime: %#v", runtime)
	}

	failed := parseStreamRuntime("ActiveState=failed\nResult=exit-code\nIPIngressBytes=[not set]\nIPEgressBytes=[not set]\n")
	if failed.Active || !failed.Failed || failed.IngressBytes != 0 || failed.EgressBytes != 0 {
		t.Fatalf("unexpected failed runtime: %#v", failed)
	}
}

func TestConferenceBroadcastLifecycle(t *testing.T) {
	m := &Manager{PublicBaseURL: "https://stream.example", BTCPPAPIBase: "https://btcpp.dev", BTCPPTokenFile: "/root/token"}
	s := &db.Stream{ID: 23, Name: "Day 3", BTCPPConference: "toronto"}
	script := m.renderRunScript(s)
	for _, want := range []string{"-conference 'toronto'", "-title 'Day 3'", "-state 'live'", "-state 'ended'", "-state 'failed'", "sleep 45", "${CREDENTIALS_DIRECTORY}/btcpp-api-token"} {
		if !strings.Contains(script, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(script, "-recording-id") {
		t.Fatal("conference callback includes recording ID")
	}
	if !strings.Contains(m.renderService(s), "LoadCredential=") {
		t.Fatal("missing token credential")
	}
}
