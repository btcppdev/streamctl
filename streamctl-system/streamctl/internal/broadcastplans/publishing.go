package broadcastplans

import (
	"context"
	"fmt"
	"time"

	"streamctl/internal/btcppclient"
)

type calendarClient interface {
	PutPublishingStreams(context.Context, []btcppclient.PublishingStream) error
}
type timerReader interface{ NextTriggerUnix(int64) int64 }

func (s *Syncer) publishCalendar(ctx context.Context, now time.Time) error {
	client, ok := s.Client.(calendarClient)
	if !ok {
		return nil
	}
	streams, err := s.DB.ListStreams()
	if err != nil {
		return err
	}
	snapshot := []btcppclient.PublishingStream{}
	for _, stream := range streams {
		if stream.AutoScheduled {
			continue
		}
		v := btcppclient.PublishingStream{ID: fmt.Sprint(stream.ID), Title: stream.Name, Conference: stream.BTCPPConference, State: "scheduled"}
		if !stream.Enabled {
			v.State = "disabled"
		}
		if timers, ok := s.Scheduler.(timerReader); ok && stream.Enabled {
			if sec := timers.NextTriggerUnix(stream.ID); sec > 0 {
				at := time.Unix(sec, 0).UTC()
				v.At = &at
			}
		}
		if v.At == nil && stream.ScheduleType == "once" {
			if at, e := time.Parse("2006-01-02 15:04:05 MST", stream.OnCalendar); e == nil && at.After(now) && at.Location().String() == "UTC" {
				v.At = &at
			}
		}
		if stream.ScheduleType == "recurring" {
			v.Title += " · " + stream.OnCalendar
		}
		// Unknown times remain visible in the unscheduled tray, never invented.
		snapshot = append(snapshot, v)
	}
	return client.PutPublishingStreams(ctx, snapshot)
}
