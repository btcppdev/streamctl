import importlib.util
import os
from pathlib import Path
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("worker", Path(__file__).with_name("render-from-spaces.py"))
worker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(worker)


class OperationTimeoutTests(unittest.TestCase):
    def test_timeout_terminates_descendants_not_just_wrapper(self):
        with tempfile.TemporaryDirectory() as directory:
            marker = Path(directory) / "orphan-ran"
            child = "import signal,time,pathlib;signal.signal(signal.SIGTERM,signal.SIG_IGN);time.sleep(0.8);pathlib.Path(%r).touch()" % str(marker)
            wrapper = "import subprocess,sys,time;subprocess.Popen([sys.executable,'-c',%r]);time.sleep(60)" % child
            started = time.monotonic()
            with self.assertRaisesRegex(SystemExit, "timed out after 0.2s"):
                worker.run(sys.executable, "-c", wrapper, timeout=0.2)
            self.assertLess(time.monotonic() - started, 3)
            time.sleep(1)
            self.assertFalse(marker.exists(), "timed-out operation left its child running")

    def test_model_preflight_has_own_deadline_and_render_uses_cache(self):
        with patch.object(worker, "run") as run, patch.dict(os.environ, {"STREAMCTL_WHISPER_TIMEOUT_SECONDS": "12"}):
            env = worker.prepare_whisper("/renderer/.venv/bin/conf-render")
        self.assertEqual(run.call_args.kwargs["timeout"], 12)
        self.assertEqual(run.call_args.args[0], "/renderer/.venv/bin/python")
        self.assertEqual(env["HF_HUB_OFFLINE"], "1")

    def test_captured_output_and_nonzero_exit_are_preserved(self):
        self.assertEqual(worker.run(sys.executable, "-c", "print('ready')", capture=True, timeout=2), "ready\n")
        with self.assertRaises(worker.subprocess.CalledProcessError) as error:
            worker.run(sys.executable, "-c", "import sys;print('failed');sys.exit(3)", capture=True, timeout=2)
        self.assertEqual(error.exception.returncode, 3)
        self.assertEqual(error.exception.output, "failed\n")

    def test_invalid_timeout_does_not_start_process(self):
        with patch.object(worker.subprocess, "Popen") as popen:
            with self.assertRaisesRegex(SystemExit, "must be positive"):
                worker.run("unused", timeout=0)
        popen.assert_not_called()


if __name__ == "__main__":
    unittest.main()
