package runpodclient

import (
	"context"
	"encoding/json"
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
				if r.Method != http.MethodPost || r.URL.Path != "/v2/pods" || r.URL.Host != "api.runpod.io" {
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

func TestListPodsFollowsV2Pagination(t *testing.T) {
	client := New("test-token")
	calls := 0
	client.HTTP.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/v2/pods" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("unexpected request: %s", r.URL)
		}
		body := `{"pods":[{"id":"first","status":"RUNNING"}],"pagination":{"hasNextPage":true,"nextCursor":"next +/="}}`
		if calls == 2 {
			if r.URL.Query().Get("cursor") != "next +/=" {
				t.Fatalf("cursor was not encoded correctly: %s", r.URL)
			}
			body = `{"pods":[{"id":"second","status":"STARTING"}],"pagination":{"hasNextPage":false,"nextCursor":null}}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	pods, err := client.ListPods(context.Background())
	if err != nil || len(pods) != 2 || calls != 2 || pods[0].Status() != "active" || pods[1].Status() != "starting" {
		t.Fatalf("unexpected result: %+v %v calls=%d", pods, err, calls)
	}
}

func TestListPodsDoesNotRetryErrorsOrLoopOnInvalidCursor(t *testing.T) {
	for _, status := range []int{429, 200} {
		client := New("test-token")
		calls := 0
		client.HTTP.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if calls > 2 {
				t.Fatal("pagination did not terminate")
			}
			body := `{"pods":[],"pagination":{"hasNextPage":true,"nextCursor":"same"}}`
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		if _, err := client.ListPods(context.Background()); err == nil {
			t.Fatal("expected an error")
		}
		if status == 429 && calls != 1 {
			t.Fatalf("rate limited request was repeated %d times", calls)
		}
	}
}

func TestGetPodUsesLiveDirectSSHMapping(t *testing.T) {
	client := New("test-token")
	client.HTTP.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v2/pods/worker" {
			t.Fatalf("unexpected request: %s", r.URL)
		}
		body := `{"id":"worker","status":"RUNNING","cost":1.09,"gpu":{"id":"NVIDIA L40S","count":1},"ssh":{"direct":{"host":"192.0.2.1","port":1234,"username":"root"}}}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	pod, err := client.GetPod(context.Background(), "worker")
	if err != nil || pod.SSHHost("root") != "ssh://root@192.0.2.1:1234" || pod.CostPerHr != 1.09 || pod.GPU.ID != "NVIDIA L40S" {
		t.Fatalf("unexpected pod: %+v %v", pod, err)
	}
}

func TestV2CreatePreservesCommandStorageAndGPU(t *testing.T) {
	client := New("test-token")
	client.HTTP.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		for _, obsolete := range []string{"imageName", "gpuTypeIds", "gpuCount", "cloudType", "containerDiskInGb", "volumeInGb", "dockerStartCmd"} {
			if _, ok := body[obsolete]; ok {
				t.Fatalf("v1 field sent: %s", obsolete)
			}
		}
		for key, expected := range map[string]string{
			"gpu":    `{"id":"NVIDIA L40S","count":1}`,
			"mounts": `{"persistent":{"size":80,"path":"/workspace"}}`,
			"disk":   `80`, "ports": `["22/tcp"]`, "cloud": `"SECURE"`,
			"image": `"worker-image"`, "cmd": `["bash","-lc","printf 'hello world'"]`,
		} {
			if string(body[key]) != expected {
				t.Errorf("%s = %s, want %s", key, body[key], expected)
			}
		}
		return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader(`{"id":"created"}`))}, nil
	})
	_, err := client.CreatePod(context.Background(), CreatePodRequest{
		Name: "worker", ImageName: "worker-image", GPU: GPUConfig{ID: "NVIDIA L40S", Count: 1},
		CloudType: "SECURE", ContainerDiskInGB: 80,
		Mounts: Mounts{Persistent: &PersistentMount{Size: 80, Path: "/workspace"}},
		Ports:  []string{"22/tcp"}, DockerStartCmd: []string{"bash", "-lc", "printf 'hello world'"},
	})
	if err != nil {
		t.Fatal(err)
	}
}
