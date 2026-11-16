import contextlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("worker", Path(__file__).with_name("render-from-spaces.py"))
worker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(worker)


class RenderDiagnosticsTests(unittest.TestCase):
    def test_cgroup_mount_roots_and_namespaces(self):
        with tempfile.TemporaryDirectory() as directory:
            proc = Path(directory)
            (proc / "self").mkdir()
            cases = [
                ("0::/jobs/render\n", "29 23 0:26 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n",
                 {"unified": Path("/sys/fs/cgroup/jobs/render")}),
                ("5:memory:/docker/pod\n", "29 23 0:26 /docker/pod /sys/fs/cgroup/memory rw - cgroup cgroup rw,memory\n",
                 {"memory": Path("/sys/fs/cgroup/memory")}),
                ("5:memory:/\n", "29 23 0:26 /docker/pod /sys/fs/cgroup/memory rw - cgroup cgroup rw,memory\n",
                 {"memory": Path("/sys/fs/cgroup/memory")}),
            ]
            for membership, mounts, expected in cases:
                with self.subTest(membership=membership):
                    (proc / "self/cgroup").write_text(membership)
                    (proc / "self/mountinfo").write_text(mounts)
                    self.assertEqual(worker.cgroup_paths(proc), expected)

    def test_unavailable_oom_counter_is_not_reported_as_zero(self):
        self.assertIsNone(worker.oom_kills({}))
        self.assertIsNone(worker.oom_kills({"memory": {"memory.failcnt": "1"}}))

    def test_failure_keeps_resource_history_and_oom_delta(self):
        for controller, filename, baseline, final in [
            ("memory", "memory.oom_control", "oom_kill_disable 0\nunder_oom 0\noom_kill 7\n",
             "oom_kill_disable 0\nunder_oom 0\noom_kill 8\n"),
            ("unified", "memory.events", "oom 0\noom_kill 0\n", "oom 1\noom_kill 1\n"),
        ]:
            with self.subTest(controller=controller), tempfile.TemporaryDirectory() as directory:
                work = Path(directory)
                counter = work / filename
                counter.write_text(baseline)
                # Simulate the kernel counter update and a killed child without
                # allocating memory or inducing a real OOM on the test host.
                code = ("from pathlib import Path; import os,signal; "
                        f"Path({str(counter)!r}).write_text({final!r}); "
                        "os.kill(os.getpid(), signal.SIGKILL)")
                captured = io.StringIO()
                with patch.object(worker, "cgroup_paths", return_value={controller: work}), \
                     contextlib.redirect_stderr(captured), \
                     self.assertRaises(subprocess.CalledProcessError) as error:
                    worker.run_render([sys.executable, "-c", code], work, interval=0.01)
                self.assertEqual(error.exception.returncode, -9)
                records = [json.loads(line) for line in (work / "diagnostics/resources.jsonl").read_text().splitlines()]
                terminal = next(item for item in records if item["event"] == "exit")
                self.assertEqual(terminal["oom_kill_delta"], 1)
                self.assertEqual(terminal["return_code"], -9)
                self.assertTrue(any(item["event"] == "start" for item in records))
                self.assertIn('"oom_kill_delta": 1', captured.getvalue())

    def test_success_records_final_counters_without_oom(self):
        with tempfile.TemporaryDirectory() as directory, contextlib.redirect_stderr(io.StringIO()):
            work = Path(directory)
            (work / "memory.events").write_text("oom_kill 3\n")
            with patch.object(worker, "cgroup_paths", return_value={"unified": work}):
                worker.run_render([sys.executable, "-c", "pass"], work, interval=0.01)
            records = [json.loads(line) for line in (work / "diagnostics/resources.jsonl").read_text().splitlines()]
            self.assertEqual(records[-1]["return_code"], 0)
            self.assertEqual(records[-1]["oom_kill_delta"], 0)

    def test_timeout_kills_descendants_and_keeps_diagnostics(self):
        with tempfile.TemporaryDirectory() as directory, contextlib.redirect_stderr(io.StringIO()):
            work = Path(directory)
            marker = work / "orphan-ran"
            child = ("import signal,time,pathlib;signal.signal(signal.SIGTERM,signal.SIG_IGN);"
                     f"time.sleep(0.8);pathlib.Path({str(marker)!r}).touch()")
            wrapper = f"import subprocess,sys,time;subprocess.Popen([sys.executable,'-c',{child!r}]);time.sleep(60)"
            with patch.object(worker, "cgroup_paths", return_value={}), \
                 self.assertRaisesRegex(SystemExit, "timed out after 0.2s"):
                worker.run_render([sys.executable, "-c", wrapper], work, interval=0.01, timeout=0.2)
            time.sleep(1)
            self.assertFalse(marker.exists(), "timed-out render left its child running")
            records = [json.loads(line) for line in (work / "diagnostics/resources.jsonl").read_text().splitlines()]
            self.assertEqual(records[-1]["event"], "exit")
            self.assertTrue(records[-1]["timed_out"])
            self.assertNotEqual(records[-1]["return_code"], 0)

    def test_render_uses_prepared_model_environment_and_diagnostics(self):
        with tempfile.TemporaryDirectory() as directory, contextlib.redirect_stderr(io.StringIO()):
            work = Path(directory)
            output = work / "environment.json"
            code = ("import json,os,pathlib;"
                    f"pathlib.Path({str(output)!r}).write_text(json.dumps(dict(os.environ)))")
            with patch.object(worker, "run"), patch.object(worker, "cgroup_paths", return_value={}):
                env = worker.prepare_whisper("/renderer/.venv/bin/conf-render")
                worker.run_render([sys.executable, "-c", code], work, env=env)
            environment = json.loads(output.read_text())
            self.assertEqual(environment["HF_HUB_OFFLINE"], "1")
            self.assertEqual(environment["STREAMCTL_RENDER_DIAGNOSTICS_DIR"], str(work / "diagnostics"))

    def test_invalid_timeout_does_not_start_render(self):
        with patch.object(worker.subprocess, "Popen") as popen:
            with self.assertRaisesRegex(SystemExit, "must be positive"):
                worker.run_render(["unused"], Path("unused"), timeout=0)
        popen.assert_not_called()


if __name__ == "__main__":
    unittest.main()
