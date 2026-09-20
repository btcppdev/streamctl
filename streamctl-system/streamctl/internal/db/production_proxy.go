package db

import (
	"database/sql"
	"fmt"
	"strings"
)

type ProductionProxyJob struct {
	ID         int64
	Source     string
	Proxy      string
	Status     string
	Attempts   int
	DurationMS int64
	Progress   int
	Stage      string
	WorkerHost string
	WorkerUnit string
	LastError  string
}

type ProductionProxyQueue struct {
	Items                             []ProductionProxyJob
	Queued, Running, Failed, Finished int
}

// ProductionProxyCounts returns proxy job totals for one conference without
// listing or scanning its remote media.
func (db *DB) ProductionProxyCounts(conference string) (ProductionProxyQueue, error) {
	prefix := strings.TrimSpace(conference) + "/recordings/"
	var counts ProductionProxyQueue
	err := db.QueryRow(`
		SELECT
			COALESCE(SUM(status = 'queued'), 0), COALESCE(SUM(status = 'running'), 0),
			COALESCE(SUM(status = 'failed'), 0), COALESCE(SUM(status = 'finished'), 0)
		FROM production_proxy_jobs
		WHERE substr(source_object_key, 1, length(?)) = ?
	`, prefix, prefix).Scan(&counts.Queued, &counts.Running, &counts.Failed, &counts.Finished)
	return counts, err
}

func (db *DB) EnqueueProductionProxyJob(source, proxy string) (ProductionProxyJob, bool, error) {
	source, proxy = strings.TrimSpace(source), strings.TrimSpace(proxy)
	if source == "" || proxy == "" {
		return ProductionProxyJob{}, false, fmt.Errorf("production proxy source and output are required")
	}
	result, err := db.Exec(`
		INSERT INTO production_proxy_jobs (source_object_key, proxy_object_key, progress_stage)
		VALUES (?, ?, 'Waiting')
		ON CONFLICT(source_object_key) DO NOTHING
	`, source, proxy)
	if err != nil {
		return ProductionProxyJob{}, false, fmt.Errorf("enqueue production proxy: %w", err)
	}
	inserted, _ := result.RowsAffected()
	if inserted == 0 {
		result, err = db.Exec(`
			UPDATE production_proxy_jobs
			SET proxy_object_key = ?, status = 'queued', duration_ms = 0, progress_percent = 0,
			    progress_stage = 'Waiting', last_error = '', finished_at = NULL,
			    updated_at = CURRENT_TIMESTAMP
			WHERE source_object_key = ? AND status IN ('failed', 'finished')
		`, proxy, source)
		if err != nil {
			return ProductionProxyJob{}, false, fmt.Errorf("requeue production proxy: %w", err)
		}
		inserted, _ = result.RowsAffected()
	}
	job, err := db.ProductionProxyJobBySource(source)
	return job, inserted > 0, err
}

func (db *DB) ProductionProxyJobBySource(source string) (ProductionProxyJob, error) {
	return scanProductionProxyJob(db.QueryRow(`
		SELECT id, source_object_key, proxy_object_key, status, attempt_count,
		       duration_ms, progress_percent, progress_stage, last_error, worker_host, worker_unit
		FROM production_proxy_jobs WHERE source_object_key = ?
	`, strings.TrimSpace(source)))
}

func (db *DB) RequeueInterruptedProductionProxyJobs() error {
	_, err := db.Exec(`
		UPDATE production_proxy_jobs
		SET status = 'queued', progress_percent = 0, progress_stage = 'Waiting',
		    last_error = 'streamctl restarted while preparing proxy', updated_at = CURRENT_TIMESTAMP
		WHERE status = 'running' AND worker_unit = ''
	`)
	return err
}

func (db *DB) ClaimProductionProxyJob() (ProductionProxyJob, error) {
	return db.ClaimProductionProxyGPUJob("")
}

func (db *DB) ClaimProductionProxyGPUJob(host string) (ProductionProxyJob, error) {
	tx, err := db.Begin()
	if err != nil {
		return ProductionProxyJob{}, err
	}
	defer tx.Rollback()
	job, err := scanProductionProxyJob(tx.QueryRow(`
		SELECT id, source_object_key, proxy_object_key, status, attempt_count,
		       duration_ms, progress_percent, progress_stage, last_error, worker_host, worker_unit
		FROM production_proxy_jobs WHERE status = 'queued' ORDER BY id LIMIT 1
	`))
	if err != nil {
		return ProductionProxyJob{}, err
	}
	unit := ""
	if host != "" {
		unit = fmt.Sprintf("streamctl-gpu-proxy-%d-attempt-%d.service", job.ID, job.Attempts+1)
	}
	result, err := tx.Exec(`
		UPDATE production_proxy_jobs
		SET status = 'running', attempt_count = attempt_count + 1, started_at = CURRENT_TIMESTAMP,
		    finished_at = NULL, progress_percent = 0, progress_stage = 'Starting',
		    last_error = '', worker_host = ?, worker_unit = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = 'queued'
	`, host, unit, job.ID)
	if err != nil {
		return ProductionProxyJob{}, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ProductionProxyJob{}, sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return ProductionProxyJob{}, err
	}
	job.WorkerHost, job.WorkerUnit = host, unit
	job.Status = "running"
	job.Attempts++
	job.Progress = 0
	job.Stage = "Starting"
	job.LastError = ""
	return job, nil
}

func (db *DB) UpdateProductionProxyJobProgress(id int64, stage string, percent int) error {
	if percent < 0 {
		percent = 0
	} else if percent > 100 {
		percent = 100
	}
	_, err := db.Exec(`
		UPDATE production_proxy_jobs
		SET progress_stage = ?, progress_percent = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = 'running'
	`, strings.TrimSpace(stage), percent, id)
	return err
}

func (db *DB) RetryProductionProxyJob(id int64) error {
	result, err := db.Exec(`
		UPDATE production_proxy_jobs
		SET status = 'queued', progress_percent = 0, progress_stage = 'Waiting',
		    last_error = '', started_at = NULL, finished_at = NULL, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = 'failed'
	`, id)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (db *DB) FinishProductionProxyJob(id, durationMS int64) error {
	_, err := db.Exec(`
		UPDATE production_proxy_jobs
		SET status = 'finished', duration_ms = ?, progress_percent = 100,
		    progress_stage = 'Complete', last_error = '', finished_at = CURRENT_TIMESTAMP,
		    updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, durationMS, id)
	return err
}

func (db *DB) FailProductionProxyJob(id int64, jobErr error) error {
	detail := "proxy preparation failed"
	if jobErr != nil {
		detail = strings.TrimSpace(jobErr.Error())
	}
	_, err := db.Exec(`
		UPDATE production_proxy_jobs
		SET status = 'failed', progress_stage = 'Failed', last_error = ?,
		    finished_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, detail, id)
	return err
}

func (db *DB) ProductionProxyQueue(limit int) (ProductionProxyQueue, error) {
	if limit <= 0 {
		limit = 100
	}
	var queue ProductionProxyQueue
	if err := db.QueryRow(`
		SELECT
			COALESCE(SUM(status = 'queued'), 0), COALESCE(SUM(status = 'running'), 0),
			COALESCE(SUM(status = 'failed'), 0), COALESCE(SUM(status = 'finished'), 0)
		FROM production_proxy_jobs
	`).Scan(&queue.Queued, &queue.Running, &queue.Failed, &queue.Finished); err != nil {
		return queue, err
	}
	rows, err := db.Query(`
		SELECT id, source_object_key, proxy_object_key, status, attempt_count,
		       duration_ms, progress_percent, progress_stage, last_error, worker_host, worker_unit
		FROM production_proxy_jobs
		WHERE status IN ('queued', 'running', 'failed')
		ORDER BY CASE status WHEN 'running' THEN 0 WHEN 'queued' THEN 1 WHEN 'failed' THEN 2 ELSE 3 END, id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return queue, err
	}
	defer rows.Close()
	for rows.Next() {
		job, err := scanProductionProxyJob(rows)
		if err != nil {
			return queue, err
		}
		queue.Items = append(queue.Items, job)
	}
	return queue, rows.Err()
}

type productionProxyScanner interface {
	Scan(...any) error
}

func scanProductionProxyJob(scanner productionProxyScanner) (ProductionProxyJob, error) {
	var job ProductionProxyJob
	err := scanner.Scan(&job.ID, &job.Source, &job.Proxy, &job.Status, &job.Attempts,
		&job.DurationMS, &job.Progress, &job.Stage, &job.LastError, &job.WorkerHost, &job.WorkerUnit)
	return job, err
}

// Apply worker updates only to the attempt that is still running. A late result
// from a lost worker must never complete a newer attempt.
func (db *DB) UpdateProductionProxyAttempt(job ProductionProxyJob, status, stage string, progress int, duration int64, detail string) error {
	if status != "running" && status != "finished" && status != "failed" && status != "queued" {
		return fmt.Errorf("invalid proxy attempt status %q", status)
	}
	progress = max(0, min(100, progress))
	result, err := db.Exec(`UPDATE production_proxy_jobs
		SET status = ?, progress_stage = ?, progress_percent = ?, duration_ms = ?, last_error = ?,
		    finished_at = CASE WHEN ? IN ('finished', 'failed') THEN CURRENT_TIMESTAMP ELSE NULL END,
		    updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = 'running' AND worker_unit = ? AND attempt_count = ?`,
		status, stage, progress, duration, detail, status, job.ID, job.WorkerUnit, job.Attempts)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (db *DB) RunningProductionProxyJobs() ([]ProductionProxyJob, error) {
	rows, err := db.Query(`SELECT id, source_object_key, proxy_object_key, status, attempt_count,
		duration_ms, progress_percent, progress_stage, last_error, worker_host, worker_unit
		FROM production_proxy_jobs WHERE status = 'running' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []ProductionProxyJob
	for rows.Next() {
		job, err := scanProductionProxyJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}
