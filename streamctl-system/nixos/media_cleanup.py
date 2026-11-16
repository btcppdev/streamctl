#!/usr/bin/env python3
"""Reclaim disposable stream media while protecting scheduled and active work."""

import argparse
from contextlib import closing
import errno
import fcntl
import json
import os
from pathlib import Path, PurePosixPath
import sqlite3
import stat
import subprocess
import time


LOCK_NAME = ".cleanup.lock"


def unit_states(prefix):
    result = subprocess.run(
        ["systemctl", "show", prefix + "stream-*.service",
         prefix + "prefetch-*.service", prefix + "stream-*.timer",
         "--property=Id,ActiveState,NextElapseUSecRealtime", "--no-pager"],
        check=True, capture_output=True, text=True, timeout=30,
    )
    states = {}
    for block in result.stdout.strip().split("\n\n"):
        values = dict(line.split("=", 1) for line in block.splitlines() if "=" in line)
        if "Id" in values:
            states[values["Id"]] = values
    return states


def protected_files(connection, states, prefix):
    protected = {LOCK_NAME}
    rows = connection.execute(
        "SELECT s.id, v.video_file FROM streams s "
        "JOIN stream_videos v ON v.stream_id = s.id WHERE s.enabled = 1"
    )
    for stream_id, source in rows:
        timer = states.get(f"{prefix}stream-{stream_id}.timer", {})
        # Unknown timer state is not proof that a recording is no longer needed.
        upcoming = timer.get("NextElapseUSecRealtime")
        if upcoming in ("", "n/a", "0"):
            continue
        if "/" not in source:
            continue  # Uploaded originals live outside the disposable cache.
        path = PurePosixPath(source)
        if path.is_absolute() or ".." in path.parts:
            raise ValueError(f"unsafe cached source in stream {stream_id}")
        protected.add(str(path))
    return protected


def prune(root, protected, cutoff, apply):
    removed = {"files": 0, "bytes": 0}
    for directory, _, files in os.walk(root, topdown=False, followlinks=False):
        directory = Path(directory)
        for name in files:
            path = directory / name
            if path.relative_to(root).as_posix() in protected:
                continue
            info = path.lstat()
            # rclone preserves the source's old mtime; ctime protects new downloads.
            if not stat.S_ISREG(info.st_mode) or max(info.st_mtime, info.st_ctime) > cutoff:
                continue
            if apply:
                path.unlink()
            removed["files"] += 1
            removed["bytes"] += info.st_size
        if apply and directory != root:
            try:
                directory.rmdir()
            except OSError as error:
                if error.errno != errno.ENOTEMPTY:
                    raise
    return removed


def clean(database, cache, hls, prefix="streamctl-", min_age=3600, apply=False):
    if min_age < 0:
        raise ValueError("minimum age must not be negative")
    roots = [Path(cache), Path(hls)]
    for root in roots:
        if not root.is_absolute() or root == Path("/") or root.is_symlink():
            raise ValueError("media roots must be absolute, non-symlink directories")
        if not root.is_dir():
            raise ValueError(f"media directory does not exist: {root}")
    cache, hls = [root.resolve() for root in roots]
    database = Path(database).resolve()
    if cache == hls or cache in hls.parents or hls in cache.parents:
        raise ValueError("cache and HLS directories must not overlap")
    if any(root in database.parents for root in (cache, hls)):
        raise ValueError("database must be outside the disposable media directories")

    # Playback and prefetch scripts hold this same lock shared. Never wait for a
    # broadcast to finish, and never race a new download or manual playback.
    with (cache / LOCK_NAME).open("a+") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return {"skipped": "media is in use"}
        states = unit_states(prefix)
        for name, values in states.items():
            if name.endswith(".service") and values.get("ActiveState") not in ("inactive", "failed"):
                return {"skipped": f"active service: {name}"}
        with closing(sqlite3.connect(database.as_uri() + "?mode=ro", uri=True)) as connection:
            protected = protected_files(connection, states, prefix)
        cutoff = time.time() - min_age
        return {
            "mode": "apply" if apply else "dry-run",
            "cache": prune(cache, protected, cutoff, apply),
            "hls": prune(hls, set(), cutoff, apply),
        }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--database", required=True)
    parser.add_argument("--cache", required=True)
    parser.add_argument("--hls", required=True)
    parser.add_argument("--unit-prefix", default="streamctl-")
    parser.add_argument("--min-age-seconds", type=int, default=3600)
    parser.add_argument("--apply", action="store_true")
    args = parser.parse_args()
    print(json.dumps(clean(args.database, args.cache, args.hls, args.unit_prefix,
                           args.min_age_seconds, args.apply)))


if __name__ == "__main__":
    main()
