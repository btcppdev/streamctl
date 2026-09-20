package handlers

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"streamctl/internal/btcppclient"
	"streamctl/internal/db"
)

type productionCandidatesStub struct {
	conferences []btcppclient.Conference
	candidates  []btcppclient.Candidate
	err         error
}

func (stub productionCandidatesStub) Conferences(context.Context) ([]btcppclient.Conference, error) {
	return stub.conferences, stub.err
}

func (stub productionCandidatesStub) RecordingCandidates(context.Context, string) ([]btcppclient.Candidate, error) {
	return stub.candidates, stub.err
}

func productionHandlerTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	return database
}

func TestProductionWorkspaceLoadsCandidatesAndCuts(t *testing.T) {
	database := productionHandlerTestDB(t)
	if err := database.ReplaceProductionCuts("toronto", "talk-1", []db.ProductionCut{{Source: "toronto/recordings/raw/mix/day-1.mp4", SourceType: "video", InMS: 1000, OutMS: 2000}}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{
		DB: database, funcs: template.FuncMap{},
		BTCPP: productionCandidatesStub{conferences: []btcppclient.Conference{{Tag: "toronto", Description: "Toronto++", StartsAt: stringPointer("2026-07-22T09:00:00-04:00")}}, candidates: []btcppclient.Candidate{{
			TalkID: "talk-1", Title: "A useful talk", Venue: "one", Eligible: true,
			StartsAt: stringPointer("2026-07-22T10:00:00-04:00"), EndsAt: stringPointer("2026-07-22T10:30:00-04:00"),
		}}},
	}
	response := httptest.NewRecorder()
	h.productionTimestamp(response, httptest.NewRequest(http.MethodGet, "/production/timestamp?conference=toronto", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{"Timestamp talks", "Conference workspace", `value="toronto"`, "selected", `onchange="this.form.submit()"`, `/production/timestamp?conference=toronto`, "A useful talk", "Day 1", "Main", "Wed, Jul 22, 2026", "10:00 AM–10:30 AM", "1 cut"} {
		if !strings.Contains(body, want) {
			t.Fatalf("workspace omitted %q: %s", want, body)
		}
	}
	if !strings.Contains(response.Header().Get("Set-Cookie"), productionConferenceCookie+"=toronto") {
		t.Fatalf("conference preference was not persisted: %q", response.Header().Get("Set-Cookie"))
	}
	if strings.Contains(body, ">Open</button>") {
		t.Fatalf("timestamp selector retained redundant open button: %s", body)
	}
}

func TestProductionTalkScheduleLabels(t *testing.T) {
	talks := []productionTalkView{
		{Candidate: btcppclient.Candidate{Venue: "1", StartsAt: stringPointer("2026-07-22T10:00:00-04:00")}},
		{Candidate: btcppclient.Candidate{Venue: "stage two", StartsAt: stringPointer("2026-07-23T11:00:00-04:00")}},
		{Candidate: btcppclient.Candidate{Venue: "three", StartsAt: stringPointer("2026-07-24T12:00:00-04:00")}},
	}
	decorateProductionTalks(talks, stringPointer("2026-07-22T09:00:00-04:00"))
	for i, want := range []struct{ day, stage string }{{"Day 1", "Main"}, {"Day 2", "Talks"}, {"Day 3", "Workshop"}} {
		if talks[i].DayLabel != want.day || talks[i].StageLabel != want.stage {
			t.Fatalf("talk %d labels = %q, %q; want %q, %q", i, talks[i].DayLabel, talks[i].StageLabel, want.day, want.stage)
		}
	}
	groups := groupProductionTalks(talks)
	if len(groups) != 3 || groups[1].DayLabel != "Day 2" || groups[1].DateLabel != "Thu, Jul 23, 2026" || len(groups[1].Stages) != 1 || len(groups[1].Stages[0].Talks) != 1 {
		t.Fatalf("unexpected talk groups: %+v", groups)
	}
}

func TestProductionTalksFollowDayStageTimeOrder(t *testing.T) {
	h := &Handler{BTCPP: productionCandidatesStub{candidates: []btcppclient.Candidate{
		{TalkID: "talks-early", Venue: "two", StartsAt: stringPointer("2026-07-22T09:00:00-04:00")},
		{TalkID: "main-late", Venue: "one", StartsAt: stringPointer("2026-07-22T11:00:00-04:00")},
		{TalkID: "main-early", Venue: "one", StartsAt: stringPointer("2026-07-22T10:00:00-04:00")},
		{TalkID: "next-day", Venue: "one", StartsAt: stringPointer("2026-07-23T08:00:00-04:00")},
	}}}
	talks, err := h.productionTalks(context.Background(), "toronto")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"main-early", "main-late", "talks-early", "next-day"}
	for i := range want {
		if talks[i].TalkID != want[i] {
			t.Fatalf("talk %d = %q; want %q (all=%+v)", i, talks[i].TalkID, want[i], talks)
		}
	}
}

func TestProductionTimestampStartsWithConferenceDropdown(t *testing.T) {
	h := &Handler{funcs: template.FuncMap{}, BTCPP: productionCandidatesStub{conferences: []btcppclient.Conference{{Tag: "toronto", Description: "Toronto++"}}}}
	response := httptest.NewRecorder()
	h.productionTimestamp(response, httptest.NewRequest(http.MethodGet, "/production/timestamp", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{"<select", "Select a conference", `value="toronto"`, "Choose a conference"} {
		if !strings.Contains(body, want) {
			t.Fatalf("timestamp selector omitted %q: %s", want, body)
		}
	}
}

func TestProductionOverviewKeepsConferenceInNavigation(t *testing.T) {
	database := productionHandlerTestDB(t)
	if err := database.ReplaceProductionCuts("toronto", "talk-1", []db.ProductionCut{{Source: "toronto/recordings/raw/mix/main.mp4", SourceType: "video", InMS: 1000, OutMS: 2000}}); err != nil {
		t.Fatal(err)
	}
	finished, _, err := database.EnqueueProductionProxyJob("toronto/recordings/raw/mix/main.mp4", "toronto/recordings/workspace/raw/mix/main.proxy.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ClaimProductionProxyJob(); err != nil {
		t.Fatal(err)
	}
	if err := database.FinishProductionProxyJob(finished.ID, 1000); err != nil {
		t.Fatal(err)
	}
	failed, _, err := database.EnqueueProductionProxyJob("toronto/recordings/raw/mix/talks.mp4", "toronto/recordings/workspace/raw/mix/talks.proxy.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ClaimProductionProxyJob(); err != nil {
		t.Fatal(err)
	}
	if err := database.FailProductionProxyJob(failed.ID, context.DeadlineExceeded); err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: database, funcs: template.FuncMap{}, BTCPPBaseURL: "https://btcpp.dev", BTCPP: productionCandidatesStub{
		conferences: []btcppclient.Conference{{Tag: "toronto", Description: "Toronto++"}},
		candidates: []btcppclient.Candidate{
			{TalkID: "talk-1", Recording: &btcppclient.Recording{ID: "recording-1", PublishedAt: stringPointer("2026-08-01T12:00:00Z")}},
			{TalkID: "talk-2", Recording: &btcppclient.Recording{ID: "recording-2", PublishedAt: stringPointer("2026-08-02T12:00:00Z")}},
		},
	}}
	response := httptest.NewRecorder()
	h.productionHome(response, httptest.NewRequest(http.MethodGet, "/production?conference=toronto", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{"Conference workspace", `value="toronto" selected`, `/production/timestamp?conference=toronto`, "Production overview", "Media preparation", "Browse and prepare media", "1 / 2", "1 ready", "1 failed", "Ready to render", "Published recordings", ">2</div><div class=\"overview-detail\">published for this conference", "https://btcpp.dev/toronto/admin/recordings", "createProductionMediaBrowser", "mode:'manage'", "allowBucketRoot:true", "context.path===overviewRoot", `name="conference" value="toronto"`, `querySelector('#media-action-form input[name="conference"]').value`} {
		if !strings.Contains(body, want) {
			t.Fatalf("overview omitted %q: %s", want, body)
		}
	}
	if strings.Contains(body, `>Media</a>`) || strings.Contains(body, `/production/media?`) {
		t.Fatalf("overview retained standalone media page navigation: %s", body)
	}
}

func TestProductionCutIsDedicatedPageWithTalkNavigation(t *testing.T) {
	database := productionHandlerTestDB(t)
	if err := database.ReplaceProductionCuts("toronto", "talk-2", []db.ProductionCut{{Source: "toronto/recordings/main.mp4", SourceType: "video", InMS: 1000, OutMS: 2000}}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: database, funcs: template.FuncMap{}, BTCPP: productionCandidatesStub{candidates: []btcppclient.Candidate{
		{TalkID: "talk-1", Title: "First", Venue: "two", StartsAt: stringPointer("2025-01-01T10:00:00Z")},
		{TalkID: "talk-2", Title: "Second", Venue: "two", StartsAt: stringPointer("2025-01-01T11:00:00Z"), EndsAt: stringPointer("2025-01-01T11:30:00Z"), Speakers: []btcppclient.Speaker{{Name: "Speaker Two"}}},
		{TalkID: "talk-3", Title: "Third", Venue: "two", StartsAt: stringPointer("2025-01-01T12:00:00Z")},
	}}}
	response := httptest.NewRecorder()
	h.productionCut(response, httptest.NewRequest(http.MethodGet, "/production/timestamp/cut?conference=toronto&talk_id=talk-2", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{"Second", "Day 1", "Wed, Jan 1, 2025", "Talks", "11:00 AM–11:30 AM", "Speaker Two", "← Previous", "Skip →", "Save &amp; next →", `"inMs":1000`, `"index":1`, `"talks":[`, "Back to talks", "cutter-head", "position:sticky", "talk-meta-separator", "cutter-workspace", "grid-template-columns:minmax(0,2fr) minmax(360px,1fr)", "cutter-side", "media-preview-seek", "/production/media/info", "proxyPath", `preload="auto"`, "seekGlobal", "bindVideo", "Loading editing proxy", "sourceFilename", "selectRange", "selectRange(index,'inMs')", "selectRange(index,'outMs')", "inheritPreviewSource", "range-source", "openMedia(index)", "requestSubmit", "settledTimeMs", "initialSeekPending", "loadSource", "showTalk", "history.pushState", "currentTalk.talk_id", "Wait for the preview to finish seeking", "preview.ready", "rangeSetKey", "range[rangeSetKey(key)]=true", "if(!range){range=newRange(currentSource,currentSourceType)", "media-preview-fine", "media-preview-fine-row", "media-preview-play", "togglePlayback", "onMark:key=>mark", "fineWindowMs=5000", "seekNoticeMs=4000", "mediaSession", "queueSeek", "setPointerCapture", "seekInFlight", "queuedSeekTarget", "recover", "node.onseeked", "seek.onpointerdown", "finishCoarse", "requestAnimationFrame", "createProductionMediaBrowser", "createProductionMediaPreview", "media-preview-mark-in", "preview.setMarks", "removeRange", "addRange", "+ New segment", "needsMedia=!inheritPreviewSource(range)", "if(needsMedia)openMedia(activeRange)", "if(!ranges.length&&currentSource)ranges.push(newRange(currentSource,currentSourceType))", "activeRange=-1", "inSet", "outSet", "file.proxyStatus==='finished'", "No prepared video files here", "maxCachedVideos=2", "preview.playing()", "Save & next →':'Save & close", "function talksURL()", "else location.href=talksURL()"} {
		if !strings.Contains(body, want) {
			t.Fatalf("cutter omitted %q: %s", want, body)
		}
	}
	for _, want := range []string{"grid-template-columns:auto minmax(0,1fr) auto", "@media(max-width:900px)", "position:fixed", "env(safe-area-inset-bottom)"} {
		if !strings.Contains(body, want) {
			t.Fatalf("cutter omitted responsive navigation %q: %s", want, body)
		}
	}
	for _, want := range []string{"media-preview-current", "Current timestamp", "parseTimecode", "currentLabel.onchange", "currentLabel.select()"} {
		if !strings.Contains(body, want) {
			t.Fatalf("cutter omitted editable timestamp behavior %q: %s", want, body)
		}
	}
	if !strings.Contains(body, "resumeAfterSeek=drag.moved&&!cancelled?false:drag.wasPlaying") {
		t.Fatalf("fine ribbon does not pause playback after scrubbing: %s", body)
	}
	if !strings.Contains(body, "cancelled||!drag.moved?drag.startMs:drag.targetMs") {
		t.Fatalf("fine ribbon retained click-to-seek behavior: %s", body)
	}
	if navigation, editor := strings.Index(body, `class="save-navigation"`), strings.Index(body, `class="editor"`); navigation < 0 || editor < 0 || navigation > editor {
		t.Fatalf("cutter navigation is not inside the sticky header: %s", body)
	}
	if count := strings.Count(body, ">Choose media</button>"); count != 1 {
		t.Fatalf("cutter rendered %d media selectors; want one: %s", count, body)
	}
	if strings.Contains(body, `id="source-pick"`) {
		t.Fatalf("cutter retained the player-level media selector: %s", body)
	}
	if strings.Contains(body, "media-preview-jump") {
		t.Fatalf("cutter retained the In/Out jump controls: %s", body)
	}
	if strings.Contains(body, "Select a talk") {
		t.Fatalf("dedicated cutter retained placeholder: %s", body)
	}
	if strings.Contains(body, "location.assign") {
		t.Fatalf("cutter still reloads between talks: %s", body)
	}
	if strings.Contains(body, "editor-toolbar") {
		t.Fatalf("cutter retained controls outside the shared preview card: %s", body)
	}
	if strings.Contains(body, "range[key+'Set']") {
		t.Fatalf("cutter writes the nonexistent inMsSet/outMsSet flags: %s", body)
	}
	for _, unwanted := range []string{"next-chunk", "/production/media/preview", "Timed out waiting for video", "locateChunk", "loadChunk", "pendingSeek", "#t=", "sessionStorage", "savedState"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("direct cutter retained %q: %s", unwanted, body)
		}
	}
}

func stringPointer(value string) *string { return &value }

func TestSelectedProductionConferenceFallsBackToCookie(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/production", nil)
	request.AddCookie(&http.Cookie{Name: productionConferenceCookie, Value: "toronto"})
	if got := selectedProductionConference(request); got != "toronto" {
		t.Fatalf("conference=%q", got)
	}
	request = httptest.NewRequest(http.MethodGet, "/production?conference=nairobi", nil)
	request.AddCookie(&http.Cookie{Name: productionConferenceCookie, Value: "toronto"})
	if got := selectedProductionConference(request); got != "nairobi" {
		t.Fatalf("query did not override cookie: %q", got)
	}
}

func TestMediaBrowserCanNavigateWholeBucket(t *testing.T) {
	binDir := t.TempDir()
	rclone := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(rclone, []byte("#!/bin/sh\nprintf 'toronto/\\nnairobi/\\nshared.mp4\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := &Handler{Remote: "btcpp:btcpp"}
	response := httptest.NewRecorder()
	h.mediaBrowse(response, httptest.NewRequest(http.MethodGet, "/production/media/browse?conference=toronto&prefix=", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, want := range []string{`"prefix":""`, `"path":"toronto/"`, `"path":"nairobi/"`, `"path":"shared.mp4"`} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("bucket browser omitted %q: %s", want, response.Body.String())
		}
	}
}

func TestMediaBrowserDefaultsToConferenceRecordingsAndHidesWorkspace(t *testing.T) {
	binDir := t.TempDir()
	rclone := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(rclone, []byte("#!/bin/sh\nprintf 'assets/\\nraw/\\nworkspace/\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := &Handler{Remote: "btcpp:btcpp"}
	response := httptest.NewRecorder()
	h.mediaBrowse(response, httptest.NewRequest(http.MethodGet, "/production/media/browse?conference=toronto", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"prefix":"toronto/recordings/"`) || !strings.Contains(response.Body.String(), `"path":"toronto/recordings/raw/"`) || strings.Contains(response.Body.String(), `"path":"toronto/recordings/workspace/"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestMediaBrowserRejectsWorkspacePrefix(t *testing.T) {
	h := &Handler{Remote: "btcpp:btcpp"}
	response := httptest.NewRecorder()
	h.mediaBrowse(response, httptest.NewRequest(http.MethodGet, "/production/media/browse?conference=toronto&prefix=toronto/recordings/workspace/", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestMediaOpenDoesNotCacheSignedRedirect(t *testing.T) {
	binDir := t.TempDir()
	rclone := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(rclone, []byte("#!/bin/sh\nprintf 'https://spaces.example/proxy.mp4?signature=test\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := &Handler{Remote: "btcpp:btcpp"}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/production/media/open?conference=toronto&path=toronto/recordings/workspace/raw/mix/talk.proxy.mp4", nil)

	h.mediaOpen(response, request)

	if response.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got, want := response.Header().Get("Location"), "https://spaces.example/proxy.mp4?signature=test"; got != want {
		t.Fatalf("Location=%q want %q", got, want)
	}
	if got, want := response.Header().Get("Cache-Control"), "private, no-store"; got != want {
		t.Fatalf("Cache-Control=%q want %q", got, want)
	}
}

func TestGroupMediaFilesUsesGeneralizedConfRenderChunkRule(t *testing.T) {
	files := groupMediaFiles("toronto/recordings/raw/mix/", []string{
		"camera-0000.mp4", "camera-0001.mp4", "camera-0002.mp4", "single.mp4", "notes.json",
	})
	if len(files) != 3 {
		t.Fatalf("files=%+v", files)
	}
	var sequence *mediaFile
	for i := range files {
		if files[i].SourceType == "chunkedVideo" {
			sequence = &files[i]
		}
	}
	if sequence == nil || sequence.Name != "camera-0000.mp4" || sequence.Kind != "video" || sequence.Path != "toronto/recordings/raw/mix/camera-0000.mp4" || len(sequence.Chunks) != 3 {
		t.Fatalf("sequence=%+v", sequence)
	}
}

func TestMediaFileKind(t *testing.T) {
	for _, test := range []struct{ name, want string }{
		{"clip.mp4", "video"}, {"card.PNG", "image"}, {"music.wav", "audio"}, {"notes.json", "other"},
	} {
		if got := mediaFileKind(test.name); got != test.want {
			t.Fatalf("mediaFileKind(%q)=%q want %q", test.name, got, test.want)
		}
	}
}

func TestGroupMediaFilesReturnsEmptyJSONArray(t *testing.T) {
	files := groupMediaFiles("toronto/recordings/", nil)
	if files == nil || len(files) != 0 {
		t.Fatalf("files=%+v, want non-nil empty slice", files)
	}
}

func TestProductionProxyObjectKeyMirrorsSourceDirectory(t *testing.T) {
	source := mediaFile{Path: "toronto/recordings/raw/mix/toronto_01main_100431_0000.mp4", SourceType: "chunkedVideo"}
	if got, want := productionProxyObjectKey("toronto", source), "toronto/recordings/workspace/raw/mix/toronto_01main_100431.proxy.mp4"; got != want {
		t.Fatalf("proxy=%q want %q", got, want)
	}
	standalone := mediaFile{Path: "toronto/recordings/raw/mix/talks.mp4", SourceType: "video"}
	if got, want := productionProxyObjectKey("toronto", standalone), "toronto/recordings/workspace/raw/mix/talks.proxy.mp4"; got != want {
		t.Fatalf("standalone proxy=%q want %q", got, want)
	}
	if got, want := productionProxySidecarObjectKey("toronto/recordings/workspace/raw/mix/talks.proxy.mp4"), "toronto/recordings/workspace/raw/mix/talks.proxy.v1.json"; got != want {
		t.Fatalf("sidecar=%q want %q", got, want)
	}
}

func TestLogicalMediaSourcesRecursiveGroupsChunksAndSkipsWorkspace(t *testing.T) {
	binDir := t.TempDir()
	rclone := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(rclone, []byte("#!/bin/sh\nprintf 'raw/mix/camera0000.mp4\\nraw/mix/camera0001.mp4\\nraw/mix/single.mov\\nworkspace/old.proxy.mp4\\nedits/talks/final.mp4\\nassets/bumper.mp4\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := &Handler{Remote: "btcpp:btcpp"}
	sources, err := h.logicalMediaSourcesRecursive(context.Background(), "toronto/recordings/")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 4 {
		t.Fatalf("sources=%+v", sources)
	}
	paths := make(map[string]bool, len(sources))
	for _, source := range sources {
		paths[source.Path] = true
	}
	for _, want := range []string{"toronto/recordings/raw/mix/camera0000.mp4", "toronto/recordings/raw/mix/single.mov", "toronto/recordings/edits/talks/final.mp4", "toronto/recordings/assets/bumper.mp4"} {
		if !paths[want] {
			t.Fatalf("sources omitted %q: %+v", want, sources)
		}
	}
}

func TestProductionProxyTargetSourcesAcceptsFolderOrLogicalFile(t *testing.T) {
	binDir := t.TempDir()
	rclone := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(rclone, []byte("#!/bin/sh\ncase \"$*\" in\n  *--recursive*) printf 'camera0000.mp4\\ncamera0001.mp4\\nsingle.mov\\n' ;;\n  *) printf 'camera0000.mp4\\ncamera0001.mp4\\nsingle.mov\\n' ;;\nesac\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := &Handler{Remote: "btcpp:btcpp"}
	folder, err := h.productionProxyTargetSources(context.Background(), "toronto", "toronto/recordings/raw/mix/")
	if err != nil || len(folder) != 2 {
		t.Fatalf("folder sources=%+v err=%v", folder, err)
	}
	file, err := h.productionProxyTargetSources(context.Background(), "toronto", "toronto/recordings/raw/mix/camera0000.mp4")
	if err != nil || len(file) != 1 || file[0].SourceType != "chunkedVideo" || len(file[0].Chunks) != 2 {
		t.Fatalf("file sources=%+v err=%v", file, err)
	}
	edits, err := h.productionProxyTargetSources(context.Background(), "toronto", "toronto/recordings/edits/talks/")
	if err != nil || len(edits) != 2 {
		t.Fatalf("edited sources=%+v err=%v", edits, err)
	}
}

func TestProductionProxyTargetSourcesRejectsDerivedAndCrossConferenceMedia(t *testing.T) {
	h := &Handler{Remote: "btcpp:btcpp"}
	for _, target := range []string{
		"toronto/recordings/",
		"toronto/recordings/workspace/source.proxy.mp4",
		"nairobi/recordings/raw/mix/source.mp4",
	} {
		if _, err := h.productionProxyTargetSources(context.Background(), "toronto", target); err == nil {
			t.Fatalf("target %q was accepted", target)
		}
	}
}

func TestProductionProxyPrepareReturnsBrowserActionResult(t *testing.T) {
	binDir := t.TempDir()
	rclone := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(rclone, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	form := url.Values{"conference": {"toronto"}, "target": {"toronto/recordings/raw/mix/"}}
	request := httptest.NewRequest(http.MethodPost, "/production/media/prepare", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	h := &Handler{DB: productionHandlerTestDB(t), Remote: "btcpp:btcpp"}
	h.productionProxyPrepare(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"queued":0`) || !strings.Contains(response.Body.String(), "No new editing proxy jobs were needed") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestLogicalMediaInfoRequiresEditingProxy(t *testing.T) {
	binDir := t.TempDir()
	rclone := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(rclone, []byte("#!/bin/sh\nprintf 'camera0000.mp4\\ncamera0001.mp4\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := &Handler{Remote: "btcpp:btcpp"}
	info, err := h.logicalMediaInfo(context.Background(), "toronto/recordings/raw/mix/camera0000.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if info.SourceType != "chunkedVideo" || info.DurationMS != 0 || info.ProxyPath != "" || !strings.Contains(info.Warning, "Prepare this recording") {
		t.Fatalf("info=%+v", info)
	}
}

func TestLogicalMediaInfoUsesFinishedEditingProxy(t *testing.T) {
	binDir := t.TempDir()
	rclone := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(rclone, []byte("#!/bin/sh\ncase \"$*\" in\n  *cat*) printf '{\"version\":1,\"source\":{\"path\":\"toronto/recordings/raw/mix/camera0000.mp4\"},\"proxy\":{\"path\":\"toronto/recordings/workspace/raw/mix/camera.proxy.mp4\",\"durationMs\":930123}}' ;;\n  *workspace/raw/mix*) printf 'camera.proxy.mp4\\ncamera.proxy.v1.json\\n' ;;\n  *) printf 'camera0000.mp4\\ncamera0001.mp4\\n' ;;\nesac\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	database := productionHandlerTestDB(t)
	source := "toronto/recordings/raw/mix/camera0000.mp4"
	proxy := "toronto/recordings/workspace/raw/mix/camera.proxy.mp4"
	job, _, err := database.EnqueueProductionProxyJob(source, proxy)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := database.ClaimProductionProxyJob()
	if err != nil || claimed.ID != job.ID {
		t.Fatalf("claimed=%+v err=%v", claimed, err)
	}
	if err := database.FinishProductionProxyJob(job.ID, 930123); err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: database, Remote: "btcpp:btcpp"}
	info, err := h.logicalMediaInfo(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if info.ProxyPath != proxy || info.ProxyStatus != "finished" || info.DurationMS != 930123 || info.Warning != "" {
		t.Fatalf("info=%+v", info)
	}
}

func TestLogicalMediaInfoDiscoversEditingProxyWithoutDatabase(t *testing.T) {
	binDir := t.TempDir()
	rclone := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(rclone, []byte("#!/bin/sh\ncase \"$*\" in\n  *cat*) printf '{\"version\":1,\"source\":{\"path\":\"toronto/recordings/raw/mix/camera0000.mp4\"},\"proxy\":{\"path\":\"toronto/recordings/workspace/raw/mix/camera.proxy.mp4\",\"durationMs\":930123}}' ;;\n  *workspace/raw/mix*) printf 'camera.proxy.mp4\\ncamera.proxy.v1.json\\n' ;;\n  *) printf 'camera0000.mp4\\ncamera0001.mp4\\n' ;;\nesac\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := &Handler{Remote: "btcpp:btcpp"}
	info, err := h.logicalMediaInfo(context.Background(), "toronto/recordings/raw/mix/camera0000.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if info.ProxyPath != "toronto/recordings/workspace/raw/mix/camera.proxy.mp4" || info.ProxyStatus != "finished" || info.DurationMS != 930123 || info.Warning != "" {
		t.Fatalf("info=%+v", info)
	}
}

func TestMediaBrowserDiscoversPreparedSourceWithoutDatabase(t *testing.T) {
	binDir := t.TempDir()
	rclone := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(rclone, []byte("#!/bin/sh\ncase \"$*\" in\n  *workspace/raw/mix*) printf 'camera.proxy.mp4\\ncamera.proxy.v1.json\\n' ;;\n  *) printf 'camera0000.mp4\\ncamera0001.mp4\\n' ;;\nesac\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := &Handler{Remote: "btcpp:btcpp"}
	_, files, err := h.listMediaSpacesPrefix(context.Background(), "toronto/recordings/raw/mix/")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].SourceType != "chunkedVideo" || files[0].ProxyStatus != "finished" {
		t.Fatalf("files=%+v", files)
	}
}

func TestProductionProxyArtifactCountUsesStoredFiles(t *testing.T) {
	binDir := t.TempDir()
	rclone := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(rclone, []byte("#!/bin/sh\nprintf 'raw/mix/main.proxy.mp4\\nraw/mix/main.proxy.v1.json\\nraw/mix/talks.proxy.mp4\\nraw/mix/talks.proxy.v1.json\\nraw/mix/incomplete.proxy.mp4\\nnotes.json\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := &Handler{Remote: "btcpp:btcpp"}
	count, err := h.productionProxyArtifactCount(context.Background(), "toronto")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("ready proxy count=%d want 2", count)
	}
}

func TestProductionProxyPrepareSkipsExistingArtifactWithoutDatabaseRecord(t *testing.T) {
	binDir := t.TempDir()
	rclone := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(rclone, []byte("#!/bin/sh\ncase \"$*\" in\n  *workspace*) printf 'raw/mix/camera.proxy.mp4\\nraw/mix/camera.proxy.v1.json\\n' ;;\n  *) printf 'camera0000.mp4\\ncamera0001.mp4\\n' ;;\nesac\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	database := productionHandlerTestDB(t)
	form := url.Values{"conference": {"toronto"}, "target": {"toronto/recordings/raw/mix/"}}
	request := httptest.NewRequest(http.MethodPost, "/production/media/prepare", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	h := &Handler{DB: database, Remote: "btcpp:btcpp"}
	h.productionProxyPrepare(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"queued":0`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := database.ProductionProxyJobBySource("toronto/recordings/raw/mix/camera0000.mp4"); err == nil {
		t.Fatal("existing proxy was unnecessarily added to the preparation queue")
	}
}

func TestRcloneEnvRemovesAmbientFileFilters(t *testing.T) {
	t.Setenv("RCLONE_FILTER_FROM", "/tmp/personal-filter")
	t.Setenv("RCLONE_EXCLUDE", "*.mp4")
	t.Setenv("RCLONE_CONFIG_PASS", "keep-this")
	env := strings.Join(rcloneEnv("/tmp/streamctl-rclone.conf"), "\n")
	for _, unwanted := range []string{"RCLONE_FILTER_FROM=", "RCLONE_EXCLUDE="} {
		if strings.Contains(env, unwanted) {
			t.Fatalf("rclone environment contains %q", unwanted)
		}
	}
	for _, wanted := range []string{"RCLONE_CONFIG_PASS=keep-this", "RCLONE_CONFIG=/tmp/streamctl-rclone.conf"} {
		if !strings.Contains(env, wanted) {
			t.Fatalf("rclone environment does not contain %q", wanted)
		}
	}
}

func TestProductionCutsSavePersistsFormRanges(t *testing.T) {
	database := productionHandlerTestDB(t)
	h := &Handler{DB: database}
	form := url.Values{
		"conference": {"toronto"}, "talk_id": {"talk-1"},
		"source":      {"toronto/recordings/main0000.mp4", "toronto/recordings/main.mp4"},
		"source_type": {"chunkedVideo", "video"},
		"in_ms":       {"1000", "3000"}, "out_ms": {"2000", "4500"},
	}
	request := httptest.NewRequest(http.MethodPost, "/production/cuts/save", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	h.productionCutsSave(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	cuts, err := database.ListProductionCuts("toronto")
	if err != nil || len(cuts["talk-1"]) != 2 || cuts["talk-1"][1].OutMS != 4500 {
		t.Fatalf("cuts=%+v err=%v", cuts, err)
	}
}

func TestProductionCutsSaveClearsAllRanges(t *testing.T) {
	database := productionHandlerTestDB(t)
	if err := database.ReplaceProductionCuts("toronto", "talk-1", []db.ProductionCut{{Source: "toronto/recordings/main.mp4", SourceType: "video", InMS: 1000, OutMS: 2000}}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: database}
	form := url.Values{"conference": {"toronto"}, "talk_id": {"talk-1"}}
	request := httptest.NewRequest(http.MethodPost, "/production/cuts/save", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	h.productionCutsSave(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	cuts, err := database.ListProductionCuts("toronto")
	if err != nil || len(cuts["talk-1"]) != 0 {
		t.Fatalf("cuts=%+v err=%v", cuts, err)
	}
}
