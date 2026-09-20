// Package broadcastplans imports the website's recording schedules into local
// playback jobs. Only one poll runs at a time.
package broadcastplans

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"streamctl/internal/btcppclient"
	"streamctl/internal/db"
)

type Client interface {
	RecordingBroadcastPlans(context.Context) ([]btcppclient.RecordingBroadcastPlan, error)
}

type Scheduler interface{ Sync(*db.Stream) error }

type Syncer struct {
	DB            *db.DB
	Client        Client
	Scheduler     Scheduler
	XEndpointID   int64
	XEndpointName string
	mu            sync.Mutex
}

func (s *Syncer) Run(ctx context.Context, interval time.Duration) {
	for {
		if err := s.Poll(ctx, time.Now()); err != nil {
			log.Printf("broadcast plan sync: %v", err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Syncer) Poll(ctx context.Context, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	plans, err := s.Client.RecordingBroadcastPlans(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, plan := range plans {
		if err := s.apply(plan, now); err != nil {
			errs = append(errs, fmt.Errorf("recording %s: %w", plan.RecordingID, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Syncer) apply(plan btcppclient.RecordingBroadcastPlan, now time.Time) error {
	if strings.TrimSpace(plan.RecordingID) == "" || plan.ScheduledAt.IsZero() {
		return fmt.Errorf("missing recording ID or scheduled time")
	}
	// Never replay old broadcasts or change jobs that may already be on air.
	if !plan.ScheduledAt.After(now) {
		return nil
	}
	stream := &db.Stream{
		Name: plan.Title, BTCPPRecordingID: plan.RecordingID,
		ScheduleType: "once", OnCalendar: plan.ScheduledAt.UTC().Format("2006-01-02 15:04:05 UTC"),
		Enabled: plan.Status == "scheduled",
	}
	var endpoint *db.Endpoint
	if stream.Enabled {
		key := plan.Source.ObjectKey
		if plan.Source.Kind != "spaces" || !strings.Contains(key, "/") || path.IsAbs(key) || path.Clean(key) != key || strings.ContainsAny(key, "\\\r\n\x00") || strings.HasPrefix(key, "../") {
			return fmt.Errorf("invalid Spaces source")
		}
		if !slices.Contains(plan.Destinations, "x") {
			return fmt.Errorf("plan has no X destination")
		}
		for _, destination := range plan.Destinations {
			if destination != "x" && destination != "website_hls" {
				return fmt.Errorf("unsupported destination %q", destination)
			}
		}
		var err error
		endpoint, err = s.xEndpoint()
		if err != nil {
			return fmt.Errorf("load configured X endpoint: %w", err)
		}
		if !endpoint.Enabled || endpoint.Type != "rtmp" || strings.TrimSpace(endpoint.RtmpURL) == "" || strings.TrimSpace(endpoint.StreamKey) == "" {
			return fmt.Errorf("configured X endpoint must be enabled with an RTMP URL and stream key")
		}
		stream.Videos = []string{key}
	} else {
		switch plan.Status {
		case "creating", "uploading_poster", "finalizing", "failed", "cancelled", "canceled":
		default:
			return fmt.Errorf("unknown plan status %q", plan.Status)
		}
	}
	// Exclude API metadata and heartbeat timestamps; they do not change playback.
	encoded, err := json.Marshal(struct {
		Stream   *db.Stream
		Endpoint *db.Endpoint
	}{stream, endpoint})
	if err != nil {
		return err
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(encoded))
	var endpointID int64
	if endpoint != nil {
		endpointID = endpoint.ID
	}
	saved, err := s.DB.SaveBroadcastPlan(stream, endpointID, fingerprint)
	if err != nil || saved == nil {
		return err
	}
	if err := s.Scheduler.Sync(saved); err != nil {
		return fmt.Errorf("job %d saved, timer sync failed: %w", saved.ID, err)
	}
	return s.DB.AcknowledgeBroadcastPlan(plan.RecordingID, fingerprint)
}

func (s *Syncer) xEndpoint() (*db.Endpoint, error) {
	if s.XEndpointID > 0 {
		if s.XEndpointName != "" {
			return nil, fmt.Errorf("configure either an X endpoint ID or name")
		}
		return s.DB.GetEndpoint(s.XEndpointID)
	}
	if s.XEndpointName == "" {
		return nil, fmt.Errorf("X endpoint is not configured")
	}
	endpoints, err := s.DB.ListEndpoints()
	if err != nil {
		return nil, err
	}
	var match *db.Endpoint
	for i := range endpoints {
		if endpoints[i].Name != s.XEndpointName {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("multiple endpoints named %q", s.XEndpointName)
		}
		match = &endpoints[i]
	}
	if match == nil {
		return nil, fmt.Errorf("endpoint %q not found", s.XEndpointName)
	}
	return match, nil
}
