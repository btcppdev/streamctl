from contextlib import closing
import fcntl
import importlib.util
import os
from pathlib import Path
import sqlite3
import subprocess
import tempfile
import time
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("media_cleanup", Path(__file__).with_name("media_cleanup.py"))
cleanup = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cleanup)


class MediaCleanupTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.cache, self.hls = self.root / "cache", self.root / "hls"
        self.cache.mkdir()
        self.hls.mkdir()
        self.database = self.root / "streamctl.db"
        with closing(sqlite3.connect(self.database)) as connection, connection:
            connection.executescript("""
                CREATE TABLE streams (id INTEGER PRIMARY KEY, enabled INTEGER);
                CREATE TABLE stream_videos (stream_id INTEGER, video_file TEXT);
            """)
        self.now = time.time() + 7200

    def file(self, relative, root=None):
        path = (root or self.cache) / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(b"cached video")
        return path

    def stream(self, number, source, enabled=True):
        with closing(sqlite3.connect(self.database)) as connection, connection:
            connection.execute("INSERT INTO streams VALUES (?, ?)", (number, enabled))
            connection.execute("INSERT INTO stream_videos VALUES (?, ?)", (number, source))

    def run_clean(self, states=None, apply=True, now=None):
        with patch.object(cleanup, "unit_states", return_value=states or {}), \
                patch.object(cleanup.time, "time", return_value=now or self.now):
            return cleanup.clean(self.database, self.cache, self.hls, apply=apply)

    def test_removes_expired_and_orphaned_files_and_stale_hls(self):
        media = self.file("conference/recordings/old.mp4")
        normalized = self.file("normalized/conference/recordings/old.mp4")
        abandoned = self.file("production-proxy-old/input.mp4")
        hls = self.file("stream-1/segment.ts", self.hls)
        self.stream(1, "conference/recordings/old.mp4")
        result = self.run_clean({"streamctl-stream-1.timer": {"NextElapseUSecRealtime": ""}})
        self.assertEqual(result["cache"]["files"], 3)
        self.assertEqual(result["hls"]["files"], 1)
        for path in (media, normalized, abandoned, hls):
            self.assertFalse(path.exists())
        self.assertTrue(self.database.exists())
        self.assertTrue((self.cache / cleanup.LOCK_NAME).exists())

    def test_shared_file_kept_for_upcoming_stream(self):
        path = self.file("conference/shared.mp4")
        obsolete = self.file("normalized/conference/shared.mp4")
        self.stream(1, "conference/shared.mp4")
        self.stream(2, "conference/shared.mp4")
        self.run_clean({
            "streamctl-stream-1.timer": {"NextElapseUSecRealtime": ""},
            "streamctl-stream-2.timer": {"NextElapseUSecRealtime": "Fri 2099-01-02 10:00:00 UTC"},
        })
        self.assertTrue(path.exists())
        self.assertFalse(obsolete.exists())

    def test_missing_timer_protects_enabled_stream(self):
        path = self.file("conference/upcoming.mp4")
        self.stream(1, "conference/upcoming.mp4")
        self.run_clean()
        self.assertTrue(path.exists())

    def test_disabled_stream_does_not_pin_cache(self):
        path = self.file("conference/disabled.mp4")
        self.stream(1, "conference/disabled.mp4", enabled=False)
        self.run_clean()
        self.assertFalse(path.exists())

    def test_active_or_starting_services_skip_cleanup(self):
        for state in ("active", "activating", "deactivating"):
            with self.subTest(state=state):
                path = self.file("unused.mp4")
                result = self.run_clean({"streamctl-prefetch-1.service": {"ActiveState": state}})
                self.assertIn("skipped", result)
                self.assertTrue(path.exists())

    def test_playback_lock_prevents_cleanup_even_before_unit_state_changes(self):
        path = self.file("unused.mp4")
        with (self.cache / cleanup.LOCK_NAME).open("a+") as lock:
            fcntl.flock(lock, fcntl.LOCK_SH)
            result = self.run_clean()
        self.assertIn("skipped", result)
        self.assertTrue(path.exists())

    def test_new_download_with_old_source_mtime_is_kept(self):
        path = self.file("new-download.mp4")
        os.utime(path, (1, 1))
        self.run_clean(now=time.time())
        self.assertTrue(path.exists())

    def test_recent_hls_and_media_are_kept(self):
        paths = [self.file("recent.mp4"), self.file("stream-1/segment.ts", self.hls)]
        for path in paths:
            os.utime(path, (self.now, self.now))
        self.run_clean()
        for path in paths:
            self.assertTrue(path.exists())

    def test_dry_run_reports_without_deleting(self):
        path = self.file("unused.mp4")
        result = self.run_clean(apply=False)
        self.assertEqual(result["mode"], "dry-run")
        self.assertEqual(result["cache"]["bytes"], len(b"cached video"))
        self.assertTrue(path.exists())

    def test_symlinks_are_not_followed_or_deleted(self):
        outside = self.root / "originals"
        outside.mkdir()
        original = self.file("original.mp4", outside)
        (self.cache / "linked-directory").symlink_to(outside, target_is_directory=True)
        (self.cache / "linked-file.mp4").symlink_to(original)
        self.run_clean()
        self.assertTrue(original.exists())
        self.assertTrue((self.cache / "linked-file.mp4").is_symlink())

    def test_systemd_failure_prevents_deletion(self):
        path = self.file("unused.mp4")
        with patch.object(cleanup, "unit_states", side_effect=subprocess.CalledProcessError(1, "systemctl")):
            with self.assertRaises(subprocess.CalledProcessError):
                cleanup.clean(self.database, self.cache, self.hls, apply=True)
        self.assertTrue(path.exists())

    def test_database_failure_prevents_deletion(self):
        path = self.file("unused.mp4")
        self.database.unlink()
        with patch.object(cleanup, "unit_states", return_value={}):
            with self.assertRaises(sqlite3.OperationalError):
                cleanup.clean(self.database, self.cache, self.hls, apply=True)
        self.assertTrue(path.exists())

    def test_systemd_snapshot_parses_expected_properties(self):
        output = "Id=streamctl-stream-1.timer\nActiveState=active\nNextElapseUSecRealtime=\n\n"
        output += "Id=streamctl-prefetch-1.service\nActiveState=inactive\n"
        with patch.object(cleanup.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, output)) as run:
            states = cleanup.unit_states("streamctl-")
        self.assertEqual(states["streamctl-stream-1.timer"]["NextElapseUSecRealtime"], "")
        self.assertEqual(states["streamctl-prefetch-1.service"]["ActiveState"], "inactive")
        self.assertIn("--property=Id,ActiveState,NextElapseUSecRealtime", run.call_args.args[0])


if __name__ == "__main__":
    unittest.main()
