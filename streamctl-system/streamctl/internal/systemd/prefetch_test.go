package systemd

import (
	"os"
	"os/exec"
	"path/filepath"
	"streamctl/internal/db"
	"strings"
	"testing"
)

func TestAllStreamsPrefetchAndPlayOriginalBytes(t *testing.T) {
	for _, imported := range []bool{false, true} {
		m := &Manager{CacheDir: "/cache", Remote: "spaces:btcpp", CleanupCache: true, SelfPath: "/app/streamctl"}
		s := &db.Stream{ID: 42, AutoScheduled: imported, Videos: []string{"dev26/recordings/renders/38/day-3.mp4"}}
		script := m.renderPrefetchScript(s)
		for _, want := range []string{"rclone copyto --checksum 'spaces:btcpp/dev26/recordings/renders/38/day-3.mp4' '/cache/dev26/recordings/renders/38/day-3.mp4'", "'/app/streamctl' stream-check -file '/cache/dev26/recordings/renders/38/day-3.mp4'"} {
			if !strings.Contains(script, want) {
				t.Fatalf("missing %s:\n%s", want, script)
			}
		}
		for _, bad := range []string{"ffmpeg", ".ready.json", "/normalized/", "fallback"} {
			if strings.Contains(script, bad) {
				t.Fatalf("conversion path present: %s", bad)
			}
		}
		for _, actual := range []string{m.renderPlaylist(s), m.renderCleanupScript(s), m.renderProbeScript(s)} {
			if !strings.Contains(actual, "/cache/dev26/recordings/renders/38/day-3.mp4") {
				t.Fatal(actual)
			}
		}
		if !strings.Contains(m.renderRunScript(s), " -c copy ") {
			t.Fatal("playback transcodes")
		}
		if len(m.remoteCachePaths(s, s.Videos[0])) != 1 {
			t.Fatal("duplicate cache path")
		}
	}
}

func TestPrefetchDeduplicatesDownloadsAndChecksWholePlaylist(t *testing.T) {
	m := &Manager{CacheDir: "/cache", Remote: "spaces:btcpp"}
	s := &db.Stream{Videos: []string{"e/recordings/a.mp4", "e/recordings/a.mp4", "e/recordings/b.mp4"}}
	script := m.renderPrefetchScript(s)
	if strings.Count(script, "rclone copyto") != 2 || strings.Count(script, " -file ") != 3 {
		t.Fatal(script)
	}
}

// A failed download or readiness check must stop prefetch rather than silently
// accepting old bytes or starting a conversion fallback.
func TestPrefetchPropagatesDownloadAndReadinessFailures(t *testing.T) {
	for _, which := range []string{"download", "readiness"} {
		t.Run(which, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			os.Mkdir(bin, 0700)
			rclone := "#!/bin/sh\nexit 0\n"
			checker := "#!/bin/sh\nexit 0\n"
			if which == "download" {
				rclone = "#!/bin/sh\nexit 7\n"
			} else {
				checker = "#!/bin/sh\nexit 9\n"
			}
			os.WriteFile(filepath.Join(bin, "rclone"), []byte(rclone), 0700)
			os.WriteFile(filepath.Join(bin, "streamctl"), []byte(checker), 0700)
			m := &Manager{CacheDir: filepath.Join(dir, "cache"), Remote: "spaces:btcpp", SelfPath: filepath.Join(bin, "streamctl")}
			s := &db.Stream{Videos: []string{"e/recordings/output.mp4"}}
			script := strings.ReplaceAll(m.renderPrefetchScript(s), "/run/current-system/sw/bin/", "")
			script = strings.Replace(script, "export PATH=/run/current-system/sw/bin:/bin", "export PATH="+shellQuote(bin+string(os.PathListSeparator)+os.Getenv("PATH")), 1)
			cmd := exec.Command("bash", "-c", script)
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			want := 7
			if which == "readiness" {
				want = 9
			}
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != want {
				t.Fatalf("%s: want exit %d, got %v: %s", which, want, err, out)
			}
		})
	}
}
