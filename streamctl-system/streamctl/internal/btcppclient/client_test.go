package btcppclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestClientListsCandidatesAndNeverPlacesTokenInURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("query leaked data: %q", r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/api/v1/conferences/dev26/recording-candidates":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{
					"talk_id": "talk-1", "title": "A talk", "eligible": true,
					"speakers": []map[string]any{{"person_id": "person-1", "name": "Ada", "company": "Babbage"}},
				}},
			})
		case "/api/v1/conferences":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "conference-1", "tag": "dev26", "description": "Dev++"}}})
		default:
			t.Errorf("path=%q", r.URL.Path)
		}
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: "secret-token", HTTPClient: server.Client()}
	candidates, err := client.RecordingCandidates(context.Background(), "dev26")
	if err != nil || len(candidates) != 1 || candidates[0].TalkID != "talk-1" || len(candidates[0].Speakers) != 1 || candidates[0].Speakers[0].Name != "Ada" {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	conferences, err := client.Conferences(context.Background())
	if err != nil || len(conferences) != 1 || conferences[0].Tag != "dev26" {
		t.Fatalf("conferences=%+v err=%v", conferences, err)
	}
}

func TestTokenFileRejectsBroadPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("secret-token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := TokenFromFile(path); err == nil {
		t.Fatal("accepted group/world-readable token")
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if token, err := TokenFromFile(path); err != nil || token != "secret-token" {
		t.Fatalf("token=%q err=%v", token, err)
	}
}

func TestClientUpdatesBroadcastHeartbeat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/recordings/recording-1/broadcast" || r.Method != http.MethodPut {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var update BroadcastUpdate
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			t.Fatal(err)
		}
		if update.State != "live" || update.HLSURL != "https://stream.btcpp.dev/live/stream-7/index.m3u8" {
			t.Fatalf("unexpected update: %#v", update)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"state": "live", "is_live": true}})
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: "secret-token", HTTPClient: server.Client()}
	broadcast, err := client.PutBroadcast(context.Background(), "recording-1", BroadcastUpdate{
		State: "live", HLSURL: "https://stream.btcpp.dev/live/stream-7/index.m3u8",
	})
	if err != nil || broadcast.State != "live" || !broadcast.IsLive {
		t.Fatalf("broadcast=%+v err=%v", broadcast, err)
	}
}

func TestTokenFileAllowsPrivateSystemdCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "btcpp-api-token")
	if err := os.WriteFile(path, []byte("credential-token"), 0440); err != nil {
		t.Fatal(err)
	}
	if _, err := TokenFromFile(path); err == nil {
		t.Fatal("accepted group-readable ordinary token")
	}
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	if err := os.Chmod(dir, 0550); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	if token, err := TokenFromFile(path); err != nil || token != "credential-token" {
		t.Fatalf("credential: %q, %v", token, err)
	}
	if err := os.Chmod(dir, 0555); err != nil {
		t.Fatal(err)
	}
	if _, err := TokenFromFile(path); err == nil {
		t.Fatal("accepted publicly accessible credential directory")
	}
}

func TestClientUpdatesConferenceBroadcast(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != "/api/v1/conferences/toronto/broadcast" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var update BroadcastUpdate
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			t.Fatal(err)
		}
		if update.Title != "Day 3" || update.State != "live" {
			t.Errorf("unexpected body: %+v", update)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"state": "live", "is_live": true}})
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: "test", HTTPClient: server.Client()}
	result, err := client.PutConferenceBroadcast(context.Background(), "toronto", BroadcastUpdate{Title: "Day 3", State: "live", HLSURL: "https://stream.example/live.m3u8"})
	if err != nil || !result.IsLive {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}
