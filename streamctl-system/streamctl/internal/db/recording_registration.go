package db

import "database/sql"

// RecordingRegistration is a durable, independently retried website update.
// The queue item owns the immutable manifest; this row snapshots its destination.
type RecordingRegistration struct {
	QueueID    int64
	Conference string
	TalkID     string
	Manifest   string
}

func (db *DB) migrateRecordingRegistration() error {
	has, err := db.hasColumn("production_renders", "recording_kind")
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if !has {
		if _, err := tx.Exec(`ALTER TABLE production_renders ADD COLUMN recording_kind TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
		// The original generator created full-talk renders exclusively. Preserve
		// their designation, but do not register historical queue submissions.
		if _, err := tx.Exec(`UPDATE production_renders SET recording_kind = 'full_talk'
			WHERE talk_id <> '' AND EXISTS (
			 SELECT 1 FROM production_templates t, json_each(t.template_json, '$.segments') s
			 WHERE t.id = production_renders.template_id AND json_extract(s.value, '$.type') = 'streamctl.talkCuts'
			)`); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS recording_registrations (
		queue_id INTEGER PRIMARY KEY,
		conference TEXT NOT NULL,
		talk_id TEXT NOT NULL,
		registered_at DATETIME,
		last_error TEXT NOT NULL DEFAULT '',
		next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS recording_registration_target ON recording_registrations (conference, talk_id, queue_id);`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Only the newest successful submission for a talk may update its recording.
// Failed or queued replacements do not suppress an existing successful render.
func (db *DB) PendingRecordingRegistrations() ([]RecordingRegistration, error) {
	rows, err := db.Query(`SELECT r.queue_id, r.conference, r.talk_id, q.manifest_json
		FROM recording_registrations r JOIN render_job_queue q ON q.id = r.queue_id
		WHERE q.status = 'finished' AND r.registered_at IS NULL AND r.next_attempt_at <= CURRENT_TIMESTAMP
		AND NOT EXISTS (SELECT 1 FROM recording_registrations newer
		 JOIN render_job_queue nq ON nq.id = newer.queue_id
		 WHERE newer.conference = r.conference AND newer.talk_id = r.talk_id
		 AND newer.queue_id > r.queue_id AND nq.status = 'finished')
		ORDER BY r.next_attempt_at, r.queue_id LIMIT 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []RecordingRegistration
	for rows.Next() {
		var item RecordingRegistration
		if err := rows.Scan(&item.QueueID, &item.Conference, &item.TalkID, &item.Manifest); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (db *DB) FinishRecordingRegistration(id int64, failure string) error {
	if failure != "" {
		_, err := db.Exec(`UPDATE recording_registrations SET last_error = ?, next_attempt_at = datetime('now', '+10 minutes') WHERE queue_id = ?`, failure, id)
		return err
	}
	_, err := db.Exec(`UPDATE recording_registrations SET registered_at = CURRENT_TIMESTAMP, last_error = '' WHERE queue_id = ?`, id)
	return err
}

func (db *DB) RecordingRegistrationStatus(id int64) (label, detail string, err error) {
	var registered sql.NullTime
	var superseded bool
	err = db.QueryRow(`SELECT r.registered_at, r.last_error, EXISTS (
		SELECT 1 FROM recording_registrations newer JOIN render_job_queue q ON q.id = newer.queue_id
		WHERE newer.conference = r.conference AND newer.talk_id = r.talk_id
		AND newer.queue_id > r.queue_id AND q.status = 'finished')
		FROM recording_registrations r WHERE r.queue_id = ?`, id).Scan(&registered, &detail, &superseded)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	if superseded {
		return "Newer recording rendered", "", nil
	}
	if registered.Valid {
		return "Recording registered", "", nil
	}
	if detail != "" {
		return "Recording registration retrying", detail, nil
	}
	return "Recording registration pending", "", nil
}
