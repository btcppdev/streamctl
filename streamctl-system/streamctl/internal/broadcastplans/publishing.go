package broadcastplans

import (
	"context"
	"fmt"
	"strings"
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
			if at, ok := oneOffCalendarTime(stream.OnCalendar, time.Local); ok {
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

// Keep a one-off schedule's original date after its timer expires. A missing
// timezone uses the streamer's local timezone, just like systemd OnCalendar.
func oneOffCalendarTime(raw string, local *time.Location) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if at, err := time.ParseInLocation(layout, raw, local); err == nil {
			return at.UTC(), true
		}
	}
	if i := strings.LastIndexByte(raw, ' '); i >= 0 {
		if zone, err := time.LoadLocation(raw[i+1:]); err == nil {
			for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04"} {
				if at, err := time.ParseInLocation(layout, raw[:i], zone); err == nil {
					return at.UTC(), true
				}
			}
		}
	}
	return time.Time{}, false
}
