package handlers

import "strings"

// Bound provisioning, but keep SSH available for diagnosis if installation fails.
// Never mark a failed installation ready or automatically create another paid pod.
func boundedWorkerSetup(setup string) string {
	return "rm -f /root/.streamctl-worker-ready /root/.streamctl-worker-setup-failed; " +
		"if ! timeout --kill-after=30s 20m bash -lc " + shellQuote(setup) +
		" > /root/streamctl-worker-setup.log 2>&1; then touch /root/.streamctl-worker-setup-failed; cat /root/streamctl-worker-setup.log >&2; fi; " +
		"exec /usr/sbin/sshd -D -e"
}

// Stop the whole container process group before removing its workspace.
// Container jobs are launched with setsid; VM jobs use systemd's control group.
func remoteGPUStopCommand(unit string) string {
	return strings.Join([]string{
		"unit=" + shellQuote(unit),
		"if command -v systemctl >/dev/null 2>&1 && systemctl show-environment >/dev/null 2>&1; then systemctl stop \"$unit\" || exit 1; else",
		"jobdir=${STREAMCTL_GPU_JOB_ROOT:-/root/streamctl-gpu-jobs}/$unit",
		"pid=$(cat \"$jobdir/pid\" 2>/dev/null || true)",
		"case \"$pid\" in ''|*[!0-9]*) ;; *) if [ \"$pid\" -gt 1 ]; then kill -TERM -- -\"$pid\" 2>/dev/null || true; for i in 1 2 3 4 5; do kill -0 -- -\"$pid\" 2>/dev/null || break; sleep 1; done; kill -KILL -- -\"$pid\" 2>/dev/null || true; fi ;; esac",
		"if [ -d \"$jobdir\" ]; then printf 'failed\\n' > \"$jobdir/active\"; printf 'cancelled\\n' > \"$jobdir/result\"; fi",
		"fi",
	}, "\n")
}
