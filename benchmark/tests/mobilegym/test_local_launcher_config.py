from mobilegym.scripts import local_launcher


def test_current_model_label_uses_only_benchmark_agent_model():
    assert local_launcher.current_model_label(
        {
            "AICO_BENCHMARK_AGENT_MODEL": "benchmark-model",
            "AICO_MODEL": "legacy-aico-model",
            "MODEL_NAME": "legacy-model",
            "OPENAI_MODEL": "legacy-openai-model",
        }
    ) == "benchmark-model"
    assert local_launcher.current_model_label({"MODEL_NAME": "legacy-model"}) == "aico-go"


def test_agent_model_environment_uses_benchmark_agent_names():
    assert local_launcher.agent_model_environment(
        {"provider": "benchmark", "model": "agent-model"},
        {
            "type": "openai",
            "base_url": "https://agent.example/v1",
            "api_key": "agent-key",
        },
    ) == {
        "AICO_BENCHMARK_AGENT_PROVIDER": "openai",
        "AICO_BENCHMARK_AGENT_MODEL": "agent-model",
        "AICO_BENCHMARK_AGENT_BASE_URL": "https://agent.example/v1",
        "AICO_BENCHMARK_AGENT_API_KEY": "agent-key",
    }


def test_parse_agent_benchmark_assignments_uses_benchmark_judge_names():
    assert local_launcher.parse_agent_benchmark_assignments(
        'api_key = "judge-key"\njudge_model = "judge-model"\nbase_url = "https://judge.example/v1"\n'
    ) == {
        "AICO_BENCHMARK_JUDGE_API_KEY": "judge-key",
        "AICO_BENCHMARK_JUDGE_MODEL": "judge-model",
        "AICO_BENCHMARK_JUDGE_BASE_URL": "https://judge.example/v1",
    }


def test_validate_model_environment_ignores_legacy_model_credentials():
    env = {
        "MODEL_PROVIDER": "openrouter",
        "MODEL_API_KEY": "legacy-model-key",
        "OPENROUTER_API_KEY": "legacy-openrouter-key",
    }

    try:
        local_launcher.validate_model_environment(env)
    except local_launcher.LauncherError as exc:
        assert str(exc) == (
            "MobileGym model config missing: set AICO_BENCHMARK_AGENT_API_KEY "
            "before starting the Mac MobileGym launcher"
        )
    else:
        raise AssertionError("expected legacy model credentials to be ignored")
