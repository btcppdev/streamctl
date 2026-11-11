package probe

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func validProfile() *Profile {
	return &Profile{VideoCodec: "h264", Width: 1920, Height: 1080, PixFmt: "yuv420p", FrameRate: "30/1", VideoTimeBase: "1/15360", AudioTimeBase: "1/48000", FieldOrder: "progressive", AudioCodec: "aac", SampleRate: "48000", Channels: 2}
}
func TestReadinessRejectsIncompatibleSingleFiles(t *testing.T) {
	cases := map[string]func(*Profile){"HEVC": func(p *Profile) { p.VideoCodec = "hevc" }, "10bit": func(p *Profile) { p.PixFmt = "yuv420p10le" }, "silent": func(p *Profile) { p.AudioCodec = "" }, "Opus": func(p *Profile) { p.AudioCodec = "opus" }, "mono": func(p *Profile) { p.Channels = 1 }, "44k": func(p *Profile) { p.SampleRate = "44100" }, "bad-fps": func(p *Profile) { p.FrameRate = "0/0" }, "interlaced": func(p *Profile) { p.FieldOrder = "tt" }, "dimensions": func(p *Profile) { p.Width = 0 }}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			p := validProfile()
			change(p)
			if err := ValidateStreamingProfiles([]*Profile{p}, []string{"clip.mp4"}); err == nil {
				t.Fatal("accepted incompatible single clip")
			}
		})
	}
	if err := ValidateStreamingProfiles([]*Profile{validProfile()}, []string{"clip.mp4"}); err != nil {
		t.Fatal(err)
	}
}
func TestPlaylistReadinessComparesTimebaseAndFormats(t *testing.T) {
	a, b := validProfile(), validProfile()
	b.FrameRate = "60/2"
	if err := ValidateStreamingProfiles([]*Profile{a, b}, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Profile){func(p *Profile) { p.Width = 1280 }, func(p *Profile) { p.FrameRate = "25/1" }, func(p *Profile) { p.VideoTimeBase = "1/90000" }} {
		b = validProfile()
		change(b)
		if err := ValidateStreamingProfiles([]*Profile{a, b}, []string{"a", "b"}); err == nil {
			t.Fatal("accepted mismatched playlist")
		}
	}
}
func TestProbeDoesNotLeakSignedURL(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ffprobe"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >&2\nexit 1\n"), 0700)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := ProbePathContext(context.Background(), "https://bucket.invalid/a.mp4?secret=private", "a.mp4")
	if err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe error: %v", err)
	}
}
func TestRealFFmpegFixtures(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "good.mp4")
	cmd := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=black:s=320x180:r=30:d=0.3", "-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo", "-shortest", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", good)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, out)
	}
	if err := ValidateStreamingPaths(context.Background(), []string{good}, []string{"good"}); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "mono.mp4")
	cmd = exec.Command("ffmpeg", "-v", "error", "-i", good, "-c:v", "copy", "-c:a", "aac", "-ac", "1", bad)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, out)
	}
	if err := ValidateStreamingPaths(context.Background(), []string{bad}, []string{"mono"}); err == nil {
		t.Fatal("accepted mono audio")
	}
}
