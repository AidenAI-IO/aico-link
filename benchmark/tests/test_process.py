import os
import signal
import subprocess
import sys
import time

import pytest

from runner.process import terminate_process_tree

pytestmark = pytest.mark.skipif(os.name != "posix", reason="process groups are POSIX-only")


def test_terminate_process_tree_kills_group_members_that_outlive_the_leader(tmp_path):
    pid_file = tmp_path / "child.pid"
    # The leader exits on SIGTERM; its child ignores SIGTERM and must still be killed.
    script = (
        "import os, signal, subprocess, sys, time\n"
        "child = subprocess.Popen([sys.executable, '-c', "
        "'import signal, time; signal.signal(signal.SIGTERM, signal.SIG_IGN); time.sleep(60)'])\n"
        f"open({str(pid_file)!r}, 'w').write(str(child.pid))\n"
        "time.sleep(60)\n"
    )
    leader = subprocess.Popen([sys.executable, "-c", script], start_new_session=True)
    deadline = time.monotonic() + 10
    while not pid_file.exists() or not pid_file.read_text():
        assert time.monotonic() < deadline, "child did not start"
        time.sleep(0.05)
    time.sleep(0.2)
    child = int(pid_file.read_text())
    terminate_process_tree(leader, timeout_sec=1)
    deadline = time.monotonic() + 5
    while True:
        try:
            os.kill(child, 0)
        except ProcessLookupError:
            break
        assert time.monotonic() < deadline, "group member survived SIGTERM"
        time.sleep(0.05)
