package handlers

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"streamctl/internal/btcppclient"
	"streamctl/internal/btcppoauth"
	"streamctl/internal/db"
	"streamctl/internal/doclient"
	"streamctl/internal/nostrpub"
	"streamctl/internal/probe"
	"streamctl/internal/runpodclient"
	"streamctl/internal/systemd"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed worker/render-from-spaces.py
var renderWorkerScriptText string

//go:embed worker/conf-render-ffmpeg-compat.patch
var confRenderFFmpegCompatibilityPatch string

const (
	gpuWorkerSSHKeyPath  = "/var/lib/streamctl/gpu-worker-ssh-key"
	confRenderRepository = "https://github.com/jgb95/conf-render.git"
	confRenderRevision   = "547f5f4be89d87034e2bfb4ea293db1fa621aa39"
)

type Handler struct {
	DB                  *db.DB
	Secret              string
	VideoDir            string
	CacheDir            string
	HLSDir              string
	Remote              string
	RcloneConfig        string
	NostrKeyDir         string
	NostrKeyOwner       string
	GPUWorkerHost       string
	GPUWorkerSSHKey     string
	RenderWorkerCommand string
	RenderOutputDir     string
	DOTokenFile         string
	RunPodTokenFile     string
	RunPodPodName       string
	RunPodGPUType       string
	RunPodImage         string
	RunPodCloudType     string
	GPUDropletName      string
	GPUDropletRegion    string
	GPUDropletSize      string
	GPUDropletImage     string
	GPUSSHKeyName       string
	GPUWorkerUser       string
	GPUDestroyAfterJob  bool
	Systemd             *systemd.Manager
	OAuth               *btcppoauth.Client
	BTCPP               productionCandidatesClient
	BTCPPBaseURL        string

	funcs       template.FuncMap
	gpuQueueMu  sync.Mutex
	gpuCreateMu sync.Mutex
}

type productionCandidatesClient interface {
	Conferences(context.Context) ([]btcppclient.Conference, error)
	RecordingCandidates(context.Context, string) ([]btcppclient.Candidate, error)
}

func (h *Handler) Register(mux *http.ServeMux) {
	h.funcs = template.FuncMap{
		"formatTime": func(t time.Time) string { return t.Format("2006-01-02 15:04") },
		"contains": func(slice []int64, id int64) bool {
			for _, x := range slice {
				if x == id {
					return true
				}
			}
			return false
		},
	}

	mux.HandleFunc("/login", h.login)
	mux.HandleFunc("/oauth/start", h.oauthStart)
	mux.HandleFunc("/oauth/callback", h.oauthCallback)
	mux.Handle("/logout", h.mutation(http.HandlerFunc(h.logout)))
	mux.Handle("/live/", http.StripPrefix("/live/", http.HandlerFunc(h.liveFile)))

	mux.Handle("/", h.auth(http.HandlerFunc(h.index)))
	mux.Handle("/streams/broadcast-catalog", h.auth(http.HandlerFunc(h.broadcastCatalog)))
	mux.Handle("/streams/new", h.auth(http.HandlerFunc(h.streamNew)))
	mux.Handle("/streams/create", h.mutation(http.HandlerFunc(h.streamCreate)))
	mux.Handle("/streams/edit/", h.auth(http.HandlerFunc(h.streamEdit)))
	mux.Handle("/streams/update/", h.mutation(http.HandlerFunc(h.streamUpdate)))
	mux.Handle("/streams/delete/", h.mutation(http.HandlerFunc(h.streamDelete)))
	mux.Handle("/streams/start/", h.mutation(http.HandlerFunc(h.streamStart)))
	mux.Handle("/streams/stop/", h.mutation(http.HandlerFunc(h.streamStop)))
	mux.Handle("/streams/logs/", h.auth(http.HandlerFunc(h.streamLogs)))
	mux.Handle("/streams/preview/", h.auth(http.HandlerFunc(h.streamPreview)))
	mux.Handle("/spaces/browse", h.auth(http.HandlerFunc(h.spacesBrowse)))
	mux.Handle("/production", h.auth(http.HandlerFunc(h.productionHome)))
	mux.Handle("/production/timestamp", h.auth(http.HandlerFunc(h.productionTimestamp)))
	mux.Handle("/production/timestamp/cut", h.auth(http.HandlerFunc(h.productionCut)))
	mux.Handle("/production/media/browse", h.auth(http.HandlerFunc(h.mediaBrowse)))
	mux.Handle("/production/media/info", h.auth(http.HandlerFunc(h.mediaInfo)))
	mux.Handle("/production/media/open", h.auth(http.HandlerFunc(h.mediaOpen)))
	mux.Handle("/production/media/prepare", h.mutation(http.HandlerFunc(h.productionProxyPrepare)))
	mux.Handle("/production/cuts/save", h.mutation(http.HandlerFunc(h.productionCutsSave)))
	mux.Handle("/production/templates", h.auth(http.HandlerFunc(h.productionTemplates)))
	mux.Handle("/production/templates/edit", h.auth(http.HandlerFunc(h.productionTemplateEdit)))
	mux.Handle("/production/templates/create", h.mutation(http.HandlerFunc(h.productionTemplateCreate)))
	mux.Handle("/production/templates/save", h.mutation(http.HandlerFunc(h.productionTemplateSave)))
	mux.Handle("/production/templates/duplicate", h.mutation(http.HandlerFunc(h.productionTemplateDuplicate)))
	mux.Handle("/production/templates/delete", h.mutation(http.HandlerFunc(h.productionTemplateDelete)))
	mux.Handle("/production/templates/delete-selected", h.mutation(http.HandlerFunc(h.productionTemplatesDelete)))
	mux.Handle("/production/renders", h.auth(http.HandlerFunc(h.productionRenders)))
	mux.Handle("/production/renders/edit", h.auth(http.HandlerFunc(h.productionRenderEdit)))
	mux.Handle("/production/renders/template-options", h.auth(http.HandlerFunc(h.productionTemplateOptions)))
	mux.Handle("/production/renders/expand-template", h.mutation(http.HandlerFunc(h.productionExpandTemplate)))
	mux.Handle("/production/renders/livestream-options", h.auth(http.HandlerFunc(h.productionLivestreamOptions)))
	mux.Handle("/production/renders/create-livestream", h.mutation(http.HandlerFunc(h.productionRenderCreateLivestream)))
	mux.Handle("/production/renders/create", h.mutation(http.HandlerFunc(h.productionRenderCreate)))
	mux.Handle("/production/renders/save", h.mutation(http.HandlerFunc(h.productionRenderSave)))
	mux.Handle("/production/renders/duplicate", h.mutation(http.HandlerFunc(h.productionRenderDuplicate)))
	mux.Handle("/production/renders/delete", h.mutation(http.HandlerFunc(h.productionRenderDelete)))
	mux.Handle("/production/renders/delete-selected", h.mutation(http.HandlerFunc(h.productionRendersDelete)))
	mux.Handle("/production/renders/queue", h.mutation(http.HandlerFunc(h.productionRendersQueue)))
	mux.Handle("/production/renders/generate", h.mutation(http.HandlerFunc(h.productionRenderGenerate)))

	// Keep retired URLs explicit so old bookmarks/forms cannot enqueue work.
	for _, path := range []string{"/normalize", "/normalize/process", "/normalize/process-selected", "/livestream", "/livestream/process", "/livestream/process-selected", "/livestream-files", "/livestream-files/process", "/livestream-files/process-selected", "/worker/normalize/requeue", "/worker/normalize/requeue-stale", "/worker/normalize/clear-resolved-failures", "/livestream-files/requeue", "/livestream-files/requeue-stale", "/livestream-files/clear-resolved-failures"} {
		mux.Handle(path, h.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "The normalization workflow has been retired. Finished videos are checked for stream readiness when scheduled and played directly.", http.StatusGone)
		})))
	}
	mux.Handle("/worker", h.auth(http.HandlerFunc(h.worker)))
	mux.Handle("/worker/jobs", h.auth(http.HandlerFunc(h.workerJobs)))
	mux.Handle("/worker/jobs/status", h.auth(http.HandlerFunc(h.workerJobsStatus)))
	mux.Handle("/worker/state", h.auth(http.HandlerFunc(h.workerState)))
	mux.Handle("/worker/media/requeue", h.mutation(http.HandlerFunc(h.productionProxyRequeue)))
	mux.Handle("/gpu-worker/create", h.mutation(http.HandlerFunc(h.gpuWorkerCreate)))
	mux.Handle("/gpu-worker/destroy", h.mutation(http.HandlerFunc(h.gpuWorkerDestroy)))
	mux.Handle("/gpu-worker/logs/", h.auth(http.HandlerFunc(h.gpuJobLogs)))
	mux.Handle("/render", h.auth(http.HandlerFunc(h.renderJobs)))
	mux.Handle("/render/create", h.mutation(http.HandlerFunc(h.renderJobCreate)))
	mux.Handle("/worker/render/retry/", h.mutation(http.HandlerFunc(h.renderJobRetry)))
	mux.Handle("/worker/render/cancel/", h.mutation(http.HandlerFunc(h.renderJobCancel)))
	mux.Handle("/render-jobs", h.auth(http.HandlerFunc(redirectTo("/render"))))
	mux.Handle("/render-jobs/create", h.mutation(http.HandlerFunc(h.renderJobCreate)))
	mux.Handle("/render-jobs/retry/", h.mutation(http.HandlerFunc(h.renderJobRetry)))
	mux.Handle("/render-jobs/cancel/", h.mutation(http.HandlerFunc(h.renderJobCancel)))

	mux.Handle("/endpoints", h.auth(http.HandlerFunc(h.endpoints)))
	mux.Handle("/endpoints/create", h.mutation(http.HandlerFunc(h.endpointCreate)))
	mux.Handle("/endpoints/update/", h.mutation(http.HandlerFunc(h.endpointUpdate)))
	mux.Handle("/endpoints/delete/", h.mutation(http.HandlerFunc(h.endpointDelete)))
	mux.Handle("/endpoints/test/", h.mutation(http.HandlerFunc(h.endpointTest)))

	mux.Handle("/nostr", h.auth(http.HandlerFunc(h.nostr)))
	mux.Handle("/nostr/keys/create", h.mutation(http.HandlerFunc(h.nostrKeyCreate)))
	mux.Handle("/nostr/keys/update/", h.mutation(http.HandlerFunc(h.nostrKeyUpdate)))
	mux.Handle("/nostr/keys/delete/", h.mutation(http.HandlerFunc(h.nostrKeyDelete)))
	mux.Handle("/nostr/relays/create", h.mutation(http.HandlerFunc(h.nostrRelayCreate)))
	mux.Handle("/nostr/relays/update/", h.mutation(http.HandlerFunc(h.nostrRelayUpdate)))
	mux.Handle("/nostr/relays/delete/", h.mutation(http.HandlerFunc(h.nostrRelayDelete)))
	if h.DB != nil {
		if err := h.DB.RequeueInterruptedProductionProxyJobs(); err != nil {
			log.Printf("requeueing interrupted CPU previews: %v", err)
		}
	}
	go h.gpuQueueDispatcher()
	go h.recordingRegistrationDispatcher()
}

func redirectTo(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, path, http.StatusPermanentRedirect)
	}
}

// liveFile serves generated HLS playlists and segments publicly. Nostr live
// activity events can point clients at these /live/... URLs.
func (h *Handler) liveFile(w http.ResponseWriter, r *http.Request) {
	clean := path.Clean("/" + r.URL.Path)
	if clean == "/" || strings.Contains(clean, "/../") {
		http.NotFound(w, r)
		return
	}
	rel := strings.TrimPrefix(clean, "/")
	full := filepath.Join(h.HLSDir, rel)
	if !strings.HasPrefix(full, filepath.Clean(h.HLSDir)+string(os.PathSeparator)) {
		http.NotFound(w, r)
		return
	}

	switch strings.ToLower(filepath.Ext(full)) {
	case ".m3u8":
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	case ".ts":
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Cache-Control", "public, max-age=30")
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	http.ServeFile(w, r, full)
}

// ---------- streams ----------

type streamView struct {
	db.Stream
	Status          string
	NextTrigger     string
	NextTriggerUnix int64
	LiveURL         string
}

func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	streams, err := h.DB.ListStreams()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	views := make([]streamView, len(streams))
	for i, s := range streams {
		views[i] = streamView{
			Stream:          s,
			Status:          h.Systemd.Status(s.ID),
			NextTrigger:     h.Systemd.NextTrigger(s.ID),
			NextTriggerUnix: h.Systemd.NextTriggerUnix(s.ID),
			LiveURL:         fmt.Sprintf("/live/stream-%d/index.m3u8", s.ID),
		}
	}
	h.render(w, r, "streams.html", map[string]any{"Streams": views})
}

func (h *Handler) streamNew(w http.ResponseWriter, r *http.Request) {
	if err := h.renderStreamForm(w, r, http.StatusOK, "New stream", "/streams/create", nil, nil, "", ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (h *Handler) streamCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s, ids, videos, err := h.streamFromForm(r, nil)
	if err != nil {
		draft := h.streamDraftFromForm(r)
		if renderErr := h.renderStreamForm(w, r, http.StatusBadRequest, "New stream", "/streams/create", draft, selectedEndpointIDsFromForm(r), draft.ScheduleType, err.Error()); renderErr != nil {
			http.Error(w, renderErr.Error(), http.StatusInternalServerError)
		}
		return
	}
	id, err := h.DB.CreateStream(s, ids, videos)
	if err != nil {
		s.Videos = videos
		if renderErr := h.renderStreamForm(w, r, http.StatusInternalServerError, "New stream", "/streams/create", s, ids, s.ScheduleType, err.Error()); renderErr != nil {
			http.Error(w, renderErr.Error(), http.StatusInternalServerError)
		}
		return
	}
	created, err := h.DB.GetStream(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.Systemd.Sync(created); err != nil {
		if renderErr := h.renderStreamForm(w, r, http.StatusInternalServerError, "Edit stream", fmt.Sprintf("/streams/update/%d", created.ID), created, endpointIDs(created.Endpoints), created.ScheduleType, "stream saved but systemd sync failed: "+err.Error()); renderErr != nil {
			http.Error(w, renderErr.Error(), http.StatusInternalServerError)
		}
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) streamEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/streams/edit/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	s, err := h.DB.GetStream(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	selected := make([]int64, len(s.Endpoints))
	for i, e := range s.Endpoints {
		selected[i] = e.ID
	}
	if err := h.renderStreamForm(w, r, http.StatusOK, "Edit stream", fmt.Sprintf("/streams/update/%d", s.ID), s, selected, s.ScheduleType, ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (h *Handler) streamUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/streams/update/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	existing, err := h.DB.GetStream(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s, ids, videos, err := h.streamFromForm(r, existing)
	if err != nil {
		draft := h.streamDraftFromForm(r)
		draft.ID = id
		draft.AutoScheduled = existing.AutoScheduled
		if renderErr := h.renderStreamForm(w, r, http.StatusBadRequest, "Edit stream", fmt.Sprintf("/streams/update/%d", id), draft, selectedEndpointIDsFromForm(r), draft.ScheduleType, err.Error()); renderErr != nil {
			http.Error(w, renderErr.Error(), http.StatusInternalServerError)
		}
		return
	}
	s.ID = id
	if err := h.DB.UpdateStream(s, ids, videos); err != nil {
		s.Videos = videos
		if renderErr := h.renderStreamForm(w, r, http.StatusInternalServerError, "Edit stream", fmt.Sprintf("/streams/update/%d", id), s, ids, s.ScheduleType, err.Error()); renderErr != nil {
			http.Error(w, renderErr.Error(), http.StatusInternalServerError)
		}
		return
	}
	updated, err := h.DB.GetStream(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.Systemd.Sync(updated); err != nil {
		if renderErr := h.renderStreamForm(w, r, http.StatusInternalServerError, "Edit stream", fmt.Sprintf("/streams/update/%d", id), updated, endpointIDs(updated.Endpoints), updated.ScheduleType, "stream saved but systemd sync failed: "+err.Error()); renderErr != nil {
			http.Error(w, renderErr.Error(), http.StatusInternalServerError)
		}
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) streamDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/streams/delete/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	s, err := h.DB.GetStream(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err := h.Systemd.Remove(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.Systemd.RemoveCachedClips(s); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.DB.DeleteStream(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) streamStart(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/streams/start/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	s, err := h.DB.GetStream(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err := h.Systemd.Start(s); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) streamStop(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/streams/stop/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := h.Systemd.Stop(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) streamLogs(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/streams/logs/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	s, err := h.DB.GetStream(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	h.render(w, r, "stream_logs.html", map[string]any{
		"Stream": s,
		"Units":  h.Systemd.Logs(id),
	})
}

func (h *Handler) streamPreview(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/streams/preview/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	s, err := h.DB.GetStream(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	liveURL := fmt.Sprintf("/live/stream-%d/index.m3u8", s.ID)
	playlistPath := filepath.Join(h.HLSDir, fmt.Sprintf("stream-%d", s.ID), "index.m3u8")
	playlistReady := false
	playlistUpdated := ""
	if info, err := os.Stat(playlistPath); err == nil && !info.IsDir() {
		playlistReady = true
		playlistUpdated = info.ModTime().Format("2006-01-02 15:04:05")
	}
	h.render(w, r, "stream_preview.html", map[string]any{
		"Stream":          s,
		"LiveURL":         liveURL,
		"PlaylistReady":   playlistReady,
		"PlaylistUpdated": playlistUpdated,
	})
}

func (h *Handler) renderStreamForm(w http.ResponseWriter, r *http.Request, status int, title, action string, s *db.Stream, selected []int64, defaultSchedule, errorMessage string) error {
	files, err := h.listVideos()
	if err != nil {
		return err
	}
	endpoints, err := h.DB.ListEndpoints()
	if err != nil {
		return err
	}
	nostrKeys, err := h.DB.ListNostrKeys()
	if err != nil {
		return err
	}
	if selected == nil {
		selected = make([]int64, len(endpoints))
		for i, e := range endpoints {
			selected[i] = e.ID
		}
	}
	if defaultSchedule == "" {
		defaultSchedule = "once"
		if s != nil && s.ScheduleType != "" {
			defaultSchedule = s.ScheduleType
		}
	}
	broadcastMode := ""
	if r.Method == http.MethodPost {
		switch value := r.FormValue("btcpp_target_mode"); value {
		case "auto", "talk", "conference", "none":
			broadcastMode = value
		}
	}
	h.renderStatus(w, r, status, "stream_form.html", map[string]any{
		"BroadcastMode":   broadcastMode,
		"Stream":          s,
		"Videos":          files,
		"BitratesJSON":    bitratesJSON(h.VideoDir, files),
		"Endpoints":       endpoints,
		"NostrKeys":       nostrKeys,
		"SelectedIDs":     selected,
		"FormAction":      action,
		"Title":           title,
		"DefaultSchedule": defaultSchedule,
		"RemoteBrowse":    strings.TrimSpace(h.Remote) != "",
		"Error":           errorMessage,
	})
	return nil
}

func (h *Handler) streamDraftFromForm(r *http.Request) *db.Stream {
	var videos []string
	for _, v := range r.Form["video_file"] {
		v = strings.TrimSpace(v)
		if v != "" {
			videos = append(videos, v)
		}
	}
	scheduleType := r.FormValue("schedule_type")
	if scheduleType == "" {
		scheduleType = "once"
	}
	return &db.Stream{
		Name:             strings.TrimSpace(r.FormValue("name")),
		Videos:           videos,
		ScheduleType:     scheduleType,
		OnCalendar:       strings.TrimSpace(r.FormValue("on_calendar")),
		NostrEnabled:     r.FormValue("nostr_enabled") == "on",
		NostrKeyID:       parseOptionalInt64(r.FormValue("nostr_key_id")),
		NostrTitle:       strings.TrimSpace(r.FormValue("nostr_title")),
		NostrSummary:     strings.TrimSpace(r.FormValue("nostr_summary")),
		BTCPPRecordingID: strings.TrimSpace(r.FormValue("btcpp_recording_id")),
		BTCPPConference:  strings.TrimSpace(r.FormValue("btcpp_conference")),
		Enabled:          r.FormValue("enabled") == "on",
	}
}

func selectedEndpointIDsFromForm(r *http.Request) []int64 {
	var ids []int64
	for _, v := range r.Form["endpoint_ids"] {
		id, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func endpointIDs(endpoints []db.Endpoint) []int64 {
	ids := make([]int64, len(endpoints))
	for i, e := range endpoints {
		ids[i] = e.ID
	}
	return ids
}

func normalizeEndpointType(value string) string {
	switch strings.TrimSpace(value) {
	case "youtube_hls":
		return "youtube_hls"
	default:
		return "rtmp"
	}
}

func validateEndpoint(e *db.Endpoint, creating bool) error {
	if e.Name == "" {
		return fmt.Errorf("name required")
	}
	if e.RtmpURL == "" {
		if e.Type == "youtube_hls" {
			return fmt.Errorf("YouTube HLS URL required")
		}
		return fmt.Errorf("RTMP URL required")
	}
	switch e.Type {
	case "rtmp":
		if e.StreamKey == "" {
			return fmt.Errorf("stream key required")
		}
	case "youtube_hls":
		if !strings.Contains(e.RtmpURL, "file=") {
			return fmt.Errorf("YouTube HLS URL must include file=")
		}
	default:
		return fmt.Errorf("unsupported endpoint type: %s", e.Type)
	}
	return nil
}

type spacesEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type spacesBrowseResponse struct {
	Prefix string        `json:"prefix"`
	Dirs   []spacesEntry `json:"dirs"`
	Files  []spacesEntry `json:"files"`
}

func (h *Handler) spacesBrowse(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(h.Remote) == "" {
		http.Error(w, "remote browsing is not configured", http.StatusBadRequest)
		return
	}

	prefix, err := cleanSpacesPrefix(r.URL.Query().Get("prefix"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var resp spacesBrowseResponse
	resp.Prefix = prefix
	if prefix == "" {
		resp.Dirs, err = h.listSpacesConferences(r.Context())
	} else {
		resp.Dirs, resp.Files, err = h.listSpacesPrefix(r.Context(), prefix)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *Handler) streamFromForm(r *http.Request, existing *db.Stream) (*db.Stream, []int64, []string, error) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		return nil, nil, nil, fmt.Errorf("name required")
	}

	var videos []string
	for _, v := range r.Form["video_file"] {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		clean, err := cleanClipSource(v)
		if err != nil {
			return nil, nil, nil, err
		}
		videos = append(videos, clean)
	}
	if len(videos) == 0 {
		return nil, nil, nil, fmt.Errorf("at least one video clip required")
	}
	autoScheduled := existing != nil && existing.AutoScheduled
	if err := h.validateStreamVideos(r.Context(), videos); err != nil {
		return nil, nil, nil, err
	}

	scheduleType := r.FormValue("schedule_type")
	if scheduleType != "once" && scheduleType != "recurring" {
		return nil, nil, nil, fmt.Errorf("schedule_type must be 'once' or 'recurring'")
	}
	onCalendar := strings.TrimSpace(r.FormValue("on_calendar"))
	if onCalendar == "" {
		return nil, nil, nil, fmt.Errorf("on_calendar required")
	}
	onCalendar = normalizeOnCalendar(onCalendar)

	var ids []int64
	for _, v := range r.Form["endpoint_ids"] {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("bad endpoint id: %s", v)
		}
		ids = append(ids, id)
	}

	s := &db.Stream{
		AutoScheduled:    autoScheduled,
		Name:             name,
		ScheduleType:     scheduleType,
		OnCalendar:       onCalendar,
		NostrEnabled:     r.FormValue("nostr_enabled") == "on",
		NostrKeyID:       parseOptionalInt64(r.FormValue("nostr_key_id")),
		NostrTitle:       strings.TrimSpace(r.FormValue("nostr_title")),
		NostrSummary:     strings.TrimSpace(r.FormValue("nostr_summary")),
		BTCPPRecordingID: strings.TrimSpace(r.FormValue("btcpp_recording_id")),
		BTCPPConference:  strings.TrimSpace(r.FormValue("btcpp_conference")),
		Enabled:          r.FormValue("enabled") == "on",
	}
	if s.BTCPPConference != "" && (s.BTCPPRecordingID != "" || !validProductionConference(s.BTCPPConference)) {
		return nil, nil, nil, fmt.Errorf("choose either a recording ID or a valid conference tag for bitcoin++ broadcasting")
	}
	if s.NostrEnabled {
		if s.NostrKeyID == 0 {
			return nil, nil, nil, fmt.Errorf("Nostr key required when Nostr publishing is enabled")
		}
		if _, err := h.DB.GetNostrKey(s.NostrKeyID); err != nil {
			return nil, nil, nil, fmt.Errorf("Nostr key not found")
		}
	}
	return s, ids, videos, nil
}

func parseOptionalInt64(value string) int64 {
	id, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return id
}

// ---------- endpoints ----------

func (h *Handler) endpoints(w http.ResponseWriter, r *http.Request) {
	eps, err := h.DB.ListEndpoints()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	testID, _ := strconv.ParseInt(q.Get("id"), 10, 64)
	h.render(w, r, "endpoints.html", map[string]any{
		"Endpoints":  eps,
		"TestResult": q.Get("test"),
		"TestID":     testID,
		"TestErr":    q.Get("err"),
	})
}

func (h *Handler) endpointCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	e := &db.Endpoint{
		Type:      normalizeEndpointType(r.FormValue("endpoint_type")),
		Name:      strings.TrimSpace(r.FormValue("name")),
		RtmpURL:   strings.TrimSpace(r.FormValue("rtmp_url")),
		StreamKey: strings.TrimSpace(r.FormValue("stream_key")),
		Enabled:   true,
	}
	if err := validateEndpoint(e, true); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := h.DB.CreateEndpoint(e); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/endpoints", http.StatusSeeOther)
}

func (h *Handler) endpointUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/endpoints/update/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	existing, err := h.DB.GetEndpoint(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	existing.Type = normalizeEndpointType(r.FormValue("endpoint_type"))
	existing.Name = strings.TrimSpace(r.FormValue("name"))
	existing.RtmpURL = strings.TrimSpace(r.FormValue("rtmp_url"))
	if k := strings.TrimSpace(r.FormValue("stream_key")); k != "" {
		existing.StreamKey = k
	}
	if existing.Type == "youtube_hls" {
		existing.StreamKey = ""
	}
	existing.Enabled = r.FormValue("enabled") == "on"
	if err := validateEndpoint(existing, false); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.DB.UpdateEndpoint(existing); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Resync any streams that use this endpoint.
	if err := h.resyncStreamsUsingEndpoint(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/endpoints", http.StatusSeeOther)
}

// endpointTest pushes a short ffmpeg-generated test pattern to the endpoint
// to confirm the URL+key are accepted. Synchronous: blocks until ffmpeg
// exits or the timeout fires, then redirects back to /endpoints with the
// result encoded in the query string.
func (h *Handler) endpointTest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/endpoints/test/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	e, err := h.DB.GetEndpoint(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if e.Type != "rtmp" {
		http.Error(w, "test broadcast is only supported for RTMP endpoints", http.StatusBadRequest)
		return
	}

	target := strings.TrimRight(e.RtmpURL, "/") + "/" + e.StreamKey

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx,
		"ffmpeg", "-hide_banner", "-loglevel", "error",
		"-re",
		"-f", "lavfi", "-i", "testsrc=size=1280x720:rate=30",
		"-f", "lavfi", "-i", "sine=frequency=440",
		"-shortest", "-t", "8",
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency",
		"-pix_fmt", "yuv420p", "-b:v", "2000k",
		"-c:a", "aac", "-b:a", "128k", "-ar", "44100",
		"-f", "flv", target,
	)
	out, runErr := cmd.CombinedOutput()

	q := url.Values{}
	q.Set("id", strconv.FormatInt(id, 10))
	if runErr != nil {
		q.Set("test", "fail")
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = runErr.Error()
		}
		if len(msg) > 500 {
			msg = msg[:500] + "..."
		}
		q.Set("err", msg)
	} else {
		q.Set("test", "ok")
	}
	http.Redirect(w, r, "/endpoints?"+q.Encode(), http.StatusSeeOther)
}

func (h *Handler) endpointDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/endpoints/delete/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	streams, err := h.DB.ListStreams()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.DB.DeleteEndpoint(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Resync any stream that referenced the deleted endpoint.
	for _, s := range streams {
		for _, ep := range s.Endpoints {
			if ep.ID == id {
				updated, err := h.DB.GetStream(s.ID)
				if err == nil {
					_ = h.Systemd.Sync(updated)
				}
				break
			}
		}
	}
	http.Redirect(w, r, "/endpoints", http.StatusSeeOther)
}

func (h *Handler) resyncStreamsUsingEndpoint(epID int64) error {
	streams, err := h.DB.ListStreams()
	if err != nil {
		return err
	}
	for _, s := range streams {
		for _, ep := range s.Endpoints {
			if ep.ID == epID {
				if err := h.Systemd.Sync(&s); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

// ---------- Shared GPU worker ----------

type gpuWorkerView struct {
	Managed    bool
	Configured bool
	Provider   string
	ID         int64
	ExternalID string
	Name       string
	Status     string
	RawStatus  string
	IP         string
	SSHHost    string
	GPUType    string
	Image      string
	Region     string
	CloudType  string
	CostHourly float64
	CreatedAt  string
	MachineID  string
	Error      string
}

type gpuJobView struct {
	UnitName    string
	RawPath     string
	Host        string
	Description string
	LoadedState string
	ActiveState string
	SubState    string
	Result      string
	Since       string
	Journal     string
	Error       string
}

type gpuStatusView struct {
	Host      string
	Available bool
	NvidiaSMI string
	Jobs      []gpuJobView
	Error     string
}

type gpuSizeView struct {
	Slug          string
	Description   string
	PriceHourly   float64
	Regions       []string
	DefaultSize   bool
	DefaultRegion bool
}

type gpuAvailabilityView struct {
	Sizes           []gpuSizeView
	DefaultSize     string
	DefaultRegion   string
	SelectedSize    string
	SelectedRegions []string
	Error           string
}

func (h *Handler) gpuJobLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	unit := strings.TrimPrefix(r.URL.Path, "/gpu-worker/logs/")
	if !validGPUUnitName(unit) {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	host, err := h.gpuWorkerSSHHost(ctx)
	job := gpuJobView{UnitName: unit}
	cached := false
	warning := ""
	if err == nil {
		job = h.gpuJob(ctx, host, unit, true)
	} else {
		warning = "Worker unavailable. Showing saved logs."
	}
	if err != nil || strings.TrimSpace(job.Journal) == "" {
		if saved, dbErr := h.DB.GetGPUJobLog(unit); dbErr == nil {
			job = gpuJobViewFromDB(*saved)
			host = saved.Host
			cached = true
		}
	}
	if err != nil && !cached {
		http.Error(w, "Worker unavailable and no saved logs were found.", http.StatusServiceUnavailable)
		return
	}
	if cached && warning == "" {
		warning = "Showing saved logs; no live log output was available."
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Query().Get("format") == "json" {
		livestreamResponse(w, 200, map[string]any{"job": job, "host": host, "cached": cached, "warning": warning})
		return
	}
	h.render(w, r, "gpu_job_logs.html", map[string]any{"Job": job, "Host": host, "Cached": cached, "Warning": warning})
}

// Both VM and container workers use this execution and status contract.
func remoteGPUJobCommand(unitName, description, rawPath, invocation string) string {
	systemdRun := strings.Join([]string{
		"systemd-run",
		"--unit=" + shellQuote(unitName),
		"--description=" + shellQuote(description),
		"--collect",
		"--property=" + shellQuote("Type=exec"),
		"--property=" + shellQuote("RuntimeMaxSec=48h"),
		"--property=" + shellQuote("WorkingDirectory=/root"),
		"--property=" + shellQuote("Environment=RCLONE_CONFIG=/root/rclone.conf"),
		"--",
		"/bin/bash", "-c", shellQuote(invocation),
	}, " ")
	return strings.Join([]string{
		"unit=" + shellQuote(unitName),
		"raw=" + shellQuote(rawPath),
		"cmd=" + shellQuote(invocation),
		"desc=" + shellQuote(description),
		"if command -v systemd-run >/dev/null 2>&1 && systemctl show-environment >/dev/null 2>&1; then if systemctl is-active --quiet \"$unit\"; then printf '%s\\n' \"$unit\"; exit 0; fi; exec " + systemdRun + "; fi",
		"jobsroot=${STREAMCTL_GPU_JOB_ROOT:-/root/streamctl-gpu-jobs}",
		"mkdir -p \"$jobsroot\"",
		"lockfile=$jobsroot/$unit.lock",
		"exec 9>\"$lockfile\"",
		"if ! flock -n 9; then printf '%s\\n' \"$unit\"; exit 0; fi",
		"jobdir=$jobsroot/$unit",
		"mkdir -p \"$jobdir\"",
		"printf '%s\\n' \"$desc\" > \"$jobdir/description\"",
		"printf '%s\\n' \"$raw\" > \"$jobdir/raw_path\"",
		"date -Is > \"$jobdir/since\"",
		"printf 'running\\n' > \"$jobdir/active\"",
		"printf 'running\\n' > \"$jobdir/sub\"",
		": > \"$jobdir/result\"",
		": > \"$jobdir/journal\"",
		"nohup setsid env RCLONE_CONFIG=/root/rclone.conf STREAMCTL_JOBDIR=\"$jobdir\" timeout --kill-after=30s 48h bash -c 'set +e; bash -c \"$1\" >> \"$STREAMCTL_JOBDIR/journal\" 2>&1; rc=$?; if [ \"$rc\" -eq 0 ]; then printf \"inactive\\n\" > \"$STREAMCTL_JOBDIR/active\"; printf \"exited\\n\" > \"$STREAMCTL_JOBDIR/sub\"; printf \"success\\n\" > \"$STREAMCTL_JOBDIR/result\"; else printf \"failed\\n\" > \"$STREAMCTL_JOBDIR/active\"; printf \"failed\\n\" > \"$STREAMCTL_JOBDIR/sub\"; printf \"exit-code\\n\" > \"$STREAMCTL_JOBDIR/result\"; fi; exit \"$rc\"' streamctl-gpu-job \"$cmd\" >/dev/null 2>&1 & printf '%s\\n' \"$!\" > \"$jobdir/pid\"",
		"printf '%s\\n' \"$unit\"",
	}, "; ")
}

func (h *Handler) gpuStatus(ctx context.Context, worker gpuWorkerView) gpuStatusView {
	status := gpuStatusView{}
	if !worker.Configured {
		return status
	}
	host := strings.TrimSpace(h.GPUWorkerHost)
	if host == "" {
		if worker.Status != "active" || worker.SSHHost == "" {
			return h.appendCachedGPUJobs(status)
		}
		host = worker.SSHHost
	}
	status.Host = host
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	nvidia, err := remoteSSH(ctx, host, "nvidia-smi --query-gpu=name,utilization.gpu,utilization.memory,memory.used,memory.total,power.draw --format=csv,noheader,nounits 2>/dev/null || true")
	if err != nil {
		status.Error = fmt.Sprintf("GPU status unavailable: %v: %s", err, strings.TrimSpace(nvidia))
		return h.appendCachedGPUJobs(status)
	}
	status.Available = true
	status.NvidiaSMI = strings.TrimSpace(nvidia)
	unitsOut, err := remoteSSH(ctx, host, "systemctl list-units 'streamctl-gpu-*' --all --no-legend --plain 2>/dev/null || true")
	if err != nil {
		status.Error = fmt.Sprintf("GPU jobs unavailable: %v: %s", err, strings.TrimSpace(unitsOut))
		return h.appendCachedGPUJobs(status)
	}
	units := parseGPUUnitList(unitsOut)
	fileUnitsOut, fileUnitsErr := remoteSSH(ctx, host, "if [ -d /root/streamctl-gpu-jobs ]; then for d in /root/streamctl-gpu-jobs/streamctl-gpu-*; do [ -d \"$d\" ] && basename \"$d\"; done; fi")
	if fileUnitsErr == nil {
		units = append(units, parseGPUFileJobList(fileUnitsOut)...)
	}
	for _, unit := range dedupeGPUUnits(units) {
		job := h.gpuJob(ctx, host, unit, false)
		h.hydrateGPUJobFromCache(&job)
		status.Jobs = append(status.Jobs, job)
		if isTerminalGPUJob(job) {
			full := h.gpuJob(ctx, host, unit, true)
			h.hydrateGPUJobFromCache(&full)
			if err := h.saveGPUJobLog(full); err != nil {
				log.Printf("saving terminal GPU job %s failed: %v", unit, err)
			}
			h.destroyManagedGPUAfterTerminalJob(ctx, full)
			go h.dispatchGPUQueueOnce(context.Background())
		}
		if len(status.Jobs) >= 10 {
			break
		}
	}
	return h.appendCachedGPUJobs(status)
}

func (h *Handler) hydrateGPUJobFromCache(job *gpuJobView) {
	if job == nil || strings.TrimSpace(job.UnitName) == "" || strings.TrimSpace(job.RawPath) != "" {
		return
	}
	cached, err := h.DB.GetGPUJobLog(job.UnitName)
	if err == nil {
		job.RawPath = cached.RawPath
		if job.Host == "" {
			job.Host = cached.Host
		}
		if job.Description == "" {
			job.Description = cached.Description
		}
		if strings.TrimSpace(job.RawPath) != "" {
			return
		}
	}

}

func (h *Handler) appendCachedGPUJobs(status gpuStatusView) gpuStatusView {
	cached, err := h.DB.ListGPUJobLogs(10)
	if err != nil {
		if status.Error == "" {
			status.Error = fmt.Sprintf("cached GPU jobs unavailable: %v", err)
		}
		return status
	}
	seen := map[string]bool{}
	for _, job := range status.Jobs {
		seen[job.UnitName] = true
	}
	for _, cachedJob := range cached {
		if seen[cachedJob.UnitName] {
			continue
		}
		status.Jobs = append(status.Jobs, gpuJobViewFromDB(cachedJob))
		seen[cachedJob.UnitName] = true
		if len(status.Jobs) >= 10 {
			break
		}
	}
	return status
}

func (h *Handler) gpuJob(ctx context.Context, host, unit string, includeJournal bool) gpuJobView {
	job := gpuJobView{UnitName: unit, Host: host}
	show, err := remoteSSH(ctx, host, "systemctl show "+shellQuote(unit)+" --property=Description --property=LoadState --property=ActiveState --property=SubState --property=Result --property=ActiveEnterTimestamp")
	if err != nil {
		fileJob := h.gpuFileJob(ctx, host, unit, includeJournal)
		if fileJob.LoadedState != "" || fileJob.ActiveState != "" || fileJob.Journal != "" {
			return fileJob
		}
		job.Error = appendGPUError(job.Error, fmt.Errorf("systemctl show: %w: %s", err, strings.TrimSpace(show)))
	} else {
		populateGPUJobState(&job, show)
		if job.LoadedState == "not-found" {
			// systemd reports an absent unit as inactive, which does not mean
			// a render completed successfully. Durable results decide that.
			job.ActiveState = ""
			job.Result = ""
		}
	}
	if includeJournal {
		journal, err := remoteSSH(ctx, host, "journalctl -u "+shellQuote(unit)+" --no-pager --output=short-iso -n 300")
		job.Journal = strings.TrimSpace(journal)
		if err != nil {
			job.Error = appendGPUError(job.Error, fmt.Errorf("journalctl: %w: %s", err, strings.TrimSpace(journal)))
		}
	}
	return job
}

func (h *Handler) gpuFileJob(ctx context.Context, host, unit string, includeJournal bool) gpuJobView {
	job := gpuJobView{UnitName: unit, Host: host, LoadedState: "loaded"}
	jobdir := "/root/streamctl-gpu-jobs/" + unit
	metaCmd := strings.Join([]string{
		"jobdir=" + shellQuote(jobdir),
		"[ -d \"$jobdir\" ] || exit 1",
		"active=$(tr -d '[:space:]' < \"$jobdir/active\" 2>/dev/null || true)",
		"pid=$(tr -d '[:space:]' < \"$jobdir/pid\" 2>/dev/null || true)",
		"if [ \"$active\" = running ] && { [ -z \"$pid\" ] || ! kill -0 \"$pid\" 2>/dev/null; }; then printf 'failed\\n' > \"$jobdir/active\"; printf 'failed\\n' > \"$jobdir/sub\"; printf 'exit-code\\n' > \"$jobdir/result\"; fi",
		"for f in description raw_path active sub result since; do printf '%s=' \"$f\"; tr '\\n' ' ' < \"$jobdir/$f\" 2>/dev/null || true; printf '\\n'; done",
	}, "; ")
	meta, err := remoteSSH(ctx, host, metaCmd)
	if err != nil {
		job.Error = appendGPUError(job.Error, fmt.Errorf("remote job metadata: %w: %s", err, strings.TrimSpace(meta)))
		return job
	}
	for _, line := range strings.Split(meta, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "description":
			job.Description = value
		case "raw_path":
			job.RawPath = value
		case "active":
			job.ActiveState = value
		case "sub":
			job.SubState = value
		case "result":
			job.Result = value
		case "since":
			job.Since = value
		}
	}
	if includeJournal {
		journal, err := remoteSSH(ctx, host, "cat "+shellQuote(jobdir+"/journal")+" 2>/dev/null || true")
		job.Journal = strings.TrimSpace(journal)
		if err != nil {
			job.Error = appendGPUError(job.Error, fmt.Errorf("remote job log: %w: %s", err, strings.TrimSpace(journal)))
		}
	}
	return job
}

func (h *Handler) saveGPUJobLog(job gpuJobView) error {
	if strings.TrimSpace(job.UnitName) == "" {
		return nil
	}
	h.hydrateGPUJobFromCache(&job)
	return h.DB.UpsertGPUJobLog(db.GPUJobLog{
		UnitName:    job.UnitName,
		RawPath:     job.RawPath,
		Host:        job.Host,
		Description: job.Description,
		ActiveState: job.ActiveState,
		SubState:    job.SubState,
		Result:      job.Result,
		Journal:     job.Journal,
		Error:       job.Error,
	})
}

func gpuJobViewFromDB(job db.GPUJobLog) gpuJobView {
	return gpuJobView{
		UnitName:    job.UnitName,
		RawPath:     job.RawPath,
		Host:        job.Host,
		Description: job.Description,
		ActiveState: job.ActiveState,
		SubState:    job.SubState,
		Result:      job.Result,
		Journal:     job.Journal,
		Error:       job.Error,
		Since:       job.UpdatedAt.Format(time.RFC3339),
	}
}

func isTerminalGPUJob(job gpuJobView) bool {
	return job.ActiveState == "inactive" || job.ActiveState == "failed"
}

func isBlockingGPUJob(job gpuJobView) bool {
	state := strings.TrimSpace(job.ActiveState)
	if state == "" || state == "queued" {
		return false
	}
	return !isTerminalGPUJob(job)
}

func formatOptionalTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format("2006-01-02 15:04")
}

func managedGPUWorkerDefinitelyUnavailable(worker gpuWorkerView) bool {
	if !worker.Managed {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(worker.Status)) {
	case "inactive", "not found", "off", "archived", "exited", "terminated":
		return true
	default:
		return false
	}
}

func (h *Handler) gpuQueueDispatcher() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		h.dispatchGPUQueueOnce(context.Background())
		<-ticker.C
	}
}

func (h *Handler) dispatchGPUQueueOnce(ctx context.Context) {
	h.gpuQueueMu.Lock()
	defer h.gpuQueueMu.Unlock()
	if !h.gpuWorkerConfigured() {
		return
	}
	worker := h.gpuWorkerView(ctx)
	if worker.Managed && worker.Status != "active" {
		h.reconcileUnavailableRenderQueue(worker)
		h.reconcileUnavailableProxyQueue(worker)
		queued := 0
		if counts, err := h.DB.RenderQueueStatusCounts(); err == nil {
			queued += counts["queued"]
		}
		if counts, err := h.DB.ProductionProxyQueue(1); err == nil {
			queued += counts.Queued
		}
		if queued > 0 && h.hasRunPodToken() && worker.Status == "not found" {
			if err := h.ensureRunPodWorker(ctx, h.RunPodGPUType); err != nil {
				log.Printf("creating RunPod worker: %v", err)
			}
		}
		return
	}
	host := strings.TrimSpace(h.GPUWorkerHost)
	if host == "" {
		host = worker.SSHHost
	}
	if host == "" {
		return
	}
	status := h.gpuStatus(ctx, worker)
	h.reconcileRenderJobs(ctx, host, status.Jobs, status.Available && status.Error == "")
	h.reconcileProxyJobs(ctx)
	if !status.Available || status.Error != "" {
		return
	}
	if counts, err := h.DB.ProductionProxyQueue(1); err != nil || counts.Running > 0 {
		return
	}
	if counts, err := h.DB.RenderQueueStatusCounts(); err != nil || counts["running"] > 0 {
		return
	}
	for _, job := range status.Jobs {
		if isBlockingGPUJob(job) {
			return
		}
	}
	if h.dispatchNextProxy(ctx, host) {
		return
	}
	h.dispatchNextRender(ctx, host)
}

func gpuFailureJournalSummary(journal string) string {
	lines := strings.Split(strings.TrimSpace(journal), "\n")
	var out []string
	for i := len(lines) - 1; i >= 0 && len(out) < 5; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return strings.Join(out, "\n")
}

func (h *Handler) destroyManagedGPUAfterTerminalJob(ctx context.Context, job gpuJobView) {
	if strings.TrimSpace(h.DOTokenFile) == "" && !h.hasRunPodToken() {
		return
	}
	failed := job.ActiveState == "failed" || (job.Result != "" && job.Result != "success")
	shouldDestroy := failed || h.GPUDestroyAfterJob
	if !shouldDestroy {
		return
	}
	renderCounts, err := h.DB.RenderQueueStatusCounts()
	if err != nil {
		log.Printf("GPU worker cleanup skipped after job %s: checking render queue failed: %v", job.UnitName, err)
		return
	}
	if renderCounts["queued"] > 0 || renderCounts["running"] > 0 {
		log.Printf("GPU worker cleanup skipped after job %s: render queue still has open work", job.UnitName)
		return
	}
	proxyCounts, err := h.DB.ProductionProxyQueue(1)
	if err != nil || proxyCounts.Queued > 0 || proxyCounts.Running > 0 {
		return
	}
	if h.hasRunPodToken() {
		client, err := h.runpodClient()
		if err != nil {
			log.Printf("RunPod worker cleanup skipped for %s: %v", job.UnitName, err)
			return
		}
		pods, err := client.ListPods(ctx)
		if err != nil {
			log.Printf("listing RunPod workers for cleanup failed: %v", err)
			return
		}
		for _, pod := range pods {
			if pod.Name != h.runpodPodName() {
				continue
			}
			log.Printf("destroying RunPod worker %s after job %s active=%s result=%s", pod.ID, job.UnitName, job.ActiveState, job.Result)
			if err := client.DeletePod(ctx, pod.ID); err != nil {
				log.Printf("destroying RunPod worker %s failed: %v", pod.ID, err)
			}
		}
		return
	}
	client, err := h.doClient()
	if err != nil {
		log.Printf("GPU worker cleanup skipped for %s: %v", job.UnitName, err)
		return
	}
	droplets, err := client.ListDropletsByTag(ctx, h.gpuWorkerTag())
	if err != nil {
		log.Printf("listing GPU workers for cleanup failed: %v", err)
		return
	}
	for _, d := range droplets {
		log.Printf("destroying GPU worker %d after job %s active=%s result=%s", d.ID, job.UnitName, job.ActiveState, job.Result)
		if err := client.DeleteDroplet(ctx, d.ID); err != nil {
			log.Printf("destroying GPU worker %d failed: %v", d.ID, err)
		}
	}
}

func remoteSSH(ctx context.Context, host, remoteCommand string) (string, error) {
	args := sshArgsWithOptions(host, remoteCommand, "-o", "ConnectTimeout=5")
	cmd := exec.CommandContext(ctx, "ssh", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func waitForRemoteSSH(ctx context.Context, host string, timeout time.Duration) (string, error) {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastOut string
	var lastErr error
	for {
		probeCtx, probeCancel := context.WithTimeout(deadline, 8*time.Second)
		out, err := remoteSSH(probeCtx, host, "true")
		probeCancel()
		if err == nil {
			return out, nil
		}
		lastOut = out
		lastErr = err
		select {
		case <-deadline.Done():
			if lastErr == nil {
				lastErr = deadline.Err()
			}
			return lastOut, lastErr
		case <-time.After(3 * time.Second):
		}
	}
}

func sshArgs(target, remoteCommand string) []string {
	return sshArgsWithOptions(target, remoteCommand)
}

func sshArgsWithOptions(target, remoteCommand string, extraOptions ...string) []string {
	args := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new"}
	args = append(args, extraOptions...)
	if u, err := url.Parse(target); err == nil && u.Scheme == "ssh" && u.Hostname() != "" {
		if identity := strings.TrimSpace(u.Query().Get("identity")); identity != "" {
			args = append(args, "-i", identity)
		}
		if port := u.Port(); port != "" {
			args = append(args, "-p", port)
		}
		userHost := u.Hostname()
		if u.User != nil && u.User.Username() != "" {
			userHost = u.User.Username() + "@" + userHost
		}
		args = append(args, userHost, remoteCommand)
		return args
	}
	return append(args, target, remoteCommand)
}

func parseGPUUnitList(out string) []string {
	var units []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		unit := fields[0]
		if validGPUUnitName(unit) && !seen[unit] {
			units = append(units, unit)
			seen[unit] = true
		}
	}
	return units
}

func parseGPUFileJobList(out string) []string {
	var units []string
	for _, line := range strings.Split(out, "\n") {
		unit := strings.TrimSpace(line)
		if validGPUUnitName(unit) {
			units = append(units, unit)
		}
	}
	return units
}

func dedupeGPUUnits(units []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, unit := range units {
		if !validGPUUnitName(unit) || seen[unit] {
			continue
		}
		seen[unit] = true
		out = append(out, unit)
	}
	return out
}

func populateGPUJobState(job *gpuJobView, out string) {
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "Description":
			job.Description = val
		case "LoadState":
			job.LoadedState = val
		case "ActiveState":
			job.ActiveState = val
		case "SubState":
			job.SubState = val
		case "Result":
			job.Result = val
		case "ActiveEnterTimestamp":
			job.Since = val
		}
	}
}

func validGPUUnitName(unit string) bool {
	if !strings.HasPrefix(unit, "streamctl-gpu-") {
		return false
	}
	for _, r := range unit {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' || r == '_' {
			continue
		}
		return false
	}
	return len(unit) <= 128
}

func appendGPUError(existing string, err error) string {
	if err == nil {
		return existing
	}
	if existing == "" {
		return err.Error()
	}
	return existing + "; " + err.Error()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func (h *Handler) gpuWorkerConfigured() bool {
	return strings.TrimSpace(h.GPUWorkerHost) != "" || h.hasRunPodToken() || strings.TrimSpace(h.DOTokenFile) != ""
}

func (h *Handler) gpuWorkerSSHHost(ctx context.Context) (string, error) {
	if strings.TrimSpace(h.GPUWorkerHost) != "" {
		return strings.TrimSpace(h.GPUWorkerHost), nil
	}
	worker := h.gpuWorkerView(ctx)
	if worker.Error != "" {
		return "", fmt.Errorf("%s", worker.Error)
	}
	if worker.IP == "" || worker.Status != "active" {
		return "", fmt.Errorf("managed GPU worker is not active: %s", firstNonEmptyString(worker.Status, "unknown"))
	}
	return worker.SSHHost, nil
}

func (h *Handler) gpuWorkerView(ctx context.Context) gpuWorkerView {
	view := gpuWorkerView{
		Managed:    strings.TrimSpace(h.DOTokenFile) != "" || h.hasRunPodToken(),
		Configured: h.gpuWorkerConfigured(),
		SSHHost:    strings.TrimSpace(h.GPUWorkerHost),
	}
	if !view.Managed {
		if strings.TrimSpace(view.SSHHost) != "" {
			view.Provider = "external"
		}
		return view
	}
	if h.hasRunPodToken() {
		return h.runpodWorkerView(ctx, view)
	}
	view.Provider = "DigitalOcean"
	client, err := h.doClient()
	if err != nil {
		view.Status = "api error"
		view.Error = err.Error()
		return view
	}
	droplets, err := client.ListDropletsByTag(ctx, h.gpuWorkerTag())
	if err != nil {
		view.Status = "api error"
		view.Error = err.Error()
		return view
	}
	if len(droplets) == 0 {
		view.Status = "not found"
		return view
	}
	d := droplets[0]
	view.ID = d.ID
	view.ExternalID = strconv.FormatInt(d.ID, 10)
	view.Name = d.Name
	view.RawStatus = d.Status
	view.Status = d.Status
	view.IP = d.PublicIPv4()
	view.GPUType = d.SizeSlug
	view.Region = d.Region.Slug
	view.Image = h.GPUDropletImage
	view.CreatedAt = d.CreatedAt
	if view.IP != "" {
		view.SSHHost = firstNonEmptyString(h.GPUWorkerUser, "root") + "@" + view.IP
	}
	return h.withManagedWorkerSSHReadiness(ctx, view)
}

func (h *Handler) gpuWorkerCreate(w http.ResponseWriter, r *http.Request) {
	if h.hasRunPodToken() {
		h.runpodWorkerCreate(w, r)
		return
	}
	client, err := h.doClient()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	size := firstNonEmptyString(r.FormValue("size"), h.GPUDropletSize, "gpu-h100x1-80gb")
	region := firstNonEmptyString(r.FormValue("region"), h.GPUDropletRegion, "nyc3")
	if err := h.validateGPUSizeRegion(r.Context(), client, size, region); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	existing, err := client.ListDropletsByTag(r.Context(), h.gpuWorkerTag())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if len(existing) > 0 {
		http.Redirect(w, r, "/worker", http.StatusSeeOther)
		return
	}
	key, err := h.gpuSSHKey(r.Context(), client)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	userData, err := h.gpuWorkerUserData()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := client.CreateDroplet(r.Context(), doclient.CreateDropletRequest{
		Name:       h.gpuDropletName(),
		Region:     region,
		Size:       size,
		Image:      firstNonEmptyString(h.GPUDropletImage, "gpu-h100x1-base"),
		SSHKeys:    []string{key.Fingerprint},
		Monitoring: true,
		Tags:       []string{h.gpuWorkerTag(), "streamctl"},
		UserData:   userData,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/worker", http.StatusSeeOther)
}

func (h *Handler) gpuAvailability(ctx context.Context) gpuAvailabilityView {
	view := gpuAvailabilityView{
		DefaultSize:   firstNonEmptyString(h.GPUDropletSize, "gpu-h100x1-80gb"),
		DefaultRegion: firstNonEmptyString(h.GPUDropletRegion, "nyc2"),
	}
	if h.hasRunPodToken() {
		return h.runpodAvailability(view)
	}
	if strings.TrimSpace(h.DOTokenFile) == "" {
		return view
	}
	client, err := h.doClient()
	if err != nil {
		view.Error = err.Error()
		return view
	}
	sizes, err := client.ListSizes(ctx)
	if err != nil {
		view.Error = err.Error()
		return view
	}
	gpuSizeCount := 0
	for _, size := range sizes {
		if !isGPUSize(size) {
			continue
		}
		gpuSizeCount++
		regions := append([]string(nil), size.Regions...)
		if len(regions) == 0 {
			continue
		}
		sort.Strings(regions)
		view.Sizes = append(view.Sizes, gpuSizeView{
			Slug:          size.Slug,
			Description:   size.Description,
			PriceHourly:   size.PriceHourly,
			Regions:       regions,
			DefaultSize:   size.Slug == view.DefaultSize,
			DefaultRegion: containsString(regions, view.DefaultRegion),
		})
	}
	sort.Slice(view.Sizes, func(i, j int) bool {
		return view.Sizes[i].Slug < view.Sizes[j].Slug
	})
	for _, size := range view.Sizes {
		if size.Slug == view.DefaultSize {
			view.SelectedSize = size.Slug
			view.SelectedRegions = size.Regions
			if !containsString(size.Regions, view.DefaultRegion) && len(size.Regions) > 0 {
				view.DefaultRegion = size.Regions[0]
			}
			return view
		}
	}
	if len(view.Sizes) > 0 {
		view.SelectedSize = view.Sizes[0].Slug
		view.SelectedRegions = view.Sizes[0].Regions
		if !containsString(view.SelectedRegions, view.DefaultRegion) && len(view.SelectedRegions) > 0 {
			view.DefaultRegion = view.SelectedRegions[0]
		}
	} else if gpuSizeCount > 0 {
		view.Error = "DigitalOcean reports GPU sizes for this account, but no GPU regions are currently createable via the Droplet API. This usually means GPU Droplet access/capacity is not enabled for the account or team."
	}
	return view
}

func (h *Handler) runpodAvailability(view gpuAvailabilityView) gpuAvailabilityView {
	view.DefaultSize = firstNonEmptyString(h.RunPodGPUType, "NVIDIA L40S")
	for _, size := range []gpuSizeView{
		{Slug: "NVIDIA L40S", Description: "NVIDIA L40S", PriceHourly: 0},
		{Slug: "NVIDIA RTX 4090", Description: "NVIDIA RTX 4090", PriceHourly: 0},
		{Slug: "NVIDIA RTX A6000", Description: "NVIDIA RTX A6000", PriceHourly: 0},
		{Slug: "NVIDIA A40", Description: "NVIDIA A40", PriceHourly: 0},
		{Slug: "NVIDIA H100 PCIe", Description: "NVIDIA H100 PCIe", PriceHourly: 0},
	} {
		size.DefaultSize = size.Slug == view.DefaultSize
		view.Sizes = append(view.Sizes, size)
	}
	view.SelectedSize = view.DefaultSize
	return view
}

func (h *Handler) validateGPUSizeRegion(ctx context.Context, client *doclient.Client, size, region string) error {
	sizes, err := client.ListSizes(ctx)
	if err != nil {
		return fmt.Errorf("listing GPU sizes: %w", err)
	}
	for _, candidate := range sizes {
		if candidate.Slug != size {
			continue
		}
		if !isGPUSize(candidate) {
			return fmt.Errorf("size %s is not a GPU size", size)
		}
		if len(candidate.Regions) == 0 {
			return fmt.Errorf("DigitalOcean reports GPU size %s, but no createable regions for it on this account", size)
		}
		if !containsString(candidate.Regions, region) {
			return fmt.Errorf("size %s is not available in region %s; available regions: %s", size, region, strings.Join(candidate.Regions, ", "))
		}
		return nil
	}
	return fmt.Errorf("GPU size %s not found", size)
}

func (h *Handler) runpodWorkerView(ctx context.Context, view gpuWorkerView) gpuWorkerView {
	view.Provider = "RunPod"
	view.GPUType = h.RunPodGPUType
	view.Image = h.RunPodImage
	view.CloudType = h.RunPodCloudType
	client, err := h.runpodClient()
	if err != nil {
		view.Status = "api error"
		view.Error = err.Error()
		return view
	}
	pods, err := client.ListPods(ctx)
	if err != nil {
		view.Status = "api error"
		view.Error = err.Error()
		return view
	}
	for _, pod := range pods {
		if pod.Name != h.runpodPodName() {
			continue
		}
		view.ID = 1
		view.ExternalID = pod.ID
		view.Name = pod.Name
		view.RawStatus = pod.Status()
		view.Status = view.RawStatus
		view.CostHourly = pod.CostPerHr
		view.MachineID = pod.MachineID
		view.GPUType = firstNonEmptyString(pod.GPUTypeID, view.GPUType)
		view.Image = firstNonEmptyString(pod.ImageName, view.Image)
		view.CreatedAt = pod.CreatedAt
		view.SSHHost = pod.SSHHost(firstNonEmptyString(h.GPUWorkerUser, "root"))
		if view.SSHHost != "" {
			view.SSHHost = appendSSHIdentity(view.SSHHost, h.gpuSSHKeyPath())
		}
		ip, _ := pod.SSHAddress()
		view.IP = ip
		return h.withManagedWorkerSSHReadiness(ctx, view)
	}
	view.Status = "not found"
	return view
}

func (h *Handler) withManagedWorkerSSHReadiness(ctx context.Context, view gpuWorkerView) gpuWorkerView {
	if view.Status != "active" {
		return view
	}
	if strings.TrimSpace(view.SSHHost) == "" {
		view.Status = "starting"
		return view
	}
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := remoteSSH(probeCtx, view.SSHHost, "if [ -f /root/.streamctl-worker-setup-failed ]; then printf 'setup-failed'; exit 1; fi; test -f /root/.streamctl-worker-ready")
	if err != nil {
		view.Status = "starting"
		view.Error = fmt.Sprintf("worker setup not ready: %v: %s", err, strings.TrimSpace(out))
		if strings.TrimSpace(out) == "setup-failed" {
			view.Status = "setup failed"
			view.Error = "Worker setup failed or timed out; inspect /root/streamctl-worker-setup.log, then destroy the worker before retrying."
		}
	}
	return view
}

func (h *Handler) runpodWorkerCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	gpuType := firstNonEmptyString(r.FormValue("size"), h.RunPodGPUType, "NVIDIA L40S")
	if err := h.ensureRunPodWorker(r.Context(), gpuType); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/worker", http.StatusSeeOther)
}

func (h *Handler) ensureRunPodWorker(ctx context.Context, gpuType string) error {
	// Manual creation and queue dispatch must share the list-before-create
	// check; RunPod names are not unique and concurrent checks can race.
	h.gpuCreateMu.Lock()
	defer h.gpuCreateMu.Unlock()
	client, err := h.runpodClient()
	if err != nil {
		return err
	}
	pods, err := client.ListPods(ctx)
	if err != nil {
		return err
	}
	for _, pod := range pods {
		if pod.Name == h.runpodPodName() {
			return nil
		}
	}
	sshPublicKey, err := ensureGPUWorkerSSHPublicKey(h.gpuSSHKeyPath())
	if err != nil {
		return fmt.Errorf("reading SSH public key for RunPod: %v", err)
	}
	env, err := h.runpodWorkerEnv(sshPublicKey)
	if err != nil {
		return err
	}
	gpuType = firstNonEmptyString(gpuType, h.RunPodGPUType, "NVIDIA L40S")
	if _, err := client.CreatePod(ctx, runpodclient.CreatePodRequest{
		Name:              h.runpodPodName(),
		ImageName:         firstNonEmptyString(h.RunPodImage, "runpod/pytorch:2.4.0-py3.11-cuda12.4.1-devel-ubuntu22.04"),
		GPUTypeIDs:        []string{gpuType},
		GPUCount:          1,
		CloudType:         firstNonEmptyString(h.RunPodCloudType, "SECURE"),
		ContainerDiskInGB: 80,
		VolumeInGB:        80,
		Ports:             []string{"22/tcp"},
		Env:               env,
		DockerStartCmd: []string{"bash", "-lc", boundedWorkerSetup(strings.Join([]string{
			"set -euo pipefail",
			"apt-get update",
			"DEBIAN_FRONTEND=noninteractive apt-get install -y python3 openssh-server rclone ffmpeg ca-certificates util-linux curl git",
			"mkdir -p /run/sshd /root/.ssh",
			"printf '%s\n' \"$SSH_PUBLIC_KEY\" > /root/.ssh/authorized_keys",
			"chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys",
			"printf '%s' \"$RCLONE_CONFIG_B64\" | base64 -d > /root/rclone.conf",
			"printf '%s' \"$RENDER_SCRIPT_B64\" | base64 -d > /root/render-from-spaces.py",
			"chmod 400 /root/rclone.conf && chmod 755 /root/render-from-spaces.py",
			"curl -LsSf https://astral.sh/uv/install.sh | env UV_INSTALL_DIR=/root/.local/bin sh",
			"git clone " + shellQuote(confRenderRepository) + " /root/conf-render",
			"git -C /root/conf-render checkout " + shellQuote(confRenderRevision),
			confRenderCompatibilityCommand(),
			"/root/.local/bin/uv sync --frozen --directory /root/conf-render",
			"timeout --kill-after=30s 180s /root/render-from-spaces.py --check",
			"touch /root/.streamctl-worker-ready",
		}, " && "))},
	}); err != nil {
		return err
	}
	return nil
}

func (h *Handler) runpodWorkerDestroy(w http.ResponseWriter, r *http.Request) {
	client, err := h.runpodClient()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pods, err := client.ListPods(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	for _, pod := range pods {
		if pod.Name != h.runpodPodName() {
			continue
		}
		if err := client.DeletePod(r.Context(), pod.ID); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	http.Redirect(w, r, "/worker", http.StatusSeeOther)
}

func (h *Handler) runpodWorkerEnv(sshPublicKey string) (map[string]string, error) {
	rcloneConfig, err := os.ReadFile(h.RcloneConfig)
	if err != nil {
		return nil, fmt.Errorf("reading rclone config: %w", err)
	}
	return map[string]string{
		"NVIDIA_DRIVER_CAPABILITIES": "compute,utility,video",
		"SSH_PUBLIC_KEY":             strings.TrimSpace(sshPublicKey),
		"RCLONE_CONFIG_B64":          base64.StdEncoding.EncodeToString(rcloneConfig),
		"RENDER_SCRIPT_B64":          base64.StdEncoding.EncodeToString([]byte(renderWorkerScript())),
	}, nil
}

func (h *Handler) runpodClient() (*runpodclient.Client, error) {
	token, err := readTokenFile(h.RunPodTokenFile, "RunPod")
	if err != nil {
		return nil, err
	}
	return runpodclient.New(token), nil
}

func (h *Handler) runpodPodName() string {
	return firstNonEmptyString(h.RunPodPodName, h.gpuDropletName(), "streamctl-gpu-worker")
}

func (h *Handler) hasRunPodToken() bool {
	tokenFile := strings.TrimSpace(h.RunPodTokenFile)
	if tokenFile == "" {
		return false
	}
	data, err := os.ReadFile(tokenFile)
	return err == nil && strings.TrimSpace(string(data)) != ""
}

func isGPUSize(size doclient.Size) bool {
	slug := strings.ToLower(size.Slug)
	desc := strings.ToLower(size.Description)
	return strings.Contains(slug, "gpu") || strings.Contains(desc, "gpu")
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (h *Handler) gpuWorkerDestroy(w http.ResponseWriter, r *http.Request) {
	if h.hasRunPodToken() {
		h.runpodWorkerDestroy(w, r)
		return
	}
	client, err := h.doClient()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	droplets, err := client.ListDropletsByTag(r.Context(), h.gpuWorkerTag())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	for _, d := range droplets {
		if err := client.DeleteDroplet(r.Context(), d.ID); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	http.Redirect(w, r, "/worker", http.StatusSeeOther)
}

func (h *Handler) doClient() (*doclient.Client, error) {
	token, err := h.doToken()
	if err != nil {
		return nil, err
	}
	return doclient.New(token), nil
}

func (h *Handler) doToken() (string, error) {
	return readTokenFile(h.DOTokenFile, "DigitalOcean")
}

func readTokenFile(file, label string) (string, error) {
	if strings.TrimSpace(file) == "" {
		return "", fmt.Errorf("%s token file is not configured", label)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("reading %s token file: %w", label, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("%s token file is empty", label)
	}
	return token, nil
}

func readFirstExistingFile(files ...string) (string, error) {
	var errs []string
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err == nil && strings.TrimSpace(string(data)) != "" {
			return strings.TrimSpace(string(data)), nil
		}
		if err != nil {
			errs = append(errs, file+": "+err.Error())
		}
	}
	return "", errors.New(strings.Join(errs, "; "))
}

func (h *Handler) gpuSSHKeyPath() string {
	return firstNonEmptyString(h.GPUWorkerSSHKey, gpuWorkerSSHKeyPath)
}

func ensureGPUWorkerSSHPublicKey(privateKey string) (string, error) {
	publicKey := privateKey + ".pub"
	data, err := os.ReadFile(publicKey)
	if err == nil && strings.TrimSpace(string(data)) != "" {
		return strings.TrimSpace(string(data)), nil
	}
	if err := os.MkdirAll(filepath.Dir(privateKey), 0700); err != nil {
		return "", err
	}
	out, err := exec.Command(
		"ssh-keygen",
		"-t", "ed25519",
		"-N", "",
		"-C", "streamctl-gpu-worker",
		"-f", privateKey,
	).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("generating GPU worker SSH key: %w: %s", err, strings.TrimSpace(string(out)))
	}
	_ = os.Chmod(privateKey, 0600)
	_ = os.Chmod(publicKey, 0644)
	data, err = os.ReadFile(publicKey)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func appendSSHIdentity(target, identity string) string {
	if strings.TrimSpace(identity) == "" {
		return target
	}
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "ssh" {
		return target
	}
	q := u.Query()
	if q.Get("identity") == "" {
		q.Set("identity", identity)
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func (h *Handler) gpuSSHKey(ctx context.Context, client *doclient.Client) (*doclient.SSHKey, error) {
	keys, err := client.ListSSHKeys(ctx)
	if err != nil {
		return nil, err
	}
	want := strings.TrimSpace(h.GPUSSHKeyName)
	for _, key := range keys {
		if key.Name == want || key.Fingerprint == want || strconv.FormatInt(key.ID, 10) == want {
			return &key, nil
		}
	}
	return nil, fmt.Errorf("DigitalOcean SSH key %q not found", want)
}

func (h *Handler) gpuWorkerUserData() (string, error) {
	rcloneConfig, err := os.ReadFile(h.RcloneConfig)
	if err != nil {
		return "", fmt.Errorf("reading rclone config: %w", err)
	}
	renderScript := renderWorkerScript()
	return fmt.Sprintf(`#!/usr/bin/env bash
set -euxo pipefail
DROPLET_ID="$(curl -fsS http://169.254.169.254/metadata/v1/id || true)"
apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y python3 ffmpeg rclone curl util-linux git
install -d -m 0700 /root
base64 -d >/root/rclone.conf <<'STREAMCTL_RCLONE'
%s
STREAMCTL_RCLONE
base64 -d >/root/render-from-spaces.py <<'STREAMCTL_RENDER_SCRIPT'
%s
STREAMCTL_RENDER_SCRIPT
chmod 0400 /root/rclone.conf
chmod 0755 /root/render-from-spaces.py
curl -LsSf https://astral.sh/uv/install.sh | env UV_INSTALL_DIR=/root/.local/bin sh
git clone %s /root/conf-render
git -C /root/conf-render checkout %s
%s
/root/.local/bin/uv sync --frozen --directory /root/conf-render
timeout --kill-after=30s 180s /root/render-from-spaces.py --check
echo "$DROPLET_ID" >/root/droplet-id
touch /root/.streamctl-worker-ready
echo 'streamctl GPU worker ready'
`, base64.StdEncoding.EncodeToString(rcloneConfig), base64.StdEncoding.EncodeToString([]byte(renderScript)), shellQuote(confRenderRepository), shellQuote(confRenderRevision), confRenderCompatibilityCommand()), nil
}

func (h *Handler) gpuWorkerTag() string {
	return "streamctl-gpu-worker"
}

func (h *Handler) gpuDropletName() string {
	return firstNonEmptyString(h.GPUDropletName, "streamctl-gpu-worker")
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func renderWorkerScript() string { return renderWorkerScriptText }

func (h *Handler) listSpacesFilesOnly(ctx context.Context, prefix string) ([]spacesEntry, error) {
	_, files, err := h.listSpacesPrefix(ctx, prefix)
	return files, err
}

func (h *Handler) listSpacesFilesRecursive(ctx context.Context, prefix string) ([]spacesEntry, error) {
	lines, err := h.rcloneLsf(ctx, prefix, "--files-only", "--recursive")
	if err != nil {
		return nil, err
	}
	var files []spacesEntry
	for _, line := range lines {
		line = strings.TrimSpace(strings.ReplaceAll(line, "\\", "/"))
		if line == "" || strings.HasSuffix(line, "/") {
			continue
		}
		name := path.Base(line)
		if !isVideoFile(name) {
			continue
		}
		files = append(files, spacesEntry{Name: name, Path: prefix + line})
	}
	sort.Slice(files, func(i, j int) bool {
		return strings.ToLower(files[i].Path) < strings.ToLower(files[j].Path)
	})
	return files, nil
}

// ---------- Nostr ----------

func (h *Handler) nostr(w http.ResponseWriter, r *http.Request) {
	keys, err := h.DB.ListNostrKeys()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	relays, err := h.DB.ListNostrRelays()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.render(w, r, "nostr.html", map[string]any{
		"Keys":   keys,
		"Relays": relays,
		"Error":  r.URL.Query().Get("err"),
	})
}

func (h *Handler) nostrKeyCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Redirect(w, r, "/nostr?err="+url.QueryEscape("key name required"), http.StatusSeeOther)
		return
	}
	info, err := nostrpub.DecodeKey(r.FormValue("nsec"))
	if err != nil {
		http.Redirect(w, r, "/nostr?err="+url.QueryEscape("invalid nsec: "+err.Error()), http.StatusSeeOther)
		return
	}
	if err := os.MkdirAll(h.NostrKeyDir, 0750); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, err := os.CreateTemp(h.NostrKeyDir, "nostr-*.key")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	secretPath := f.Name()
	_ = f.Close()
	if err := nostrpub.WriteSecretFile(secretPath, info.Secret); err != nil {
		_ = os.Remove(secretPath)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.chownNostrKey(secretPath); err != nil {
		_ = os.Remove(secretPath)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, err = h.DB.CreateNostrKey(&db.NostrKey{
		Name:       name,
		PubKey:     info.PubKey,
		Npub:       info.Npub,
		SecretPath: secretPath,
		Enabled:    true,
	})
	if err != nil {
		_ = os.Remove(secretPath)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/nostr", http.StatusSeeOther)
}

func (h *Handler) nostrKeyUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/nostr/keys/update/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	key, err := h.DB.GetNostrKey(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	key.Name = strings.TrimSpace(r.FormValue("name"))
	key.Enabled = r.FormValue("enabled") == "on"
	if key.Name == "" {
		http.Redirect(w, r, "/nostr?err="+url.QueryEscape("key name required"), http.StatusSeeOther)
		return
	}
	if err := h.DB.UpdateNostrKey(key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.resyncNostrStreams(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/nostr", http.StatusSeeOther)
}

func (h *Handler) nostrKeyDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/nostr/keys/delete/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	key, err := h.DB.GetNostrKey(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err := h.DB.DeleteNostrKey(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = os.Remove(key.SecretPath)
	if err := h.resyncNostrStreams(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/nostr", http.StatusSeeOther)
}

func (h *Handler) nostrRelayCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	relay := &db.NostrRelay{
		URL:     normalizeRelayURL(r.FormValue("url")),
		Enabled: true,
	}
	if relay.URL == "" {
		http.Redirect(w, r, "/nostr?err="+url.QueryEscape("relay URL required"), http.StatusSeeOther)
		return
	}
	if _, err := h.DB.CreateNostrRelay(relay); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.resyncNostrStreams(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/nostr", http.StatusSeeOther)
}

func (h *Handler) nostrRelayUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/nostr/relays/update/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	relay := &db.NostrRelay{
		ID:      id,
		URL:     normalizeRelayURL(r.FormValue("url")),
		Enabled: r.FormValue("enabled") == "on",
	}
	if relay.URL == "" {
		http.Redirect(w, r, "/nostr?err="+url.QueryEscape("relay URL required"), http.StatusSeeOther)
		return
	}
	if err := h.DB.UpdateNostrRelay(relay); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.resyncNostrStreams(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/nostr", http.StatusSeeOther)
}

func (h *Handler) nostrRelayDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "/nostr/relays/delete/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := h.DB.DeleteNostrRelay(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.resyncNostrStreams(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/nostr", http.StatusSeeOther)
}

func (h *Handler) chownNostrKey(path string) error {
	owner := strings.TrimSpace(h.NostrKeyOwner)
	if owner == "" {
		return nil
	}
	u, err := user.Lookup(owner)
	if err != nil {
		return nil
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return nil
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return nil
	}
	return os.Chown(path, uid, gid)
}

func normalizeRelayURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if !strings.HasPrefix(value, "wss://") && !strings.HasPrefix(value, "ws://") {
		value = "wss://" + value
	}
	return value
}

func (h *Handler) resyncNostrStreams() error {
	streams, err := h.DB.ListStreams()
	if err != nil {
		return err
	}
	for _, s := range streams {
		if s.NostrEnabled {
			if err := h.Systemd.Sync(&s); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *Handler) listSpacesConferences(ctx context.Context) ([]spacesEntry, error) {
	lines, err := h.rcloneLsf(ctx, "", "--dirs-only")
	if err != nil {
		return nil, err
	}
	var dirs []spacesEntry
	for _, line := range lines {
		name := strings.Trim(strings.TrimSpace(line), "/")
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		dirs = append(dirs, spacesEntry{
			Name: name,
			Path: name + "/recordings/",
		})
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name)
	})
	return dirs, nil
}

func (h *Handler) listSpacesPrefix(ctx context.Context, prefix string) ([]spacesEntry, []spacesEntry, error) {
	lines, err := h.rcloneLsf(ctx, prefix)
	if err != nil {
		return nil, nil, err
	}
	var dirs []spacesEntry
	var files []spacesEntry
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name := strings.Trim(line, "/")
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		if strings.HasSuffix(line, "/") {
			dirs = append(dirs, spacesEntry{Name: name, Path: prefix + name + "/"})
			continue
		}
		if isVideoFile(name) {
			files = append(files, spacesEntry{Name: name, Path: prefix + name})
		}
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name)
	})
	sort.Slice(files, func(i, j int) bool {
		return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name)
	})
	return dirs, files, nil
}

func (h *Handler) rcloneLsf(ctx context.Context, prefix string, extraArgs ...string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	args := []string{"lsf", h.remotePath(prefix)}
	args = append(args, extraArgs...)
	cmd := exec.CommandContext(ctx, "rclone", args...)
	cmd.Env = rcloneEnv(h.RcloneConfig)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("rclone lsf %s: %s", prefix, msg)
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

func (h *Handler) rcloneLsl(ctx context.Context, prefix string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "rclone", "lsl", h.remotePath(prefix))
	cmd.Env = rcloneEnv(h.RcloneConfig)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("rclone lsl %s: %s", prefix, msg)
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

func (h *Handler) rcloneCat(ctx context.Context, prefix string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "rclone", "cat", h.remotePath(prefix))
	cmd.Env = rcloneEnv(h.RcloneConfig)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("rclone cat %s: %s", prefix, msg)
	}
	return out, nil
}

// rcloneEnv prevents personal shell filters from silently changing the files
// streamctl can see. Credentials and other rclone settings remain inherited.
func rcloneEnv(config string) []string {
	blocked := map[string]bool{
		"RCLONE_FILTER": true, "RCLONE_FILTER_FROM": true,
		"RCLONE_INCLUDE": true, "RCLONE_INCLUDE_FROM": true,
		"RCLONE_EXCLUDE": true, "RCLONE_EXCLUDE_FROM": true,
		"RCLONE_FILES_FROM": true, "RCLONE_FILES_FROM_RAW": true,
	}
	env := make([]string, 0, len(os.Environ())+1)
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		if !blocked[name] {
			env = append(env, item)
		}
	}
	if strings.TrimSpace(config) != "" {
		env = append(env, "RCLONE_CONFIG="+config)
	}
	return env
}

func (h *Handler) spacesFileExists(ctx context.Context, filePath string) (bool, error) {
	filePath = strings.Trim(strings.ReplaceAll(filePath, "\\", "/"), "/")
	if filePath == "" {
		return false, fmt.Errorf("empty file path")
	}
	want := path.Base(filePath)
	lines, err := h.rcloneLsl(ctx, filePath)
	if err != nil {
		return false, err
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.Trim(strings.TrimSpace(fields[len(fields)-1]), "/") == want {
			return true, nil
		}
	}
	return false, nil
}

func (h *Handler) remotePath(prefix string) string {
	remote := strings.TrimRight(strings.TrimSpace(h.Remote), "/")
	if strings.HasSuffix(remote, ":") || prefix == "" {
		return remote + prefix
	}
	return remote + "/" + prefix
}

// ---------- helpers ----------

// bitratesJSON probes each file for its container bitrate and returns a JSON
// object {"file.mp4": 9876543, ...} suitable for embedding in a <script> tag.
// Files that ffprobe can't read or that don't expose a bitrate are omitted;
// the client falls back to 0 for those, which is fine for an estimate.
func bitratesJSON(videoDir string, files []string) template.JS {
	out := make(map[string]int, len(files))
	for _, f := range files {
		bps, err := probe.Bitrate(videoDir, f)
		if err != nil || bps == 0 {
			continue
		}
		out[f] = bps
	}
	b, err := json.Marshal(out)
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(b)
}

func (h *Handler) localClipPath(source string) string {
	if isRemoteClip(source) {
		return filepath.Join(h.CacheDir, source)
	}
	return filepath.Join(h.VideoDir, source)
}

func isRemoteClip(source string) bool {
	return strings.Contains(source, "/")
}

func cleanSpacesPrefix(prefix string) (string, error) {
	clean, err := cleanBucketPrefix(prefix)
	if err != nil || clean == "" {
		return clean, err
	}
	parts := strings.Split(strings.TrimSuffix(clean, "/"), "/")
	if len(parts) < 2 || parts[1] != "recordings" {
		return "", fmt.Errorf("Spaces prefix must be inside <conference>/recordings")
	}
	return clean, nil
}

func cleanBucketPrefix(prefix string) (string, error) {
	prefix = strings.TrimSpace(strings.ReplaceAll(prefix, "\\", "/"))
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return "", nil
	}
	for _, part := range strings.Split(prefix, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("bad Spaces prefix: %q", prefix)
		}
	}
	clean := path.Clean(prefix)
	if clean == "." {
		return "", nil
	}
	return clean + "/", nil
}

func isVideoFile(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".mp4", ".mov", ".mkv", ".flv", ".ts":
		return true
	default:
		return false
	}
}

func isImageFile(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".bmp", ".tif", ".tiff":
		return true
	default:
		return false
	}
}

func isAudioFile(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".wav", ".mp3", ".m4a", ".aac", ".flac", ".ogg", ".opus":
		return true
	default:
		return false
	}
}

func mediaFileKind(name string) string {
	switch {
	case isVideoFile(name):
		return "video"
	case isImageFile(name):
		return "image"
	case isAudioFile(name):
		return "audio"
	default:
		return "other"
	}
}

func cleanClipSource(source string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", nil
	}
	if filepath.IsAbs(source) {
		return "", fmt.Errorf("video_file must be relative: %q", source)
	}
	clean := filepath.ToSlash(filepath.Clean(source))
	if clean == "." || clean == "" {
		return "", nil
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("video_file must not contain traversal: %q", source)
		}
	}
	return clean, nil
}

func normalizeOnCalendar(onCalendar string) string {
	onCalendar = strings.TrimSpace(onCalendar)
	if len(onCalendar) > len("UTC") && strings.HasSuffix(onCalendar, "UTC") {
		i := len(onCalendar) - len("UTC") - 1
		if i >= 0 && onCalendar[i] != ' ' && onCalendar[i] != '\t' {
			return onCalendar[:i+1] + " UTC"
		}
	}
	return onCalendar
}

func (h *Handler) listVideos() ([]string, error) {
	entries, err := os.ReadDir(h.VideoDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		switch ext {
		case ".mp4", ".mov", ".mkv", ".flv", ".ts":
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, name string, data any) {
	h.renderStatus(w, r, http.StatusOK, name, data)
}

func (h *Handler) renderStatus(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	tmpls, err := fs.Sub(templateFS, "templates")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	funcs := maps.Clone(h.funcs)
	funcs["recordingPolicyCatalog"] = func() map[string]productionRecordingNotice {
		catalog := map[string]productionRecordingNotice{}
		for _, policy := range []string{"full", "record", "audio_only", "prohibited", ""} {
			catalog[policy] = recordingPolicyNotice(policy)
		}
		return catalog
	}
	funcs["csrfField"] = func() template.HTML { return csrfField(r) }
	funcs["currentUser"] = func() string {
		if principal := requestAuthentication(r); principal != nil {
			return principal.DisplayName
		}
		return ""
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(tmpls, "layout.html", "production_media_browser.html", "production_media_preview.html", "production_project_preview.html", "production_clip_editor.html", "production_recording_policy.html", "broadcast_picker.html", "worker_jobs.html", "worker_panel.html", "live_updates.html", name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func pathID(r *http.Request, prefix string) (int64, bool) {
	tail := strings.TrimPrefix(r.URL.Path, prefix)
	id, err := strconv.ParseInt(tail, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}
