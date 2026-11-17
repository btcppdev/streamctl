package db

import (
	"context"
	"database/sql"
	"time"
)

type GPUWatchdogActivity struct {
	Open        int
	LastSuccess time.Time
}

func (db *DB) GPUWatchdogActivity(ctx context.Context) (GPUWatchdogActivity, error) {
	var a GPUWatchdogActivity
	var last sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT
		coalesce(sum(CASE WHEN status IN ('queued', 'running') THEN 1 ELSE 0 END), 0),
		max(CASE WHEN status = 'finished' THEN unixepoch(finished_at) END)
		FROM (
		 SELECT status, finished_at FROM render_job_queue
		 UNION ALL SELECT status, finished_at FROM production_proxy_jobs
		 UNION ALL SELECT status, finished_at FROM gpu_job_queue
		)`).Scan(&a.Open, &last)
	if last.Valid {
		a.LastSuccess = time.Unix(last.Int64, 0).UTC()
	}
	return a, err
}

func (db *DB) FailWatchdogGPUJobs(ctx context.Context, reason string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"render_job_queue", "production_proxy_jobs", "gpu_job_queue"} {
		if _, err := tx.ExecContext(ctx, "UPDATE "+table+" SET status = 'failed', last_error = ?, finished_at = CURRENT_TIMESTAMP WHERE status = 'running'", "Spending watchdog: "+reason); err != nil {
			return err
		}
	}
	return tx.Commit()
}
