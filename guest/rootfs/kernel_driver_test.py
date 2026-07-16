"""Drives kernel_driver.py over its stdin/stdout JSON protocol.

The protocol tests skip entirely if ipykernel/jupyter_client are not importable, so
they do not fail on a machine without the kernel deps; the real-VM proof is Task 11.

The fork-reseed tests below stub jupyter_client, so they always run.
"""
import json
import os
import signal
import subprocess
import sys
import types
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
DRIVER = os.path.join(HERE, "kernel_driver.py")


def _have_kernel():
    try:
        import ipykernel  # noqa: F401
        import jupyter_client  # noqa: F401
        return True
    except Exception:
        return False


@unittest.skipUnless(_have_kernel(), "ipykernel/jupyter_client not installed")
class KernelDriverTest(unittest.TestCase):
    def _run(self, requests):
        proc = subprocess.Popen(
            [sys.executable, DRIVER],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True,
        )
        payload = "".join(json.dumps(r) + "\n" for r in requests)
        out, err = proc.communicate(payload, timeout=120)
        events = [json.loads(line) for line in out.splitlines() if line.strip()]
        return events, err

    def test_stdout_and_last_expression(self):
        events, _ = self._run([{"id": "a", "code": "print('hi')\n40 + 2"}])
        kinds = [e["kind"] for e in events if e["id"] == "a"]
        self.assertIn("stdout", kinds)
        self.assertIn("result", kinds)
        self.assertIn("done", kinds)
        stdout = next(e for e in events if e["kind"] == "stdout")
        self.assertIn("hi", stdout["text"])
        result = next(e for e in events if e["kind"] == "result")
        self.assertEqual(result["data"]["text/plain"].strip(), "42")

    def test_state_persists(self):
        events, _ = self._run([
            {"id": "a", "code": "x = 7"},
            {"id": "b", "code": "x * 6"},
        ])
        result = next(e for e in events if e["kind"] == "result" and e["id"] == "b")
        self.assertEqual(result["data"]["text/plain"].strip(), "42")

    def test_error_frame(self):
        events, _ = self._run([{"id": "a", "code": "raise ValueError('bad')"}])
        err = next(e for e in events if e["kind"] == "error")
        self.assertEqual(err["name"], "ValueError")
        self.assertEqual(err["value"], "bad")
        self.assertTrue(any("ValueError" in line for line in err["traceback"]))

    def test_timeout_interrupts_and_kernel_survives(self):
        # A runaway cell that exceeds a short timeout must report TimeoutError
        # with a status:error done, and the kernel must still run the NEXT cell,
        # proving it was interrupted rather than left wedged on the busy loop.
        events, _ = self._run([
            {"id": "a", "code": "while True:\n    pass", "timeout": 2},
            {"id": "b", "code": "21 * 2"},
        ])
        err = next(e for e in events if e["kind"] == "error" and e["id"] == "a")
        self.assertEqual(err["name"], "TimeoutError")
        done_a = next(e for e in events if e["kind"] == "done" and e["id"] == "a")
        self.assertEqual(done_a["status"], "error")
        result_b = next(e for e in events if e["kind"] == "result" and e["id"] == "b")
        self.assertEqual(result_b["data"]["text/plain"].strip(), "42")
        done_b = next(e for e in events if e["kind"] == "done" and e["id"] == "b")
        self.assertEqual(done_b["status"], "ok")

    def test_kernel_death_reports_error(self):
        # A cell that kills the kernel mid-execution must report KernelDied with
        # a status:error done, not a clean status:ok. os._exit(0) terminates the
        # kernel process without a normal shutdown, mimicking a crash.
        events, _ = self._run([
            {"id": "a", "code": "import os\nos._exit(0)", "timeout": 30},
        ])
        done_a = next(e for e in events if e["kind"] == "done" and e["id"] == "a")
        self.assertEqual(done_a["status"], "error")
        self.assertTrue(
            any(e["kind"] == "error" and e.get("name") == "KernelDied"
                for e in events if e["id"] == "a")
        )


def _import_driver():
    """Import kernel_driver with jupyter_client stubbed, so no kernel deps are needed."""
    if "kernel_driver" in sys.modules:
        return sys.modules["kernel_driver"]
    if "jupyter_client.manager" not in sys.modules:
        stub = types.ModuleType("jupyter_client")
        manager = types.ModuleType("jupyter_client.manager")
        manager.start_new_kernel = lambda **_kw: (None, None)
        stub.manager = manager
        sys.modules.setdefault("jupyter_client", stub)
        sys.modules["jupyter_client.manager"] = manager
    sys.path.insert(0, HERE)
    import kernel_driver
    return kernel_driver


class _FakeClient:
    def __init__(self):
        self.executed = []

    def execute(self, code, store_history=True, silent=False):
        self.executed.append({"code": code, "store_history": store_history, "silent": silent})
        return "msg-1"


class ForkReseedTest(unittest.TestCase):
    """Fork correctness for the run_code kernel.

    The hosted python pool sets warmKernel: true, so the template build starts the
    ipykernel BEFORE the snapshot and every sandbox restores a live interpreter.
    ipykernel's own import chain imports `random`, so random.Random's Mersenne state is
    fixed in the snapshot and inherited identically by every restore. Measured against
    prod before this fix, three INDEPENDENT sandboxes each returned

        random.random() == 0.993005259705148

    while os.urandom() and numpy.random diverged correctly (the kernel CRNG is credibly
    reseeded per fork, and numpy seeds lazily from it).

    The agent broadcasts SIGUSR2 after a fork, AFTER reseeding the CRNG, and only to
    processes that installed a SIGUSR2 handler. The driver installs one and reseeds the
    kernel's Python PRNGs before the next cell runs.
    """

    def setUp(self):
        self.kd = _import_driver()
        self.kd._reseed_pending = False
        self._drained = []
        self._real_drain = self.kd._drain_to_idle
        self.kd._drain_to_idle = lambda km, client, msg_id, budget=10.0: self._drained.append(msg_id)

    def tearDown(self):
        self.kd._drain_to_idle = self._real_drain
        self.kd._reseed_pending = False

    def test_sigusr2_handler_only_sets_a_flag(self):
        """A signal handler must not drive jupyter_client: a cell may be mid-flight."""
        self.assertFalse(self.kd._reseed_pending)
        self.kd._on_sigusr2(signal.SIGUSR2, None)
        self.assertTrue(self.kd._reseed_pending)

    def test_the_driver_installs_a_sigusr2_handler(self):
        """The agent signals only processes that installed a handler, so without this
        the post-fork broadcast never reaches the driver at all."""
        previous = signal.getsignal(signal.SIGUSR2)
        try:
            self.kd._install_fork_handler()
            self.assertIs(signal.getsignal(signal.SIGUSR2), self.kd._on_sigusr2)
        finally:
            signal.signal(signal.SIGUSR2, previous)

    def test_reseed_runs_once_before_the_next_cell_and_not_again(self):
        client = _FakeClient()
        self.kd._on_sigusr2(signal.SIGUSR2, None)

        self.assertTrue(self.kd._maybe_reseed(None, client), "a forked kernel must reseed")
        self.assertEqual(len(client.executed), 1)
        self.assertEqual(self._drained, ["msg-1"])

        # A later cell in the same (unforked) sandbox must not pay for another reseed.
        self.assertFalse(self.kd._maybe_reseed(None, client))
        self.assertEqual(len(client.executed), 1)

    def test_reseed_is_silent_and_leaves_no_history(self):
        """The reseed is ours, not the tenant's: it must not appear in In[]/Out[] or
        emit output frames that would corrupt the caller's stream."""
        client = _FakeClient()
        self.kd._on_sigusr2(signal.SIGUSR2, None)
        self.kd._maybe_reseed(None, client)
        sent = client.executed[0]
        self.assertTrue(sent["silent"])
        self.assertFalse(sent["store_history"])

    def test_a_failing_reseed_never_fails_the_callers_cell(self):
        class _Boom:
            def execute(self, *_a, **_kw):
                raise RuntimeError("kernel is wedged")

        self.kd._on_sigusr2(signal.SIGUSR2, None)
        self.kd._maybe_reseed(None, _Boom())  # must not raise
        self.assertFalse(self.kd._reseed_pending)

    def test_the_reseed_source_actually_reseeds_python_random(self):
        """Execute the exact source the driver sends, here, and prove it moves
        random.Random off the state it had. Two reseeds must not agree either."""
        import random

        random.seed(1234)
        before = random.random()

        random.seed(1234)
        exec(self.kd._RESEED_SRC, {})
        after_first = random.random()

        random.seed(1234)
        exec(self.kd._RESEED_SRC, {})
        after_second = random.random()

        self.assertNotEqual(before, after_first, "the reseed did not move random's state")
        self.assertNotEqual(after_first, after_second, "two reseeds produced the same stream")

    def test_the_reseed_source_leaves_no_names_behind(self):
        """It runs in the tenant's namespace, so it must not litter it."""
        ns = {}
        exec(self.kd._RESEED_SRC, ns)
        leaked = [k for k in ns if not k.startswith("__")]
        self.assertEqual(leaked, [], "reseed leaked names into the user namespace: %r" % leaked)

    def test_the_reseed_source_does_not_import_numpy_when_absent(self):
        """Importing numpy just to reseed it would cost real time on every fork of an
        image that never uses it."""
        self.assertNotIn("import numpy", self.kd._RESEED_SRC)
        self.assertIn('sys.modules.get("numpy")', self.kd._RESEED_SRC)


if __name__ == "__main__":
    unittest.main()
