#!/usr/bin/env python3
"""Follow the board's USB Internet switch and maintain an SSH SOCKS lease."""

import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time
import uuid


API = "http://127.0.0.1/api/network/usb-internet"


class Host:
    def __init__(self, board):
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.@-]*", board):
            raise ValueError("Use user@host or an SSH config alias")
        self.board = board
        self.directory = Path.home() / ".cache" / "aico-usb-internet"
        self.directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.directory.chmod(0o700)
        self.prefix = self.directory / hashlib.sha256(board.encode()).hexdigest()[:12]
        self.control = str(self.prefix) + ".sock"
        self.stop_file = Path(str(self.prefix) + ".stop")
        self.state_file = Path(str(self.prefix) + ".json")
        self.lock_file = Path(str(self.prefix) + ".lock")
        self.log_file = Path(str(self.prefix) + ".log")
        self.session = uuid.uuid4().hex
        self.forwarding = False
        self.stopping = False

    def ssh(self, *args, data=None, timeout=12, check=True):
        return subprocess.run(
            ["ssh", "-S", self.control, "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", *args, self.board],
            input=data, text=True, capture_output=True, timeout=timeout, check=check,
        )

    def master(self):
        if self.ssh("-O", "check", check=False).returncode == 0:
            return
        self.forwarding = False
        self.ssh("-fNT", "-M", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2")

    def request(self, method="GET", payload=None):
        path = "/session" if method != "GET" else ""
        command = ["ssh", "-S", self.control, "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", self.board,
                   "curl --noproxy '*' -sS --connect-timeout 3 --max-time 5 -w '\\n%{http_code}' "
                   + ("-H 'Content-Type: application/json' --data-binary @- " if payload is not None else "")
                   + f"-X {method} {API}{path}"]
        result = subprocess.run(command, input=json.dumps(payload) if payload is not None else None,
                                capture_output=True, text=True, timeout=12, check=True)
        body, _, code = result.stdout.rpartition("\n")
        if code != "200":
            # Never include request data/credentials or untrusted HTML in logs.
            raise RuntimeError(f"Board USB Internet API returned HTTP {code}")
        return json.loads(body)

    def enable_forward(self):
        if self.forwarding:
            return
        self.ssh("-O", "forward", "-o", "ExitOnForwardFailure=yes", "-R", "127.0.0.1:18081")
        self.forwarding = True

    def release(self):
        if not self.forwarding:
            return
        try:
            self.request("DELETE", {"session_id": self.session})
        except (OSError, RuntimeError, ValueError, subprocess.SubprocessError):
            pass  # The board independently expires the lease within 30 seconds.
        try:
            self.ssh("-O", "cancel", "-R", "127.0.0.1:18081", check=False)
        except (OSError, subprocess.SubprocessError):
            pass
        self.forwarding = False

    def state(self, value):
        self.state_file.write_text(json.dumps({"state": value, "updated_at": int(time.time()), "pid": os.getpid()}) + "\n")

    def run(self):
        with self.lock_file.open("a") as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                return
            self.stop_file.unlink(missing_ok=True)
            for signum in (signal.SIGTERM, signal.SIGINT):
                signal.signal(signum, lambda *_: setattr(self, "stopping", True))
            previous = None
            try:
                while not self.stopping and not self.stop_file.exists():
                    try:
                        self.master()
                        if self.request().get("enabled") is True:
                            self.enable_forward()
                            self.request("PUT", {"host": "mac", "session_id": self.session,
                                                 "unix_time_ms": int(time.time() * 1000)})
                            state = "sharing"
                        else:
                            self.release()
                            state = "disabled in Config Web"
                    except (OSError, RuntimeError, ValueError, subprocess.SubprocessError):
                        self.release()
                        state = "waiting for board or enabled USB Internet service"
                    self.state(state)
                    if state != previous:
                        print(state, flush=True)
                        previous = state
                    for _ in range(10):
                        if self.stopping or self.stop_file.exists():
                            break
                        time.sleep(0.5)
            finally:
                self.release()
                try:
                    self.ssh("-O", "exit", check=False)
                except (OSError, subprocess.SubprocessError):
                    pass
                self.state("stopped")

    def running(self):
        with self.lock_file.open("a") as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                return False
            except BlockingIOError:
                return True

    def start(self):
        if not self.running():
            self.stop_file.unlink(missing_ok=True)
            with self.log_file.open("w") as log:
                subprocess.Popen([sys.executable, str(Path(__file__).resolve()), "run", self.board],
                                 stdin=subprocess.DEVNULL, stdout=log, stderr=log, start_new_session=True)
            for _ in range(20):
                if self.running():
                    break
                time.sleep(0.1)
            else:
                raise RuntimeError(f"Helper failed to start; inspect {self.log_file}")
        print("Host helper running. Enable USB Internet in Config Web > Advanced Settings.")

    def stop(self):
        self.stop_file.touch()
        for _ in range(60):
            if not self.running():
                print("Host sharing stopped. The board switch is unchanged.")
                return
            time.sleep(0.5)
        raise RuntimeError("Stop requested; a pending SSH operation is still finishing")

    def check(self):
        if not self.running() or not self.request().get("active"):
            raise RuntimeError("No active USB Internet session")
        result = subprocess.run(
            ["ssh", "-S", self.control, "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", self.board,
             "/usr/lib/aico/aico-managed-env-run curl -fsS --connect-timeout 10 --max-time 25 "
             "-o /dev/null -w 'HTTPS: %{http_code}\\n' https://example.com"],
            capture_output=True, text=True, timeout=30, check=True,
        )
        print(result.stdout.strip())


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("start", "stop", "status", "check", "run"))
    parser.add_argument("board", nargs="?", default="aico@192.168.42.1")
    args = parser.parse_args()
    host = Host(args.board)
    if args.action == "status":
        if not host.running():
            print("stopped")
        else:
            print(host.state_file.read_text().strip() if host.state_file.exists() else "starting")
    else:
        getattr(host, args.action)()


if __name__ == "__main__":
    try:
        main()
    except (OSError, RuntimeError, ValueError, subprocess.SubprocessError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
