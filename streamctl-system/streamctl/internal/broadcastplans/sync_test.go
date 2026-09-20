package broadcastplans

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"streamctl/internal/btcppclient"
	"streamctl/internal/db"
)

type fakeClient struct {
	plans []btcppclient.RecordingBroadcastPlan
	err   error
}

func (c *fakeClient) RecordingBroadcastPlans(context.Context) ([]btcppclient.RecordingBroadcastPlan, error) {
	return c.plans, c.err
}

type fakeScheduler struct {
	calls int
	err   error
	last  *db.Stream
}

func (s *fakeScheduler) Sync(stream *db.Stream) error { s.calls++; s.last = stream; return s.err }

func setup(t *testing.T) (*Syncer, *fakeClient, *fakeScheduler, time.Time) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	id, err := database.CreateEndpoint(&db.Endpoint{Name: "X", Type: "rtmp", RtmpURL: "rtmps://example.test/live", StreamKey: "secret", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	plan := btcppclient.RecordingBroadcastPlan{RecordingID: "recording-1", Title: "A talk", Status: "scheduled", ScheduledAt: now.Add(time.Hour), Destinations: []string{"website_hls", "x"}}
	plan.Source.Kind = "spaces"
	plan.Source.ObjectKey = "dev26/recordings/edits/talk.mp4"
	client := &fakeClient{plans: []btcppclient.RecordingBroadcastPlan{plan}}
	scheduler := &fakeScheduler{}
	return &Syncer{DB: database, Client: client, Scheduler: scheduler, XEndpointID: id}, client, scheduler, now
}

func TestImportRescheduleAndRestart(t *testing.T) {
	s, client, scheduler, now := setup(t)
	if err := s.Poll(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	first := scheduler.last
	if !first.AutoScheduled {
		t.Fatal("imported job lost its automatic scheduling marker")
	}
	if scheduler.calls != 1 || first.ScheduleType != "once" || first.OnCalendar != "2026-09-20 13:00:00 UTC" || first.BTCPPRecordingID != "recording-1" || len(first.Videos) != 1 || first.Videos[0] != client.plans[0].Source.ObjectKey || len(first.Endpoints) != 1 || first.Endpoints[0].ID != s.XEndpointID || !first.Enabled {
		t.Fatalf("import: %+v", first)
	}
	// A new synchronizer models process restart; the acknowledgement is durable.
	s = &Syncer{DB: s.DB, Client: client, Scheduler: scheduler, XEndpointID: s.XEndpointID}
	if err := s.Poll(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if scheduler.calls != 1 {
		t.Fatal("unchanged plan touched systemd")
	}
	client.plans[0].ScheduledAt = now.Add(2 * time.Hour)
	client.plans[0].Source.ObjectKey = "dev26/recordings/edits/revised.mp4"
	if err := s.Poll(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if scheduler.calls != 2 || scheduler.last.ID != first.ID || scheduler.last.OnCalendar != "2026-09-20 14:00:00 UTC" || scheduler.last.Videos[0] != client.plans[0].Source.ObjectKey {
		t.Fatalf("reschedule: %+v", scheduler.last)
	}
	streams, err := s.DB.ListStreams()
	if err != nil || len(streams) != 1 {
		t.Fatalf("jobs=%v err=%v", streams, err)
	}
	if !streams[0].AutoScheduled {
		t.Fatal("listed job lost automatic scheduling marker")
	}
}

func TestReuseManualJobAndRetryFailedSystemd(t *testing.T) {
	s, _, scheduler, now := setup(t)
	id, err := s.DB.CreateStream(&db.Stream{Name: "Manual", BTCPPRecordingID: "recording-1", ScheduleType: "once", OnCalendar: "2026-09-21 10:00:00 UTC"}, nil, []string{"old.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	scheduler.err = errors.New("systemd unavailable")
	if err := s.Poll(context.Background(), now); err == nil {
		t.Fatal("expected sync error")
	}
	scheduler.err = nil
	if err := s.Poll(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if scheduler.calls != 2 || scheduler.last.ID != id {
		t.Fatal("failed import was not retried on the same job")
	}
	streams, err := s.DB.ListStreams()
	if err != nil || len(streams) != 1 {
		t.Fatalf("jobs=%v err=%v", streams, err)
	}
}

func TestCancelledPlanDisablesJobAndCanBeRescheduled(t *testing.T) {
	s, client, scheduler, now := setup(t)
	for _, status := range []string{"scheduled", "cancelled", "scheduled"} {
		client.plans[0].Status = status
		if err := s.Poll(context.Background(), now); err != nil {
			t.Fatal(err)
		}
		if scheduler.last.Enabled != (status == "scheduled") {
			t.Fatalf("status %s: %+v", status, scheduler.last)
		}
	}
	if scheduler.calls != 3 {
		t.Fatalf("calls=%d", scheduler.calls)
	}
}

func TestIgnorePastAndUnfinishedPlans(t *testing.T) {
	for _, status := range []string{"scheduled", "creating", "uploading_poster", "finalizing", "failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			s, client, scheduler, now := setup(t)
			client.plans[0].Status = status
			if status == "scheduled" {
				client.plans[0].ScheduledAt = now.Add(-time.Minute)
			}
			if err := s.Poll(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			streams, err := s.DB.ListStreams()
			if err != nil || len(streams) != 0 || scheduler.calls != 0 {
				t.Fatalf("jobs=%v calls=%d err=%v", streams, scheduler.calls, err)
			}
		})
	}
}

func TestInvalidPlanDoesNotBlockOtherRecordings(t *testing.T) {
	s, client, scheduler, now := setup(t)
	bad := client.plans[0]
	bad.RecordingID = "bad"
	bad.Source.ObjectKey = "../secret.mp4"
	client.plans = append([]btcppclient.RecordingBroadcastPlan{bad}, client.plans...)
	if err := s.Poll(context.Background(), now); err == nil {
		t.Fatal("expected validation error")
	}
	if scheduler.calls != 1 || scheduler.last.BTCPPRecordingID != "recording-1" {
		t.Fatal("valid plan was not imported")
	}
}

func TestAPIErrorDoesNotCancelExistingJobs(t *testing.T) {
	s, client, scheduler, now := setup(t)
	if err := s.Poll(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	client.err = errors.New("API unavailable")
	if err := s.Poll(context.Background(), now); err == nil {
		t.Fatal("expected API error")
	}
	stream, err := s.DB.GetStream(scheduler.last.ID)
	if err != nil || !stream.Enabled || scheduler.calls != 1 {
		t.Fatalf("stream=%+v err=%v", stream, err)
	}
}

func TestRejectDisabledEndpointAndDuplicateJobs(t *testing.T) {
	s, _, scheduler, now := setup(t)
	endpoint, err := s.DB.GetEndpoint(s.XEndpointID)
	if err != nil {
		t.Fatal(err)
	}
	endpoint.Enabled = false
	if err := s.DB.UpdateEndpoint(endpoint); err != nil {
		t.Fatal(err)
	}
	if err := s.Poll(context.Background(), now); err == nil {
		t.Fatal("accepted disabled endpoint")
	}
	endpoint.Enabled = true
	if err := s.DB.UpdateEndpoint(endpoint); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.DB.CreateStream(&db.Stream{Name: "duplicate", BTCPPRecordingID: "recording-1", ScheduleType: "once"}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Poll(context.Background(), now); err == nil {
		t.Fatal("accepted ambiguous existing jobs")
	}
	if scheduler.calls != 0 {
		t.Fatal("invalid import touched systemd")
	}
}

func TestNamedTwitterEndpoint(t *testing.T) {
	for _, scenario := range []string{"matching", "missing", "duplicate", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			s, _, scheduler, now := setup(t)
			expectedID := s.XEndpointID
			endpoint, err := s.DB.GetEndpoint(expectedID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "missing" {
				endpoint.Name = "Twitter Livestreams"
			}
			if scenario == "disabled" {
				endpoint.Enabled = false
			}
			if err := s.DB.UpdateEndpoint(endpoint); err != nil {
				t.Fatal(err)
			}
			if scenario == "duplicate" {
				if _, err := s.DB.CreateEndpoint(endpoint); err != nil {
					t.Fatal(err)
				}
			}
			s.XEndpointID = 0
			s.XEndpointName = "Twitter Livestreams"
			err = s.Poll(context.Background(), now)
			if scenario != "matching" {
				if err == nil || scheduler.calls != 0 {
					t.Fatalf("expected error without scheduling; err=%v calls=%d", err, scheduler.calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(scheduler.last.Endpoints) != 1 || scheduler.last.Endpoints[0].ID != expectedID {
				t.Fatalf("wrong destination: %+v", scheduler.last.Endpoints)
			}
			if err := s.Poll(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			if scheduler.calls != 1 {
				t.Fatal("repeated named import resynced timer")
			}
		})
	}
}
