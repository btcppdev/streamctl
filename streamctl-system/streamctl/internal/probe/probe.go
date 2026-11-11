// Package probe wraps ffprobe to extract stream parameters and verify that
// a list of clips can be concatenated with `-c copy` (no re-encode).
package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Profile captures the codec/format parameters that must match across clips
// for ffmpeg's concat demuxer with `-c copy` to work.
type Profile struct {
	VideoCodec    string
	Width         int
	Height        int
	PixFmt        string
	FrameRate     string // e.g. "30000/1001"
	VideoTimeBase string
	AudioTimeBase string
	FieldOrder    string
	AudioCodec    string
	SampleRate    string
	Channels      int
}

func Probe(videoDir, file string) (*Profile, error) {
	return ProbePath(filepath.Join(videoDir, file), file)
}

func ProbePath(path, label string) (*Profile, error) {
	return ProbePathContext(context.Background(), path, label)
}

func ProbePathContext(ctx context.Context, path, label string) (*Profile, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
				return nil, fmt.Errorf("could not inspect %s (missing, unreadable, or timed out)", label)
			}
			return nil, fmt.Errorf("ffprobe %s: %s", label, string(ee.Stderr))
		}
		return nil, fmt.Errorf("ffprobe %s: %w", label, err)
	}

	var raw struct {
		Streams []struct {
			TimeBase   string `json:"time_base"`
			FieldOrder string `json:"field_order"`
			CodecType  string `json:"codec_type"`
			CodecName  string `json:"codec_name"`
			Width      int    `json:"width"`
			Height     int    `json:"height"`
			PixFmt     string `json:"pix_fmt"`
			RFrameRate string `json:"r_frame_rate"`
			SampleRate string `json:"sample_rate"`
			Channels   int    `json:"channels"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parsing ffprobe output for %s: %w", label, err)
	}

	p := &Profile{}
	for _, s := range raw.Streams {
		switch s.CodecType {
		case "video":
			if p.VideoCodec == "" {
				p.VideoCodec = s.CodecName
				p.Width = s.Width
				p.Height = s.Height
				p.PixFmt = s.PixFmt
				p.FrameRate = s.RFrameRate
				p.VideoTimeBase = s.TimeBase
				p.FieldOrder = s.FieldOrder
			}
		case "audio":
			if p.AudioCodec == "" {
				p.AudioCodec = s.CodecName
				p.AudioTimeBase = s.TimeBase
				p.SampleRate = s.SampleRate
				p.Channels = s.Channels
			}
		}
	}
	if p.VideoCodec == "" {
		return nil, fmt.Errorf("%s has no video stream", label)
	}
	return p, nil
}

// ValidatePlaylist probes every file and returns a descriptive error if any
// pair of clips would break a `-c copy` concat (codec/resolution/framerate/
// pixel format / audio params mismatch).
func ValidatePlaylist(videoDir string, files []string) error {
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = filepath.Join(videoDir, f)
	}
	return ValidatePlaylistPaths(paths, files)
}

func ValidatePlaylistPaths(paths, labels []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("playlist is empty")
	}
	var first *Profile
	var firstFile string
	for i, path := range paths {
		label := path
		if i < len(labels) && labels[i] != "" {
			label = labels[i]
		}
		p, err := ProbePath(path, label)
		if err != nil {
			return err
		}
		if first == nil {
			first = p
			firstFile = label
			continue
		}
		if d := profileDiff(first, p); d != "" {
			return fmt.Errorf("clips %q and %q differ on %s — concat with -c copy requires identical streams", firstFile, label, d)
		}
	}
	return nil
}

// Bitrate returns the format-level bitrate of a file in bits/sec, or 0 if
// ffprobe doesn't report one (some MKV/MOV containers omit it).
func Bitrate(videoDir, file string) (int, error) {
	path := filepath.Join(videoDir, file)
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "format=bit_rate",
		"-print_format", "json",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return 0, fmt.Errorf("ffprobe %s: %s", file, string(ee.Stderr))
		}
		return 0, fmt.Errorf("ffprobe %s: %w", file, err)
	}
	var raw struct {
		Format struct {
			BitRate string `json:"bit_rate"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return 0, fmt.Errorf("parsing ffprobe output for %s: %w", file, err)
	}
	if raw.Format.BitRate == "" {
		return 0, nil
	}
	var bps int
	if _, err := fmt.Sscanf(raw.Format.BitRate, "%d", &bps); err != nil {
		return 0, nil
	}
	return bps, nil
}

func profileDiff(a, b *Profile) string {
	switch {
	case a.VideoCodec != b.VideoCodec:
		return fmt.Sprintf("video codec (%s vs %s)", a.VideoCodec, b.VideoCodec)
	case a.Width != b.Width || a.Height != b.Height:
		return fmt.Sprintf("resolution (%dx%d vs %dx%d)", a.Width, a.Height, b.Width, b.Height)
	case a.PixFmt != b.PixFmt:
		return fmt.Sprintf("pixel format (%s vs %s)", a.PixFmt, b.PixFmt)
	case !sameRate(a.FrameRate, b.FrameRate):
		return fmt.Sprintf("frame rate (%s vs %s)", a.FrameRate, b.FrameRate)
	case a.VideoTimeBase != b.VideoTimeBase:
		return fmt.Sprintf("video time base (%s vs %s)", a.VideoTimeBase, b.VideoTimeBase)
	case a.AudioTimeBase != b.AudioTimeBase:
		return fmt.Sprintf("audio time base (%s vs %s)", a.AudioTimeBase, b.AudioTimeBase)
	case a.AudioCodec != b.AudioCodec:
		return fmt.Sprintf("audio codec (%s vs %s)", a.AudioCodec, b.AudioCodec)
	case a.SampleRate != b.SampleRate:
		return fmt.Sprintf("audio sample rate (%s vs %s)", a.SampleRate, b.SampleRate)
	case a.Channels != b.Channels:
		return fmt.Sprintf("audio channels (%d vs %d)", a.Channels, b.Channels)
	}
	return ""
}

// ValidateStreamingProfile checks the codec contract used by RTMP and HLS stream
// copy. Resolution and rate may vary between projects, but must match within a
// concatenated playlist. No bitrate-dependent encoding or conversion happens.
func ValidateStreamingProfile(p *Profile) error {
	if p.VideoCodec != "h264" {
		return fmt.Errorf("video must be H.264; found %q", p.VideoCodec)
	}
	if p.PixFmt != "yuv420p" {
		return fmt.Errorf("video must use 8-bit yuv420p; found %q", p.PixFmt)
	}
	if p.Width <= 0 || p.Height <= 0 || p.Width%2 != 0 || p.Height%2 != 0 {
		return fmt.Errorf("video dimensions must be positive and even")
	}
	if p.FieldOrder != "" && p.FieldOrder != "unknown" && p.FieldOrder != "progressive" {
		return fmt.Errorf("video must be progressive; found %q", p.FieldOrder)
	}
	rate, ok := new(big.Rat).SetString(p.FrameRate)
	if !ok || rate.Sign() <= 0 || rate.Cmp(big.NewRat(60, 1)) > 0 {
		return fmt.Errorf("video frame rate must be greater than zero and at most 60 fps; found %q", p.FrameRate)
	}
	if p.AudioCodec != "aac" {
		return fmt.Errorf("audio must be AAC; found %q", p.AudioCodec)
	}
	if p.SampleRate != "48000" || p.Channels != 2 {
		return fmt.Errorf("audio must be 48 kHz stereo; found %s Hz and %d channels", p.SampleRate, p.Channels)
	}
	return nil
}

func sameRate(a, b string) bool {
	x, ok := new(big.Rat).SetString(a)
	if !ok {
		return a == b
	}
	y, ok := new(big.Rat).SetString(b)
	return ok && x.Cmp(y) == 0
}

// ValidateStreamingProfiles also checks one-file playlists and all transitions.
func ValidateStreamingProfiles(profiles []*Profile, labels []string) error {
	if len(profiles) == 0 {
		return fmt.Errorf("playlist is empty")
	}
	for i, p := range profiles {
		label := fmt.Sprintf("clip %d", i+1)
		if i < len(labels) {
			label = labels[i]
		}
		if err := ValidateStreamingProfile(p); err != nil {
			return fmt.Errorf("%s is not stream-ready: %w. Render it with conf-render's streaming-compatible settings", label, err)
		}
		if i > 0 {
			if d := profileDiff(profiles[0], p); d != "" {
				firstLabel := "clip 1"
				if len(labels) > 0 {
					firstLabel = labels[0]
				}
				return fmt.Errorf("playlist clips %q and %q differ on %s; stream copy requires matching streams", firstLabel, label, d)
			}
		}
	}
	return nil
}

func ValidateStreamingPaths(ctx context.Context, paths, labels []string) error {
	profiles := make([]*Profile, 0, len(paths))
	names := make([]string, 0, len(paths))
	seen := map[string]*Profile{}
	for i, path := range paths {
		label := path
		if i < len(labels) {
			label = labels[i]
		}
		names = append(names, label)
		p := seen[path]
		if p == nil {
			var err error
			p, err = ProbePathContext(ctx, path, label)
			if err != nil {
				return err
			}
			seen[path] = p
		}
		profiles = append(profiles, p)
	}
	return ValidateStreamingProfiles(profiles, names)
}
