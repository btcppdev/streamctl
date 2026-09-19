import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("worker", Path(__file__).with_name("render-from-spaces.py"))
worker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(worker)
real_run = worker.run


class InputCacheTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.cache = self.root / "cache"
        self.objects = {"conf/clip.mp4": b"video", "conf/card.png": b"image"}
        self.downloads = []
        self.counter = 0
        mock = patch.object(worker, "run", side_effect=self.run_remote)
        mock.start()
        self.addCleanup(mock.stop)

    def run_remote(self, *args, **kwargs):
        command = args[1]
        if command == "lsjson":
            key = args[-1].split("bucket/", 1)[1]
            data = self.objects[key]
            return json.dumps({"Size": len(data), "ModTime": "2026-09-18T00:00:00Z",
                               "Hashes": {"MD5": hashlib.md5(data).hexdigest()}})
        if command == "lsf":
            parent = args[-1].split("bucket/", 1)[1]
            return "\n".join(key[len(parent):] for key in self.objects if key.startswith(parent))
        if command == "copyto":
            key = args[-2].split("bucket/", 1)[1]
            self.downloads.append(key)
            Path(args[-1]).write_bytes(self.objects[key])
            return ""
        self.fail(f"unexpected command {args}")

    def prepare(self, fields=None):
        self.counter += 1
        inputs = self.root / f"job-{self.counter}"
        inputs.mkdir()
        worker.prepare_inputs("spaces:bucket", inputs, fields or [("conf/clip.mp4", False)], self.cache, reserve=0)
        return inputs

    def test_reuses_inputs_after_job_cleanup_without_copying_bytes(self):
        first = self.prepare()
        cached = next(path for path in self.cache.iterdir() if len(path.name) == 64)
        self.assertTrue(os.path.samefile(cached, first / "conf/clip.mp4"))
        shutil.rmtree(first)
        second = self.prepare()
        self.assertEqual(self.downloads, ["conf/clip.mp4"])
        self.assertEqual((second / "conf/clip.mp4").read_bytes(), b"video")

    def test_same_size_remote_change_gets_new_version_without_changing_running_job(self):
        first = self.prepare()
        self.objects["conf/clip.mp4"] = b"other"
        second = self.prepare()
        self.assertEqual(len(self.downloads), 2)
        self.assertEqual((first / "conf/clip.mp4").read_bytes(), b"video")
        self.assertEqual((second / "conf/clip.mp4").read_bytes(), b"other")

    def test_overlapping_chunk_sequences_download_each_object_once(self):
        self.objects.update({"conf/mix_0000.mp4": b"zero", "conf/mix_0001.mp4": b"one"})
        fields = [("conf/mix_0000.mp4", True), ("conf/mix_0001.mp4", True), ("conf/card.png", False)]
        first = self.prepare(fields)
        self.assertCountEqual(self.downloads, ["conf/mix_0000.mp4", "conf/mix_0001.mp4", "conf/card.png"])
        shutil.rmtree(first)
        self.prepare(fields)
        self.assertEqual(len(self.downloads), 3)

    def test_failed_partial_download_is_not_reused(self):
        def interrupted(*args, **kwargs):
            if args[1] == "copyto":
                Path(args[-1]).write_bytes(b"bad")
                raise RuntimeError("connection lost")
            return self.run_remote(*args, **kwargs)
        with patch.object(worker, "run", side_effect=interrupted):
            with self.assertRaisesRegex(RuntimeError, "connection lost"):
                self.prepare()
        self.assertEqual(list(self.cache.glob("*.part")), [])
        self.prepare()
        self.assertEqual(self.downloads, ["conf/clip.mp4"])

    def test_changing_object_during_download_is_not_published(self):
        def changing(*args, **kwargs):
            result = self.run_remote(*args, **kwargs)
            if args[1] == "copyto":
                self.objects["conf/clip.mp4"] = b"changed"
            return result
        with patch.object(worker, "run", side_effect=changing):
            with self.assertRaisesRegex(SystemExit, "source changed"):
                self.prepare()
        self.assertEqual([path for path in self.cache.iterdir() if path.name != ".lock"], [])

    def test_eviction_uses_oldest_unpinned_entry_and_preserves_unrelated_files(self):
        self.cache.mkdir()
        oldest, newer, busy = [self.cache / (letter * 64) for letter in "abc"]
        for index, path in enumerate((oldest, newer, busy)):
            path.write_bytes(b"data")
            os.utime(path, (index + 1, index + 1))
        os.link(busy, self.root / "active-input")
        unrelated = self.cache / "keep.txt"
        unrelated.write_text("keep")
        with patch.object(worker.shutil, "disk_usage", side_effect=lambda _: SimpleNamespace(free=0 if oldest.exists() else 10)):
            worker.make_cache_space(self.cache, 10, set())
        self.assertFalse(oldest.exists())
        self.assertTrue(newer.exists())
        self.assertTrue(busy.exists())
        self.assertTrue(unrelated.exists())

    def test_full_disk_never_evicts_current_or_running_job_inputs(self):
        self.prepare()
        with patch.object(worker.shutil, "disk_usage", return_value=SimpleNamespace(free=0)):
            with self.assertRaisesRegex(SystemExit, "larger worker volume"):
                worker.make_cache_space(self.cache, 1, set())
        cached = next(path for path in self.cache.iterdir() if len(path.name) == 64)
        self.assertTrue(cached.exists())
        shutil.rmtree(self.root / "job-1")
        with patch.object(worker.shutil, "disk_usage", return_value=SimpleNamespace(free=0)):
            with self.assertRaises(SystemExit):
                worker.make_cache_space(self.cache, 1, {cached.name})
        self.assertTrue(cached.exists())

    @unittest.skipUnless(shutil.which("rclone"), "rclone required for local integration")
    def test_real_rclone_local_metadata_download_and_reuse(self):
        remote = self.root / "remote"
        (remote / "conf").mkdir(parents=True)
        (remote / "conf/source.mp4").write_bytes(b"local fixture")
        first, second = self.root / "first", self.root / "second"
        first.mkdir()
        second.mkdir()
        with patch.object(worker, "run", side_effect=real_run) as calls, patch.dict(os.environ, {"RCLONE_CONFIG": os.devnull, "RCLONE_FILTER_FROM": ""}):
            for inputs in (first, second):
                worker.prepare_inputs(str(remote), inputs, [("conf/source.mp4", False)], self.cache, reserve=0)
        self.assertEqual(sum(call.args[1] == "copyto" for call in calls.call_args_list), 1)
        self.assertTrue(os.path.samefile(first / "conf/source.mp4", second / "conf/source.mp4"))
class PublicationTests(unittest.TestCase):
    def test_marker_removal_ignores_only_missing_file(self):
        with patch.object(worker, "run", side_effect=subprocess.CalledProcessError(4, "rclone")):
            worker.invalidate_ready_marker("spaces:bucket", "conf/recordings/renders/42")
        with patch.object(worker, "run", side_effect=subprocess.CalledProcessError(5, "rclone")):
            with self.assertRaises(subprocess.CalledProcessError):
                worker.invalidate_ready_marker("spaces:bucket", "conf/recordings/renders/42")

    def test_replacement_publishes_marker_only_after_successful_upload(self):
        for fail_upload in (False, True):
            with self.subTest(fail_upload=fail_upload), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                output, work = root / "42", root / "work"
                manifest = root / "manifest.json"
                manifest.write_text(json.dumps({"version": 1, "jobs": [{"id": "intro", "segments": [{"type": "image", "src": "conf/card.png"}]}]}))
                marker = root / "remote-ready.json"
                marker.write_text("old ready marker")
                events = []

                def command(*args, **kwargs):
                    if args[1] == "validate":
                        return ""
                    if args[1] == "render":
                        self.assertEqual(marker.read_text(), "old ready marker")
                        (output / "intro.mp4").write_bytes(b"rendered")
                    elif args[1] == "deletefile":
                        self.assertTrue(args[-1].endswith("/42/ready.json"))
                        marker.unlink()
                        events.append("invalidate")
                    elif args[1] == "copy":
                        self.assertFalse(marker.exists())
                        events.append("upload")
                        if fail_upload:
                            raise subprocess.CalledProcessError(1, "rclone")
                    elif args[1] == "copyto":
                        self.assertFalse(marker.exists())
                        events.append("ready")
                        shutil.copyfile(args[-2], marker)
                    else:
                        self.fail(f"unexpected command {args}")
                    return ""

                with patch.object(worker, "run", side_effect=command), patch.object(worker, "prepare_inputs"), patch.object(worker.sys, "argv", ["worker", str(manifest), str(output), str(work)]), patch.dict(os.environ, {"SPACES_REMOTE": "spaces:bucket", "STREAMCTL_RENDER_QUEUE_ID": "42"}):
                    if fail_upload:
                        with self.assertRaises(subprocess.CalledProcessError):
                            worker.main()
                        self.assertFalse(marker.exists())
                        self.assertEqual(events, ["invalidate", "upload"])
                    else:
                        worker.main()
                        self.assertEqual(json.loads(marker.read_text())["status"], "ready")
                        self.assertEqual(events, ["invalidate", "upload", "ready"])


if __name__ == "__main__":
    unittest.main()
