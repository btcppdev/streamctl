#!/usr/bin/env python3
"""Download a conf-render manifest's inputs, render, and upload its outputs."""

from __future__ import annotations

import fcntl
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path, PurePosixPath


def fail(message: str) -> None:
    raise SystemExit(message)


def run(*args: str, capture: bool = False) -> str:
    print("+ " + " ".join(args), file=sys.stderr)
    result = subprocess.run(args, check=True, text=True, capture_output=capture)
    return result.stdout if capture else ""


def remote_path(remote: str, key: str) -> str:
    return remote + key if remote.endswith((":", "/")) else remote + "/" + key


def valid_key(value: object) -> str:
    if not isinstance(value, str) or not value or value != value.strip():
        fail(f"invalid source object key: {value!r}")
    key = PurePosixPath(value)
    if key.is_absolute() or ".." in key.parts or len(key.parts) < 2 or str(key) != value:
        fail(f"source must be a relative <conference>/... object key: {value!r}")
    return value


def source_fields(manifest: dict) -> list[tuple[str, bool]]:
    fields: list[tuple[str, bool]] = []
    for job in manifest.get("jobs", []):
        for segment in job.get("segments", []):
            fields.append((valid_key(segment.get("src")), segment.get("type") == "chunkedVideo"))
            if segment.get("overlay") is not None:
                fields.append((valid_key(segment["overlay"]), False))
            audio = segment.get("audio")
            if isinstance(audio, dict) and audio.get("src") is not None:
                fields.append((valid_key(audio["src"]), False))
    if not fields:
        fail("manifest contains no source object keys")
    conferences = {PurePosixPath(key).parts[0] for key, _ in fields}
    if len(conferences) != 1:
        fail("all source object keys must belong to one conference")
    return fields


def object_metadata(remote: str, key: str) -> dict:
    info = json.loads(run("rclone", "lsjson", "--stat", "--hash", remote_path(remote, key), capture=True))
    hashes = {name: value for name, value in info.get("Hashes", {}).items() if value}
    modified = info.get("ModTime", "")
    if info.get("IsDir") or info["Size"] < 0 or (not hashes and (not modified or modified.startswith("0001-"))):
        fail(f"cannot establish cache freshness for {key}")
    return {"size": info["Size"], "modified": modified, "hashes": hashes}


def chunk_keys(remote: str, key: str) -> list[str]:
    source = PurePosixPath(key)
    match = re.fullmatch(r"(.*?)(\d+)(\.[^/]*)", source.name)
    if match is None:
        fail(f"chunkedVideo source must end in a numeric sequence: {key}")
    prefix, _, suffix = match.groups()
    parent = "" if str(source.parent) == "." else str(source.parent)
    listing = run("rclone", "lsf", "--files-only", remote_path(remote, parent + "/"), capture=True)
    names = sorted(name for name in listing.splitlines() if re.fullmatch(re.escape(prefix) + r"\d+" + re.escape(suffix), name))
    if source.name not in names:
        fail(f"chunkedVideo source not found: {key}")
    return [str(source.parent / name) for name in names[names.index(source.name):]]


def cache_id(remote: str, key: str, metadata: dict) -> str:
    return hashlib.sha256(json.dumps([remote, key, metadata], sort_keys=True).encode()).hexdigest()


def make_cache_space(cache: Path, required: int, protected: set[str]) -> None:
    # Hard links held by any render protect its inputs even if jobs overlap.
    candidates = sorted((path for path in cache.iterdir()
                         if re.fullmatch(r"[0-9a-f]{64}", path.name)
                         and path.name not in protected and not path.is_symlink()
                         and path.is_file() and path.stat().st_nlink == 1),
                        key=lambda path: path.stat().st_mtime_ns)
    for path in candidates:
        if shutil.disk_usage(cache).free >= required:
            return
        path.unlink()
    if shutil.disk_usage(cache).free < required:
        fail("not enough worker disk space for this job's inputs and render reserve; use a larger worker volume")


def prepare_inputs(remote: str, inputs: Path, fields: list[tuple[str, bool]], cache: Path,
                   reserve: int = 2 * 1024**3) -> None:
    if reserve < 0:
        fail("render disk reserve must not be negative")
    keys = dict.fromkeys(key for key, _ in fields)
    for key, chunked in fields:
        if chunked:
            keys.update(dict.fromkeys(chunk_keys(remote, key)))
    cache.mkdir(parents=True, exist_ok=True)
    if cache.stat().st_dev != inputs.stat().st_dev:
        fail("input cache and render workspace must be on the same filesystem")
    # Serialise only cache preparation, not rendering. An OS lock is released
    # automatically on cancellation; incomplete downloads are never cache hits.
    with (cache / ".lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        for partial in cache.glob("*.part"):
            if re.fullmatch(r"[0-9a-f]{64}\.part", partial.name):
                partial.unlink()
        entries = [(key, object_metadata(remote, key)) for key in keys]
        protected = {cache_id(remote, key, metadata) for key, metadata in entries}
        for key, metadata in entries:
            cached = cache / cache_id(remote, key, metadata)
            if not cached.is_file() or cached.stat().st_size != metadata["size"]:
                make_cache_space(cache, metadata["size"] + reserve, protected)
                partial = cached.with_suffix(".part")
                try:
                    run("rclone", "copyto", "--stats", "30s", "--stats-one-line",
                        remote_path(remote, key), str(partial))
                    if partial.stat().st_size != metadata["size"] or object_metadata(remote, key) != metadata:
                        fail(f"source changed during download: {key}; retry the job")
                    partial.replace(cached)
                finally:
                    partial.unlink(missing_ok=True)
            else:
                print(f"render: reusing cached input {key}", file=sys.stderr)
            destination = inputs / key
            destination.parent.mkdir(parents=True, exist_ok=True)
            os.link(cached, destination)
            os.utime(cached, None)
        make_cache_space(cache, reserve, protected)


def localize_manifest(manifest: dict, inputs: Path) -> None:
    for job in manifest["jobs"]:
        for segment in job["segments"]:
            segment["src"] = str((inputs / segment["src"]).resolve())
            if segment.get("overlay") is not None:
                segment["overlay"] = str((inputs / segment["overlay"]).resolve())
            audio = segment.get("audio")
            if isinstance(audio, dict) and audio.get("src") is not None:
                audio["src"] = str((inputs / audio["src"]).resolve())


def transcription_enabled(job: dict) -> bool:
    return any(segment.get("transcribe") is True for segment in job.get("segments", []))


def invalidate_ready_marker(remote: str, output_prefix: str) -> None:
    # The prior successful render remains ready until replacement starts.
    # Once any output can change, consumers must wait for the new final marker.
    try:
        run("rclone", "deletefile", "--retries", "1", remote_path(remote, output_prefix + "/ready.json"))
    except subprocess.CalledProcessError as error:
        if error.returncode != 4:  # rclone: file not found (first upload).
            raise


def job_outputs(job: dict) -> dict:
    job_id = job["id"]
    outputs: dict[str, object] = {
        "id": job_id,
        "video": f"{job_id}.mp4",
        "manifest": f"{job_id}.manifest.json",
        "subtitles": None,
    }
    if transcription_enabled(job):
        outputs["subtitles"] = {
            "readable": f"{job_id}.subs.srt",
            "words": f"{job_id}.words.srt",
        }
    return outputs


def check_renderer() -> None:
    """Exercise the installed renderer and NVENC before accepting any jobs."""
    renderer = os.environ.get("CONF_RENDER_COMMAND", "/root/conf-render/.venv/bin/conf-render")
    with tempfile.TemporaryDirectory(prefix="streamctl-render-check-") as directory:
        root = Path(directory)
        image = root / "card.png"
        run("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=black:s=320x180",
            "-frames:v", "1", "-threads", "1", str(image))
        manifest = root / "check.json"
        manifest.write_text(json.dumps({"version": 1,
            "settings": {"width": 320, "height": 180, "videoEncoder": "nvenc",
                         "videoBitrate": "6800k", "audioBitrate": "160k", "keyframeInterval": 2},
            "jobs": [{"id": "check", "segments": [{"type": "image", "src": str(image), "durationMs": 2000}]}]}))
        run(renderer, "render", str(manifest), "--render-only", "--output", str(root / "output"))


def main() -> None:
    if sys.argv[1:] == ["--check"]:
        check_renderer()
        return
    if len(sys.argv) != 4:
        fail("usage: render-from-spaces.py <manifest.json> <output-dir> <work-dir>")
    manifest_path, output, work = map(Path, sys.argv[1:])
    queue_id = os.environ.get("STREAMCTL_RENDER_QUEUE_ID", output.name)
    if not queue_id.isdecimal():
        fail("render queue ID must be numeric")
    remote = os.environ.get("SPACES_REMOTE", "").strip()
    if not remote:
        fail("SPACES_REMOTE is required")
    conf_render = os.environ.get("CONF_RENDER_COMMAND", "/root/conf-render/.venv/bin/conf-render")

    manifest_text = manifest_path.read_text(encoding="utf-8")
    original_manifest = json.loads(manifest_text)
    manifest = json.loads(manifest_text)
    fields = source_fields(manifest)
    conference = PurePosixPath(fields[0][0]).parts[0]
    inputs = work / "inputs"
    localized = work / "manifest.local.json"
    shutil.rmtree(work, ignore_errors=True)
    shutil.rmtree(output, ignore_errors=True)
    inputs.mkdir(parents=True)
    output.mkdir(parents=True)

    cache = Path(os.environ.get("STREAMCTL_INPUT_CACHE", "/workspace/streamctl-input-cache"))
    reserve = int(os.environ.get("STREAMCTL_RENDER_DISK_RESERVE_BYTES", str(2 * 1024**3)))
    prepare_inputs(remote, inputs, fields, cache, reserve)

    localize_manifest(manifest, inputs)
    localized.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    run(conf_render, "validate", str(localized))
    run(conf_render, "render", str(localized), "--output", str(output), "--work-dir", str(work / "conf-render"), "--overwrite")

    missing: list[str] = []
    for job in manifest["jobs"]:
        job_id = job["id"]
        expected = [f"{job_id}.mp4"]
        if transcription_enabled(job):
            expected.extend((f"{job_id}.subs.srt", f"{job_id}.words.srt"))
        missing.extend(name for name in expected if not (output / name).is_file())
    if missing:
        fail("conf-render did not produce expected output(s): " + ", ".join(missing))

    for job in original_manifest["jobs"]:
        job_manifest = {**original_manifest, "jobs": [job]}
        (output / f"{job['id']}.manifest.json").write_text(
            json.dumps(job_manifest, indent=2) + "\n", encoding="utf-8"
        )
    output_prefix = f"{conference}/recordings/renders/{queue_id}"
    invalidate_ready_marker(remote, output_prefix)
    run("rclone", "copy", "--stats", "30s", "--stats-one-line", str(output), remote_path(remote, output_prefix + "/"))
    files = sorted(str(item.relative_to(output)) for item in output.rglob("*") if item.is_file())
    marker = work / "ready.json"
    marker.write_text(json.dumps({
        "status": "ready",
        "manifest_job_ids": [job["id"] for job in manifest["jobs"]],
        "output_prefix": output_prefix,
        "jobs": [job_outputs(job) for job in manifest["jobs"]],
        "files": files,
    }, indent=2) + "\n", encoding="utf-8")
    run("rclone", "copyto", str(marker), remote_path(remote, output_prefix + "/ready.json"))
    print(f"render: ready {output_prefix}")
    # The bucket is authoritative once the final marker has been uploaded.
    shutil.rmtree(work)
    shutil.rmtree(output)


if __name__ == "__main__":
    main()
