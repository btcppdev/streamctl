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
