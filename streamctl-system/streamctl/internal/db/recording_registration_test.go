package db

import "testing"

func TestRecordingRegistrationMigration(t *testing.T) {
	db := productionTestDB(t)
	manifest := `{"version":1,"jobs":[{"id":"talk","segments":[]}]}`
	var fullTalkID int64
	for _, kind := range []string{"streamctl.talkCuts", "streamctl.talkCard"} {
		templateID, err := db.CreateProductionTemplate("toronto", kind, `{"segments":[{"type":"`+kind+`"}]}`)
		if err != nil {
			t.Fatal(err)
		}
		id, _, err := db.CreateProductionRender("toronto", kind, manifest, &templateID, "talk-1")
		if err != nil {
			t.Fatal(err)
		}
		if kind == "streamctl.talkCuts" {
			fullTalkID = id
		}
		job, err := db.EnqueueProductionRender(id, kind, manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.MarkRenderQueueFinished(job.ID, "finished", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`ALTER TABLE production_renders DROP COLUMN recording_kind`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	if pending, err := db.PendingRecordingRegistrations(); err != nil || len(pending) != 0 {
		t.Fatalf("retroactive registration: %v %v", pending, err)
	}
	var designated int
	if err := db.QueryRow(`SELECT COUNT(*) FROM production_renders WHERE recording_kind = 'full_talk'`).Scan(&designated); err != nil || designated != 1 {
		t.Fatalf("designation=%d error=%v", designated, err)
	}
	job, err := db.EnqueueProductionRender(fullTalkID, "Talk", manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkRenderQueueFinished(job.ID, "finished", ""); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingRecordingRegistrations()
	if err != nil || len(pending) != 1 || pending[0].QueueID != job.ID {
		t.Fatalf("new submission: %+v %v", pending, err)
	}
}

func TestRecordingRegistrationLifecycle(t *testing.T) {
	db := productionTestDB(t)
	templateID, err := db.CreateProductionTemplate("toronto", "Talk", `{"segments":[{"type":"streamctl.talkCuts"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	manifest := `{"version":1,"jobs":[{"id":"talk","segments":[{"type":"video","src":"toronto/raw.mp4"}]}]}`
	renderID, _, err := db.CreateProductionRender("toronto", "Talk", manifest, &templateID, "talk-1", "full_talk")
	if err != nil {
		t.Fatal(err)
	}
	first, err := db.EnqueueProductionRender(renderID, "Talk", manifest)
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := db.PendingRecordingRegistrations(); err != nil || len(pending) != 0 {
		t.Fatalf("queued jobs eligible: %v %v", pending, err)
	}
	if err := db.MarkRenderQueueFinished(first.ID, "finished", ""); err != nil {
		t.Fatal(err)
	}
	check := func(want int64) {
		t.Helper()
		items, err := db.PendingRecordingRegistrations()
		if err != nil {
			t.Fatal(err)
		}
		if want == 0 {
			if len(items) != 0 {
				t.Fatalf("unexpected pending: %v", items)
			}
			return
		}
		if len(items) != 1 || items[0].QueueID != want || items[0].TalkID != "talk-1" || items[0].Conference != "toronto" || items[0].Manifest != manifest {
			t.Fatalf("pending: %+v", items)
		}
	}
	check(first.ID)
	if err := db.FinishRecordingRegistration(first.ID, "API unavailable"); err != nil {
		t.Fatal(err)
	}
	check(0)
	if label, detail, err := db.RecordingRegistrationStatus(first.ID); err != nil || detail != "API unavailable" || label != "Recording registration retrying" {
		t.Fatalf("status: %s %s %v", label, detail, err)
	}
	if _, err := db.Exec(`UPDATE recording_registrations SET next_attempt_at = datetime('now', '-1 minute')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	check(first.ID) // Survives migrations/restarts; retry only registration.
	second, err := db.EnqueueProductionRender(renderID, "Talk", manifest)
	if err != nil {
		t.Fatal(err)
	}
	check(first.ID) // Queuing a replacement does not suppress the finished job.
	if err := db.MarkRenderQueueFinished(second.ID, "finished", ""); err != nil {
		t.Fatal(err)
	}
	check(second.ID) // The older failed registration must never overwrite this.
	if err := db.ArchiveProductionRender(renderID, "toronto"); err != nil {
		t.Fatal(err)
	}
	check(second.ID) // Target survives deleting the editable render.
	if err := db.FinishRecordingRegistration(second.ID, ""); err != nil {
		t.Fatal(err)
	}
	check(0)
	if label, _, err := db.RecordingRegistrationStatus(second.ID); err != nil || label != "Recording registered" {
		t.Fatalf("status: %s %v", label, err)
	}
	// Association alone (e.g. a future clip) does not designate a full recording.
	clipID, _, err := db.CreateProductionRender("toronto", "Clip", manifest, nil, "talk-1")
	if err != nil {
		t.Fatal(err)
	}
	clip, err := db.EnqueueProductionRender(clipID, "Clip", manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkRenderQueueFinished(clip.ID, "finished", ""); err != nil {
		t.Fatal(err)
	}
	check(0)
}
