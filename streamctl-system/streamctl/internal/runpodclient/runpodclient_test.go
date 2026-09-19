package runpodclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCreatePodNeverRepeatsPOST(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		lost       bool
		wantError  bool
	}{
		{"direct", `{"id":"created"}`, 201, false, false},
		{"wrapped", `{"pod":{"id":"created"}}`, 201, false, false},
		{"lost response", "", 0, true, true},
		{"server error", `{"error":"unavailable"}`, 500, false, true},
		{"invalid JSON", `{`, 201, false, true},
		{"missing ID", `{}`, 201, false, true},
		{"empty response", ``, 201, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := New("test-token")
			client.HTTP.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/v1/pods" {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
				}
				if test.lost {
					return nil, errors.New("response lost after creation")
				}
				return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader(test.body)), Header: make(http.Header)}, nil
			})
			pod, err := client.CreatePod(context.Background(), CreatePodRequest{Name: "worker"})
			if calls != 1 {
				t.Fatalf("issued %d POSTs, want exactly one", calls)
			}
			if (err != nil) != test.wantError {
				t.Fatalf("unexpected result: pod=%+v err=%v", pod, err)
			}
			if err == nil && pod.ID != "created" {
				t.Fatalf("unexpected pod: %+v", pod)
			}
		})
	}
}
