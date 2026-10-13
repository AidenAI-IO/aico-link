# Aiden MobileGym Integration

MobileGym is integrated into Aiden benchmarks solely as a simulator, using the unified `benchmark/runner` framework.

## 🎯 Architecture

MobileGym **serves only as a device simulator**, exposed through the unified environment bridge API:

```text
benchmark/runner/main.py (test orchestration)
  ↓ /api/chat
Aiden Go Daemon (environment-bridge mode)
  ↓ /api/providers/mnk + /api/providers/screenshot
MobileGym Bridge Server (HTTP ↔ env.step)
  ↓ env.step(action)
MobileGym Simulator (bench_env)
```

**Key points**:
- ✅ Uses `benchmark/runner` for a unified test workflow
- ✅ Defines test tasks in `benchmark/suites/*.json`
- ✅ The Bridge Server exposes environment bridge interfaces aligned with the Go agent's dispatch protocol
- ✅ Connects through environment bridge mode without special configuration
- ❌ Does not use MobileGym's `SerialRunner`, `factory`, or agent registration

## 🚀 Quick start

### Option 1: Run locally (development)

```bash
# 1. Start the MobileGym simulator and Bridge Server in the background
python benchmark/mobilegym/scripts/start_simulator.py \
  --env-url http://localhost:4173 \
  --bridge-port 8888 &

# 2. Start the Aiden daemon in environment bridge mode.
# When manually debugging a single task, use the same benchmark-task-id for the daemon and runner.
go run src/agent/cmd/daemon/main.go \
  --config /path/to/agent.toml \
  --environment-bridge-mode \
  --environment-bridge-endpoint http://localhost:8888 \
  --benchmark-task-id cli-task &

# 3. Run the benchmark with the standard runner
cd benchmark
uv run python -m runner run \
  --suite suites/mobilegym_basic.json \
  --agent-url http://localhost:8080 \
  --environment-url http://localhost:8888 \
  --benchmark-task-id cli-task
```

**Note**: The unified environment bridge mode is now used; `device.backend=mobilegym` is no longer required.

### Option 2: WebUI / CLI services (recommended)

```bash
# The WebUI builds the MobileGym simulator and agent daemon images on demand.
cd benchmark
uv run python -m runner webui
```

You can also start the environment and daemon from the CLI:

```bash
cd benchmark
uv run python -m runner start-mobilegym-env --envs 5
uv run python -m runner start-agent-daemon --environment-bridge-endpoint http://127.0.0.1:<bridge-port>
uv run python -m runner run \
  --suite suites/mobilegym_basic.json \
  --agent-url http://127.0.0.1:<agent-port> \
  --environment-url http://127.0.0.1:<bridge-port>
```

## 📁 Directory structure

```text
benchmark/mobilegym/
├── README.md                       # This file
├── bridge/                         # Bridge server (HTTP ↔ MobileGym env) ⭐
│   ├── server.py                   # HTTP endpoints
│   ├── episode.py                  # Episode state management
│   ├── protocol.py                 # Protocol definitions
│   └── actions.py                  # Action conversion
├── docker/                         # MobileGym base image used by the WebUI/CLI
│   ├── Dockerfile                  # mobilegym-base target
│   └── README.md                   # Current Docker entry point documentation
├── scripts/                        # Startup scripts ⭐
│   ├── start_simulator.py          # Start the standalone simulator
│   └── configure_daemon.py         # Configure the daemon bridge
└── vendor/mobilegym/               # Upstream MobileGym (submodule)
```

**Removed components** (no longer needed):
- ❌ `adapter/register.py` - agent registration
- ❌ `adapter/aiden_go_agent.py` - MobileGym Agent adapter
- ❌ `scripts/run_aiden.py` - MobileGym test framework entry point
- ❌ `suites/*.yaml` - custom MobileGym suites (replaced by benchmark/suites/*.json)

## 🎯 Test definitions

All test tasks are defined in `benchmark/suites/*.json` using a unified format:

```json
{
  "name": "mobilegym_basic",
  "prompt_prefix": "You are controlling an Android emulator.",
  "global_reset": {
    "type": "agent_prompt",
    "prompt": "Please reset the device to its initial state",
    "timeout_sec": 30,
    "clear_history_after": true
  },
  "tasks": [
    {
      "id": "clock_count_alarms",
      "category": "device_operation",
      "prompt": "Open the Clock app and tell me how many alarms there are.",
      "description_for_judge": "The agent should open the Clock app and correctly report the number of alarms",
      "rubric": [
        {
          "id": "opened_clock_app",
          "check": "Post-screenshot shows the Clock app."
        },
        {
          "id": "counted_alarms",
          "check": "Final response reports the correct alarm count."
        }
      ],
      "hard_assertions": {
        "must_complete_within_sec": 120,
        "min_tool_calls": 2,
        "required_tools": ["screenshot", "touch_gesture"]
      }
    }
  ]
}
```

Use `benchmark/suites/mobilegym_scroll_regression.json` for the two fixed
Scroll Lab targets (positions 24 and 83). Final selection and irreversible
scroll overshoot are checked through `/state` and `/route`. The 100-depth
`mobilegym_scroll_sweep.json` is optional calibration, not the regression gate.

## 🔧 Configuration

### Environment Bridge mode

Start the daemon with these command-line arguments:

```bash
go run cmd/daemon/main.go \
  --config agent.toml \
  --environment-bridge-mode \
  --environment-bridge-endpoint http://localhost:8888 \
  --benchmark-task-id cli-task
```

### Bridge environment variables

Environment variables available when starting the simulator:

- `MOBILEGYM_ENV_URL`: MobileGym web simulator URL (default: `http://localhost:4173`)
- `AIDEN_BRIDGE_BIND_HOST`: Bridge bind address (default: `127.0.0.1`)
- `AIDEN_BRIDGE_PORT`: Bridge port (automatically assigned by default)
- `AIDEN_BRIDGE_PUBLIC_HOST`: Bridge public address (required for Docker)

## ✅ Validation

```bash
# Check simulator health
curl http://localhost:8888/health

# Claim a task route
curl -X POST http://localhost:8888/api/setup \
  -H "Content-Type: application/json" \
  -H "benchmark-task-id: cli-task" \
  -d '{}'

# Get a screenshot for the runner/judge
curl -X POST http://localhost:8888/api/providers/screenshot \
  -H "Content-Type: application/json" \
  -H "benchmark-task-id: cli-task" \
  -d '{"format": "jpeg", "quality": 80}'

# Test an MNK provider operation
curl -X POST http://localhost:8888/api/providers/mnk \
  -H "Content-Type: application/json" \
  -H "benchmark-task-id: cli-task" \
  -d '{"operation":"click","click":{"x":500,"y":800,"button":"left","hold_ms":0}}'

# Run a single test
cd benchmark
uv run python -m runner run \
  --suite suites/mobilegym_basic.json \
  --agent-url http://localhost:8080 \
  --environment-url http://localhost:8888 \
  --benchmark-task-id cli-task \
  --no-judge  # Skip the judge for quick validation
```

## 📚 Related documentation

- **Unified Tool API**: [bridge/TOOLS_API.md](bridge/TOOLS_API.md) ⭐ **New**
- **Using Docker**: [docker/README.md](docker/README.md)
- **Bridge protocol**: See the Python implementation in `bridge/`

## 🐛 Troubleshooting

### Simulator fails to start
```bash
# Check MobileGym dependencies
pip install -r benchmark/mobilegym/vendor/mobilegym/bench_env/requirements.txt
playwright install chromium
```

### Bridge connection fails
```bash
# Check bridge health
curl http://localhost:8888/health
```

### Daemon cannot call device tools
```bash
# Verify daemon health
curl http://localhost:8080/health

# Verify the bridge tool catalog
curl http://localhost:8888/api/tools
```

## Input coverage and result interpretation

Environment-bridge mode keeps the Agent's `enter_text` engine: mode probing,
probe cleanup, ASCII/IME planning, and verification still run in the Agent.
Its internal ASCII keyboard primitive uses the MNK `keyboard_text` operation
instead of opening a local HID device. MobileGym sends native browser keyboard
events so undo, selection, copy/paste, and deletion retain editing semantics.
This does not validate physical USB HID or real-phone IME compatibility.

The bridge supports persistent `drag_start` / `move` / `drag_release` contact.
Start holds for 500 ms and performs the same bounded activation movement as the
Agent's local provider; release moves to the destination, holds, and lifts.
Contacts belong to one environment slot and are cancelled on episode reset,
end, or task release. Atomic `touch_actions` programs remain unsupported.

A successful screenshot preflight proves connectivity, not input coverage.
Judge traces include tool results as well as attempted calls; failed calls are
not evidence that an operation happened. Human handoff is recorded explicitly
as unfinished execution. An episode final answer can supply missing response
text, but does not itself prove task completion.

The phone-control rubric accepts a single successful long press. Device-info
navigation accepts "About phone" or "My device" and requires scrolling only
when necessary. Compare these rubric corrections separately from changes in
Agent completion rates; suite hashes identify the changed evaluation inputs.
