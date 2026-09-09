# streamctl (Go web app)

This directory contains the Go source for streamctl. For deployment, see the [top-level README](../README.md). This README is just for working on the app itself.

## Layout

```
streamctl/
├── cmd/main.go            # entry point with CLI flags
├── go.mod
├── internal/
│   ├── db/                # SQLite schema and queries
│   ├── handlers/          # HTTP handlers
│   │   └── templates/     # embedded HTML templates
│   └── systemd/           # generates and manages systemd units for streams
└── README.md
```

## How it fits together

- `cmd/main.go` opens the SQLite database, sets up the systemd manager, and starts an HTTP server.
- HTTP handlers in `internal/handlers/` render server-side HTML and handle form submissions. Templates are embedded with `//go:embed`.
- When a user creates or edits a stream in the UI, the handler calls `Systemd.Sync()`, which generates `.service` and `.timer` unit files in `/etc/systemd/system/` (one of each, per stream), runs `systemctl daemon-reload`, and `systemctl enable --now` on the timer.
- systemd handles the actual scheduling. When a timer fires, its associated service runs ffmpeg with the `tee` muxer to push to all configured RTMP endpoints simultaneously.
- The web app polls `systemctl is-active` and `systemctl show ... NextElapseRealtime` to display status in the UI.

This means scheduled streams keep running even if the web app is restarted or crashes — systemd is the queue.

## CLI flags

```
-listen        address to listen on (default ":8080")
-db            path to SQLite database
-video-dir     directory containing video files
-unit-dir      directory for generated systemd units (default /etc/systemd/system)
-unit-prefix   prefix for generated unit names (default "streamctl-")
-run-user      user to run the ffmpeg processes as (default "streamctl")
-btcpp-oauth-base                 Bitcoin++ OAuth server (default "https://btcpp.dev")
-btcpp-oauth-client-id            confidential OAuth client ID
-btcpp-oauth-client-secret-file   path to the OAuth client secret (mode 0400)
-btcpp-oauth-redirect-url         callback URL (defaults from -public-base-url)
-btcpp-api-token-file             API token used by the timestamp workspace (mode 0400)
```

Interactive access uses Bitcoin++ OAuth and is restricted to accounts whose
current roles include `global-admin`. streamctl rechecks that role at least
every five minutes. `STREAMCTL_SECRET` may remain configured as emergency
break-glass access; it is not required when OAuth is configured.

Register streamctl as a confidential OAuth application at
`https://btcpp.dev/dashboard/settings` with this exact callback:

```text
https://stream.btcpp.dev/oauth/callback
```

Grant only `identity:self:read` and `offline_access`. Save the generated client
secret outside Git and the Nix store, for example:

```bash
install -m 0400 /dev/stdin /var/lib/streamctl/btcpp-oauth-client-secret
```

Then set `btcppOAuthClientID` and `btcppOAuthClientSecretFile` in the NixOS
service configuration. Login sessions are stored server-side and all streamctl
mutations require a per-session CSRF token.

## Running locally

From the repo root:

```bash
make build     # nix build .#streamctl
make run-local # runs on :8080 with /tmp/streamctl-local/ as data dir
```

To exercise the timestamp workspace with the real API and Spaces previews:

```bash
BTCPP_API_TOKEN_FILE=/path/to/btcpp-api-token \
STREAMCTL_REMOTE=btcpp:btcpp \
make run-local
```

Set `RCLONE_CONFIG_FILE=/path/to/rclone.conf` too when the remote is not in
rclone's default configuration file.

`run-local` uses a non-system data directory and writes "fake" unit files to a temp dir (you won't have permission to write to `/etc/systemd/system` as a normal user). The UI works for testing forms and rendering, but enabling timers will fail since `systemctl` calls won't have permission.

For end-to-end testing of the systemd integration, deploy to a VM (or to the droplet via `make deploy`).

## Tests

From this directory:

```bash
go test ./...
node --test internal/handlers/tests/*.test.cjs
```

The shared preview tests use Node's built-in test runner and a simulated media
element to cover delayed/adjusted seeks, source loading, and error recovery.
They do not replace a browser playback check against real proxies.

## Bitcoin++ recording API

Create a personal API token from a `global-admin` Bitcoin++ account with only
the `recordings:write` scope. Install it outside the Nix store:

```bash
install -m 0400 /dev/stdin /var/lib/streamctl/btcpp-api-token
```

The token is read from that file and is never accepted as a CLI argument.
This service credential is intentionally separate from interactive OAuth login:
the PAT belongs to the streamctl automation, while OAuth sessions identify the
human administrator operating the UI.

Set `services.streamctl.btcppAPITokenFile` to that path to enable the
**Production → Timestamp** workspace. The workspace reads eligible talks from
Bitcoin++, presents a conference selector and talk list, and keeps source
paths and cuts in streamctl's SQLite database. Media is selected directly from
the event browser; numbered chunk sequences are grouped automatically and do
not need to be registered. **Production → Media** provides a browser rooted at
each conference's `<conference>/recordings/` folder in Spaces. When matching
vMix logs are present beside a recording in a `logs/` folder, streamctl uses
their restart interval and total duration to construct a chunked timeline
without scanning the video. Other media still opens directly, with reduced
sequence-wide seeking when no timing metadata is available.

Discover eligible talks and their recording-consent policy:

```bash
streamctl btcpp-candidates -conference dev26
```

Idempotently attach a Spaces object key or published links to a conference
talk UUID:

```bash
streamctl btcpp-recording -conference dev26 -talk-id TALK_UUID \
  -file-uri dev26/recordings/normalized/stage-one/talk.mp4
```

Use `-api-base http://localhost:8888` for local development. The default is
`https://btcpp.dev`. The service token must never be placed in Nix source,
Terraform state, a URL, or shell history.

## Database

SQLite. Schema lives in `internal/db/db.go`. Tables:

- `endpoints` — RTMP destinations (name, url, stream key, enabled flag)
- `streams` — scheduled stream definitions (name, video file, OnCalendar expression, enabled flag)
- `stream_endpoints` — many-to-many join

The DB file lives at `<data-dir>/streamctl.db` (default `/var/lib/streamctl/streamctl.db`).

## What's intentionally not here

- **No file uploads through the UI**: scp into the video directory directly. Browsers handle multi-GB uploads poorly; ssh handles them well.
- **No transcoding**: the stream uses `ffmpeg -c copy`. Whatever is in the file is what goes to the platforms.
- **No multi-user auth**: single shared secret. If you need user accounts, this is the wrong tool.
- **No queue or worker**: systemd is the queue. The web app generates unit files; systemd does the scheduling.

## Extending

A few places where extensions would slot in cleanly:

- **Per-stream CBR transcoding toggle**: in `internal/systemd/manager.go`, the `renderService` function builds the ffmpeg command line. Adding a `TranscodeBitrate` field to `db.Stream` and modifying the command to include `-c:v libx264 -b:v ... -minrate ... -maxrate ...` instead of `-c copy` would be ~30 lines.
- **Per-event egress tracking**: parse ffmpeg's stderr (currently `>StandardError=journal`) to record bytes sent. Useful if you're worried about VPS bandwidth caps.
- **Webhook on stream start/stop**: a `systemd.services.streamctl-stream-N.serviceConfig.ExecStartPre` / `ExecStopPost` hook that POSTs to a URL.
- **Multiple data tiers**: run multiple stream services in parallel for ABR (1080p + 720p simultaneously). Would need ffmpeg to scale and re-encode, so loses the `-c copy` advantage.
