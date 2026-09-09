package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"streamctl/internal/btcppclient"
	"streamctl/internal/db"
)

type productionConferenceView struct {
	btcppclient.Conference
	Date string
}

type productionNavView struct {
	Active      string
	Action      string
	Conference  string
	Conferences []productionConferenceView
}

type productionHomePage struct {
	Nav           productionNavView
	Conference    string
	TalksTotal    int
	TalksCut      int
	ReadyToRender int
	Published     int
	RecordingsURL string
	ProxyCounts   db.ProductionProxyQueue
	Error         string
}

type productionTalkView struct {
	btcppclient.Candidate
	Cuts       []db.ProductionCut `json:"cuts"`
	CutCount   int                `json:"-"`
	URL        string             `json:"-"`
	DayLabel   string             `json:"dayLabel"`
	StageLabel string             `json:"stageLabel"`
	DateLabel  string             `json:"dateLabel"`
	TimeLabel  string             `json:"timeLabel"`
}

type productionTalksPage struct {
	Nav        productionNavView
	Conference string
	TalkGroups []productionTalkGroup
	Error      string
}

type productionTalkGroup struct {
	DayLabel  string
	DateLabel string
	Stages    []productionStageGroup
}

type productionStageGroup struct {
	StageLabel string
	Talks      []productionTalkView
}

type productionCutterJSON struct {
	Conference string               `json:"conference"`
	Talks      []productionTalkView `json:"talks"`
	Index      int                  `json:"index"`
}

type productionCutPage struct {
	Nav        productionNavView
	Conference string
	Talk       productionTalkView
	CutterJSON template.JS
	Previous   string
	Next       string
}

const productionConferenceCookie = "streamctl_production_conference"

func selectedProductionConference(r *http.Request) string {
	if conference := strings.TrimSpace(r.URL.Query().Get("conference")); conference != "" {
		return conference
	}
	cookie, err := r.Cookie(productionConferenceCookie)
	if err != nil || !validProductionConference(cookie.Value) {
		return ""
	}
	return cookie.Value
}

func rememberProductionConference(w http.ResponseWriter, r *http.Request, conference string) {
	if !validProductionConference(conference) {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: productionConferenceCookie, Value: conference, Path: "/production",
		MaxAge: 365 * 24 * 60 * 60, HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) productionHome(w http.ResponseWriter, r *http.Request) {
	conference := selectedProductionConference(r)
	rememberProductionConference(w, r, conference)
	conferences, conferenceError := h.productionConferences(r.Context())
	page := productionHomePage{
		Nav: productionNavView{
			Active: "overview", Action: "/production", Conference: conference, Conferences: conferences,
		},
		Conference:    conference,
		RecordingsURL: productionRecordingsURL(h.BTCPPBaseURL, conference),
		Error:         conferenceError,
	}
	if conference == "" {
		h.render(w, r, "production.html", page)
		return
	}
	if !validProductionConference(conference) {
		page.Error = "Invalid conference tag."
		h.renderStatus(w, r, http.StatusBadRequest, "production.html", page)
		return
	}
	addError := func(err error) {
		if err == nil {
			return
		}
		if page.Error != "" {
			page.Error += " "
		}
		page.Error += err.Error()
	}
	if conferenceError == "" {
		talks, talksErr := h.productionTalks(r.Context(), conference)
		if talksErr != nil {
			addError(talksErr)
		} else {
			page.TalksTotal = len(talks)
			for _, talk := range talks {
				if talk.Recording != nil && talk.Recording.PublishedAt != nil {
					page.Published++
				}
			}
			if h.DB != nil {
				cuts, cutsErr := h.DB.ListProductionCuts(conference)
				if cutsErr != nil {
					addError(cutsErr)
				} else {
					for _, talk := range talks {
						if len(cuts[talk.TalkID]) > 0 {
							page.TalksCut++
						}
					}
					page.ReadyToRender = page.TalksCut
				}
			}
		}
	}
	if h.DB != nil {
		proxyCounts, proxyErr := h.DB.ProductionProxyCounts(conference)
		page.ProxyCounts = proxyCounts
		addError(proxyErr)
	}
	if strings.TrimSpace(h.Remote) != "" {
		if ready, proxyErr := h.productionProxyArtifactCount(r.Context(), conference); proxyErr != nil {
			addError(proxyErr)
		} else {
			page.ProxyCounts.Finished = ready
		}
	}
	h.render(w, r, "production.html", page)
}

func productionRecordingsURL(baseURL, conference string) string {
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || !validProductionConference(conference) {
		return ""
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/" + url.PathEscape(conference) + "/admin/recordings"
	base.RawQuery = ""
	base.Fragment = ""
	return base.String()
}

func (h *Handler) productionTimestamp(w http.ResponseWriter, r *http.Request) {
	conference := selectedProductionConference(r)
	rememberProductionConference(w, r, conference)
	page := productionTalksPage{Conference: conference}
	var conferenceError string
	page.Nav.Conferences, conferenceError = h.productionConferences(r.Context())
	page.Nav.Active = "timestamp"
	page.Nav.Action = "/production/timestamp"
	page.Nav.Conference = conference
	page.Error = conferenceError
	if conference == "" {
		h.render(w, r, "production_talks.html", page)
		return
	}
	if !validProductionConference(conference) {
		page.Error = "Invalid conference tag."
		h.renderStatus(w, r, http.StatusBadRequest, "production_talks.html", page)
		return
	}
	talks, err := h.productionTalks(r.Context(), conference)
	if err != nil {
		if page.Error == "" {
			page.Error = err.Error()
		}
	} else {
		decorateProductionTalks(talks, productionConferenceStart(page.Nav.Conferences, conference))
		cuts, err := h.DB.ListProductionCuts(conference)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for i := range talks {
			talks[i].Cuts = cuts[talks[i].TalkID]
			talks[i].CutCount = len(talks[i].Cuts)
			talks[i].URL = productionCutURL(conference, talks[i].TalkID)
		}
		page.TalkGroups = groupProductionTalks(talks)
	}
	h.render(w, r, "production_talks.html", page)
}

func (h *Handler) productionCut(w http.ResponseWriter, r *http.Request) {
	conference := selectedProductionConference(r)
	rememberProductionConference(w, r, conference)
	talkID := strings.TrimSpace(r.URL.Query().Get("talk_id"))
	if !validProductionConference(conference) || talkID == "" {
		http.Error(w, "conference and talk_id are required", http.StatusBadRequest)
		return
	}
	talks, err := h.productionTalks(r.Context(), conference)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	index := -1
	for i := range talks {
		if talks[i].TalkID == talkID {
			index = i
			break
		}
	}
	if index < 0 {
		http.Error(w, "talk not found", http.StatusNotFound)
		return
	}
	cuts, err := h.DB.ListProductionCuts(conference)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	conferences, _ := h.productionConferences(r.Context())
	decorateProductionTalks(talks, productionConferenceStart(conferences, conference))
	for i := range talks {
		talks[i].Cuts = cuts[talks[i].TalkID]
	}
	talk := talks[index]
	page := productionCutPage{Conference: conference, Talk: talk}
	page.Nav.Active = "timestamp"
	page.Nav.Action = "/production/timestamp"
	page.Nav.Conference = conference
	page.Nav.Conferences = conferences
	if index > 0 {
		page.Previous = productionCutURL(conference, talks[index-1].TalkID)
	}
	if index+1 < len(talks) {
		page.Next = productionCutURL(conference, talks[index+1].TalkID)
	}
	encoded, err := json.Marshal(productionCutterJSON{Conference: conference, Talks: talks, Index: index})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	page.CutterJSON = template.JS(encoded)
	h.render(w, r, "production_cut.html", page)
}

func (h *Handler) productionConferences(ctx context.Context) ([]productionConferenceView, string) {
	if h.BTCPP == nil {
		return nil, "Bitcoin++ production API is not configured; set -btcpp-api-token-file."
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conferences, err := h.BTCPP.Conferences(ctx)
	if err != nil {
		return nil, "Loading Bitcoin++ conferences failed: " + err.Error()
	}
	sort.SliceStable(conferences, func(i, j int) bool { return conferenceSortKey(conferences[i]) > conferenceSortKey(conferences[j]) })
	views := make([]productionConferenceView, 0, len(conferences))
	for _, conference := range conferences {
		date := "Date not set"
		if conference.StartsAt != nil {
			if parsed, err := time.Parse(time.RFC3339, *conference.StartsAt); err == nil {
				date = parsed.Format("Jan 2, 2006")
			} else {
				date = *conference.StartsAt
			}
		}
		views = append(views, productionConferenceView{Conference: conference, Date: date})
	}
	return views, ""
}

func conferenceSortKey(conference btcppclient.Conference) string {
	if conference.StartsAt == nil {
		return ""
	}
	return *conference.StartsAt
}

func (h *Handler) productionTalks(ctx context.Context, conference string) ([]productionTalkView, error) {
	if h.BTCPP == nil {
		return nil, fmt.Errorf("Bitcoin++ production API is not configured; set -btcpp-api-token-file")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	candidates, err := h.BTCPP.RecordingCandidates(ctx, conference)
	if err != nil {
		return nil, fmt.Errorf("loading Bitcoin++ talks failed: %w", err)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		leftDay, rightDay := candidateDayKey(candidates[i]), candidateDayKey(candidates[j])
		if leftDay != rightDay {
			return leftDay < rightDay
		}
		leftStage, rightStage := productionStageRank(candidates[i].Venue), productionStageRank(candidates[j].Venue)
		if leftStage != rightStage {
			return leftStage < rightStage
		}
		return candidateSortKey(candidates[i]) < candidateSortKey(candidates[j])
	})
	talks := make([]productionTalkView, len(candidates))
	for i, candidate := range candidates {
		talks[i].Candidate = candidate
	}
	return talks, nil
}

func candidateSortKey(candidate btcppclient.Candidate) string {
	start := "9999"
	if candidate.StartsAt != nil {
		start = *candidate.StartsAt
	}
	return start + "\x00" + candidate.Venue + "\x00" + candidate.Title
}

func candidateDayKey(candidate btcppclient.Candidate) string {
	if candidate.StartsAt == nil {
		return "9999"
	}
	if parsed, err := time.Parse(time.RFC3339, *candidate.StartsAt); err == nil {
		return parsed.Format("2006-01-02")
	}
	return *candidate.StartsAt
}

func productionConferenceStart(conferences []productionConferenceView, tag string) *string {
	for _, conference := range conferences {
		if conference.Tag == tag {
			return conference.StartsAt
		}
	}
	return nil
}

func decorateProductionTalks(talks []productionTalkView, conferenceStart *string) {
	var firstDay time.Time
	if conferenceStart != nil {
		firstDay, _ = time.Parse(time.RFC3339, *conferenceStart)
	}
	if firstDay.IsZero() {
		for _, talk := range talks {
			if talk.StartsAt == nil {
				continue
			}
			parsed, err := time.Parse(time.RFC3339, *talk.StartsAt)
			if err == nil && (firstDay.IsZero() || parsed.Before(firstDay)) {
				firstDay = parsed
			}
		}
	}
	for i := range talks {
		talks[i].StageLabel = productionStageLabel(talks[i].Venue)
		if talks[i].StartsAt == nil {
			continue
		}
		start, err := time.Parse(time.RFC3339, *talks[i].StartsAt)
		if err != nil {
			continue
		}
		talks[i].DateLabel = start.Format("Mon, Jan 2, 2006")
		talks[i].TimeLabel = start.Format("3:04 PM")
		if talks[i].EndsAt != nil {
			if end, err := time.Parse(time.RFC3339, *talks[i].EndsAt); err == nil {
				talks[i].TimeLabel += "–" + end.Format("3:04 PM")
			}
		}
		if !firstDay.IsZero() {
			base := firstDay.In(start.Location())
			baseDate := time.Date(base.Year(), base.Month(), base.Day(), 0, 0, 0, 0, time.UTC)
			startDate := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
			day := int(startDate.Sub(baseDate)/(24*time.Hour)) + 1
			if day > 0 {
				talks[i].DayLabel = fmt.Sprintf("Day %d", day)
			}
		}
	}
}

func productionStageLabel(venue string) string {
	switch strings.ToLower(strings.TrimSpace(venue)) {
	case "1", "one", "stage 1", "stage one", "main":
		return "Main"
	case "2", "two", "stage 2", "stage two", "talks":
		return "Talks"
	case "3", "three", "stage 3", "stage three", "workshop":
		return "Workshop"
	default:
		return strings.TrimSpace(venue)
	}
}

func productionStageRank(venue string) int {
	switch productionStageLabel(venue) {
	case "Main":
		return 1
	case "Talks":
		return 2
	case "Workshop":
		return 3
	default:
		return 4
	}
}

func groupProductionTalks(talks []productionTalkView) []productionTalkGroup {
	var groups []productionTalkGroup
	for _, talk := range talks {
		dayLabel := talk.DayLabel
		if dayLabel == "" {
			dayLabel = "Unscheduled"
		}
		if len(groups) == 0 || groups[len(groups)-1].DayLabel != dayLabel || groups[len(groups)-1].DateLabel != talk.DateLabel {
			groups = append(groups, productionTalkGroup{DayLabel: dayLabel, DateLabel: talk.DateLabel})
		}
		group := &groups[len(groups)-1]
		stageLabel := talk.StageLabel
		if stageLabel == "" {
			stageLabel = "Other"
		}
		stageIndex := -1
		for i := range group.Stages {
			if group.Stages[i].StageLabel == stageLabel {
				stageIndex = i
				break
			}
		}
		if stageIndex < 0 {
			group.Stages = append(group.Stages, productionStageGroup{StageLabel: stageLabel})
			stageIndex = len(group.Stages) - 1
		}
		group.Stages[stageIndex].Talks = append(group.Stages[stageIndex].Talks, talk)
	}
	return groups
}

func productionCutURL(conference, talkID string) string {
	return "/production/timestamp/cut?conference=" + url.QueryEscape(conference) + "&talk_id=" + url.QueryEscape(talkID)
}

func validProductionConference(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r) {
			continue
		}
		return false
	}
	return true
}

type mediaFile struct {
	Name        string   `json:"name"`
	Path        string   `json:"path"`
	Kind        string   `json:"kind,omitempty"`
	SourceType  string   `json:"sourceType,omitempty"`
	Chunks      []string `json:"chunks,omitempty"`
	ProxyStatus string   `json:"proxyStatus,omitempty"`
}

type mediaBrowseResponse struct {
	Prefix string        `json:"prefix"`
	Dirs   []spacesEntry `json:"dirs"`
	Files  []mediaFile   `json:"files"`
}

type logicalMediaInfo struct {
	Path        string `json:"path"`
	SourceType  string `json:"sourceType"`
	DurationMS  int64  `json:"durationMs"`
	ProxyPath   string `json:"proxyPath,omitempty"`
	ProxyStatus string `json:"proxyStatus,omitempty"`
	Warning     string `json:"warning,omitempty"`
}

func (h *Handler) listMediaSpacesPrefix(ctx context.Context, prefix string) ([]spacesEntry, []mediaFile, error) {
	lines, err := h.rcloneLsf(ctx, prefix)
	if err != nil {
		return nil, nil, err
	}
	dirs := make([]spacesEntry, 0)
	names := make([]string, 0)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		name := strings.Trim(line, "/")
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		entry := spacesEntry{Name: name, Path: prefix + name}
		if strings.HasSuffix(line, "/") {
			entry.Path += "/"
			dirs = append(dirs, entry)
		} else {
			names = append(names, name)
		}
	}
	files := groupMediaFiles(prefix, names)
	h.attachProductionProxyStatuses(ctx, files)
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name) })
	return dirs, files, nil
}

var chunkSuffix = regexp.MustCompile(`^(.*?)(\d+)(\.[^.]+)$`)

func groupMediaFiles(prefix string, names []string) []mediaFile {
	type chunk struct {
		name   string
		number int
	}
	groups := make(map[string][]chunk)
	for _, name := range names {
		match := chunkSuffix.FindStringSubmatch(name)
		if len(match) == 4 && len(match[2]) >= 2 && strings.HasPrefix(match[2], "0") {
			number, _ := strconv.Atoi(match[2])
			key := match[1] + "\x00" + strconv.Itoa(len(match[2])) + "\x00" + match[3]
			groups[key] = append(groups[key], chunk{name: name, number: number})
		}
	}
	consumed := make(map[string]bool)
	files := make([]mediaFile, 0)
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		sort.Slice(group, func(i, j int) bool { return group[i].number < group[j].number })
		consecutive := true
		for i := 1; i < len(group); i++ {
			if group[i].number != group[i-1].number+1 {
				consecutive = false
			}
		}
		if !consecutive {
			continue
		}
		chunks := make([]string, len(group))
		for i, item := range group {
			chunks[i] = prefix + item.name
			consumed[item.name] = true
		}
		files = append(files, mediaFile{Name: group[0].name, Path: chunks[0], Kind: "video", SourceType: "chunkedVideo", Chunks: chunks})
	}
	for _, name := range names {
		if consumed[name] {
			continue
		}
		sourceType := ""
		if isVideoFile(name) {
			sourceType = "video"
		}
		files = append(files, mediaFile{Name: name, Path: prefix + name, Kind: mediaFileKind(name), SourceType: sourceType})
	}
	sort.Slice(files, func(i, j int) bool { return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name) })
	return files
}

func (h *Handler) mediaBrowse(w http.ResponseWriter, r *http.Request) {
	conference := strings.TrimSpace(r.URL.Query().Get("conference"))
	prefix := r.URL.Query().Get("prefix")
	if !r.URL.Query().Has("prefix") {
		prefix = conference + "/recordings/"
	}
	clean, err := cleanBucketPrefix(prefix)
	if !validProductionConference(conference) || err != nil || isProductionWorkspacePath(clean) {
		http.Error(w, "invalid media path", http.StatusBadRequest)
		return
	}
	dirs, files, err := h.listMediaSpacesPrefix(r.Context(), clean)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	dirs = slices.DeleteFunc(dirs, func(dir spacesEntry) bool { return isProductionWorkspacePath(dir.Path) })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(mediaBrowseResponse{Prefix: clean, Dirs: dirs, Files: files})
}

func (h *Handler) mediaInfo(w http.ResponseWriter, r *http.Request) {
	conference := strings.TrimSpace(r.URL.Query().Get("conference"))
	objectKey := strings.Trim(strings.TrimSpace(r.URL.Query().Get("path")), "/")
	if !validProductionConference(conference) || !strings.HasPrefix(objectKey, conference+"/recordings/") {
		http.Error(w, "invalid media path", http.StatusBadRequest)
		return
	}
	clean, err := validateRenderObjectKey(objectKey)
	if err != nil || clean != objectKey {
		http.Error(w, "invalid media path", http.StatusBadRequest)
		return
	}
	info, err := h.logicalMediaInfo(r.Context(), clean)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

func (h *Handler) logicalMediaInfo(ctx context.Context, objectKey string) (logicalMediaInfo, error) {
	selected, err := h.logicalMediaSource(ctx, objectKey)
	if err != nil {
		return logicalMediaInfo{}, err
	}
	info := h.attachProductionProxyInfo(ctx, logicalMediaInfo{Path: selected.Path, SourceType: selected.SourceType})
	if info.ProxyStatus == "" {
		info.Warning = "Prepare this recording from the Production overview before cutting it."
	}
	return info, nil
}

func (h *Handler) attachProductionProxyStatuses(ctx context.Context, files []mediaFile) {
	for i := range files {
		if files[i].SourceType == "" {
			continue
		}
		if h.DB != nil {
			if job, err := h.DB.ProductionProxyJobBySource(files[i].Path); err == nil {
				files[i].ProxyStatus = job.Status
			}
		}
	}
	if strings.TrimSpace(h.Remote) == "" {
		return
	}
	proxies, err := h.productionProxyArtifactsForSources(ctx, files)
	if err != nil {
		return
	}
	for i := range files {
		if files[i].SourceType == "" {
			continue
		}
		proxy := productionProxyObjectKey(productionConferenceFromRecording(files[i].Path), files[i])
		if proxies[proxy] {
			files[i].ProxyStatus = "finished"
		} else if files[i].ProxyStatus == "finished" {
			files[i].ProxyStatus = ""
		}
	}
}

func (h *Handler) attachProductionProxyInfo(ctx context.Context, info logicalMediaInfo) logicalMediaInfo {
	if h.DB != nil {
		if job, err := h.DB.ProductionProxyJobBySource(info.Path); err == nil {
			info.ProxyStatus = job.Status
			info.DurationMS = job.DurationMS
			if job.Status == "finished" {
				info.ProxyPath = job.Proxy
			} else if job.Status == "failed" {
				info.Warning = "Proxy preparation failed: " + job.LastError
			} else if job.Status != "finished" {
				info.Warning = "Editing proxy is " + job.Status + "."
			}
		}
	}
	source := mediaFile{Path: info.Path, SourceType: info.SourceType}
	proxy := productionProxyObjectKey(productionConferenceFromRecording(info.Path), source)
	present, checked := h.productionProxyArtifactPresent(ctx, proxy)
	if present {
		metadata, err := h.readProductionProxyMetadata(ctx, proxy)
		if err != nil || metadata.Source.Path != info.Path {
			info.ProxyStatus = ""
			info.ProxyPath = ""
			info.DurationMS = 0
			info.Warning = "The editing proxy metadata is invalid; prepare this recording again."
			return info
		}
		info.ProxyStatus = "finished"
		info.ProxyPath = proxy
		info.DurationMS = metadata.Proxy.DurationMS
		info.Warning = ""
	} else if checked && info.ProxyStatus == "finished" {
		info.ProxyStatus = ""
		info.DurationMS = 0
		info.Warning = "The editing proxy is missing; prepare this recording again."
	}
	return info
}

func (h *Handler) logicalMediaSource(ctx context.Context, objectKey string) (mediaFile, error) {
	prefix := path.Dir(objectKey) + "/"
	_, files, err := h.listMediaSpacesPrefix(ctx, prefix)
	if err != nil {
		return mediaFile{}, err
	}
	for _, file := range files {
		if file.Path == objectKey && file.SourceType != "" {
			return file, nil
		}
	}
	return mediaFile{}, fmt.Errorf("media source was not found")
}

func (h *Handler) mediaOpen(w http.ResponseWriter, r *http.Request) {
	conference := strings.TrimSpace(r.URL.Query().Get("conference"))
	objectKey := strings.Trim(strings.TrimSpace(r.URL.Query().Get("path")), "/")
	if !validProductionConference(conference) || !strings.HasPrefix(objectKey, conference+"/") {
		http.Error(w, "invalid media path", http.StatusBadRequest)
		return
	}
	clean, err := validateRenderObjectKey(objectKey)
	if err != nil || clean != objectKey {
		http.Error(w, "invalid media path", http.StatusBadRequest)
		return
	}
	link, err := h.productionObjectLink(r.Context(), clean)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	http.Redirect(w, r, link, http.StatusTemporaryRedirect)
}

func (h *Handler) productionCutsSave(w http.ResponseWriter, r *http.Request) {
	conference := strings.TrimSpace(r.FormValue("conference"))
	talkID := strings.TrimSpace(r.FormValue("talk_id"))
	sources, sourceTypes, inValues, outValues := r.Form["source"], r.Form["source_type"], r.Form["in_ms"], r.Form["out_ms"]
	if !validProductionConference(conference) || talkID == "" || len(sources) != len(sourceTypes) || len(sources) != len(inValues) || len(inValues) != len(outValues) {
		http.Error(w, "invalid cut request", http.StatusBadRequest)
		return
	}
	cuts := make([]db.ProductionCut, 0, len(sources))
	for i := range sources {
		source, sourceErr := validateRenderObjectKey(sources[i])
		inMS, inErr := strconv.ParseInt(inValues[i], 10, 64)
		outMS, outErr := strconv.ParseInt(outValues[i], 10, 64)
		if sourceErr != nil || !strings.HasPrefix(source, conference+"/recordings/") || inErr != nil || outErr != nil {
			http.Error(w, fmt.Sprintf("cut %d contains an invalid number", i+1), http.StatusBadRequest)
			return
		}
		cuts = append(cuts, db.ProductionCut{Source: source, SourceType: sourceTypes[i], InMS: inMS, OutMS: outMS})
	}
	if err := h.DB.ReplaceProductionCuts(conference, talkID, cuts); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "talk_id": talkID, "cuts": len(cuts)})
}

func (h *Handler) productionObjectLink(ctx context.Context, objectKey string) (string, error) {
	if strings.TrimSpace(h.Remote) == "" {
		return "", fmt.Errorf("Spaces remote is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rclone", "link", "--expire", "1h", h.remotePath(objectKey))
	cmd.Env = rcloneEnv(h.RcloneConfig)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("create source preview link: %s", detail)
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) == 0 {
		return "", fmt.Errorf("rclone returned an empty preview link")
	}
	link := lines[len(lines)-1]
	parsed, err := url.Parse(link)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return "", fmt.Errorf("rclone returned an invalid preview link")
	}
	return link, nil
}
