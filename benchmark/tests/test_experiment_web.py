import json
import threading
import urllib.error
import urllib.request

import pytest

from runner.experiment import ExperimentManager, run_experiment
from runner.webui import BenchmarkHTTPServer, BenchmarkWebApp, WebUIConfig
from test_experiment import FakeAgent, SUITE_PATH, fake_judge


@pytest.fixture
def experiment_server(tmp_path, monkeypatch):
    def fake_run(self, suite, root, record, config_text, stop, judge_cfg, judge_key):
        adapters = {agent["id"]: FakeAgent([], agent["id"]) for agent in record["agents"]}
        run_experiment(suite, root, record["agents"], adapters, record["repeats"], record["seed"], stop, record, judge_cfg)
    monkeypatch.setattr(ExperimentManager, "_run", fake_run)
    app = BenchmarkWebApp(WebUIConfig(suites_dir=SUITE_PATH.parent, runs_dir=tmp_path / "runs",
                                      base_config_dir=tmp_path / "config", build_daemon_image=False))
    monkeypatch.setattr(app, "list_mobilegym_environments", lambda: [])
    app.experiments.config_loader = lambda: "frozen config"
    app.experiments.judge_loader = lambda: {"api_key": "test-key"}
    server = BenchmarkHTTPServer(("127.0.0.1", 0), app)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield app, f"http://127.0.0.1:{server.server_port}"
    finally:
        app.shutdown()
        server.shutdown()
        server.server_close()
        thread.join(timeout=3)


def request(base, path, payload=None):
    data = json.dumps(payload).encode() if payload is not None else None
    with urllib.request.urlopen(urllib.request.Request(base + path, data=data, headers={"Content-Type": "application/json"}), timeout=10) as response:
        return response.status, response.read().decode()


def test_experiment_http_create_list_and_follow_report_links(experiment_server):
    app, base = experiment_server
    assert "Agent experiments" in request(base, "/")[1]
    assert "Start experiment" in request(base, "/experiments")[1]
    _, data = request(base, "/api/experiments")
    assert any(s["key"] == SUITE_PATH.name for s in json.loads(data)["suites"])
    status, data = request(base, "/api/experiments", {
        "suite": SUITE_PATH.name, "environment_url": "http://env", "agents": [{"kind": "pi", "provider": "test", "model": "model"}], "repeats": 1,
    })
    assert status == 201
    record = json.loads(data)["experiment"]
    app.experiments._active[2].join(timeout=10)
    records = json.loads(request(base, "/api/experiments")[1])["experiments"]
    assert records[0]["status"] == "completed"
    report = request(base, records[0]["report_url"])[1]
    assert "runs/pi/report.html" in report
    assert "pi" in request(base, f"/experiments/{record['id']}/runs/pi/report.html")[1]
    results = request(base, f"/experiments/{record['id']}/runs/pi/results.jsonl")[1]
    assert len(results.splitlines()) == 3


def test_experiment_http_rejects_bad_config_and_artifact_traversal(experiment_server):
    app, base = experiment_server
    with pytest.raises(urllib.error.HTTPError) as error:
        request(base, "/api/experiments", {"suite": "../manual.md", "agents": []})
    assert error.value.code == 400
    for path in ("/experiments/exp-test/../../manual.md", "/experiments/exp-test/config/agent.toml"):
        with pytest.raises(urllib.error.HTTPError) as error:
            request(base, path)
        assert error.value.code == 404


def test_pi_experiment_can_only_be_started_over_loopback(experiment_server):
    app, _ = experiment_server
    class Handler:
        client_address = ("192.0.2.5", 1234)
        server = type("Server", (), {"app": app})()
        def _read_json(self):
            return {"agents": [{"kind": "pi"}]}
        def _send_json(self, payload, status):
            self.payload, self.status = payload, status
    handler = Handler()
    from runner.experiment_web import handle_experiment_request
    assert handle_experiment_request(handler, "/api/experiments", post=True)
    assert handler.status == 403


def test_operation_ui_requires_environment_and_serves_screenshot_evidence(experiment_server):
    app, base = experiment_server
    html = request(base, "/experiments")[1]
    assert "MobileGym environment URL" in html
    suites = json.loads(request(base, "/api/experiments")[1])["suites"]
    assert any(s["key"] == "mobilegym_basic.json" and s["track"] == "operations" for s in suites)
    with pytest.raises(urllib.error.HTTPError) as error:
        request(base, "/api/experiments", {
            "suite": "mobilegym_basic.json", "agents": [{"kind": "pi", "provider": "test", "model": "test"}]})
    assert error.value.code == 400
    root = app.experiments.root / "exp-image" / "runs" / "pi" / "tasks" / "test" / "attempt_1"
    root.mkdir(parents=True)
    (root / "post.jpg").write_bytes(b"\xff\xd8\xff\xd9")
    with urllib.request.urlopen(base + "/experiments/exp-image/runs/pi/tasks/test/attempt_1/post.jpg") as response:
        assert response.headers["Content-Type"] == "image/jpeg"
        assert response.read() == b"\xff\xd8\xff\xd9"


def test_custom_endpoint_http_round_trip_keeps_secret_out_of_manifest(experiment_server, monkeypatch):
    monkeypatch.setenv("MY_GATEWAY_KEY", "http-test-secret")
    app, base = experiment_server
    page = request(base, "/experiments")[1]
    assert 'id="piBaseUrl"' in page and 'id="piKeyEnv"' in page
    assert "Anthropic Messages" in page and "OpenAI Responses" in page
    status, body = request(base, "/api/experiments", {
        "suite": SUITE_PATH.name, "environment_url": "http://env", "repeats": 1,
        "agents": [{"kind": "pi", "provider": "custom", "model": "vision", "thinking": "off",
                    "base_url": "http://127.0.0.1:9000/v1", "api_key_env": "MY_GATEWAY_KEY"}],
    })
    assert status == 201
    record = json.loads(body)["experiment"]
    app.experiments._active[2].join(timeout=10)
    manifest = request(base, f"/experiments/{record['id']}/manifest.json")[1]
    assert "http-test-secret" not in manifest
    agent = json.loads(manifest)["agents"][0]
    assert agent["base_url"] == "http://127.0.0.1:9000/v1"
    assert agent["api_key_env"] == "MY_GATEWAY_KEY"
    assert agent["api"] == "openai-completions"



def test_managed_environment_resolves_host_endpoint_without_url_input(experiment_server, monkeypatch):
    app, base = experiment_server
    env = {"id": "env-ready", "name": "Comparison", "status": "running",
           "endpoint": "http://host.docker.internal:9123", "public_endpoint": "http://127.0.0.1:9123"}
    monkeypatch.setattr(app, "list_mobilegym_environments", lambda: [env])
    assert json.loads(request(base, "/api/experiments")[1])["environments"] == [env]
    status, body = request(base, "/api/experiments", {
        "suite": SUITE_PATH.name, "environment_id": env["id"], "repeats": 1,
        "agents": [{"kind": "aiden"}],
    })
    assert status == 201
    record = json.loads(body)["experiment"]
    assert record["environment_url"] == env["public_endpoint"]
    app.experiments._active[2].join(timeout=10)


@pytest.mark.parametrize("state", ["starting", "building", "stopped", "stale", "failed", "missing"])
def test_unready_managed_environment_does_not_start_experiment(experiment_server, monkeypatch, state):
    app, base = experiment_server
    monkeypatch.setattr(app, "list_mobilegym_environments", lambda: [] if state == "missing" else [
        {"id": "env-test", "status": state, "public_endpoint": "http://env"}])
    with pytest.raises(urllib.error.HTTPError) as error:
        request(base, "/api/experiments", {"suite": SUITE_PATH.name, "environment_id": "env-test",
                                          "agents": [{"kind": "aiden"}]})
    assert error.value.code == 400
    assert "not running" in error.value.read().decode()
    assert app.experiments._active is None


def test_environment_id_cannot_override_an_explicit_url(experiment_server):
    app, base = experiment_server
    with pytest.raises(urllib.error.HTTPError) as error:
        request(base, "/api/experiments", {"suite": SUITE_PATH.name, "environment_id": "env-test",
                                          "environment_url": "http://env", "agents": [{"kind": "aiden"}]})
    assert error.value.code == 400
    assert app.experiments._active is None


def test_saved_pi_settings_survive_new_manager_and_are_used_to_start(experiment_server, monkeypatch):
    app, base = experiment_server
    monkeypatch.setenv('AIDEN_BENCHMARK_PI_API_KEY', 'fixture-secret')
    pi = {'kind': 'pi', 'provider': 'apibest', 'model': 'gpt-5.6-sol', 'thinking': 'medium',
          'base_url': 'https://apibest.ai/v1', 'api': 'openai-completions',
          'api_key_env': 'AIDEN_BENCHMARK_PI_API_KEY', 'context_window': 1050000, 'max_tokens': 16384}
    status, response = request(base, '/api/experiments/settings', {'pi': pi})
    assert status == 200
    saved = json.loads(response)['settings']
    replacement = ExperimentManager(runs_dir=app.experiments.root.parent,
                                    suites_dir=SUITE_PATH.parent, base_config_dir=app.experiments.base_config_dir,
                                    daemon_image='unused', build_daemon_image=False, config_loader=lambda: '')
    assert replacement.settings() == saved
    defaults = json.loads(request(base, '/api/experiments')[1])['settings']
    assert defaults == saved
    assert 'fixture-secret' not in (app.experiments.root.parent / 'experiment-settings.json').read_text()
    status, response = request(base, '/api/experiments', {
        'suite': SUITE_PATH.name, 'environment_url': 'http://env', 'repeats': 1,
        'agents': [defaults['pi']],
    })
    assert status == 201
    app.experiments._active[2].join(timeout=10)
    assert json.loads(response)['experiment']['agents'][0]['model'] == 'gpt-5.6-sol'


def test_invalid_settings_do_not_replace_saved_values(experiment_server):
    app, base = experiment_server
    saved = {'pi': {'kind': 'pi', 'provider': 'test', 'model': 'test'}}
    request(base, '/api/experiments/settings', saved)
    before = app.experiments.settings()
    for pi in ({'kind': 'pi', 'provider': 'test', 'model': ''},
               {'kind': 'pi', 'provider': 'test', 'model': 'test', 'api_key': 'secret'}):
        with pytest.raises(urllib.error.HTTPError) as error:
            request(base, '/api/experiments/settings', {'pi': pi})
        assert error.value.code == 400
        assert app.experiments.settings() == before


def test_failed_preparation_has_log_link_without_report(experiment_server):
    app, base = experiment_server
    root = app.experiments.root / 'exp-build-failed'
    root.mkdir()
    (root / 'manifest.json').write_text(json.dumps({'id':root.name, 'status':'failed',
        'stage':'Building daemon image', 'error':'registry timeout', 'completed':0, 'total':6}))
    (root / 'setup.log').write_text('registry timeout')
    record = json.loads(request(base, '/api/experiments')[1])['experiments'][0]
    assert record['setup_log_url'] == '/experiments/exp-build-failed/setup.log'
    assert record['report_url'] == ''
    assert request(base, record['setup_log_url'])[1] == 'registry timeout'
