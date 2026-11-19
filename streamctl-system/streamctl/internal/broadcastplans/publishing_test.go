package broadcastplans

import (
	"context"
	"testing"
	"time"

	"streamctl/internal/btcppclient"
	"streamctl/internal/db"
)

type publishingClient struct {
	fakeClient
	streams []btcppclient.PublishingStream
}

func (c *publishingClient) PutPublishingStreams(_ context.Context, s []btcppclient.PublishingStream) error {
	c.streams = s
	return nil
}
func TestPublishingSnapshotExcludesImportedPlans(t *testing.T) {
	s, original, _, now := setup(t)
	client := &publishingClient{fakeClient: *original}
	s.Client = client
	_, err := s.DB.CreateStream(&db.Stream{Name: "Manual", BTCPPConference: "berlin26", ScheduleType: "once", OnCalendar: now.Add(time.Hour).Format("2006-01-02 15:04:05 UTC"), Enabled: true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB.CreateStream(&db.Stream{Name: "Recurring", ScheduleType: "recurring", OnCalendar: "Mon *-*-* 12:00:00 UTC", Enabled: true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Poll(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if len(client.streams) != 2 {
		t.Fatalf("snapshot duplicated imported plan: %+v", client.streams)
	}
	var known, unknown bool
	for _, v := range client.streams {
		if v.Conference == "berlin26" {
			known = v.At != nil && v.At.Equal(now.Add(time.Hour))
		} else {
			unknown = v.At == nil
		}
	}
	if !known || !unknown {
		t.Fatalf("times: %+v", client.streams)
	}
}

func TestPublishingSnapshotRetainsPastAndDisabledOneOffDates(t *testing.T) {
	s, original, _, now := setup(t)
	client := &publishingClient{fakeClient: *original}
	s.Client = client
	for _, stream := range []db.Stream{
		{Name: "Past day stream", ScheduleType: "once", OnCalendar: "2026-09-01 10:00:00 UTC", Enabled: true},
		{Name: "Disabled", ScheduleType: "once", OnCalendar: "2026-09-21 10:00:00 UTC", Enabled: false},
		{Name: "Unknown recurring", ScheduleType: "recurring", OnCalendar: "Mon *-*-* 12:00:00 UTC", Enabled: true},
	} {
		if _, err := s.DB.CreateStream(&stream, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.publishCalendar(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if len(client.streams) != 3 {
		t.Fatalf("streams=%+v", client.streams)
	}
	for _, v := range client.streams {
		switch v.Title {
		case "Past day stream":
			if v.At == nil || !v.At.Before(now) {
				t.Fatalf("past stream lost its date: %+v", v)
			}
		case "Disabled":
			if v.At == nil || v.State != "disabled" {
				t.Fatalf("disabled schedule lost: %+v", v)
			}
		default:
			if v.At != nil {
				t.Fatalf("invented time: %+v", v)
			}
		}
	}
}

func TestOneOffCalendarTimeUsesSystemTimezone(t *testing.T) {
	local, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ raw, want string }{
		{"2026-10-08 17:00:00", "2026-10-09T00:00:00Z"},
		{"2026-10-08 17:00:00 UTC", "2026-10-08T17:00:00Z"},
		{"2026-10-08 17:00 America/Chicago", "2026-10-08T22:00:00Z"},
		{"2026-10-08T17:00:00+09:00", "2026-10-08T08:00:00Z"},
	} {
		at, ok := oneOffCalendarTime(tc.raw, local)
		if !ok || at.Format(time.RFC3339) != tc.want {
			t.Errorf("%s: %v %v", tc.raw, at, ok)
		}
	}
	for _, raw := range []string{"Mon *-*-* 12:00:00 UTC", "2026-10-08 17:00:00 FAKE", "garbage"} {
		if at, ok := oneOffCalendarTime(raw, local); ok {
			t.Fatalf("invented time for %s: %v", raw, at)
		}
	}
}
