import os
import select
import subprocess
import sys
import time

import pytest

from runner.process import terminate_process_tree

pytestmark = pytest.mark.skipif(os.name != "posix", reason="process groups are POSIX-only")


def test_terminate_process_tree_kills_group_members_that_outlive_the_leader():
    # The leader exits on SIGTERM; its child ignores SIGTERM and must still be killed.
    # Both inherit the pipe's write end. EOF on the read end means every holder exited:
    # the kernel closes descriptors at exit, so this does not depend on reaping zombies.
    read_fd, write_fd = os.pipe()
    ready_fd, ready_write = os.pipe()
    # argv[1] is the readiness descriptor, argv[2] the pipe write end both processes hold.
    child = ("import os, signal, sys, time\n"
             "signal.signal(signal.SIGTERM, signal.SIG_IGN)\n"
             "os.write(int(sys.argv[1]), b'x')\n"
             "time.sleep(60)\n")
    script = ("import subprocess, sys, time\n"
              f"subprocess.Popen([sys.executable, '-c', {child!r}, sys.argv[1], sys.argv[2]], "
              "pass_fds=(int(sys.argv[1]), int(sys.argv[2])))\n"
              "time.sleep(60)\n")
    leader = subprocess.Popen([sys.executable, "-c", script, str(ready_write), str(write_fd)],
                              pass_fds=(ready_write, write_fd), start_new_session=True)
    os.close(write_fd)
    os.close(ready_write)
    try:
        # Wait until the child has installed its SIGTERM handler.
        assert select.select([ready_fd], [], [], 10)[0], "child did not start"
        assert os.read(ready_fd, 1) == b"x"
        terminate_process_tree(leader, timeout_sec=1)
        deadline = time.monotonic() + 5
        while True:
            remaining = deadline - time.monotonic()
            assert remaining > 0, "group member survived SIGTERM"
            if select.select([read_fd], [], [], remaining)[0] and os.read(read_fd, 1) == b"":
                break
    finally:
        os.close(read_fd)
        os.close(ready_fd)
