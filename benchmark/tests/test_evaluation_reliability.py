import json
from contextlib import contextmanager

import pytest

from runner.judge import JUDGE_TEMPLATE, JudgeConfig, judge_task
from runner.suite import RubricItem
from runner.langfuse_reporter import publish_run
from test_langfuse_reporter import FakeLangfuse, _write_run


VALID = {'items': [{'id': 'ok', 'verdict': 'yes', 'reason': 'supported'}], 'overall_notes': ''}


def judge(monkeypatch, responses, **kwargs):
    requests = []

    @contextmanager
    def urlopen(req, timeout):
        requests.append(json.loads(req.data))
        value = responses.pop(0)
        if isinstance(value, Exception):
            raise value
        class Response:
            status = 200
            def read(self):
                return json.dumps({'choices': [{'message': {'content': value}}]}).encode()
        yield Response()

    monkeypatch.setenv('AICO_BENCHMARK_JUDGE_API_KEY', 'test')
    monkeypatch.setattr('urllib.request.urlopen', urlopen)
    result = judge_task(description='Recommend a relaxing game', rubric=[RubricItem(id='ok', check='Uses preference')],
                        pre_screenshot=None, post_screenshot=None, trace={}, final_response='<final_answer>(a)</final_answer>',
                        cfg=JudgeConfig(), **kwargs)
    return result, requests


def test_judge_json_example_is_valid():
    prompt = JUDGE_TEMPLATE.format(description='goal', rubric_lines='rubric', task_prompt='prompt')
    json.loads(prompt.split('Respond as JSON only, no prose:')[1].strip())


def test_judge_receives_options_and_keys_cache_by_prompt(monkeypatch, tmp_path):
    prompts = ['Options: (a) Monument Valley (b) RPG', 'Options: (a) RPG (b) Monument Valley']
    keys = []
    for prompt in prompts:
        out, requests = judge(monkeypatch, [json.dumps(VALID)], task_prompt=prompt, cache_dir=tmp_path)
        keys.append(out.cache_key)
        assert prompt in json.dumps(requests)
    assert keys[0] != keys[1]


@pytest.mark.parametrize('bad', [
    '{"items": [{"id": "ok", "verdict": "yes" or "no"}]}',
    json.dumps({'items': []}),
    json.dumps({'items': [VALID['items'][0], VALID['items'][0]]}),
    json.dumps({'items': [{'id': 'other', 'verdict': 'yes', 'reason': 'wrong id'}]}),
    json.dumps({'items': [{'id': 'ok', 'verdict': 'maybe', 'reason': 'invalid'}]}),
])
def test_judge_retries_invalid_output_before_caching(monkeypatch, tmp_path, bad):
    out, requests = judge(monkeypatch, [bad, json.dumps(VALID)], cache_dir=tmp_path)
    assert len(requests) == 2
    assert out.verdicts[0].verdict == 'yes'
    assert len(list(tmp_path.glob('*.json'))) == 1


def test_judge_stops_after_bounded_invalid_output(monkeypatch, tmp_path):
    with pytest.raises((ValueError, RuntimeError)):
        judge(monkeypatch, ['{}'] * 3, cache_dir=tmp_path)
    assert not list(tmp_path.glob('*.json'))


def test_score_disconnect_retries_same_id_after_uncertain_write(monkeypatch, tmp_path):
    import httpx
    client = FakeLangfuse()
    calls = []
    original = client.api.scores.create
    def create(**kwargs):
        calls.append(kwargs)
        result = original(**kwargs)
        if len(calls) == 1:
            raise httpx.RemoteProtocolError('Server disconnected without sending a response.')
        return result
    client.api.scores.create = create
    result = publish_run(_write_run(tmp_path), client=client)
    assert result.item_count == 1
    assert calls[0] == calls[1]
    assert len({call['id'] for call in calls}) == len(calls) - 1
