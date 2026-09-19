package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ProductionRender is an editable, conference-scoped conf-render manifest.
// Generated drafts retain their template and talk IDs so generation is idempotent.
type ProductionRender struct {
	ID         int64
	Conference string
	Name       string
	JSON       string
	TemplateID sql.NullInt64
	TalkID     string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type ProductionRenderQueueState struct {
	Latest      *RenderJobQueueItem
	HasFinished bool
}

func (db *DB) ListProductionRenders(conference string) ([]ProductionRender, error) {
	rows, err := db.Query(`
		SELECT id, conference, name, manifest_json, template_id, talk_id, created_at, updated_at
		FROM production_renders WHERE conference = ? AND archived_at IS NULL
		ORDER BY updated_at DESC, id DESC
	`, strings.TrimSpace(conference))
	if err != nil {
		return nil, fmt.Errorf("list production renders: %w", err)
	}
	defer rows.Close()
	var renders []ProductionRender
	for rows.Next() {
		item, err := scanProductionRender(rows)
		if err != nil {
			return nil, err
		}
		renders = append(renders, item)
	}
	return renders, rows.Err()
}

func (db *DB) ProductionRender(id int64, conference string) (ProductionRender, error) {
	return scanProductionRender(db.QueryRow(`
		SELECT id, conference, name, manifest_json, template_id, talk_id, created_at, updated_at
		FROM production_renders WHERE id = ? AND conference = ? AND archived_at IS NULL
	`, id, strings.TrimSpace(conference)))
}

func (db *DB) CreateProductionRender(conference, name, manifest string, templateID *int64, talkID string, recordingKind ...string) (int64, bool, error) {
	kind := ""
	if len(recordingKind) > 0 {
		kind = recordingKind[0]
	}
	if kind != "" && (kind != "full_talk" || templateID == nil || strings.TrimSpace(talkID) == "") {
		return 0, false, fmt.Errorf("invalid recording designation")
	}
	conference, name, manifest, talkID = strings.TrimSpace(conference), strings.TrimSpace(name), strings.TrimSpace(manifest), strings.TrimSpace(talkID)
	if conference == "" || name == "" || manifest == "" {
		return 0, false, fmt.Errorf("conference, render name, and manifest are required")
	}
	result, err := db.Exec(`
		INSERT OR IGNORE INTO production_renders (conference, name, manifest_json, template_id, talk_id, recording_kind)
		VALUES (?, ?, ?, ?, ?, ?)
	`, conference, name, manifest, templateID, talkID, kind)
	if err != nil {
		return 0, false, fmt.Errorf("create production render: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		if templateID == nil || talkID == "" {
			return 0, false, nil
		}
		var id int64
		var archivedAt sql.NullTime
		if err := db.QueryRow(`
			SELECT id, archived_at FROM production_renders
			WHERE conference = ? AND template_id = ? AND talk_id = ?
		`, conference, *templateID, talkID).Scan(&id, &archivedAt); err != nil {
			return 0, false, err
		}
		if !archivedAt.Valid {
			return id, false, nil
		}
		result, err := db.Exec(`
			UPDATE production_renders
			SET name = ?, manifest_json = ?, recording_kind = ?, archived_at = NULL, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND archived_at IS NOT NULL
		`, name, manifest, kind, id)
		if err != nil {
			return 0, false, fmt.Errorf("restore production render: %w", err)
		}
		restored, _ := result.RowsAffected()
		return id, restored == 1, nil
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("read production render ID: %w", err)
	}
	return id, true, nil
}

func (db *DB) UpdateProductionRender(id int64, conference, name, manifest string) error {
	conference, name, manifest = strings.TrimSpace(conference), strings.TrimSpace(name), strings.TrimSpace(manifest)
	if id <= 0 || conference == "" || name == "" || manifest == "" {
		return fmt.Errorf("render ID, conference, name, and manifest are required")
	}
	result, err := db.Exec(`
		UPDATE production_renders SET name = ?, manifest_json = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND conference = ? AND archived_at IS NULL
	`, name, manifest, id, conference)
	if err != nil {
		return fmt.Errorf("update production render: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (db *DB) ArchiveProductionRender(id int64, conference string) error {
	result, err := db.Exec(`
		UPDATE production_renders SET archived_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND conference = ? AND archived_at IS NULL
	`, id, strings.TrimSpace(conference))
	if err != nil {
		return fmt.Errorf("archive production render: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (db *DB) ArchiveProductionRenders(conference string, ids []int64) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	deleted := 0
	for _, id := range ids {
		result, err := tx.Exec(`
			UPDATE production_renders SET archived_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND conference = ? AND archived_at IS NULL
		`, id, strings.TrimSpace(conference))
		if err != nil {
			return 0, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		if changed != 1 {
			return 0, fmt.Errorf("render %d is missing", id)
		}
		deleted++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

func (db *DB) ProductionRenderQueueStates(conference string) (map[int64]ProductionRenderQueueState, error) {
	rows, err := db.Query(`
		SELECT `+renderJobColumns+`
		FROM render_job_queue
		WHERE production_render_id IN (SELECT id FROM production_renders WHERE conference = ? AND archived_at IS NULL)
		ORDER BY production_render_id, id DESC
	`, strings.TrimSpace(conference))
	if err != nil {
		return nil, fmt.Errorf("list production render jobs: %w", err)
	}
	defer rows.Close()
	states := map[int64]ProductionRenderQueueState{}
	for rows.Next() {
		item, err := scanRenderJob(rows)
		if err != nil {
			return nil, err
		}
		if !item.ProductionRenderID.Valid {
			continue
		}
		id := item.ProductionRenderID.Int64
		state := states[id]
		if state.Latest == nil {
			copy := *item
			state.Latest = &copy
		}
		if item.Status == "finished" {
			state.HasFinished = true
		}
		states[id] = state
	}
	return states, rows.Err()
}

type productionRenderScanner interface{ Scan(...any) error }

func scanProductionRender(scanner productionRenderScanner) (ProductionRender, error) {
	var item ProductionRender
	if err := scanner.Scan(&item.ID, &item.Conference, &item.Name, &item.JSON, &item.TemplateID, &item.TalkID, &item.CreatedAt, &item.UpdatedAt); err != nil {
		return ProductionRender{}, err
	}
	return item, nil
}
