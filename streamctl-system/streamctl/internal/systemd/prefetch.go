package systemd

import (
	"fmt"
	"path/filepath"
	"streamctl/internal/db"
	"strings"
)

// Prefetch the selected object itself. There is no alternate object or encoding
// fallback: readiness failures must stop playback, never change its source.
func (m *Manager) renderPrefetchScript(s *db.Stream) string {
	var b strings.Builder
	b.WriteString("#!/run/current-system/sw/bin/bash\nset -euo pipefail\nexport PATH=/run/current-system/sw/bin:/bin\n")
	if strings.TrimSpace(m.NotifyEmail) != "" {
		b.WriteString(m.renderNotifyFunction(s))
		b.WriteString("trap 'rc=$?; notify failure; exit $rc' ERR\n")
	}
	seen := map[string]bool{}
	for _, source := range s.Videos {
		if !isRemoteClip(source) || seen[source] {
			continue
		}
		seen[source] = true
		local := m.rawRemoteClipPath(source)
		// rclone checks the source on every prefetch, including when a same-named
		// cache file exists. Use checksum comparison to detect replaced objects.
		fmt.Fprintf(&b, "/run/current-system/sw/bin/mkdir -p %s\n", shellQuote(filepath.Dir(local)))
		fmt.Fprintf(&b, "/run/current-system/sw/bin/rclone copyto --checksum %s %s\n", shellQuote(m.remoteObjectPath(source)), shellQuote(local))
	}
	b.WriteString(m.renderProbeScriptBody(s))
	if strings.TrimSpace(m.NotifyEmail) != "" {
		b.WriteString("notify success\n")
	}
	return b.String()
}
