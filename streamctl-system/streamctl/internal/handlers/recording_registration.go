package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"streamctl/internal/btcppclient"
	"streamctl/internal/db"
)

type recordingWriter interface {
	PutRecording(context.Context, string, string, btcppclient.RecordingUpdate) (*btcppclient.Recording, error)
}

func (h *Handler) recordingRegistrationDispatcher() {
	// The website's recording-edit endpoint permits 30 writes/hour. Keep this
	// independent of GPU dispatch and process one update at a time, below that cap.
	ticker := time.NewTicker(125 * time.Second)
	defer ticker.Stop()
	for {
		h.registerCompletedRecordings(context.Background(), h.rcloneCat)
		<-ticker.C
	}
}

func (h *Handler) registerCompletedRecordings(ctx context.Context, readObject func(context.Context, string) ([]byte, error)) {
	client, ok := h.BTCPP.(recordingWriter)
	if !ok || h.DB == nil {
		return
	}
	items, err := h.DB.PendingRecordingRegistrations()
	if err != nil {
		log.Printf("list recording registrations: %v", err)
		return
	}
	for _, item := range items {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		err := registerCompletedRecording(ctx, client, item, readObject)
		cancel()
		failure := ""
		if err != nil {
			failure = err.Error()
			log.Printf("recording registration for render job %d will retry: %v", item.QueueID, err)
		}
		if err := h.DB.FinishRecordingRegistration(item.QueueID, failure); err != nil {
			log.Printf("save recording registration for render job %d: %v", item.QueueID, err)
		}
	}
}

func registerCompletedRecording(ctx context.Context, client recordingWriter, item db.RecordingRegistration, readObject func(context.Context, string) ([]byte, error)) error {
	var manifest renderManifestEnvelope
	if err := json.Unmarshal([]byte(item.Manifest), &manifest); err != nil || len(manifest.Jobs) != 1 {
		return fmt.Errorf("full-talk recording requires exactly one render output")
	}
	prefix := strings.TrimSuffix(renderOutputPrefix(item.QueueID, item.Manifest), "/")
	if prefix == "" {
		return fmt.Errorf("render output prefix is missing")
	}
	raw, err := readObject(ctx, prefix+"/ready.json")
	if err != nil {
		return err
	}
	var ready struct {
		Status string   `json:"status"`
		Prefix string   `json:"output_prefix"`
		IDs    []string `json:"manifest_job_ids"`
		Jobs   []struct {
			ID    string `json:"id"`
			Video string `json:"video"`
		} `json:"jobs"`
		Files []string `json:"files"`
	}
	if err := json.Unmarshal(raw, &ready); err != nil {
		return fmt.Errorf("invalid render ready index: %w", err)
	}
	id := manifest.Jobs[0].ID
	video := id + ".mp4"
	if ready.Status != "ready" || ready.Prefix != prefix || len(ready.IDs) != 1 || ready.IDs[0] != id || len(ready.Jobs) != 1 || ready.Jobs[0].ID != id || ready.Jobs[0].Video != video {
		return fmt.Errorf("render ready index does not match submitted job")
	}
	found := false
	for _, file := range ready.Files {
		if file == video {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("render ready index is missing the video")
	}
	key, err := validateRenderObjectKey(prefix + "/" + video)
	if err != nil {
		return err
	}
	// Deliberately omit publication and social URLs. The website derives the
	// talk name and preserves all other existing recording metadata.
	_, err = client.PutRecording(ctx, item.Conference, item.TalkID, btcppclient.RecordingUpdate{FileURI: &key})
	return err
}
