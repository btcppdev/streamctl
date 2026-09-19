# streamctl deploy commands.
# Run `make help` to see the list. All commands assume `doctl` is authenticated
# (run `doctl auth init` first if not).

# Use bash, fail loudly, fail fast.
SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

DIGITALOCEAN_TOKEN := $(strip $(DIGITAL_OCEAN_TOKEN))
export DIGITALOCEAN_TOKEN

TERRAFORM := $(shell if command -v terraform >/dev/null 2>&1; then printf 'terraform'; else printf 'nix develop --command terraform'; fi)
NIXOS_REBUILD := $(shell if command -v nixos-rebuild >/dev/null 2>&1; then printf 'nixos-rebuild'; else printf 'nix develop --command nixos-rebuild'; fi)

# Optional machine-local deployment settings, excluded from the public repo.
-include ../.private/deploy.mk
DEPLOY_FLAKE ?= .\#streamctl

# Cache the droplet's IP across recipes that need it.
# This shells out to terraform once per make invocation, not per recipe.
IP := $(shell cd terraform && $(TERRAFORM) output -raw ipv4 2>/dev/null)

.PHONY: help init plan create ip wait-for-nixos pull-hardware-config \
        bootstrap-secret deploy deploy-local-build deploy-dry update \
	build shell run-local \
        ssh logs timers destroy check-token check-gpu-token check-ip stream-logs upload \
        install-rclone-config maybe-install-rclone-config \
        install-digitalocean-gpu-token maybe-install-digitalocean-gpu-token verify-digitalocean-gpu-token backup-db \
        install-runpod-token maybe-install-runpod-token verify-runpod-token \
        gpu-upload gpu-transcode

# ---------- help ----------

help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} \
	      /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

# Internal sanity check: bail if no DigitalOcean token is configured.
check-token:
	@if [ -z "$${DIGITALOCEAN_TOKEN:-}" ]; then \
	  echo "ERROR: DIGITAL_OCEAN_TOKEN is not set." >&2; \
	  exit 1; \
	fi

# Internal sanity check: bail if no managed GPU worker token is configured.
check-gpu-token:
	@GPU_TOKEN="$${DIGITAL_OCEAN_GPU_TOKEN:-$$(doctl auth token --context streamctl-gpu 2>/dev/null || true)}"; \
	if [ -z "$$GPU_TOKEN" ]; then \
	  echo "ERROR: no GPU token found. Set DIGITAL_OCEAN_GPU_TOKEN or run 'doctl auth init --context streamctl-gpu'." >&2; \
	  exit 1; \
	fi

# Internal sanity check: bail if terraform hasn't been applied yet.
check-ip:
	@if [ -z "$(IP)" ]; then \
	  echo "ERROR: no droplet IP in terraform state." >&2; \
	  echo "Run 'make create' first." >&2; \
	  exit 1; \
	fi

# ---------- one-time setup ----------

init: ## Initialize Terraform (run once after clone).
	cd terraform && $(TERRAFORM) init

plan: check-token ## Show what Terraform would do without applying.
	cd terraform && $(TERRAFORM) plan

# ---------- provisioning ----------

create: check-token ## Create the droplet via Terraform (~1 min + ~5 min for nixos-infect).
	cd terraform && $(TERRAFORM) apply
	@echo
	@echo "Droplet created. Wait ~5 minutes for nixos-infect to finish, then:"
	@echo "  make wait-for-nixos"
	@echo "  make pull-hardware-config"
	@echo "  make bootstrap-secret"
	@echo "  make deploy"
	@echo "If DIGITAL_OCEAN_GPU_TOKEN is set, deploy will also install it for managed GPU workers."

ip: check-ip ## Print the droplet's IPv4 address.
	@echo "$(IP)"

wait-for-nixos: check-ip ## Block until the droplet is reachable as NixOS.
	@echo "Waiting for NixOS to come up on $(IP)..."
	@for i in $$(seq 1 60); do \
	  if ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 -o BatchMode=yes \
	      root@$(IP) 'test -e /etc/NIXOS' 2>/dev/null; then \
	    echo "NixOS is up."; \
	    exit 0; \
	  fi; \
	  echo "  attempt $$i/60: not ready yet, sleeping 10s..."; \
	  sleep 10; \
	done; \
	echo "Timed out after 10 minutes." >&2; \
	exit 1

pull-hardware-config: check-ip ## Copy hardware-configuration.nix from droplet into our flake.
	scp -o StrictHostKeyChecking=no \
	  root@$(IP):/etc/nixos/hardware-configuration.nix \
	  nixos/hardware-configuration.nix
	@echo "Pulled hardware-configuration.nix from $(IP)."
	@if scp -o StrictHostKeyChecking=no \
	  root@$(IP):/etc/nixos/networking.nix \
	  nixos/networking.nix; then \
	  echo "Pulled networking.nix from $(IP)."; \
	else \
	  echo "No /etc/nixos/networking.nix found on $(IP); keeping existing nixos/networking.nix."; \
	fi

bootstrap-secret: check-ip ## Generate a streamctl login secret on the droplet.
	@SECRET=$$(openssl rand -base64 32 | tr -d '/+=' | head -c 32); \
	ssh -o StrictHostKeyChecking=no root@$(IP) \
	  "install -d -m 0750 -o streamctl -g streamctl /var/lib/streamctl 2>/dev/null || true; \
	   echo '$$SECRET' > /var/lib/streamctl/secret; \
	   chmod 0400 /var/lib/streamctl/secret; \
	   chown streamctl:streamctl /var/lib/streamctl/secret 2>/dev/null || true"; \
	echo; \
	echo "================================================================"; \
	echo "Login secret for streamctl web UI:"; \
	echo "  $$SECRET"; \
	echo "================================================================"; \
	echo "Save this somewhere safe. It's also at /var/lib/streamctl/secret on the droplet."

# ---------- deploys ----------

deploy: check-ip ## Push the current NixOS config to the droplet (build on droplet).
	$(NIXOS_REBUILD) switch \
	  --flake "$(DEPLOY_FLAKE)" \
	  --target-host root@$(IP) \
	  --build-host root@$(IP) \
          --no-reexec
	@$(MAKE) maybe-install-rclone-config
	@$(MAKE) maybe-install-digitalocean-gpu-token
	@$(MAKE) maybe-install-runpod-token

deploy-local-build: check-ip ## Deploy but build locally, then copy result to droplet.
	$(NIXOS_REBUILD) switch \
	  --flake "$(DEPLOY_FLAKE)" \
	  --target-host root@$(IP)
	@$(MAKE) maybe-install-rclone-config
	@$(MAKE) maybe-install-digitalocean-gpu-token
	@$(MAKE) maybe-install-runpod-token

deploy-dry: check-ip ## Test the config without switching (validation only).
	$(NIXOS_REBUILD) dry-activate \
	  --flake "$(DEPLOY_FLAKE)" \
	  --target-host root@$(IP) \
	  --build-host root@$(IP)

update: ## Update flake inputs (nixpkgs) to their latest versions.
	nix flake update

# ---------- local dev ----------

build: ## Build the streamctl binary locally (output: ./result/bin/cmd).
	nix build .#streamctl

shell: ## Enter the dev shell with go, terraform, doctl, etc.
	nix develop

run-local: build ## Run streamctl locally on :8080 with a test config.
	@mkdir -p /tmp/streamctl-local/videos
	@if [ ! -e /tmp/streamctl-local/secret ]; then \
	  echo "test-secret" > /tmp/streamctl-local/secret; \
	fi
	@set -- \
	  -listen :8080 \
	  -db /tmp/streamctl-local/streamctl.db \
	  -video-dir /tmp/streamctl-local/videos \
	  -cache-dir /tmp/streamctl-local/cache \
	  -unit-dir /tmp/streamctl-local \
	  -unit-prefix test- \
	  -run-user "$$USER"; \
	if [ -n "$${BTCPP_API_TOKEN_FILE:-}" ]; then set -- "$$@" -btcpp-api-token-file "$$BTCPP_API_TOKEN_FILE"; fi; \
	if [ -n "$${STREAMCTL_REMOTE:-}" ]; then set -- "$$@" -remote "$$STREAMCTL_REMOTE"; fi; \
	if [ -n "$${RCLONE_CONFIG_FILE:-}" ]; then set -- "$$@" -rclone-config "$$RCLONE_CONFIG_FILE"; fi; \
	if [ -n "$${RUNPOD_TOKEN_FILE:-}" ]; then set -- "$$@" -runpod-token-file "$$RUNPOD_TOKEN_FILE" -runpod-pod-name "$${STREAMCTL_WORKER_NAME:-streamctl-dev-worker}" -gpu-worker-ssh-key /tmp/streamctl-local/gpu-worker-ssh-key -gpu-destroy-after-job=true; fi; \
	if [ -n "$${STREAMCTL_GPU_TYPE:-}" ]; then set -- "$$@" -runpod-gpu-type "$$STREAMCTL_GPU_TYPE"; fi; \
	if [ -n "$${STREAMCTL_GPU_WORKER_HOST:-}" ]; then set -- "$$@" -gpu-worker-host "$$STREAMCTL_GPU_WORKER_HOST"; fi; \
	STREAMCTL_SECRET=$$(cat /tmp/streamctl-local/secret) ./result/bin/cmd "$$@"

# ---------- ops ----------

ssh: check-ip ## SSH into the droplet.
	ssh root@$(IP)

logs: check-ip ## Tail streamctl logs.
	ssh root@$(IP) 'journalctl -u streamctl -f'

backup-db: check-ip ## Copy the server SQLite DB to ./backups with a timestamp.
	@mkdir -p backups
	@set -eu; umask 077; TS=$$(date -u +%Y%m%dT%H%M%SZ); \
	  ssh root@$(IP) 'umask 077; sqlite3 /var/lib/streamctl/streamctl.db ".backup /var/lib/streamctl/backup.db"'; \
	  scp root@$(IP):/var/lib/streamctl/backup.db backups/streamctl-$$TS.db; \
	  echo "Backed up database to backups/streamctl-$$TS.db"

# Usage: make stream-logs ID=3
stream-logs: check-ip ## Tail logs for a specific scheduled stream (ID=N).
	@if [ -z "$(ID)" ]; then \
	  echo "Usage: make stream-logs ID=<n>" >&2; \
	  exit 1; \
	fi
	ssh root@$(IP) "journalctl -u streamctl-stream-$(ID) -f"

timers: check-ip ## List all scheduled streamctl timers.
	ssh root@$(IP) 'systemctl list-timers "streamctl-*"'

# Usage: make upload FILE=path/to/video.mp4
upload: check-ip ## Upload a video file via scp (FILE=path).
	@if [ -z "$(FILE)" ]; then \
	  echo "Usage: make upload FILE=<path>" >&2; \
	  exit 1; \
	fi
	scp "$(FILE)" "streamctl@$(IP):/var/lib/streamctl/videos/"
	@echo "Uploaded $(FILE). It should appear in the UI immediately."

install-rclone-config: check-ip ## Install local rclone.conf on the droplet for Spaces prefetch.
	@if [ ! -f rclone.conf ]; then \
	  echo "ERROR: ./rclone.conf not found." >&2; \
	  exit 1; \
	fi
	scp rclone.conf root@$(IP):/tmp/streamctl-rclone.conf
	ssh root@$(IP) 'install -d -m 0750 -o streamctl -g streamctl /var/lib/streamctl && \
	  install -m 0400 -o streamctl -g streamctl /tmp/streamctl-rclone.conf /var/lib/streamctl/rclone.conf && \
	  rm -f /tmp/streamctl-rclone.conf'
	@echo "Installed rclone.conf at /var/lib/streamctl/rclone.conf."

maybe-install-rclone-config: check-ip ## Install local rclone.conf when present; skip otherwise.
	@if [ -f rclone.conf ]; then \
	  $(MAKE) install-rclone-config; \
	else \
	  echo "No local rclone.conf found; skipping rclone config install."; \
	fi

install-digitalocean-gpu-token: check-ip check-gpu-token ## Install DIGITAL_OCEAN_GPU_TOKEN on the droplet for managed GPU workers.
	@GPU_TOKEN="$${DIGITAL_OCEAN_GPU_TOKEN:-$$(doctl auth token --context streamctl-gpu)}"; \
	printf '%s\n' "$$GPU_TOKEN" | ssh root@$(IP) \
	  'install -d -m 0750 /var/lib/streamctl && \
	   install -m 0400 -o root -g root /dev/stdin /var/lib/streamctl/digitalocean-token'
	@echo "Installed DigitalOcean GPU token at /var/lib/streamctl/digitalocean-token."

maybe-install-digitalocean-gpu-token: check-ip ## Install DIGITAL_OCEAN_GPU_TOKEN when present; skip otherwise.
	@if [ -n "$${DIGITAL_OCEAN_GPU_TOKEN:-}" ] || doctl auth token --context streamctl-gpu >/dev/null 2>&1; then \
	  $(MAKE) install-digitalocean-gpu-token; \
	else \
	  echo "No DIGITAL_OCEAN_GPU_TOKEN or doctl streamctl-gpu context found; skipping DigitalOcean GPU token install."; \
	fi

verify-digitalocean-gpu-token: check-ip ## Verify the installed GPU token can list managed GPU droplets.
	@ssh root@$(IP) 'token=$$(tr -d "\r\n" </var/lib/streamctl/digitalocean-token); \
	  curl -sS -w "\nHTTP %{http_code}\n" \
	    -H "Authorization: Bearer $$token" \
	    "https://api.digitalocean.com/v2/droplets?tag_name=streamctl-gpu-worker"'

install-runpod-token: check-ip ## Install RUNPOD_API_KEY on the droplet for managed RunPod GPU workers.
	@if [ -z "$${RUNPOD_API_KEY:-}" ]; then \
	  echo "ERROR: RUNPOD_API_KEY is not set." >&2; \
	  exit 1; \
	fi
	@printf '%s\n' "$${RUNPOD_API_KEY}" | ssh root@$(IP) \
	  'install -d -m 0750 /var/lib/streamctl && \
	   install -m 0400 -o root -g root /dev/stdin /var/lib/streamctl/runpod-token'
	@echo "Installed RunPod token at /var/lib/streamctl/runpod-token."

maybe-install-runpod-token: check-ip ## Install RUNPOD_API_KEY when present; skip otherwise.
	@if [ -n "$${RUNPOD_API_KEY:-}" ]; then \
	  $(MAKE) install-runpod-token; \
	else \
	  echo "No RUNPOD_API_KEY found; skipping RunPod token install."; \
	fi

verify-runpod-token: check-ip ## Verify the installed RunPod token can list pods.
	@ssh root@$(IP) 'token=$$(tr -d "\r\n" </var/lib/streamctl/runpod-token); \
	  curl -sS -w "\nHTTP %{http_code}\n" \
	    -H "Authorization: Bearer $$token" \
	    "https://rest.runpod.io/v1/pods"'

# Usage: make gpu-upload GPU_HOST=ubuntu@1.2.3.4
gpu-upload: ## Upload the NVENC transcode worker script and rclone config to a GPU VM.
	@if [ -z "$(GPU_HOST)" ]; then \
	  echo "Usage: make gpu-upload GPU_HOST=<user@host>" >&2; \
	  exit 1; \
	fi
	@if [ ! -f rclone.conf ]; then \
	  echo "ERROR: ./rclone.conf not found." >&2; \
	  exit 1; \
	fi
	scp scripts/transcode-nvenc.sh "$(GPU_HOST):/home/ubuntu/transcode-nvenc.sh"
	scp rclone.conf "$(GPU_HOST):/home/ubuntu/rclone.conf"
	ssh "$(GPU_HOST)" 'chmod +x /home/ubuntu/transcode-nvenc.sh'

# Usage: make gpu-transcode GPU_HOST=ubuntu@1.2.3.4 FILE=vienna/recordings/edits/livestream/file.mp4
gpu-transcode: ## Start one NVENC transcode job on the GPU VM.
	@if [ -z "$(GPU_HOST)" ] || [ -z "$(FILE)" ]; then \
	  echo "Usage: make gpu-transcode GPU_HOST=<user@host> FILE=<spaces-path>" >&2; \
	  exit 1; \
	fi
	ssh "$(GPU_HOST)" 'RCLONE_CONFIG=/home/ubuntu/rclone.conf /home/ubuntu/transcode-nvenc.sh "$(FILE)"'

# ---------- teardown ----------

destroy: check-token ## Destroy the droplet. Prompts for confirmation.
	cd terraform && $(TERRAFORM) destroy

.DEFAULT_GOAL := help
