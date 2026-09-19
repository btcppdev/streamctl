package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"streamctl/internal/btcppclient"
	"streamctl/internal/db"
)

type recordingWriterStub struct {
	productionCandidatesStub
	calls  int
	update btcppclient.RecordingUpdate
	err    error
}

func (s *recordingWriterStub) PutRecording(_ context.Context, conference, talk string, update btcppclient.RecordingUpdate) (*btcppclient.Recording, error) {
	if conference != "toronto" || talk != "talk-1" {
		return nil, errors.New("wrong recording target")
	}
	s.calls++
	s.update = update
	return &btcppclient.Recording{ID: "recording-1"}, s.err
}

func TestRegisterCompletedRecording(t *testing.T) {
	item := db.RecordingRegistration{QueueID: 7, Conference: "toronto", TalkID: "talk-1", Manifest: `{"version":1,"jobs":[{"id":"talk","segments":[{"type":"video","src":"berlin25/recordings/raw/source.mp4"}]}]}`}
	// Cross-conference sources use the worker's actual output location, not an
	// assumed directory based on the target talk's conference.
	ready := `{"status":"ready","output_prefix":"berlin25/recordings/renders/7","manifest_job_ids":["talk"],"jobs":[{"id":"talk","video":"talk.mp4"}],"files":["talk.mp4"]}`
	for _, tc := range []struct {
		name, ready     string
		readErr, apiErr error
		wantCalls       int
		wantErr         bool
	}{
		{"success", ready, nil, nil, 1, false},
		{"api failure", ready, nil, errors.New("offline"), 1, true},
		{"missing index", "", errors.New("not found"), nil, 0, true},
		{"bad JSON", "{", nil, nil, 0, true},
		{"not ready", strings.Replace(ready, `"ready"`, `"uploading"`, 1), nil, nil, 0, true},
		{"wrong queue", strings.ReplaceAll(ready, "renders/7", "renders/8"), nil, nil, 0, true},
		{"wrong job", strings.ReplaceAll(ready, `"talk"`, `"other"`), nil, nil, 0, true},
		{"wrong video", strings.ReplaceAll(ready, "talk.mp4", "../other.mp4"), nil, nil, 0, true},
		{"missing video", strings.Replace(ready, `"files":["talk.mp4"]`, `"files":[]`, 1), nil, nil, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &recordingWriterStub{err: tc.apiErr}
			err := registerCompletedRecording(context.Background(), client, item, func(_ context.Context, key string) ([]byte, error) {
				if key != "berlin25/recordings/renders/7/ready.json" {
					t.Fatalf("key %s", key)
				}
				return []byte(tc.ready), tc.readErr
			})
			if (err != nil) != tc.wantErr || client.calls != tc.wantCalls {
				t.Fatalf("calls=%d error=%v", client.calls, err)
			}
			if client.calls > 0 {
				if client.update.FileURI == nil || *client.update.FileURI != "berlin25/recordings/renders/7/talk.mp4" {
					t.Fatalf("update: %+v", client.update)
				}
				client.update.FileURI = nil
				if client.update != (btcppclient.RecordingUpdate{}) {
					t.Fatalf("unexpected metadata mutation: %+v", client.update)
				}
			}
		})
	}
}

func TestRecordingRegistrationDispatcherRecovery(t *testing.T) {
	database := productionHandlerTestDB(t)
	templateID, err := database.CreateProductionTemplate("toronto", "Talk", `{"segments":[{"type":"streamctl.talkCuts"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	manifest := `{"version":1,"jobs":[{"id":"talk","segments":[{"type":"video","src":"toronto/raw.mp4"}]}]}`
	id, _, err := database.CreateProductionRender("toronto", "Talk", manifest, &templateID, "talk-1", "full_talk")
	if err != nil {
		t.Fatal(err)
	}
	job, err := database.EnqueueProductionRender(id, "Talk", manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.MarkRenderQueueFinished(job.ID, "finished", ""); err != nil {
		t.Fatal(err)
	}
	client := &recordingWriterStub{err: errors.New("API offline")}
	h := &Handler{DB: database, BTCPP: client}
	read := func(_ context.Context, key string) ([]byte, error) {
		prefix := strings.TrimSuffix(key, "/ready.json")
		return []byte(`{"status":"ready","output_prefix":"` + prefix + `","manifest_job_ids":["talk"],"jobs":[{"id":"talk","video":"talk.mp4"}],"files":["talk.mp4"]}`), nil
	}
	h.registerCompletedRecordings(context.Background(), read)
	if client.calls != 1 {
		t.Fatalf("calls=%d", client.calls)
	}
	h.registerCompletedRecordings(context.Background(), read)
	if client.calls != 1 {
		t.Fatal("ignored retry delay")
	}
	if _, err := database.Exec(`UPDATE recording_registrations SET next_attempt_at = datetime('now', '-1 minute')`); err != nil {
		t.Fatal(err)
	}
	client.err = nil
	h = &Handler{DB: database, BTCPP: client} // Reconstructed controller, no GPU configured.
	h.registerCompletedRecordings(context.Background(), read)
	h.registerCompletedRecordings(context.Background(), read)
	if client.calls != 2 {
		t.Fatalf("calls=%d", client.calls)
	}
	updated, err := database.GetRenderQueueItem(job.ID)
	if err != nil || updated.Status != "finished" || updated.AttemptCount != 0 {
		t.Fatalf("render was changed: %+v %v", updated, err)
	}
	if label, _, err := database.RecordingRegistrationStatus(job.ID); err != nil || label != "Recording registered" {
		t.Fatalf("registration: %s %v", label, err)
	}
}
