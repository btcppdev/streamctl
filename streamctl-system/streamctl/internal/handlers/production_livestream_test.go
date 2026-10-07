package handlers

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"streamctl/internal/btcppclient"
	"streamctl/internal/db"
)

func livestreamCandidate(id, start, stage string) productionTalkView {
	return productionTalkView{Candidate: btcppclient.Candidate{TalkID: id, Title: id, StartsAt: stringPointer(start), Venue: stage, SocialCard: "toronto/talks/" + id + ".png"}}
}

func TestAssembleLivestreamScheduleAndOmissions(t *testing.T) {
	talks := []productionTalkView{
		livestreamCandidate("b", "2026-07-22T10:00:00-04:00", "one"),
		livestreamCandidate("later", "2026-07-22T11:00:00-04:00", "main"),
		livestreamCandidate("a", "2026-07-22T14:00:00Z", "Main"),
		livestreamCandidate("uncut", "2026-07-22T12:00:00-04:00", "1"),
		livestreamCandidate("bad-card", "2026-07-22T13:00:00-04:00", "one"),
		livestreamCandidate("other-stage", "2026-07-22T10:00:00-04:00", "two"),
		livestreamCandidate("other-day", "2026-07-23T10:00:00-04:00", "one"),
	}
	talks[4].SocialCard = "https://example.com/card.png"
	decorateProductionTalks(talks, stringPointer("2026-07-22T09:00:00-04:00"))
	cut := db.ProductionCut{SourceType: "video", Source: "toronto/recordings/raw/talk.mp4", InMS: 1000, OutMS: 2000}
	cuts := map[string][]db.ProductionCut{"a": {cut, {SourceType: "chunkedVideo", Source: "toronto/recordings/raw/part0000.mp4", InMS: 3000, OutMS: 4000}}, "b": {cut}, "later": {cut}, "bad-card": {cut}, "other-stage": {cut}, "other-day": {cut}}
	name, segments, omitted, err := assembleLivestream(talks, cuts, "toronto", "2026-07-22", "Main")
	if err != nil || name != "Day 1 — Main livestream" || len(segments) != 7 || len(omitted) != 2 {
		t.Fatalf("name=%s segments=%s omitted=%+v err=%v", name, segments, omitted, err)
	}
	for i, want := range map[int]string{0: "toronto/talks/a.png", 1: "toronto/recordings/raw/talk.mp4", 2: "toronto/recordings/raw/part0000.mp4", 3: "toronto/talks/b.png", 5: "toronto/talks/later.png"} {
		if !strings.Contains(string(segments[i]), want) {
			t.Fatalf("segment %d=%s wanted %s", i, segments[i], want)
		}
	}
	if !strings.Contains(string(segments[2]), "00:00:03.000") || omitted[0].Reason != "No saved cuts" || omitted[1].Reason != "Missing or invalid saved title card" {
		t.Fatalf("segments=%s omitted=%+v", segments, omitted)
	}
	if _, _, _, err := assembleLivestream(talks, cuts, "toronto", "2026-07-23", "Talks"); err == nil {
		t.Fatal("accepted nonexistent day/stage pair")
	}
	choices := livestreamChoices(talks)
	if len(choices) != 2 || len(choices[0].Stages) != 2 || len(choices[1].Stages) != 1 {
		t.Fatalf("choices=%+v", choices)
	}
}

func TestLivestreamCreateEditQueueSnapshot(t *testing.T) {
	database := productionHandlerTestDB(t)
	talk := livestreamCandidate("talk", "2026-07-22T10:00:00-04:00", "one")
	uncut := livestreamCandidate("uncut", "2026-07-22T11:00:00-04:00", "one")
	cut := db.ProductionCut{SourceType: "video", Source: "toronto/recordings/raw/talk.mp4", InMS: 1000, OutMS: 2000}
	if err := database.ReplaceProductionCuts("toronto", "talk", []db.ProductionCut{cut}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: database, funcs: template.FuncMap{}, BTCPP: productionCandidatesStub{conferences: []btcppclient.Conference{{Tag: "toronto", StartsAt: stringPointer("2026-07-22T09:00:00-04:00")}}, candidates: []btcppclient.Candidate{talk.Candidate, uncut.Candidate}}}
	form := url.Values{"conference": {"toronto"}, "day": {"2026-07-22"}, "stage": {"Main"}}
	create := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/production/renders/create-livestream", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.productionRenderCreateLivestream(w, r)
		return w
	}
	w := create()
	var result struct {
		URL     string                         `json:"url"`
		Omitted []productionLivestreamOmission `json:"omitted"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || len(result.Omitted) != 1 || result.URL == "" {
		t.Fatalf("status=%d body=%s err=%v", w.Code, w.Body.String(), err)
	}
	items, err := database.ListProductionRenders("toronto")
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	item := items[0]
	if item.TemplateID.Valid || item.TalkID != "" || strings.Contains(item.JSON, "streamctl.") {
		t.Fatalf("unexpected association/dynamic block: %+v", item)
	}
	var kind string
	if err := database.QueryRow("SELECT recording_kind FROM production_renders WHERE id = ?", item.ID).Scan(&kind); err != nil || kind != "" {
		t.Fatalf("kind=%q err=%v", kind, err)
	}
	if err := database.ReplaceProductionCuts("toronto", "talk", nil); err != nil {
		t.Fatal(err)
	}
	saved, err := database.ProductionRender(item.ID, "toronto")
	if err != nil || saved.JSON != item.JSON {
		t.Fatalf("snapshot changed: %+v %v", saved, err)
	}
	w = httptest.NewRecorder()
	h.productionRenderEdit(w, httptest.NewRequest(http.MethodGet, result.URL, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Skipped ${items.length}") {
		t.Fatalf("editor status=%d body=%s", w.Code, w.Body.String())
	}
	queue := url.Values{"conference": {"toronto"}, "ids": {strconv.FormatInt(item.ID, 10)}}
	r := httptest.NewRequest(http.MethodPost, "/production/renders/queue", strings.NewReader(queue.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.productionRendersQueue(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("queue status=%d body=%s", w.Code, w.Body.String())
	}
	// An independent creation produces a fresh snapshot rather than overwriting.
	if err := database.ReplaceProductionCuts("toronto", "talk", []db.ProductionCut{cut}); err != nil {
		t.Fatal(err)
	}
	if w = create(); w.Code != 200 {
		t.Fatalf("second create=%s", w.Body.String())
	}
	items, _ = database.ListProductionRenders("toronto")
	if len(items) != 2 {
		t.Fatalf("renders=%+v", items)
	}
}

func TestLivestreamFailuresCreateNothing(t *testing.T) {
	for _, tc := range []struct {
		name, day, stage string
		apiErr           error
		candidates       []btcppclient.Candidate
		status           int
	}{
		{name: "all skipped", day: "2026-07-22", stage: "Main", candidates: []btcppclient.Candidate{livestreamCandidate("uncut", "2026-07-22T10:00:00-04:00", "one").Candidate}, status: 400},
		{name: "invalid pair", day: "2026-07-22", stage: "Talks", candidates: []btcppclient.Candidate{livestreamCandidate("talk", "2026-07-22T10:00:00-04:00", "one").Candidate}, status: 400},
		{name: "API failure", day: "2026-07-22", stage: "Main", apiErr: errors.New("schedule unavailable"), status: 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := productionHandlerTestDB(t)
			h := &Handler{DB: database, BTCPP: productionCandidatesStub{err: tc.apiErr, candidates: tc.candidates}}
			form := url.Values{"conference": {"toronto"}, "day": {tc.day}, "stage": {tc.stage}}
			r := httptest.NewRequest(http.MethodPost, "/production/renders/create-livestream", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			h.productionRenderCreateLivestream(w, r)
			items, _ := database.ListProductionRenders("toronto")
			if w.Code != tc.status || len(items) != 0 || !strings.Contains(w.Body.String(), `"error"`) {
				t.Fatalf("status=%d items=%+v body=%s", w.Code, items, w.Body.String())
			}
		})
	}
}

func TestLivestreamOptionsAndDialog(t *testing.T) {
	database := productionHandlerTestDB(t)
	h := &Handler{DB: database, funcs: template.FuncMap{}, BTCPP: productionCandidatesStub{candidates: []btcppclient.Candidate{livestreamCandidate("talk", "2026-07-22T10:00:00-04:00", "one").Candidate, {TalkID: "unscheduled", Venue: "two"}}}}
	w := httptest.NewRecorder()
	h.productionLivestreamOptions(w, httptest.NewRequest(http.MethodGet, "/production/renders/livestream-options?conference=toronto", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"stages":["Main"]`) || strings.Contains(w.Body.String(), "Talks") {
		t.Fatalf("options=%s", w.Body.String())
	}
	w = httptest.NewRecorder()
	h.productionRenders(w, httptest.NewRequest(http.MethodGet, "/production/renders?conference=toronto", nil))
	for _, want := range []string{">Livestream<", "livestream-dialog", "name=\"day\"", "name=\"stage\"", "create-livestream", "showModal()", "day.onchange"} {
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("missing %s status=%d body=%s", want, w.Code, w.Body.String())
		}
	}
	mux := http.NewServeMux()
	h.Register(mux)
	for _, path := range []string{"/production/renders/livestream-options?conference=toronto", "/production/renders/create-livestream"} {
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/login") {
			t.Fatalf("unprotected %s: %d", path, w.Code)
		}
	}
}

func TestLivestreamMutationRequiresCSRFAndPOST(t *testing.T) {
	h := &Handler{DB: productionHandlerTestDB(t), Secret: "test-secret"}
	mux := http.NewServeMux()
	h.Register(mux)
	for _, tc := range []struct {
		method, token string
		status        int
	}{
		{http.MethodGet, "", http.StatusMethodNotAllowed},
		{http.MethodPost, "wrong", http.StatusForbidden},
	} {
		form := url.Values{"conference": {"toronto"}, "csrf_token": {tc.token}}
		r := httptest.NewRequest(tc.method, "/production/renders/create-livestream", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: h.Secret})
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s status=%d body=%s", tc.method, w.Code, w.Body.String())
		}
	}
}
