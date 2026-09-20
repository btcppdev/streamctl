{ config, lib, pkgs, modulesPath, ... }:

{
  imports = [
    # nixos-infect generates this file with the right disk/filesystem config.
    # If it's not present yet, you're deploying to a host that hasn't been
    # infected. SSH in once and copy /etc/nixos/hardware-configuration.nix
    # back to this directory.
    ./hardware-configuration.nix
    ./networking.nix
  ];

  # ---------- bootloader / DigitalOcean essentials ----------

  boot.loader.grub.enable = true;
  boot.loader.grub.device = "/dev/vda";
  boot.tmp.cleanOnBoot = true;

  # nixos-infect leaves networking config in /etc/nixos/networking.nix; if
  # you need static config, import it here. DHCP works fine on DO by default.
  networking.useDHCP = lib.mkDefault true;

  # ---------- system basics ----------

  networking.hostName = "streamctl";
  time.timeZone = "America/Los_Angeles"; # change as desired

  # Required for systemd timers using OnCalendar in local time.
  # If you'd rather use UTC everywhere, set time.timeZone = "UTC".

  # ---------- users / SSH ----------

  services.openssh = {
    enable = true;
    settings = {
      PermitRootLogin = "prohibit-password"; # key-only root, for nixos-rebuild --target-host
      PasswordAuthentication = false;
    };
  };

  # The SSH key Terraform installed on first boot lives in /root/.ssh/authorized_keys.
  # If you want to add more keys declaratively:
  users.users.root.openssh.authorizedKeys.keys = [
    "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDY8YVy1Y6QezGvJaKU3RKz+dSUFS2ieYW+1r5HFr6oL niftynei@gmail.com"
  ];

  # Allow `make upload` to scp directly to the streamctl user's video dir.
  # NixOS writes these keys to /etc/ssh/authorized_keys.d/streamctl, so no
  # home directory is needed for SSH key auth.
  users.users.streamctl.openssh.authorizedKeys.keys = [
    "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDY8YVy1Y6QezGvJaKU3RKz+dSUFS2ieYW+1r5HFr6oL niftynei@gmail.com"
  ];

  # ---------- streamctl ----------

  services.streamctl = {
    enable = true;
    listen = "127.0.0.1:8080";
    secretFile = "/var/lib/streamctl/secret";
    metricsTokenFile = "/var/lib/streamctl/metrics-token";
    # Configure these once rclone has a Spaces remote.
    remote = "spaces:btcpp";
    rcloneConfigFile = "/var/lib/streamctl/rclone.conf";
    notificationEmail = "inbox@btcpp.dev";
    cleanupCache = true;
    publicBaseURL = "https://stream.btcpp.dev";

    btcppOAuthClientID = "btcpp_client_CxsrZn-911KA_MRwhzwkUvHK";
    btcppOAuthClientSecretFile = "/var/lib/streamctl/btcpp-oauth-client-secret";
    btcppOAuthRedirectURL = "https://stream.btcpp.dev/oauth/callback";
    btcppAPITokenFile = "/var/lib/streamctl/btcpp-api-token";
    btcppXEndpointName = "Twitter Livestreams";
    digitalOceanTokenFile = "/var/lib/streamctl/digitalocean-token";
    runpodTokenFile = "/var/lib/streamctl/runpod-token";
    gpuDestroyAfterJob = true;
    # videoDir defaults to /var/lib/streamctl/videos — that's where you scp event files.
    # cacheDir defaults to /var/lib/streamctl/cache — remote clips are prefetched there.
  };

  # ---------- reverse proxy + TLS ----------
  #
  # Replace stream.example.com with your actual hostname before the first deploy.
  # Make sure the A/AAAA records point at the droplet's IP.

  services.nginx = {
    enable = true;
    recommendedProxySettings = true;
    recommendedTlsSettings = true;

    virtualHosts."stream.btcpp.dev" = {
      enableACME = true;
      forceSSL = true;
      locations."/" = {
        proxyPass = "http://127.0.0.1:8080";
        extraConfig = ''
          # Don't limit upload size on this proxy. We don't accept uploads
          # via the UI, but htmx forms etc. still go through here.
          client_max_body_size 0;
          # streamctl long-polls status; keep connections alive.
          proxy_read_timeout 300s;
        '';
      };
    };
  };

  security.acme = {
    acceptTerms = true;
    defaults.email = "hello@btcpp.dev";
  };

  networking.firewall.allowedTCPPorts = [ 22 80 443 ];

  # ---------- system packages ----------

  environment.systemPackages = with pkgs; [
    htop
    tmux
    ffmpeg # so you can manually test pushes from the shell
    rclone # for DO Spaces prefetch
    msmtp # sendmail-compatible client for prefetch status email
    sqlite # for poking at the streamctl db
  ];

  # ---------- nix settings ----------

  nix.settings = {
    experimental-features = [ "nix-command" "flakes" ];
    auto-optimise-store = true;
  };

  nix.gc = {
    automatic = true;
    dates = "weekly";
    options = "--delete-older-than 14d";
  };

  # NixOS state version. Don't bump this casually — read the manual if you
  # want to upgrade. It's a marker for the schema of stateful directories,
  # not the version of NixOS you're running.
  system.stateVersion = "25.05";
}
