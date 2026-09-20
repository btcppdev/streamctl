package db

import (
	"database/sql"
	"fmt"
)

// SaveBroadcastPlan atomically finds or creates the job by recording ID. The
// applied fingerprint is only acknowledged after systemd succeeds, so a crash
// or a failed sync can be retried without creating another job.
func (db *DB) SaveBroadcastPlan(s *Stream, endpointID int64, fingerprint string) (*Stream, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id int64
	var applied string
	err = tx.QueryRow(`SELECT p.stream_id, p.applied_fingerprint FROM broadcast_plan_sync p JOIN streams s ON s.id = p.stream_id WHERE p.recording_id = ? AND s.btcpp_recording_id = p.recording_id`, s.BTCPPRecordingID).Scan(&id, &applied)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err == nil && applied == fingerprint {
		return nil, nil
	}
	var count int
	if err := tx.QueryRow(`SELECT count(*), coalesce(min(id), 0) FROM streams WHERE btcpp_recording_id = ?`, s.BTCPPRecordingID).Scan(&count, &id); err != nil {
		return nil, err
	}
	if count > 1 {
		return nil, fmt.Errorf("multiple jobs already reference recording %s; resolve duplicates before importing", s.BTCPPRecordingID)
	}
	if count == 0 {
		if !s.Enabled {
			return nil, nil
		}
		res, err := tx.Exec(`INSERT INTO streams (name, schedule_type, on_calendar, btcpp_recording_id, enabled) VALUES (?, 'once', ?, ?, 1)`, s.Name, s.OnCalendar, s.BTCPPRecordingID)
		if err != nil {
			return nil, err
		}
		id, err = res.LastInsertId()
		if err != nil {
			return nil, err
		}
	} else if !s.Enabled {
		if _, err := tx.Exec(`UPDATE streams SET enabled = 0 WHERE id = ?`, id); err != nil {
			return nil, err
		}
	} else {
		if _, err := tx.Exec(`UPDATE streams SET name = ?, schedule_type = 'once', on_calendar = ?, btcpp_conference = '', enabled = 1 WHERE id = ?`, s.Name, s.OnCalendar, id); err != nil {
			return nil, err
		}
	}
	if s.Enabled {
		if _, err := tx.Exec(`DELETE FROM stream_videos WHERE stream_id = ?`, id); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`INSERT INTO stream_videos (stream_id, position, video_file) VALUES (?, 0, ?)`, id, s.Videos[0]); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`DELETE FROM stream_endpoints WHERE stream_id = ?`, id); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`INSERT INTO stream_endpoints (stream_id, endpoint_id) VALUES (?, ?)`, id, endpointID); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO broadcast_plan_sync (recording_id, stream_id) VALUES (?, ?) ON CONFLICT(recording_id) DO UPDATE SET stream_id = excluded.stream_id, applied_fingerprint = ''`, s.BTCPPRecordingID, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return db.GetStream(id)
}

func (db *DB) AcknowledgeBroadcastPlan(recordingID, fingerprint string) error {
	_, err := db.Exec(`UPDATE broadcast_plan_sync SET applied_fingerprint = ? WHERE recording_id = ?`, fingerprint, recordingID)
	return err
}
