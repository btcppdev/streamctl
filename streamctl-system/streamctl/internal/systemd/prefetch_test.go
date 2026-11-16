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
	if _, err := exec.LookPath("flock"); err != nil {
		python, err := exec.LookPath("python3")
		if err != nil {
			t.Fatal("python3 is required for the macOS flock adapter")
		}
		bin := t.TempDir()
		adapter := "#!" + python + "\nimport fcntl,sys\nfcntl.flock(int(sys.argv[-1]), fcntl.LOCK_SH)\n"
		if err := os.WriteFile(filepath.Join(bin, "flock"), []byte(adapter), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
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

func TestMediaScriptsLockAgainstCleanup(t *testing.T) {
	m := &Manager{CacheDir: "/cache", HLSDir: "/hls", Remote: "spaces:test", CleanupCache: true}
	s := &db.Stream{ID: 1, Videos: []string{"conference/shared.mp4"}}
	for _, script := range []string{m.renderRunScript(s), m.renderPrefetchScript(s)} {
		lock := strings.Index(script, "exec 9>>'/cache/.cleanup.lock'")
		shared := strings.Index(script, "flock --shared 9")
		if lock < 0 || shared < lock {
			t.Fatalf("media operation does not hold cleanup lock:\n%s", script)
		}
	}
	if strings.Contains(m.renderRunScript(s), "rm -f -- '/cache/conference/shared.mp4'") {
		t.Fatal("stream completion must not delete another stream's shared media")
	}
}

func TestSyncOnlyPrefetchesStreamsWithUpcomingTimers(t *testing.T) {
	for _, tt := range []struct {
		name, scheduleType, next, showExit string
		wantPrefetch                       bool
	}{
		{"expired one-time stream", "once", "", "0", false},
		{"unavailable trigger", "once", "n/a", "0", false},
		{"timer query failure", "once", "", "1", false},
		{"upcoming one-time stream", "once", "Fri 2099-01-02 10:00:00 UTC", "0", true},
		{"recurring stream", "recurring", "Fri 2099-01-02 10:00:00 UTC", "0", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			fakeSystemctl := `#!/bin/sh
printf '%s\n' "$*" >> "$TEST_SYSTEMCTL_CALLS"
if [ "$1" = show ]; then
  [ "$*" = "show streamctl-stream-42.timer --property=NextElapseUSecRealtime --value" ] || exit 2
  printf '%s\n' "$TEST_NEXT_TRIGGER"
  exit "$TEST_SHOW_EXIT"
fi
`
			if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(fakeSystemctl), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("TEST_SYSTEMCTL_CALLS", calls)
			t.Setenv("TEST_NEXT_TRIGGER", tt.next)
			t.Setenv("TEST_SHOW_EXIT", tt.showExit)
			m := &Manager{UnitDir: dir, UnitPrefix: "streamctl-", CacheDir: "/cache", Remote: "spaces:btcpp"}
			s := &db.Stream{ID: 42, Name: "A stream", Enabled: true, ScheduleType: tt.scheduleType,
				OnCalendar: "2099-01-02 10:00:00 UTC", Videos: []string{"e/recordings/output.mp4"}}
			if err := m.Sync(s); err != nil {
				t.Fatal(err)
			}
			log, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(log), "start --no-block streamctl-prefetch-42.service\n"); got != tt.wantPrefetch {
				t.Fatalf("prefetch = %v, want %v; commands:\n%s", got, tt.wantPrefetch, log)
			}
			// Manual playback must still download its media, even after expiry.
			service, err := os.ReadFile(filepath.Join(dir, "streamctl-stream-42.service"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(service), "Requires=streamctl-prefetch-42.service\n") {
				t.Fatal("stream service lost its prefetch dependency")
			}
			if err := m.StartNow(s.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}
