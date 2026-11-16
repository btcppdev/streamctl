#!/usr/bin/env python3
"""Download a conf-render manifest's inputs, render, and upload its outputs."""

from __future__ import annotations

import fcntl
import hashlib
import json
import math
import os
import re
import signal
import shutil
import subprocess
import sys
import tempfile
import threading
import time
from datetime import datetime, timezone
from pathlib import Path, PurePosixPath


def fail(message: str) -> None:
    raise SystemExit(message)


def terminate_operation(process: subprocess.Popen) -> None:
    """Kill the entire operation, including FFmpeg/download subprocesses."""
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    try:
        process.communicate(timeout=5)
    except subprocess.TimeoutExpired:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.communicate()
    # The wrapper may have exited while a child ignored SIGTERM.
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass


def run(*args: str, capture: bool = False, timeout: float | None = None,
        env: dict | None = None) -> str:
    if timeout is None:
        timeout = 120 if args[0] == "rclone" and args[1] in {"lsjson", "lsf", "delete"} else float(os.environ.get("STREAMCTL_TRANSFER_TIMEOUT_SECONDS", "1800"))
    if not math.isfinite(timeout) or timeout <= 0:
        fail("worker operation timeout must be positive")
    if args[0] == "rclone":
        args = (*args[:2], "--contimeout", "15s", "--timeout", "90s", *args[2:])
    print("+ " + " ".join(args), file=sys.stderr, flush=True)
    process = subprocess.Popen(args, text=True, stdout=subprocess.PIPE if capture else None,
                               stderr=subprocess.PIPE if capture else None,
                               start_new_session=True, env=env)
    try:
        stdout, stderr = process.communicate(timeout=timeout)
    except subprocess.TimeoutExpired:
        terminate_operation(process)
        fail(f"{Path(args[0]).name} operation timed out after {timeout:g}s")
    if process.returncode:
        if capture and stderr:
            print(stderr, file=sys.stderr)
        raise subprocess.CalledProcessError(process.returncode, args, stdout, stderr)
    return stdout if capture else ""


def prepare_whisper(renderer: str) -> dict:
    """Bound cold model download/initialization separately from long encoding."""
    python = str(Path(renderer).with_name("python"))
    timeout = float(os.environ.get("STREAMCTL_WHISPER_TIMEOUT_SECONDS", "300"))
    print(f"render: preparing Whisper model ({timeout:g}s deadline)", file=sys.stderr, flush=True)
    run(python, "-c",
        "from conf_render.transcription import load_whisper_runtime; "
        "load_whisper_runtime('distil-large-v3', print)", timeout=timeout)
    # The warmed model is now local; avoid network retries in the render lane.
    return {**os.environ, "HF_HUB_OFFLINE": "1"}


def read_text(path: Path) -> str | None:
    try:
        return path.read_text().strip()
    except OSError:
        return None


def counters(value: str | None) -> dict[str, int]:
    result = {}
    for line in (value or "").splitlines():
        fields = line.split()
        if len(fields) == 2 and fields[1].lstrip("-").isdigit():
            result[fields[0]] = int(fields[1])
    return result


def cgroup_paths(proc: Path = Path("/proc")) -> dict[str, Path]:
    """Resolve the current cgroup through mount roots, including container namespaces."""
    memberships = {}
    for line in (read_text(proc / "self/cgroup") or "").splitlines():
        _, controllers, member = line.split(":", 2)
        for controller in controllers.split(","):
            memberships[controller] = member
    paths = {}
    for line in (read_text(proc / "self/mountinfo") or "").splitlines():
        left, _, right = line.partition(" - ")
        fields, filesystem = left.split(), right.split()
        if len(fields) < 5 or len(filesystem) < 3 or filesystem[0] not in {"cgroup", "cgroup2"}:
            continue
        controllers = [""] if filesystem[0] == "cgroup2" else filesystem[2].split(",")
        for controller in controllers:
            if controller not in memberships:
                continue
            root, mount = (Path(re.sub(r"\\([0-7]{3})", lambda m: chr(int(m[1], 8)), value))
                           for value in fields[3:5])
            member = Path(memberships[controller])
            try:
                relative = member.relative_to(root)
            except ValueError:
                # A private cgroup namespace reports membership relative to its root.
                relative = Path(str(member).lstrip("/"))
            paths[controller or "unified"] = mount / relative
    return paths


def cgroup_snapshot(paths: dict[str, Path]) -> dict:
    result = {}
    for controller, path in paths.items():
        if controller == "unified":
            names = ("memory.current", "memory.peak", "memory.max", "memory.swap.current",
                     "memory.events", "memory.stat", "cpu.max", "cpu.stat", "pids.current", "pids.max")
        elif controller == "memory":
            names = ("memory.usage_in_bytes", "memory.max_usage_in_bytes", "memory.limit_in_bytes",
                     "memory.failcnt", "memory.oom_control", "memory.stat")
        elif controller == "cpu":
            names = ("cpu.cfs_quota_us", "cpu.cfs_period_us", "cpu.stat")
        else:
            continue
        result[controller] = {name: value for name in names if (value := read_text(path / name)) is not None}
    return result


def oom_kills(snapshot: dict) -> int | None:
    for controller, filename in (("unified", "memory.events"), ("memory", "memory.oom_control")):
        count = counters(snapshot.get(controller, {}).get(filename)).get("oom_kill")
        if count is not None:
            return count
    return None


def process_snapshot(root_pid: int, proc: Path = Path("/proc")) -> list[dict]:
    processes = {}
    try:
        entries = list(proc.iterdir())
    except OSError:
        return []
    for entry in entries:
        if not entry.name.isdecimal():
            continue
        status = dict(line.split(":", 1) for line in (read_text(entry / "status") or "").splitlines() if ":" in line)
        if "PPid" not in status:
            continue
        item = {"pid": int(entry.name), "ppid": int(status["PPid"]), "name": status.get("Name", "").strip()}
        for key in ("Threads", "VmRSS", "VmHWM", "VmSwap"):
            if key in status:
                item[key] = int(status[key].split()[0])  # Vm* values are KiB.
        item["io"] = counters(read_text(entry / "io"))
        stat = (read_text(entry / "stat") or "").rpartition(") ")[2].split()
        if len(stat) > 12:
            item["cpu_seconds"] = (int(stat[11]) + int(stat[12])) / os.sysconf("SC_CLK_TCK")
        processes[item["pid"]] = item
    descendants = {root_pid}
    while True:
        found = {pid for pid, item in processes.items() if item["ppid"] in descendants}
        if found <= descendants:
            break
        descendants |= found
    return [processes[pid] for pid in sorted(descendants) if pid in processes]


def run_render(args: list[str], work: Path, interval: float = 10, *,
               timeout: float = 48 * 3600, env: dict | None = None) -> None:
    """Keep resource evidence both on disk and in the controller-retained journal."""
    if not math.isfinite(timeout) or timeout <= 0:
        fail("worker operation timeout must be positive")
    directory = work / "diagnostics"
    directory.mkdir(parents=True, exist_ok=True)
    paths = cgroup_paths()
    baseline = cgroup_snapshot(paths)
    started = time.monotonic()
    stopped = threading.Event()
    environment = dict(os.environ if env is None else env, STREAMCTL_RENDER_DIAGNOSTICS_DIR=str(directory))
    with (directory / "resources.jsonl").open("w", buffering=1) as log:
        def emit(kind: str, **values) -> None:
            record = {"event": kind, "at": datetime.now(timezone.utc).isoformat(),
                      "elapsed_seconds": round(time.monotonic() - started, 2), **values}
            line = json.dumps(record, sort_keys=True)
            log.write(line + "\n")
            print("render-diagnostics: " + line, file=sys.stderr, flush=True)

        emit("start", cgroup_paths={key: str(value) for key, value in paths.items()},
             cgroup=baseline, host_cpu_count=os.cpu_count(),
             input_threads=environment.get("STREAMCTL_RENDER_INPUT_THREADS", "2"),
             filter_threads=environment.get("STREAMCTL_RENDER_FILTER_THREADS", "4"))
        print("+ " + " ".join(args), file=sys.stderr, flush=True)
        process = subprocess.Popen(args, env=environment, start_new_session=True)

        def sample() -> None:
            while not stopped.is_set():
                try:
                    emit("sample", cgroup=cgroup_snapshot(paths), processes=process_snapshot(process.pid),
                         disk_free_bytes=shutil.disk_usage(work).free)
                except Exception as error:
                    emit("monitor_error", error=str(error))
                stopped.wait(interval)

        monitor = threading.Thread(target=sample, name="render-resource-monitor", daemon=True)
        monitor.start()
        timed_out = False
        try:
            return_code = process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            timed_out = True
            terminate_operation(process)
            return_code = process.returncode
        finally:
            stopped.set()
            monitor.join()
        final = cgroup_snapshot(paths)
        before, after = oom_kills(baseline), oom_kills(final)
        emit("exit", return_code=return_code, timed_out=timed_out, cgroup=final,
             oom_kill_delta=after - before if before is not None and after is not None else None)
        if timed_out:
            fail(f"{Path(args[0]).name} operation timed out after {timeout:g}s")
        if return_code:
            try:
                kernel = subprocess.run(["dmesg", "--ctime"], capture_output=True, text=True, timeout=5)
                lines = [line for line in kernel.stdout.splitlines()
                         if re.search(r"oom|out of memory|killed process|nvrm|xid", line, re.I)]
                emit("kernel", return_code=kernel.returncode, lines=lines[-40:], error=kernel.stderr.strip())
            except (OSError, subprocess.TimeoutExpired) as error:
                emit("kernel", error=str(error))
            raise subprocess.CalledProcessError(return_code, args)


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
    # Ubuntu's worker rclone predates --stat. A file listing returns one entry.
    entries = json.loads(run("rclone", "lsjson", "--hash", remote_path(remote, key), capture=True))
    if len(entries) != 1 or entries[0].get("Path") != PurePosixPath(key).name:
        fail(f"source object not found or ambiguous: {key}")
    info = entries[0]
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
        deadline = time.monotonic() + 120
        while True:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    fail("input cache lock timed out after 120s")
                time.sleep(0.2)
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
                    run("rclone", "copyto", "--stats", "30s", "--stats-one-line", "--retries", "3",
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
    key = output_prefix + "/ready.json"
    if "\n" in key or "\r" in key:
        fail("invalid ready marker key")
    # Exact-key deletion is idempotent even on older rclone releases whose
    # deletefile reports a generic error when the marker does not exist yet.
    with tempfile.TemporaryDirectory(prefix="streamctl-render-marker-") as directory:
        selected = Path(directory) / "files.txt"
        selected.write_text(key + "\n", encoding="utf-8")
        run("rclone", "delete", remote, "--files-from-raw", str(selected), "--retries", "1")


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
        run(renderer, "render", str(manifest), "--render-only", "--output", str(root / "output"), timeout=120)


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
    run(conf_render, "validate", str(localized), timeout=120)
    render_env = prepare_whisper(conf_render) if any(transcription_enabled(job) for job in manifest["jobs"]) else None
    run_render([conf_render, "render", str(localized), "--output", str(output),
                "--work-dir", str(work / "conf-render"), "--overwrite"], work, timeout=48 * 3600, env=render_env)

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
