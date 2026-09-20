package btcppclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maximumResponseBytes = 2 << 20

type Client struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

type Conference struct {
	ID          string  `json:"id"`
	Tag         string  `json:"tag"`
	Description string  `json:"description"`
	Tagline     string  `json:"tagline"`
	Location    string  `json:"location"`
	Venue       string  `json:"venue"`
	StartsAt    *string `json:"starts_at"`
	EndsAt      *string `json:"ends_at"`
}

type Candidate struct {
	TalkID          string     `json:"talk_id"`
	Title           string     `json:"title"`
	Status          string     `json:"status"`
	StartsAt        *string    `json:"starts_at"`
	EndsAt          *string    `json:"ends_at"`
	Venue           string     `json:"venue"`
	SocialCard      string     `json:"social_card"`
	Speakers        []Speaker  `json:"speakers"`
	RecordingPolicy string     `json:"recording_policy"`
	Eligible        bool       `json:"eligible"`
	Reasons         []string   `json:"reasons"`
	Recording       *Recording `json:"recording"`
}

type Speaker struct {
	PersonID string `json:"person_id"`
	Name     string `json:"name"`
	Company  string `json:"company"`
}

type Recording struct {
	ID          string  `json:"id"`
	TalkID      string  `json:"talk_id"`
	TalkName    string  `json:"talk_name"`
	FileURI     string  `json:"file_uri"`
	YouTubeURL  string  `json:"youtube_url"`
	XURL        string  `json:"x_url"`
	XReplyURL   string  `json:"x_reply_url"`
	PublishedAt *string `json:"published_at"`
}

type RecordingUpdate struct {
	TalkName    *string `json:"talk_name,omitempty"`
	FileURI     *string `json:"file_uri,omitempty"`
	YouTubeURL  *string `json:"youtube_url,omitempty"`
	XURL        *string `json:"x_url,omitempty"`
	XReplyURL   *string `json:"x_reply_url,omitempty"`
	PublishedAt *string `json:"published_at,omitempty"`
}

type BroadcastUpdate struct {
	Title         string `json:"title,omitempty"`
	State         string `json:"state"`
	HLSURL        string `json:"hls_url,omitempty"`
	XBroadcastURL string `json:"x_broadcast_url,omitempty"`
}

type Broadcast struct {
	State         string  `json:"state"`
	HLSURL        *string `json:"hls_url"`
	XBroadcastURL *string `json:"x_broadcast_url"`
	StartedAt     *string `json:"started_at"`
	EndedAt       *string `json:"ended_at"`
	HeartbeatAt   *string `json:"heartbeat_at"`
	IsLive        bool    `json:"is_live"`
}

func TokenFromFile(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("bitcoin++ API token file is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat bitcoin++ API token file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("bitcoin++ API token path is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 && !privateSystemdCredential(path, info.Mode()) {
		return "", fmt.Errorf("bitcoin++ API token file must not be accessible by group or other users")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read bitcoin++ API token file: %w", err)
	}
	token := strings.TrimSpace(string(contents))
	if token == "" {
		return "", fmt.Errorf("bitcoin++ API token file is empty")
	}
	return token, nil
}

// systemd may expose credentials as 0440 inside a read-only, private mount.
// Ordinary token files must still have no group or world permissions.
func privateSystemdCredential(path string, mode os.FileMode) bool {
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	if dir == "" || !filepath.IsAbs(dir) || mode.Perm() != 0o440 {
		return false
	}
	absolute, err := filepath.Abs(path)
	if err != nil || absolute != filepath.Join(dir, "btcpp-api-token") {
		return false
	}
	info, err := os.Stat(dir)
	return err == nil && info.IsDir() && info.Mode().Perm()&0o227 == 0
}

func (client *Client) RecordingCandidates(ctx context.Context, conference string) ([]Candidate, error) {
	var response []Candidate
	path := "/api/v1/conferences/" + url.PathEscape(strings.TrimSpace(conference)) + "/recording-candidates"
	if err := client.do(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	return response, nil
}

func (client *Client) Conferences(ctx context.Context) ([]Conference, error) {
	var response []Conference
	if err := client.do(ctx, http.MethodGet, "/api/v1/conferences", nil, &response); err != nil {
		return nil, err
	}
	return response, nil
}

func (client *Client) PutRecording(ctx context.Context, conference, talkID string, update RecordingUpdate) (*Recording, error) {
	path := "/api/v1/conferences/" + url.PathEscape(strings.TrimSpace(conference)) + "/talks/" + url.PathEscape(strings.TrimSpace(talkID)) + "/recording"
	var response Recording
	if err := client.do(ctx, http.MethodPut, path, update, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

func (client *Client) PutBroadcast(ctx context.Context, recordingID string, update BroadcastUpdate) (*Broadcast, error) {
	path := "/api/v1/recordings/" + url.PathEscape(strings.TrimSpace(recordingID)) + "/broadcast"
	var response Broadcast
	if err := client.do(ctx, http.MethodPut, path, update, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

func (client *Client) PutConferenceBroadcast(ctx context.Context, conference string, update BroadcastUpdate) (*Broadcast, error) {
	path := "/api/v1/conferences/" + url.PathEscape(strings.TrimSpace(conference)) + "/broadcast"
	var response Broadcast
	if err := client.do(ctx, http.MethodPut, path, update, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

func (client *Client) do(ctx context.Context, method, path string, requestBody, responseBody any) error {
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(client.BaseURL), "/"))
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return fmt.Errorf("invalid bitcoin++ API base URL")
	}
	if strings.TrimSpace(client.Token) == "" {
		return fmt.Errorf("bitcoin++ API token is required")
	}
	base.Path = strings.TrimRight(base.Path, "/") + path
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("encode bitcoin++ API request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, base.String(), body)
	if err != nil {
		return fmt.Errorf("create bitcoin++ API request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+client.Token)
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	httpClient := client.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("bitcoin++ API request: %w", err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maximumResponseBytes)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var apiError struct {
			Error struct{ Code, Message, RequestID string } `json:"error"`
		}
		_ = json.NewDecoder(limited).Decode(&apiError)
		if apiError.Error.Message != "" {
			return fmt.Errorf("bitcoin++ API %s: %s (request %s)", apiError.Error.Code, apiError.Error.Message, apiError.Error.RequestID)
		}
		return fmt.Errorf("bitcoin++ API returned HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(limited).Decode(&envelope); err != nil {
		return fmt.Errorf("decode bitcoin++ API envelope: %w", err)
	}
	if len(envelope.Data) == 0 {
		return fmt.Errorf("bitcoin++ API response omitted data")
	}
	if err := json.Unmarshal(envelope.Data, responseBody); err != nil {
		return fmt.Errorf("decode bitcoin++ API data: %w", err)
	}
	return nil
}
