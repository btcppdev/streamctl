package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	productionWorkspaceDirectory   = "workspace"
	productionProxyArtifactVersion = 1
	productionProxyHeight          = 480
)

type productionProxyMetadata struct {
	Version     int                             `json:"version"`
	GeneratedAt time.Time                       `json:"generatedAt"`
	Source      productionProxyMetadataSource   `json:"source"`
	Proxy       productionProxyMetadataArtifact `json:"proxy"`
}

type productionProxyMetadataSource struct {
	Path       string                         `json:"path"`
	Type       string                         `json:"type"`
	DurationMS int64                          `json:"durationMs"`
	Chunks     []productionProxyMetadataChunk `json:"chunks"`
}

type productionProxyMetadataChunk struct {
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modifiedAt,omitempty"`
}

type productionProxyMetadataArtifact struct {
	Path                 string `json:"path"`
	DurationMS           int64  `json:"durationMs"`
	Height               int    `json:"height"`
	VideoCodec           string `json:"videoCodec"`
	Encoder              string `json:"encoder,omitempty"`
	CQ                   int    `json:"cq,omitempty"`
	CRF                  int    `json:"crf,omitempty"`
	KeyframeIntervalMS   int    `json:"keyframeIntervalMs"`
	InterleaveDurationMS int    `json:"interleaveDurationMs,omitempty"`
	AudioCodec           string `json:"audioCodec"`
	AudioBitrate         string `json:"audioBitrate"`
}

func (h *Handler) productionProxyPrepare(w http.ResponseWriter, r *http.Request) {
	conference := strings.TrimSpace(r.FormValue("conference"))
	target := strings.TrimSpace(r.FormValue("target"))
	if !validProductionConference(conference) {
		http.Error(w, "invalid conference", http.StatusBadRequest)
		return
	}
	if h.DB == nil || strings.TrimSpace(h.Remote) == "" {
		http.Error(w, "media preparation is not configured", http.StatusServiceUnavailable)
		return
	}
	sources, err := h.productionProxyTargetSources(r.Context(), conference, target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	existing, inventoryErr := h.productionProxyArtifactInventory(r.Context(), conference)
	if inventoryErr != nil {
		http.Error(w, inventoryErr.Error(), http.StatusBadGateway)
		return
	}
	queued := 0
	for _, source := range sources {
		proxy := productionProxyObjectKey(conference, source)
		if proxy == "" || existing[proxy] {
			continue
		}
		_, inserted, err := h.DB.EnqueueProductionProxyJob(source.Path, proxy)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if inserted {
			queued++
		}
	}
	if queued > 0 {
		go h.dispatchGPUQueueOnce(context.Background())
	}
	message := "No new editing proxy jobs were needed."
	if queued == 1 {
		message = "Queued 1 new editing proxy job."
	} else if queued > 1 {
		message = fmt.Sprintf("Queued %d new editing proxy jobs.", queued)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"queued":  queued,
		"status":  "queued",
		"message": message,
	})
}

func (h *Handler) productionProxyTargetSources(ctx context.Context, conference, target string) ([]mediaFile, error) {
	target = strings.TrimSpace(strings.ReplaceAll(target, "\\", "/"))
	root := conference + "/recordings/"
	if strings.HasSuffix(target, "/") {
		prefix, err := cleanSpacesPrefix(target)
		if err != nil || prefix == root || !strings.HasPrefix(prefix, root) || isProductionWorkspacePath(prefix) {
			return nil, fmt.Errorf("choose a source folder inside this conference's recordings folder")
		}
		return h.logicalMediaSourcesRecursive(ctx, prefix)
	}
	objectKey, err := validateRenderObjectKey(target)
	if err != nil || !strings.HasPrefix(objectKey, root) || isProductionWorkspacePath(objectKey) || !isVideoFile(objectKey) {
		return nil, fmt.Errorf("choose a source video inside this conference's recordings folder")
	}
	source, err := h.logicalMediaSource(ctx, objectKey)
	if err != nil {
		return nil, err
	}
	return []mediaFile{source}, nil
}

func (h *Handler) productionProxyRequeue(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("id")), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid media preparation job", http.StatusBadRequest)
		return
	}
	if err := h.DB.RetryProductionProxyJob(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "media preparation job is not failed", http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	go h.dispatchGPUQueueOnce(context.Background())
	http.Redirect(w, r, "/worker?requeued_proxy="+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (h *Handler) logicalMediaSourcesRecursive(ctx context.Context, prefix string) ([]mediaFile, error) {
	lines, err := h.rcloneLsf(ctx, prefix, "--recursive", "--files-only")
	if err != nil {
		return nil, err
	}
	byDirectory := make(map[string][]string)
	for _, line := range lines {
		relative := strings.Trim(strings.TrimSpace(line), "/")
		if relative == "" || !isVideoFile(relative) || isProductionWorkspacePath(prefix+relative) {
			continue
		}
		directory, name := path.Split(relative)
		byDirectory[directory] = append(byDirectory[directory], name)
	}
	var sources []mediaFile
	for directory, names := range byDirectory {
		for _, source := range groupMediaFiles(prefix+directory, names) {
			if source.SourceType != "" {
				sources = append(sources, source)
			}
		}
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })
	return sources, nil
}

func isProductionWorkspacePath(objectKey string) bool {
	return strings.Contains(objectKey, "/recordings/workspace/")
}

func productionProxyObjectKey(conference string, source mediaFile) string {
	recordingsPrefix := strings.TrimSuffix(conference, "/") + "/recordings/"
	relative := strings.TrimPrefix(source.Path, recordingsPrefix)
	if relative == source.Path || relative == "" {
		return ""
	}
	directory, filename := path.Split(relative)
	extension := path.Ext(filename)
	stem := strings.TrimSuffix(filename, extension)
	if source.SourceType == "chunkedVideo" {
		if match := chunkSuffix.FindStringSubmatch(filename); len(match) == 4 {
			stem = strings.TrimRight(match[1], " ._-")
		}
	}
	if stem == "" {
		return ""
	}
	return recordingsPrefix + productionWorkspaceDirectory + "/" + directory + stem + ".proxy.mp4"
}

func productionProxySidecarObjectKey(proxy string) string {
	return strings.TrimSuffix(proxy, path.Ext(proxy)) + ".v" + strconv.Itoa(productionProxyArtifactVersion) + ".json"
}

func productionConferenceFromRecording(objectKey string) string {
	conference, _, ok := strings.Cut(strings.Trim(objectKey, "/"), "/recordings/")
	if !ok || !validProductionConference(conference) {
		return ""
	}
	return conference
}

func (h *Handler) productionProxyArtifactInventory(ctx context.Context, conference string) (map[string]bool, error) {
	proxies := make(map[string]bool)
	if strings.TrimSpace(h.Remote) == "" || !validProductionConference(conference) {
		return proxies, nil
	}
	prefix := conference + "/recordings/" + productionWorkspaceDirectory + "/"
	lines, err := h.rcloneLsf(ctx, prefix, "--recursive", "--files-only")
	if err != nil {
		return nil, fmt.Errorf("inspect prepared media: %w", err)
	}
	files := make(map[string]bool)
	for _, line := range lines {
		relative := strings.Trim(strings.TrimSpace(line), "/")
		if relative != "" {
			files[prefix+relative] = true
		}
	}
	for objectKey := range files {
		if strings.HasSuffix(objectKey, ".proxy.mp4") && files[productionProxySidecarObjectKey(objectKey)] {
			proxies[objectKey] = true
		}
	}
	return proxies, nil
}

func (h *Handler) productionProxyArtifactCount(ctx context.Context, conference string) (int, error) {
	proxies, err := h.productionProxyArtifactInventory(ctx, conference)
	if err != nil {
		return 0, err
	}
	return len(proxies), nil
}

func (h *Handler) productionProxyArtifactsForSources(ctx context.Context, sources []mediaFile) (map[string]bool, error) {
	wanted := make(map[string]bool)
	directories := make(map[string]bool)
	for _, source := range sources {
		conference := productionConferenceFromRecording(source.Path)
		proxy := productionProxyObjectKey(conference, source)
		if proxy == "" {
			continue
		}
		wanted[proxy] = true
		directories[path.Dir(proxy)+"/"] = true
	}
	found := make(map[string]bool)
	if strings.TrimSpace(h.Remote) == "" {
		return found, nil
	}
	for directory := range directories {
		lines, err := h.rcloneLsf(ctx, directory, "--files-only")
		if err != nil {
			return nil, err
		}
		files := make(map[string]bool)
		for _, line := range lines {
			files[directory+strings.Trim(strings.TrimSpace(line), "/")] = true
		}
		for proxy := range wanted {
			if path.Dir(proxy)+"/" == directory && files[proxy] && files[productionProxySidecarObjectKey(proxy)] {
				found[proxy] = true
			}
		}
	}
	return found, nil
}

func (h *Handler) productionProxyArtifactPresent(ctx context.Context, proxy string) (present, checked bool) {
	if proxy == "" || strings.TrimSpace(h.Remote) == "" {
		return false, false
	}
	files, err := h.rcloneLsf(ctx, path.Dir(proxy)+"/", "--files-only")
	if err != nil {
		return false, false
	}
	name := path.Base(proxy)
	sidecar := path.Base(productionProxySidecarObjectKey(proxy))
	hasProxy, hasSidecar := false, false
	for _, file := range files {
		switch strings.Trim(strings.TrimSpace(file), "/") {
		case name:
			hasProxy = true
		case sidecar:
			hasSidecar = true
		}
	}
	return hasProxy && hasSidecar, true
}

func (h *Handler) readProductionProxyMetadata(ctx context.Context, proxy string) (productionProxyMetadata, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	sidecar := productionProxySidecarObjectKey(proxy)
	cmd := exec.CommandContext(ctx, "rclone", "cat", h.remotePath(sidecar))
	cmd.Env = rcloneEnv(h.RcloneConfig)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return productionProxyMetadata{}, fmt.Errorf("read %s: %s", sidecar, commandError(output, err))
	}
	var metadata productionProxyMetadata
	if err := json.Unmarshal(output, &metadata); err != nil {
		return productionProxyMetadata{}, fmt.Errorf("read %s: %w", sidecar, err)
	}
	if metadata.Version != productionProxyArtifactVersion || metadata.Proxy.Path != proxy {
		return productionProxyMetadata{}, fmt.Errorf("read %s: incompatible proxy metadata", sidecar)
	}
	return metadata, nil
}

func commandError(output []byte, err error) string {
	detail := strings.TrimSpace(string(output))
	if len(detail) > 4000 {
		detail = detail[len(detail)-4000:]
	}
	if detail != "" {
		return detail
	}
	return err.Error()
}
