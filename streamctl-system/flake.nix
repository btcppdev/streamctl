{
  description = "streamctl — scheduled RTMP streamer with NixOS deployment";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    let
      # The streamctl Go package, parameterized by pkgs so it works on any system.
      streamctlPackage = pkgs: pkgs.buildGoModule {
        pname = "streamctl";
        version = "0.1.0";
        src = ./streamctl;
        vendorHash = "sha256-xul/VcwVEZrNo+ew/b1YmRUfFW4kd1bayNdG163Of7Y=";
        subPackages = [ "cmd" ];
        nativeBuildInputs = [ pkgs.makeWrapper ];
        postInstall = ''
          wrapProgram $out/bin/cmd \
            --prefix PATH : ${pkgs.lib.makeBinPath [ pkgs.ffmpeg pkgs.rclone ]}
        '';
        meta = with pkgs.lib; {
          description = "Schedule pre-recorded RTMP streams via systemd";
          license = licenses.mit;
          mainProgram = "cmd";
        };
      };

      # The NixOS module, lifted to the top level so the host config can
      # import it directly without going through a separate flake.
      streamctlModule = { config, lib, pkgs, ... }:
        let
          cfg = config.services.streamctl;
          pkg = streamctlPackage pkgs;
        in
        {
          options.services.streamctl = {
            enable = lib.mkEnableOption "streamctl scheduled RTMP streamer";

            listen = lib.mkOption {
              type = lib.types.str;
              default = "127.0.0.1:8080";
              description = "Address:port for the web UI to listen on. Put a reverse proxy in front for TLS.";
            };

            secretFile = lib.mkOption {
              type = lib.types.path;
              description = ''
                Path to a file containing the login secret for the web UI.
                File should contain a single line with the secret. Mode 0400, owned by the streamctl user.
              '';
              example = "/var/lib/streamctl/secret";
            };

            metricsTokenFile = lib.mkOption {
              type = lib.types.str;
              default = "";
              description = ''
                Optional path to a streamctl-owned 0400 bearer token file.
                When configured, the Prometheus endpoint is available at /metrics.
              '';
            };

            videoDir = lib.mkOption {
              type = lib.types.str;
              default = "/var/lib/streamctl/videos";
              description = "Directory where video files are uploaded via scp.";
            };

            cacheDir = lib.mkOption {
              type = lib.types.str;
              default = "/var/lib/streamctl/cache";
              description = "Directory where remote video files are prefetched before streaming.";
            };

            hlsDir = lib.mkOption {
              type = lib.types.str;
              default = "/var/lib/streamctl/hls";
              description = "Directory where generated HLS playlists and segments are written.";
            };

            nostrKeyDir = lib.mkOption {
              type = lib.types.str;
              default = "/var/lib/streamctl/nostr-keys";
              description = "Directory where Nostr private keys are stored.";
            };

            publicBaseURL = lib.mkOption {
              type = lib.types.str;
              default = "";
              description = "Public HTTPS base URL used for HLS links in Nostr live events.";
            };

            btcppOAuthBaseURL = lib.mkOption {
              type = lib.types.str;
              default = "https://btcpp.dev";
              description = "bitcoin++ OAuth authorization server base URL.";
            };

            btcppOAuthClientID = lib.mkOption {
              type = lib.types.str;
              default = "";
              description = "Registered confidential OAuth client ID for interactive streamctl login.";
            };

            btcppOAuthClientSecretFile = lib.mkOption {
              type = lib.types.str;
              default = "";
              description = "Root-readable 0400 file containing the bitcoin++ OAuth client secret.";
            };

            btcppOAuthRedirectURL = lib.mkOption {
              type = lib.types.str;
              default = "";
              description = "Registered OAuth callback URL. Empty derives it from publicBaseURL.";
            };

            btcppAPIBaseURL = lib.mkOption {
              type = lib.types.str;
              default = "https://btcpp.dev";
              description = "bitcoin++ API base URL used by production workspaces and for public broadcast status.";
            };

            btcppAPITokenFile = lib.mkOption {
              type = lib.types.str;
              default = "";
              description = "Root-readable 0400 bitcoin++ machine token used by production workspaces and broadcast status. Linked stream units receive a private copy through systemd credentials.";
            };

            gpuWorkerHost = lib.mkOption {
              type = lib.types.str;
              default = "";
              description = "Optional SSH target for a GPU transcode worker, e.g. ubuntu@1.2.3.4.";
            };

            gpuWorkerCommand = lib.mkOption {
              type = lib.types.str;
              default = "/root/transcode-nvenc.sh";
              description = "Command path on the GPU worker that accepts one Spaces object path.";
            };

            renderWorkerCommand = lib.mkOption {
              type = lib.types.str;
              default = "/root/render-from-spaces.py";
              description = "Bucket-aware conf-render wrapper on the shared GPU worker.";
            };

            renderOutputDir = lib.mkOption {
              type = lib.types.str;
              default = "/workspace/streamctl-render-output";
              description = "Temporary output directory on the GPU worker, partitioned by streamctl render job ID.";
            };

            digitalOceanTokenFile = lib.mkOption {
              type = lib.types.str;
              default = "";
              description = "Optional DigitalOcean API token file. Enables creating/destroying GPU worker Droplets from the web UI.";
            };

            runpodTokenFile = lib.mkOption {
              type = lib.types.str;
              default = "";
              description = "Optional RunPod API token file. Enables creating/destroying GPU worker pods from the web UI.";
            };

            runpodPodName = lib.mkOption {
              type = lib.types.str;
              default = "streamctl-gpu-worker";
              description = "Name for the managed RunPod worker pod.";
            };

            runpodGPUType = lib.mkOption {
              type = lib.types.str;
              default = "NVIDIA L40S";
              description = "RunPod GPU type ID for managed workers.";
            };

            runpodImage = lib.mkOption {
              type = lib.types.str;
              default = "runpod/pytorch:2.4.0-py3.11-cuda12.4.1-devel-ubuntu22.04";
              description = "RunPod container image for managed workers.";
            };

            runpodCloudType = lib.mkOption {
              type = lib.types.str;
              default = "SECURE";
              description = "RunPod cloud type for managed workers.";
            };

            gpuDropletName = lib.mkOption {
              type = lib.types.str;
              default = "streamctl-gpu-worker";
              description = "Name for the managed GPU worker Droplet.";
            };

            gpuDropletRegion = lib.mkOption {
              type = lib.types.str;
              default = "nyc2";
              description = "DigitalOcean region for the managed GPU worker.";
            };

            gpuDropletSize = lib.mkOption {
              type = lib.types.str;
              default = "gpu-h100x1-80gb";
              description = "DigitalOcean size slug for the managed GPU worker.";
            };

            gpuDropletImage = lib.mkOption {
              type = lib.types.str;
              default = "gpu-h100x1-base";
              description = "DigitalOcean image slug for the managed GPU worker.";
            };

            gpuSSHKeyName = lib.mkOption {
              type = lib.types.str;
              default = "streamctl-deploy";
              description = "DigitalOcean SSH key name, fingerprint, or ID to install on managed GPU workers.";
            };

            gpuWorkerUser = lib.mkOption {
              type = lib.types.str;
              default = "root";
              description = "SSH user for managed GPU workers.";
            };

            gpuDestroyAfterJob = lib.mkOption {
              type = lib.types.bool;
              default = false;
              description = "Destroy the managed GPU worker Droplet after a successful transcode job.";
            };

            remote = lib.mkOption {
              type = lib.types.str;
              default = "";
              description = "rclone remote root for prefetched clips, e.g. spaces:bucket.";
            };

            rcloneConfigFile = lib.mkOption {
              type = lib.types.str;
              default = "";
              description = "Optional path to an rclone config file used by generated prefetch units.";
            };

            notificationEmail = lib.mkOption {
              type = lib.types.str;
              default = "";
              description = "Email address for prefetch success/failure notifications. Empty disables email.";
            };

            sendmailPath = lib.mkOption {
              type = lib.types.str;
              default = "${pkgs.msmtp}/bin/sendmail";
              description = "sendmail-compatible binary used for prefetch notification email.";
            };

            cleanupCache = lib.mkOption {
              type = lib.types.bool;
              default = true;
              description = "Delete cached remote files after ffmpeg exits successfully.";
            };

            normalizePrefetch = lib.mkOption {
              type = lib.types.bool;
              default = true;
              description = "Re-encode prefetched remote clips to streaming-friendly CBR before streaming.";
            };

            normalizeVideoBitrate = lib.mkOption {
              type = lib.types.str;
              default = "6800k";
              description = "Target video bitrate for normalized prefetched clips.";
            };

            normalizeAudioBitrate = lib.mkOption {
              type = lib.types.str;
              default = "160k";
              description = "Target audio bitrate for normalized prefetched clips.";
            };

            dataDir = lib.mkOption {
              type = lib.types.str;
              default = "/var/lib/streamctl";
              description = "Directory for the SQLite database.";
            };

            user = lib.mkOption {
              type = lib.types.str;
              default = "streamctl";
              description = "User the ffmpeg processes run as.";
            };

            group = lib.mkOption {
              type = lib.types.str;
              default = "streamctl";
              description = "Group for the streamctl user.";
            };

            openFirewall = lib.mkOption {
              type = lib.types.bool;
              default = false;
              description = "Open the listen port in the firewall. Usually you want a reverse proxy instead.";
            };
          };

          config = lib.mkIf cfg.enable {
            assertions = [
              {
                assertion = (cfg.btcppOAuthClientID == "") == (cfg.btcppOAuthClientSecretFile == "");
                message = "services.streamctl.btcppOAuthClientID and btcppOAuthClientSecretFile must be configured together";
              }
            ];

            users.users.${cfg.user} = {
              isSystemUser = true;
              group = cfg.group;
              home = cfg.dataDir;
              createHome = false;
              description = "streamctl service user";
              useDefaultShell = true; # so scp uploads to videoDir work
            };

            users.groups.${cfg.group} = { };

            systemd.tmpfiles.rules = [
              "d ${cfg.dataDir} 0750 ${cfg.user} ${cfg.group} - -"
              "d ${cfg.videoDir} 0750 ${cfg.user} ${cfg.group} - -"
              "d ${cfg.cacheDir} 0750 ${cfg.user} ${cfg.group} - -"
              "d ${cfg.hlsDir} 0750 ${cfg.user} ${cfg.group} - -"
              "d ${cfg.nostrKeyDir} 0750 ${cfg.user} ${cfg.group} - -"
            ];

            systemd.services.streamctl = {
              description = "streamctl web UI";
              after = [ "network.target" ];
              wantedBy = [ "multi-user.target" ];

              path = [ pkgs.systemd pkgs.ffmpeg pkgs.rclone pkgs.msmtp pkgs.openssh ];

              serviceConfig = {
                Type = "simple";
                # Root: needed to write systemd units and run systemctl.
                # ffmpeg processes drop to cfg.user via the generated units.
                User = "root";
                Group = "root";
                ExecStart = ''
                  ${pkg}/bin/cmd \
                    -listen=${cfg.listen} \
                    -db=${cfg.dataDir}/streamctl.db \
                    -video-dir=${cfg.videoDir} \
                    -cache-dir=${cfg.cacheDir} \
                    -hls-dir=${cfg.hlsDir} \
                    -nostr-key-dir=${cfg.nostrKeyDir} \
                    -public-base-url=${cfg.publicBaseURL} \
                    ${lib.optionalString (cfg.btcppOAuthClientID != "") "-btcpp-oauth-base=${lib.escapeShellArg cfg.btcppOAuthBaseURL} \\"}
                    ${lib.optionalString (cfg.btcppOAuthClientID != "") "-btcpp-oauth-client-id=${lib.escapeShellArg cfg.btcppOAuthClientID} \\"}
                    ${lib.optionalString (cfg.btcppOAuthClientID != "") "-btcpp-oauth-client-secret-file=${lib.escapeShellArg cfg.btcppOAuthClientSecretFile} \\"}
                    ${lib.optionalString (cfg.btcppOAuthRedirectURL != "") "-btcpp-oauth-redirect-url=${lib.escapeShellArg cfg.btcppOAuthRedirectURL} \\"}
                    ${lib.optionalString (cfg.btcppAPITokenFile != "") "-btcpp-api-base=${lib.escapeShellArg cfg.btcppAPIBaseURL} \\"}
                    ${lib.optionalString (cfg.btcppAPITokenFile != "") "-btcpp-api-token-file=${lib.escapeShellArg cfg.btcppAPITokenFile} \\"}
                    -gpu-worker-host=${cfg.gpuWorkerHost} \
                    -gpu-worker-ssh-key=${cfg.dataDir}/gpu-worker-ssh-key \
                    -gpu-worker-command=${cfg.gpuWorkerCommand} \
                    -render-worker-command=${cfg.renderWorkerCommand} \
                    -render-output-dir=${cfg.renderOutputDir} \
                    -do-token-file=${cfg.digitalOceanTokenFile} \
                    -runpod-token-file=${cfg.runpodTokenFile} \
                    -runpod-pod-name=${lib.escapeShellArg cfg.runpodPodName} \
                    -runpod-gpu-type=${lib.escapeShellArg cfg.runpodGPUType} \
                    -runpod-image=${lib.escapeShellArg cfg.runpodImage} \
                    -runpod-cloud-type=${lib.escapeShellArg cfg.runpodCloudType} \
                    -gpu-droplet-name=${cfg.gpuDropletName} \
                    -gpu-droplet-region=${cfg.gpuDropletRegion} \
                    -gpu-droplet-size=${cfg.gpuDropletSize} \
                    -gpu-droplet-image=${cfg.gpuDropletImage} \
                    -gpu-ssh-key-name=${cfg.gpuSSHKeyName} \
                    -gpu-worker-user=${cfg.gpuWorkerUser} \
                    -gpu-destroy-after-job=${lib.boolToString cfg.gpuDestroyAfterJob} \
                    -remote=${cfg.remote} \
                    -rclone-config=${cfg.rcloneConfigFile} \
                    -notify-email=${cfg.notificationEmail} \
                    -sendmail=${cfg.sendmailPath} \
                    -cleanup-cache=${lib.boolToString cfg.cleanupCache} \
                    -normalize-prefetch=${lib.boolToString cfg.normalizePrefetch} \
                    -normalize-video-bitrate=${cfg.normalizeVideoBitrate} \
                    -normalize-audio-bitrate=${cfg.normalizeAudioBitrate} \
                    -unit-dir=/run/systemd/system \
                    -unit-prefix=streamctl- \
                    -run-user=${cfg.user}
                '';
                EnvironmentFile = "-/run/streamctl/env";
                Restart = "on-failure";
                RestartSec = 5;
              };

              preStart = ''
                mkdir -p /run/streamctl
                umask 077
                echo "STREAMCTL_SECRET=$(cat ${cfg.secretFile})" > /run/streamctl/env
                ${lib.optionalString (cfg.metricsTokenFile != "") ''
                  if [ -s ${lib.escapeShellArg cfg.metricsTokenFile} ]; then
                    echo "STREAMCTL_METRICS_TOKEN=$(cat ${lib.escapeShellArg cfg.metricsTokenFile})" >> /run/streamctl/env
                  fi
                ''}
                chmod 0400 /run/streamctl/env
              '';
            };

            networking.firewall = lib.mkIf cfg.openFirewall {
              allowedTCPPorts = [
                (lib.toInt (lib.last (lib.splitString ":" cfg.listen)))
              ];
            };
          };
        };
    in
    # Per-system outputs (the package, devshell).
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs {
          inherit system;
          config.allowUnfreePredicate = pkg: builtins.elem (nixpkgs.lib.getName pkg) [
            "terraform"
          ];
        };
      in
      {
        packages.default = streamctlPackage pkgs;
        packages.streamctl = streamctlPackage pkgs;

        checks.worker-input-cache = pkgs.runCommand "streamctl-worker-input-cache-tests" {
          nativeBuildInputs = [ pkgs.python3 pkgs.rclone ];
        } ''
          python3 -B -m unittest discover -s ${./streamctl/internal/handlers/worker} -p test_input_cache.py -v
          touch $out
        '';

        checks.worker-editing-previews = pkgs.runCommand "streamctl-worker-preview-tests" {
          nativeBuildInputs = [ pkgs.python3 pkgs.rclone pkgs.ffmpeg ];
        } ''
          python3 -B -m unittest discover -s ${./streamctl/internal/handlers/worker} -p test_prepare_proxy.py -v
          touch $out
        '';

        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go
            git
            gopls
            sqlite
            ffmpeg
            terraform
            doctl
            gnumake
            openssl
            nixos-rebuild
          ];
        };
      }
    ) // {
      # System-independent outputs (NixOS modules and configurations).
      nixosModules.default = streamctlModule;
      nixosModules.streamctl = streamctlModule;

      # The host configuration. `make deploy` builds this and pushes it.
      nixosConfigurations.streamctl = nixpkgs.lib.nixosSystem {
        system = "x86_64-linux";
        modules = [
          streamctlModule
          ./nixos/configuration.nix
        ];
      };
    };
}
