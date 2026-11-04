from pathlib import Path

from vphone.scripts.start_bridge import _agent_daemon_command, _parse_args


_RUNTIME_ENV_KEYS = [
    "VPHONE_ENV_FILE",
    "VPHONE_SOCKET",
    "VPHONE_BRIDGE_HOST",
    "VPHONE_BRIDGE_PORT",
    "VPHONE_GUEST_SSH_HOST",
    "VPHONE_GUEST_SSH_PORT",
    "VPHONE_GUEST_SSH_USER",
    "VPHONE_GUEST_SSH_IDENTITY",
    "VPHONE_BENCHMARK_TASK_ID",
]


def _clear_runtime_env(monkeypatch) -> None:
    for key in _RUNTIME_ENV_KEYS:
        monkeypatch.delenv(key, raising=False)


def test_start_bridge_loads_mac_black_values_from_env_file(tmp_path: Path, monkeypatch):
    _clear_runtime_env(monkeypatch)
    env_file = tmp_path / "vphone.env"
    env_file.write_text(
        "\n".join(
            [
                "VPHONE_SOCKET=/Users/miao/dev/vphone-cli/vm/vphone.sock",
                "VPHONE_BRIDGE_HOST=127.0.0.1",
                "VPHONE_BRIDGE_PORT=8899",
                "VPHONE_GUEST_SSH_HOST=192.168.64.8",
                "VPHONE_GUEST_SSH_PORT=22222",
                "VPHONE_GUEST_SSH_USER=root",
                "VPHONE_GUEST_SSH_IDENTITY=/Users/miao/.ssh/vphone_ecdsa",
                "VPHONE_BENCHMARK_TASK_ID=vphone-ios-cli",
            ]
        )
        + "\n",
        encoding="utf-8",
    )

    args = _parse_args(["--env-file", str(env_file)])

    assert args.socket == "/Users/miao/dev/vphone-cli/vm/vphone.sock"
    assert args.host == "127.0.0.1"
    assert args.port == 8899
    assert args.guest_ssh_host == "192.168.64.8"
    assert args.guest_ssh_port == 22222
    assert args.guest_ssh_user == "root"
    assert args.guest_ssh_identity == "/Users/miao/.ssh/vphone_ecdsa"
    assert args.benchmark_task_id == "vphone-ios-cli"


def test_start_bridge_cli_values_override_env_file(tmp_path: Path, monkeypatch):
    _clear_runtime_env(monkeypatch)
    env_file = tmp_path / "vphone.env"
    env_file.write_text(
        "VPHONE_SOCKET=/tmp/from-env.sock\nVPHONE_GUEST_SSH_HOST=192.168.64.8\n",
        encoding="utf-8",
    )

    args = _parse_args(
        [
            "--env-file",
            str(env_file),
            "--socket",
            "/tmp/from-cli.sock",
            "--guest-ssh-host",
            "192.168.64.9",
        ]
    )

    assert args.socket == "/tmp/from-cli.sock"
    assert args.guest_ssh_host == "192.168.64.9"


def test_start_bridge_has_no_hardcoded_guest_ip(tmp_path: Path, monkeypatch):
    _clear_runtime_env(monkeypatch)
    env_file = tmp_path / "vphone.env"
    env_file.write_text("VPHONE_SOCKET=/tmp/vphone.sock\n", encoding="utf-8")

    args = _parse_args(["--env-file", str(env_file)])

    assert args.guest_ssh_host == ""


def test_start_bridge_agent_command_relies_on_environment_platform():
    command = _agent_daemon_command("http://127.0.0.1:8899", "vphone-ios-cli")

    assert command.endswith("--benchmark-task-id vphone-ios-cli")
    assert "--device-type" not in command

