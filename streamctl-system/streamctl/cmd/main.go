package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"streamctl/internal/broadcastplans"
	"streamctl/internal/btcppclient"
	"streamctl/internal/btcppoauth"
	"streamctl/internal/db"
	"streamctl/internal/handlers"
	"streamctl/internal/nostrpub"
	appmetrics "streamctl/internal/observability"
	"streamctl/internal/systemd"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "btcpp-candidates" {
		if err := runBTCPPCandidates(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "btcpp-recording" {
		if err := runBTCPPRecording(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "btcpp-broadcast" {
		if err := runBTCPPBroadcast(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "nostr-publish" {
		if err := runNostrPublish(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}

	var (
		listen              = flag.String("listen", ":8080", "address to listen on")
		dbPath              = flag.String("db", "/var/lib/streamctl/streamctl.db", "path to SQLite database")
		videoDir            = flag.String("video-dir", "/var/lib/streamctl/videos", "directory containing video files")
		cacheDir            = flag.String("cache-dir", "/var/lib/streamctl/cache", "directory for prefetched remote video files")
		hlsDir              = flag.String("hls-dir", "/var/lib/streamctl/hls", "directory for generated HLS playlists and segments")
		remote              = flag.String("remote", "", "rclone remote for Spaces objects, e.g. spaces:bucket")
		rcloneCfg           = flag.String("rclone-config", "", "path to rclone config file for generated prefetch services")
		notifyEmail         = flag.String("notify-email", "", "email address for prefetch success/failure notifications")
		sendmailPath        = flag.String("sendmail", "/run/current-system/sw/bin/sendmail", "sendmail-compatible binary for notifications")
		cleanupCache        = flag.Bool("cleanup-cache", true, "delete cached remote files after successful streaming")
		normalize           = flag.Bool("normalize-prefetch", true, "re-encode prefetched remote clips to streaming-friendly CBR before streaming")
		videoBitrate        = flag.String("normalize-video-bitrate", "6800k", "video bitrate for normalized prefetched clips")
		audioBitrate        = flag.String("normalize-audio-bitrate", "160k", "audio bitrate for normalized prefetched clips")
		nostrKeyDir         = flag.String("nostr-key-dir", "/var/lib/streamctl/nostr-keys", "directory for stored Nostr private keys")
		publicBaseURL       = flag.String("public-base-url", "", "public base URL used in Nostr live events, e.g. https://stream.example.com")
		btcppOAuthBase      = flag.String("btcpp-oauth-base", "https://btcpp.dev", "bitcoin++ OAuth server base URL")
		btcppOAuthClientID  = flag.String("btcpp-oauth-client-id", "", "registered bitcoin++ OAuth client ID")
		btcppOAuthSecret    = flag.String("btcpp-oauth-client-secret-file", "", "path to a private bitcoin++ OAuth client secret file")
		btcppOAuthRedirect  = flag.String("btcpp-oauth-redirect-url", "", "OAuth callback URL; defaults to <public-base-url>/oauth/callback")
		btcppAPIBase        = flag.String("btcpp-api-base", "https://btcpp.dev", "bitcoin++ API base URL used by production workspaces and broadcast status")
		btcppAPITokenFile   = flag.String("btcpp-api-token-file", "", "path to the bitcoin++ machine API token")
		btcppXEndpointID    = flag.Int64("btcpp-x-endpoint-id", 0, "saved X RTMP endpoint ID for importing website broadcast plans; use either ID or name")
		btcppXEndpointName  = flag.String("btcpp-x-endpoint-name", "", "exact saved X RTMP endpoint name for importing website broadcast plans")
		btcppPlanInterval   = flag.Duration("btcpp-plan-poll-interval", 3*time.Minute, "interval between website broadcast plan imports")
		gpuWorkerHost       = flag.String("gpu-worker-host", "", "SSH target for GPU transcode worker, e.g. ubuntu@1.2.3.4")
		gpuWorkerSSHKey     = flag.String("gpu-worker-ssh-key", "", "managed worker SSH key path; defaults to the production data directory")
		gpuWorkerCommand    = flag.String("gpu-worker-command", "/root/transcode-nvenc.sh", "command path on GPU worker used to process one Spaces path")
		renderWorkerCommand = flag.String("render-worker-command", "/root/render-from-spaces.py", "render wrapper command on the GPU worker")
		renderOutputDir     = flag.String("render-output-dir", "/workspace/streamctl-render-output", "temporary render output directory on the GPU worker")
		doTokenFile         = flag.String("do-token-file", "", "DigitalOcean API token file for managed GPU workers")
		runpodTokenFile     = flag.String("runpod-token-file", "", "RunPod API token file for managed GPU workers")
		runpodPodName       = flag.String("runpod-pod-name", "streamctl-gpu-worker", "managed RunPod pod name")
		runpodGPUType       = flag.String("runpod-gpu-type", "NVIDIA L40S", "managed RunPod GPU type ID")
		runpodImage         = flag.String("runpod-image", "runpod/pytorch:2.4.0-py3.11-cuda12.4.1-devel-ubuntu22.04", "managed RunPod container image")
		runpodCloudType     = flag.String("runpod-cloud-type", "SECURE", "managed RunPod cloud type")
		gpuDropletName      = flag.String("gpu-droplet-name", "streamctl-gpu-worker", "managed GPU Droplet name")
		gpuDropletRegion    = flag.String("gpu-droplet-region", "nyc2", "managed GPU Droplet region")
		gpuDropletSize      = flag.String("gpu-droplet-size", "gpu-h100x1-80gb", "managed GPU Droplet size slug")
		gpuDropletImage     = flag.String("gpu-droplet-image", "gpu-h100x1-base", "managed GPU Droplet image slug")
		gpuSSHKeyName       = flag.String("gpu-ssh-key-name", "streamctl-deploy", "DigitalOcean SSH key name to install on managed GPU Droplets")
		gpuWorkerUser       = flag.String("gpu-worker-user", "root", "SSH user for managed GPU Droplets")
		gpuDestroyAfterJob  = flag.Bool("gpu-destroy-after-job", false, "destroy managed GPU worker after a successful transcode job")
		unitDir             = flag.String("unit-dir", "/etc/systemd/system", "directory for generated systemd units")
		unitPrefix          = flag.String("unit-prefix", "streamctl-", "prefix for generated systemd unit names")
		runUser             = flag.String("run-user", "streamctl", "user to run streams as")
	)
	flag.Parse()
	if *btcppXEndpointID < 0 || *btcppPlanInterval <= 0 {
		log.Fatal("broadcast plan endpoint ID must be nonnegative and poll interval must be positive")
	}
	if *btcppXEndpointID > 0 && *btcppXEndpointName != "" {
		log.Fatal("configure either -btcpp-x-endpoint-id or -btcpp-x-endpoint-name")
	}
	importBroadcastPlans := *btcppXEndpointID > 0 || *btcppXEndpointName != ""
	if importBroadcastPlans && (strings.TrimSpace(*btcppAPITokenFile) == "" || strings.TrimSpace(*remote) == "") {
		log.Fatal("broadcast plan imports require -btcpp-api-token-file and -remote")
	}

	secret := strings.TrimSpace(os.Getenv("STREAMCTL_SECRET"))
	var oauthClient *btcppoauth.Client
	if strings.TrimSpace(*btcppOAuthClientID) != "" || strings.TrimSpace(*btcppOAuthSecret) != "" {
		if strings.TrimSpace(*btcppOAuthClientID) == "" || strings.TrimSpace(*btcppOAuthSecret) == "" {
			log.Fatal("both -btcpp-oauth-client-id and -btcpp-oauth-client-secret-file are required")
		}
		clientSecret, err := btcppoauth.SecretFromFile(*btcppOAuthSecret)
		if err != nil {
			log.Fatal(err)
		}
		redirectURL := strings.TrimSpace(*btcppOAuthRedirect)
		if redirectURL == "" && strings.TrimSpace(*publicBaseURL) != "" {
			redirectURL = strings.TrimRight(strings.TrimSpace(*publicBaseURL), "/") + "/oauth/callback"
		}
		oauthClient = &btcppoauth.Client{
			BaseURL: *btcppOAuthBase, ClientID: *btcppOAuthClientID,
			ClientSecret: clientSecret, RedirectURL: redirectURL,
		}
		if err := oauthClient.Validate(); err != nil {
			log.Fatalf("configure bitcoin++ OAuth: %v", err)
		}
	}
	if secret == "" && oauthClient == nil {
		log.Fatal("configure bitcoin++ OAuth or set STREAMCTL_SECRET for break-glass access")
	}
	var btcppAPIClient *btcppclient.Client
	if strings.TrimSpace(*btcppAPITokenFile) != "" {
		token, err := btcppclient.TokenFromFile(*btcppAPITokenFile)
		if err != nil {
			log.Fatalf("configure bitcoin++ production API: %v", err)
		}
		btcppAPIClient = &btcppclient.Client{BaseURL: *btcppAPIBase, Token: token}
	}

	database, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("opening database: %v", err)
	}
	defer database.Close()

	if err := database.Migrate(); err != nil {
		log.Fatalf("migrating database: %v", err)
	}

	sysd := &systemd.Manager{
		UnitDir:        *unitDir,
		UnitPrefix:     *unitPrefix,
		RunUser:        *runUser,
		VideoDir:       *videoDir,
		CacheDir:       *cacheDir,
		HLSDir:         *hlsDir,
		Remote:         *remote,
		RcloneConfig:   *rcloneCfg,
		NotifyEmail:    *notifyEmail,
		SendmailPath:   *sendmailPath,
		CleanupCache:   *cleanupCache,
		Normalize:      *normalize,
		VideoBitrate:   *videoBitrate,
		AudioBitrate:   *audioBitrate,
		NostrKeyDir:    *nostrKeyDir,
		PublicBaseURL:  strings.TrimRight(*publicBaseURL, "/"),
		SelfPath:       os.Args[0],
		BTCPPAPIBase:   strings.TrimRight(*btcppAPIBase, "/"),
		BTCPPTokenFile: strings.TrimSpace(*btcppAPITokenFile),
	}

	streams, err := database.ListStreams()
	if err != nil {
		log.Printf("listing streams for startup sync: %v", err)
	} else {
		for i := range streams {
			if err := sysd.Sync(&streams[i]); err != nil {
				log.Printf("startup sync for stream %d failed: %v", streams[i].ID, err)
			}
		}
	}

	h := &handlers.Handler{
		DB:                  database,
		Secret:              secret,
		VideoDir:            *videoDir,
		CacheDir:            *cacheDir,
		HLSDir:              *hlsDir,
		Remote:              *remote,
		RcloneConfig:        *rcloneCfg,
		NostrKeyDir:         *nostrKeyDir,
		NostrKeyOwner:       *runUser,
		GPUWorkerHost:       strings.TrimSpace(*gpuWorkerHost),
		GPUWorkerSSHKey:     strings.TrimSpace(*gpuWorkerSSHKey),
		GPUWorkerCommand:    strings.TrimSpace(*gpuWorkerCommand),
		RenderWorkerCommand: strings.TrimSpace(*renderWorkerCommand),
		RenderOutputDir:     strings.TrimRight(strings.TrimSpace(*renderOutputDir), "/"),
		DOTokenFile:         strings.TrimSpace(*doTokenFile),
		RunPodTokenFile:     strings.TrimSpace(*runpodTokenFile),
		RunPodPodName:       strings.TrimSpace(*runpodPodName),
		RunPodGPUType:       strings.TrimSpace(*runpodGPUType),
		RunPodImage:         strings.TrimSpace(*runpodImage),
		RunPodCloudType:     strings.TrimSpace(*runpodCloudType),
		GPUDropletName:      strings.TrimSpace(*gpuDropletName),
		GPUDropletRegion:    strings.TrimSpace(*gpuDropletRegion),
		GPUDropletSize:      strings.TrimSpace(*gpuDropletSize),
		GPUDropletImage:     strings.TrimSpace(*gpuDropletImage),
		GPUSSHKeyName:       strings.TrimSpace(*gpuSSHKeyName),
		GPUWorkerUser:       strings.TrimSpace(*gpuWorkerUser),
		GPUDestroyAfterJob:  *gpuDestroyAfterJob,
		Systemd:             sysd,
		OAuth:               oauthClient,
		BTCPPBaseURL:        strings.TrimRight(strings.TrimSpace(*btcppAPIBase), "/"),
	}
	// Keep the interface nil when no API token is configured.
	if btcppAPIClient != nil {
		h.BTCPP = btcppAPIClient
	}
	if importBroadcastPlans {
		planSync := &broadcastplans.Syncer{DB: database, Client: btcppAPIClient, Scheduler: sysd, XEndpointID: *btcppXEndpointID, XEndpointName: *btcppXEndpointName}
		go planSync.Run(context.Background(), *btcppPlanInterval)
	}

	mux := http.NewServeMux()
	metrics := appmetrics.New("streamctl")
	if err := metrics.Register(appmetrics.NewOperationsCollector(database, sysd)); err != nil {
		log.Fatalf("registering operations metrics: %v", err)
	}
	mux.Handle("/metrics", metrics.Handler(strings.TrimSpace(os.Getenv("STREAMCTL_METRICS_TOKEN"))))
	h.Register(mux)

	log.Printf("streamctl listening on %s", *listen)
	if err := http.ListenAndServe(*listen, metrics.Middleware(mux)); err != nil {
		log.Fatal(err)
	}
}

func btcppCommandClient(fs *flag.FlagSet) (*string, *string) {
	baseURL := fs.String("api-base", "https://btcpp.dev", "bitcoin++ website base URL")
	tokenFile := fs.String("token-file", "/var/lib/streamctl/btcpp-api-token", "path to a 0400 bitcoin++ API token file")
	return baseURL, tokenFile
}

func runBTCPPCandidates(args []string) error {
	fs := flag.NewFlagSet("btcpp-candidates", flag.ContinueOnError)
	baseURL, tokenFile := btcppCommandClient(fs)
	conference := fs.String("conference", "", "bitcoin++ conference tag")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*conference) == "" {
		return fmt.Errorf("conference is required")
	}
	token, err := btcppclient.TokenFromFile(*tokenFile)
	if err != nil {
		return err
	}
	client := &btcppclient.Client{BaseURL: *baseURL, Token: token}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	candidates, err := client.RecordingCandidates(ctx, *conference)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(candidates)
}

func runBTCPPRecording(args []string) error {
	fs := flag.NewFlagSet("btcpp-recording", flag.ContinueOnError)
	baseURL, tokenFile := btcppCommandClient(fs)
	conference := fs.String("conference", "", "bitcoin++ conference tag")
	talkID := fs.String("talk-id", "", "bitcoin++ conference talk UUID")
	fileURI := fs.String("file-uri", "", "DigitalOcean Spaces object key")
	youtubeURL := fs.String("youtube-url", "", "published YouTube URL")
	xURL := fs.String("x-url", "", "published X post URL")
	xReplyURL := fs.String("x-reply-url", "", "published X reply URL")
	publishedAt := fs.String("published-at", "", "RFC3339 publication time; empty leaves it unchanged")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*conference) == "" || strings.TrimSpace(*talkID) == "" {
		return fmt.Errorf("conference and talk-id are required")
	}
	token, err := btcppclient.TokenFromFile(*tokenFile)
	if err != nil {
		return err
	}
	update := btcppclient.RecordingUpdate{}
	if fsHasFlag(fs, "file-uri") {
		update.FileURI = fileURI
	}
	if fsHasFlag(fs, "youtube-url") {
		update.YouTubeURL = youtubeURL
	}
	if fsHasFlag(fs, "x-url") {
		update.XURL = xURL
	}
	if fsHasFlag(fs, "x-reply-url") {
		update.XReplyURL = xReplyURL
	}
	if fsHasFlag(fs, "published-at") {
		update.PublishedAt = publishedAt
	}
	client := &btcppclient.Client{BaseURL: *baseURL, Token: token}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	recording, err := client.PutRecording(ctx, *conference, *talkID, update)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(recording)
}

func runBTCPPBroadcast(args []string) error {
	fs := flag.NewFlagSet("btcpp-broadcast", flag.ContinueOnError)
	baseURL, tokenFile := btcppCommandClient(fs)
	recordingID := fs.String("recording-id", "", "bitcoin++ recording UUID")
	conference := fs.String("conference", "", "bitcoin++ conference tag for a conference-wide broadcast")
	title := fs.String("title", "", "title of a conference-wide broadcast")
	state := fs.String("state", "", "scheduled, live, ended, or failed")
	hlsURL := fs.String("hls-url", "", "public HLS playlist URL")
	xBroadcastURL := fs.String("x-broadcast-url", "", "optional X broadcast URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (strings.TrimSpace(*recordingID) == "") == (strings.TrimSpace(*conference) == "") || strings.TrimSpace(*state) == "" {
		return fmt.Errorf("exactly one of recording-id or conference, and state, are required")
	}
	token, err := btcppclient.TokenFromFile(*tokenFile)
	if err != nil {
		return err
	}
	client := &btcppclient.Client{BaseURL: *baseURL, Token: token}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	update := btcppclient.BroadcastUpdate{
		State: *state, HLSURL: *hlsURL, XBroadcastURL: *xBroadcastURL,
	}
	var broadcast *btcppclient.Broadcast
	if strings.TrimSpace(*conference) != "" {
		update.Title = strings.TrimSpace(*title)
		broadcast, err = client.PutConferenceBroadcast(ctx, *conference, update)
	} else {
		broadcast, err = client.PutBroadcast(ctx, *recordingID, update)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(broadcast)
}

func fsHasFlag(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(flag *flag.Flag) {
		if flag.Name == name {
			found = true
		}
	})
	return found
}

func runNostrPublish(args []string) error {
	fs := flag.NewFlagSet("nostr-publish", flag.ContinueOnError)
	keyFile := fs.String("key-file", "", "path to stored hex private key")
	relays := fs.String("relays", "", "comma-separated relay URLs")
	status := fs.String("status", "", "planned, live, or ended")
	dTag := fs.String("d", "", "NIP-53 d tag")
	title := fs.String("title", "", "live event title")
	summary := fs.String("summary", "", "live event summary")
	streamingURL := fs.String("streaming", "", "HLS playlist URL")
	starts := fs.Int64("starts", 0, "Unix start timestamp")
	if err := fs.Parse(args); err != nil {
		return err
	}
	results, err := nostrpub.PublishLiveEvent(context.Background(), nostrpub.PublishOptions{
		KeyFile:      *keyFile,
		Relays:       strings.Split(*relays, ","),
		Status:       *status,
		DTag:         *dTag,
		Title:        *title,
		Summary:      *summary,
		StreamingURL: *streamingURL,
		Starts:       *starts,
	})
	for _, res := range results {
		if res.Error != nil {
			log.Printf("nostr publish %s failed: %v", res.RelayURL, res.Error)
		} else {
			log.Printf("nostr publish %s ok", res.RelayURL)
		}
	}
	if err != nil {
		return fmt.Errorf("nostr publish: %w", err)
	}
	return nil
}
