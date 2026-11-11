package handlers

import (
	"context"
	"fmt"
	"path/filepath"
	"streamctl/internal/probe"
)

// Inspect the selected originals, including uncached render outputs and imported
// broadcasts. Never treat a marker, folder name or editing proxy as proof.
func (h *Handler) validateStreamVideos(ctx context.Context, videos []string) error {
	paths := make([]string, 0, len(videos))
	resolved := map[string]string{}
	for _, source := range videos {
		path, found := resolved[source]
		if !found {
			path = filepath.Join(h.VideoDir, source)
			if isRemoteClip(source) {
				var err error
				path, err = h.productionObjectLink(ctx, source)
				if err != nil {
					return fmt.Errorf("checking stream readiness of %s: %w", source, err)
				}
			}
			resolved[source] = path
		}
		paths = append(paths, path)
	}
	return probe.ValidateStreamingPaths(ctx, paths, videos)
}
