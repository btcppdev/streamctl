#!/usr/bin/env python3
"""Prepare a browser editing proxy on an NVIDIA worker (stdlib only)."""
import collections
import datetime
import json
import math
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys


def write_status(root, state="running", stage="Starting", progress=0, **extra):
    data = dict(state=state, stage=stage, progress=max(0, min(100, progress)), **extra)
    temporary = root / "status.tmp"
    temporary.write_text(json.dumps(data))
    temporary.replace(root / "status.json")


def duration_ms(filename):
    value = subprocess.check_output([
        "ffprobe", "-v", "error", "-show_entries", "format=duration",
        "-of", "default=nw=1:nk=1", str(filename)], text=True)
    seconds = float(value.strip())
    if not math.isfinite(seconds) or seconds <= 0:
        raise ValueError("Invalid media duration")
    return round(seconds * 1000)


def run_progress(args, root, stage, duration=0):
    write_status(root, stage=stage)
    tail = collections.deque(maxlen=30)
    last = -1
    with subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                          text=True, errors="replace") as process:
        for line in process.stdout:
            tail.append(line[-1000:])
            percent = None
            if duration and line.startswith("out_time_us="):
                try:
                    percent = min(99, int(line.split("=", 1)[1]) // 1000 * 100 // duration)
                except ValueError:
                    pass
            elif not duration:
                match = re.search(r"(\d+)%", line)
                if match:
                    percent = min(99, int(match[1]))
            if percent is not None and percent != last:
                write_status(root, stage=stage, progress=percent)
                last = percent
        if process.wait():
            raise RuntimeError("".join(tail)[-4000:] or f"{args[0]} failed")


def encode_args(concat, output, height):
    # Decode/scale on CPU for compatibility with camera formats; NVENC handles
    # H.264 encoding. Do not silently fall back to software encoding.
    return ["ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
            "-fflags", "+genpts", "-f", "concat", "-safe", "0", "-i", str(concat),
            "-map", "0:v:0", "-map", "0:a:0?", "-sn", "-dn",
            "-vf", f"scale=-2:{height}", "-c:v", "h264_nvenc", "-preset", "p4",
            "-rc", "vbr", "-cq", "26", "-b:v", "0", "-pix_fmt", "yuv420p",
            "-force_key_frames", "expr:gte(t,n_forced*1)", "-forced-idr", "1",
            "-c:a", "aac", "-b:a", "96k", "-ac", "2",
            "-chunk_duration", "1000000", "-movflags", "+faststart",
            "-avoid_negative_ts", "make_zero", "-progress", "pipe:1", "-nostats", str(output)]


def prepare(request, root):
    work = root / "work"
    work.mkdir(exist_ok=True)
    remote = request["remote"].strip().rstrip("/")
    if not remote.endswith(":"):
        remote += "/"
    chunks = request["chunks"]
    if not chunks:
        raise ValueError("No source chunks")
    # A missing encoder should fail before downloading hours of source video.
    encoders = subprocess.check_output(["ffmpeg", "-hide_banner", "-encoders"], text=True)
    if "h264_nvenc" not in encoders:
        raise RuntimeError("Worker ffmpeg does not support h264_nvenc")
    metadata_chunks, inputs, source_duration = [], [], 0

    def transfer(source, destination, stage):
        run_progress(["rclone", "--stats", "1s", "--stats-one-line",
                      "--stats-log-level", "NOTICE", "copyto", "--no-traverse",
                      str(source), str(destination)], root, stage)

    for index, key in enumerate(chunks):
        local = work / f"input-{index:05d}.mp4"
        transfer(remote + key, local, f"Downloading chunk {index+1} of {len(chunks)}")
        source_duration += duration_ms(local)
        stat = local.stat()
        metadata_chunks.append(dict(path=key, size=stat.st_size,
                                    modifiedAt=datetime.datetime.fromtimestamp(
                                        stat.st_mtime, datetime.timezone.utc).isoformat()))
        inputs.append(f"file '{local}'\n")
    concat = work / "inputs.txt"
    concat.write_text("".join(inputs))
    output = work / "proxy.mp4"
    run_progress(encode_args(concat, output, request["height"]), root,
                 "Encoding proxy on GPU", source_duration)
    duration = duration_ms(output)
    metadata = dict(
        version=request["version"], generatedAt=datetime.datetime.now(datetime.timezone.utc).isoformat(),
        source=dict(path=request["source"], type=request["sourceType"],
                    durationMs=source_duration, chunks=metadata_chunks),
        proxy=dict(path=request["proxy"], durationMs=duration, height=request["height"],
                   videoCodec="h264", encoder="h264_nvenc", cq=26,
                   keyframeIntervalMs=1000, interleaveDurationMs=1000,
                   audioCodec="aac", audioBitrate="96k"))
    sidecar = work / "proxy.json"
    sidecar.write_text(json.dumps(metadata, indent=2) + "\n")
    # Metadata is the ready marker. Invalidate an older marker before replacing
    # its MP4 so an interrupted upload cannot be mistaken for a prepared source.
    removed = subprocess.run(["rclone", "deletefile", "--retries", "1", remote + request["sidecar"]],
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    if removed.returncode not in (0, 4):  # rclone: 4 means the file is absent
        raise RuntimeError("Remove old preview metadata: " + removed.stderr[-3000:])
    transfer(output, remote + request["proxy"], "Uploading proxy")
    transfer(sidecar, remote + request["sidecar"], "Uploading metadata")
    write_status(root, state="finished", stage="Complete", progress=100, durationMs=duration)


def main():
    root = Path(sys.argv[1]).resolve()
    try:
        prepare(json.loads((root / "request.json").read_text()), root)
    except Exception as error:
        write_status(root, state="failed", stage="Failed", error=str(error)[-4000:])
        print(str(error), file=sys.stderr)
        return 1
    finally:
        shutil.rmtree(root / "work", ignore_errors=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
