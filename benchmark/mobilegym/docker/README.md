# MobileGym Docker Image

This directory contains the Dockerfile used by the active benchmark WebUI and
CLI service paths.

The supported entry points are:

```bash
cd benchmark
uv run python -m runner webui
uv run python -m runner start-mobilegym-env
```

Both paths resolve the MobileGym repository from `.gitmodules` and the exact
commit from the firmware repository's MobileGym gitlink. They reuse the local
image only when its `io.aico.mobilegym.commit` label matches that commit;
otherwise they build `benchmark/mobilegym/docker/Dockerfile` with the
`mobilegym-base` target. The runner and daemon orchestration lives in
`benchmark/runner/webui.py`, `benchmark/runner/services.py`, and
`benchmark/docker/`.

Standalone MobileGym compose/test-runner files were removed because they used
the old `run_aico.py` flow and were no longer part of the WebUI or benchmark
CLI execution path.
