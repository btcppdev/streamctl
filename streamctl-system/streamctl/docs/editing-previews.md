# Editing previews on the GPU worker

Preparing media queues a durable `production_proxy_jobs` entry. The same GPU
dispatcher handles previews, normalization and renders, with only one active job.
An active job is never preempted. When idle, the worker takes a preview first,
then normalization, then a render; previews are FIFO. Continuous preview arrivals
can delay the other queues.

The controller resolves source chunks and stages an embedded Python runner over
SSH. The worker downloads the chunks from Spaces, concatenates their timelines,
encodes 480p H.264 with `h264_nvenc` and optional AAC audio, and uploads the MP4
followed by its versioned metadata. The metadata is the ready marker. Source
paths, output paths and metadata remain compatible with existing editing cuts.
Decoding, scaling and audio still use CPU for camera-format compatibility; the
expensive H.264 encoding uses NVENC. There is no software encoder fallback.

Requirements: the existing worker's rclone configuration, Python 3, ffprobe, and
ffmpeg with working NVENC support. New managed workers install Python 3 during
setup. The runner is staged per attempt, so existing workers do not need a new
image, provided those tools are already installed. No new API credentials are
needed. The existing RunPod automatic provisioning policy also applies when
previews are the only queued work. Other workers use the existing Create GPU or
external-host setup. Do not create a separate worker for previews.

The Worker page reports download, GPU encoding and upload progress, with logs
and failed-job requeue controls. A queued or running preview keeps a managed
worker from being automatically destroyed. Worker execution retains the existing
48-hour limit.

## Deploying with existing work

Finished previews are reused. Queued preparation jobs automatically use the GPU
after deployment. An interrupted legacy CPU preparation is requeued and restarts
from the source; partial CPU encoding cannot be resumed. GPU attempts persist
their worker host and unique unit name and are reconciled after controller
restarts instead of being blindly requeued. A confirmed missing managed worker
requeues its work. An SSH outage leaves it running until state can be checked.
Late results from previous attempts cannot change the current attempt's state.

Deploying this code restarts streamctl, so let a nearly finished CPU preparation
finish first if its progress is worth preserving. Ensure the worker has enough
scratch disk for all source chunks plus the preview, as with the previous CPU
implementation. Transferring footage and decoding can still limit total speed.

## Validation

Run Go tests with `go test ./...`. With Python 3, ffmpeg and rclone on PATH:

```
python3 -B -m unittest discover -s internal/handlers/worker -p test_prepare_proxy.py -v
```

The local integration test uses real ffmpeg and rclone with a temporary local
bucket, substituting only the video encoder with libx264. It checks a two-chunk,
no-audio source, combined duration, frequent keyframes, ready metadata and retry.
On an NVIDIA machine, set `STREAMCTL_TEST_GPU=1` to run the same test using NVENC.
Before production rollout, run that GPU test and one representative source
through the queue, then verify browser seeking and a saved cut against the source.
