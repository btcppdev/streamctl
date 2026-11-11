package db

// WorkerJob presents render and editing-media jobs together without changing
// their separate execution lifecycles or IDs.
type WorkerJob struct {
	ID                                             int64
	Kind, Name, Status, UnitName, Stage, LastError string
	Attempts, Progress                             int
	CreatedAt, StartedAt, FinishedAt               string
}

const workerJobsSQL = `
 SELECT id, 'render' AS kind, name, status, unit_name, '' AS stage, last_error,
 attempt_count, 0 AS progress, CAST(created_at AS TEXT) AS created_at,
 COALESCE(CAST(started_at AS TEXT), '') AS started_at,
 COALESCE(CAST(finished_at AS TEXT), '') AS finished_at,
 COALESCE(finished_at, started_at, created_at) AS activity
 FROM render_job_queue
 UNION ALL
 SELECT id, 'media', source_object_key, status, worker_unit, progress_stage, last_error,
 attempt_count, progress_percent, CAST(created_at AS TEXT),
 COALESCE(CAST(started_at AS TEXT), ''), COALESCE(CAST(finished_at AS TEXT), ''),
 COALESCE(finished_at, started_at, created_at)
 FROM production_proxy_jobs`

// scope is active, recent (terminal only), or history. Active work is never
// truncated by a history limit. Terminal jobs use completion time, not status.
func (db *DB) WorkerJobs(scope string, limit, offset int) ([]WorkerJob, error) {
	query := `SELECT id, kind, name, status, unit_name, stage, last_error, attempt_count, progress, created_at, started_at, finished_at FROM (` + workerJobsSQL + `) WHERE `
	switch scope {
	case "active":
		query += `status IN ('queued', 'running') ORDER BY CASE status WHEN 'running' THEN 0 ELSE 1 END, created_at, kind, id`
	case "recent":
		query += `status NOT IN ('queued', 'running') ORDER BY activity DESC, kind, id DESC LIMIT ? OFFSET ?`
	default:
		query += `1=1 ORDER BY activity DESC, kind, id DESC LIMIT ? OFFSET ?`
	}
	var args []any
	if scope != "active" {
		args = []any{max(1, min(limit, 101)), max(0, offset)}
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []WorkerJob{}
	for rows.Next() {
		var j WorkerJob
		if err := rows.Scan(&j.ID, &j.Kind, &j.Name, &j.Status, &j.UnitName, &j.Stage, &j.LastError, &j.Attempts, &j.Progress, &j.CreatedAt, &j.StartedAt, &j.FinishedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}
