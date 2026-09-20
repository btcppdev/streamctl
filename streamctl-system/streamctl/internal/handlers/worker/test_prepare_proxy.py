"""Local pipeline tests; set STREAMCTL_TEST_GPU=1 to exercise real NVENC."""
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("proxy", Path(__file__).with_name("prepare-proxy.py"))
proxy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(proxy)


class PreviewTests(unittest.TestCase):
    def test_gpu_encoding_keeps_browser_seek_settings(self):
        args = proxy.encode_args("inputs", "output", 480)
        self.assertEqual(args[args.index("-c:v") + 1], "h264_nvenc")
        self.assertEqual(args[args.index("-force_key_frames") + 1], "expr:gte(t,n_forced*1)")
        self.assertEqual(args[args.index("-chunk_duration") + 1], "1000000")
        self.assertIn("+faststart", args)
        self.assertIn("0:a:0?", args)

    def test_failed_encode_never_uploads_ready_marker(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            request = dict(remote="test:bucket", chunks=["source.mp4"], source="source.mp4",
                           sourceType="video", proxy="proxy.mp4", sidecar="proxy.v1.json", height=480, version=1)
            (root / "request.json").write_text(json.dumps(request))
            calls = []

            def progress(args, directory, stage, duration=0):
                calls.append(stage)
                if stage.startswith("Downloading"):
                    Path(args[-1]).write_bytes(b"source")
                else:
                    raise RuntimeError("NVENC unavailable")

            with patch.object(proxy, "run_progress", side_effect=progress), \
                 patch.object(proxy, "duration_ms", return_value=1000), \
                 patch.object(proxy.subprocess, "check_output", return_value="h264_nvenc"), \
                 patch.object(proxy.sys, "argv", ["prepare-proxy.py", str(root)]):
                self.assertEqual(proxy.main(), 1)
            status = json.loads((root / "status.json").read_text())
            self.assertEqual(status["state"], "failed")
            self.assertIn("NVENC unavailable", status["error"])
            self.assertFalse(any(stage.startswith("Uploading") for stage in calls))
            self.assertFalse((root / "work").exists())

    @unittest.skipUnless(shutil.which("ffmpeg") and shutil.which("rclone"), "ffmpeg and rclone required")
    def test_chunked_source_artifacts_and_timeline(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            bucket, job = root / "bucket", root / "job"
            bucket.mkdir()
            job.mkdir()
            # A local rclone backend exercises the actual transfer and marker
            # ordering without credentials or a cloud account.
            for index in range(2):
                subprocess.run(["ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
                                "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=25",
                                "-t", "2", "-c:v", "libx264", "-pix_fmt", "yuv420p",
                                str(bucket / f"source{index}.mp4")], check=True)
            request = dict(remote=str(bucket), chunks=["source0.mp4", "source1.mp4"],
                           source="source0.mp4", sourceType="chunkedVideo", proxy="preview.proxy.mp4",
                           sidecar="preview.proxy.v1.json", height=480, version=1)
            original_encode_args = proxy.encode_args
            original_check_output = subprocess.check_output
            use_gpu = os.environ.get("STREAMCTL_TEST_GPU") == "1"

            def encode(*args):
                command = original_encode_args(*args)
                if not use_gpu:
                    # Only the codec is substituted. Real ffmpeg verifies concat,
                    # audio-optional mapping, timestamps, muxing and keyframes.
                    for option in ("-rc", "-cq", "-b:v", "-forced-idr"):
                        i = command.index(option)
                        del command[i:i+2]
                    command[command.index("h264_nvenc")] = "libx264"
                    command[command.index("p4")] = "veryfast"
                return command

            def check_output(command, **kwargs):
                if command[-1] == "-encoders" and not use_gpu:
                    return "h264_nvenc"
                return original_check_output(command, **kwargs)

            with patch.object(proxy, "encode_args", side_effect=encode), \
                 patch.object(proxy.subprocess, "check_output", side_effect=check_output), \
                 patch.dict(os.environ, {"RCLONE_CONFIG": os.devnull}):
                proxy.prepare(request, job)
                # Retrying must also replace an existing ready marker cleanly.
                proxy.prepare(request, job)
            metadata = json.loads((bucket / request["sidecar"]).read_text())
            status = json.loads((job / "status.json").read_text())
            self.assertEqual(status["state"], "finished")
            self.assertEqual(metadata["source"]["durationMs"], 4000)
            self.assertEqual(len(metadata["source"]["chunks"]), 2)
            self.assertEqual(metadata["proxy"]["height"], 480)
            output = bucket / request["proxy"]
            self.assertAlmostEqual(proxy.duration_ms(output), 4000, delta=100)
            frames = json.loads(subprocess.check_output([
                "ffprobe", "-v", "error", "-select_streams", "v:0", "-skip_frame", "nokey",
                "-show_frames", "-show_entries", "frame=best_effort_timestamp_time", "-of", "json", str(output)], text=True))
            times = [float(frame["best_effort_timestamp_time"]) for frame in frames["frames"]]
            self.assertGreaterEqual(len(times), 4)
            self.assertTrue(all(b - a <= 1.05 for a, b in zip(times, times[1:])), times)


if __name__ == "__main__":
    unittest.main()
