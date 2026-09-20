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
For 8-bit 4:2:0 H.264/HEVC sources, NVDEC decodes directly to CUDA frames,
`scale_cuda` resizes them on the GPU, and NVENC encodes without downloading frames
to the CPU. Bicubic resizing and a separate output frame pool avoid retaining
all decoder surfaces. Other source formats (including 10-bit and 4:2:2 camera
footage) retain CPU decoding/scaling with NVENC encoding for compatibility with
the worker's FFmpeg. A sequence uses CUDA only if every chunk is eligible.
The progress stage and metadata's `videoProcessing` field distinguish these
paths. Audio remains on the CPU; there is no software video encoder fallback.

Requirements: the existing worker's rclone configuration, Python 3, ffprobe, and
ffmpeg with working NVENC, CUDA decoding and `scale_cuda` support. New managed workers install Python 3 during
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
bucket, substituting GPU processing with CPU scaling and libx264. It checks two-chunk
sources with and without audio, combined duration, frequent keyframes, seeking,
browser-compatible output, ready metadata and retry.
On an NVIDIA machine, set `STREAMCTL_TEST_GPU=1` to run the same tests with CUDA decoding/resizing and NVENC.
Before production rollout, run that GPU test and one representative source
through the queue, then verify browser seeking and a saved cut against the source.


## CUDA benchmark

On 2026-09-20, a two-minute sample of staged event footage was tested on the
L40S worker with FFmpeg 4.4.2. Alternating CPU/CUDA/CUDA/CPU preprocessing runs
averaged 7.824 seconds with CPU decoding/scaling and 5.155 seconds with CUDA,
using NVENC and the same output settings in both cases (1.52x throughput).
Output video/audio start times, duration, resolution and pixel format matched.
Another production job remained running during this test. These are short-clip
encoding measurements, not full-job times including download and upload.
