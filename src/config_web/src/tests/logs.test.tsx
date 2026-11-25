import assert from 'node:assert/strict';
import { it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import * as logs from '../lib/logs';
import type { LogMessage } from '../lib/logs';
import { LogMessageView } from '../components/LogMessage';
const context = {
  ...logs,
  renderMessages: (messages: LogMessage[], _show?: boolean, _prefix?: string) =>
    renderToStaticMarkup(
      <>
        {messages.map((message, index) => (
          <LogMessageView key={index} message={message} index={index} />
        ))}
      </>,
    ),
};
const extract = (body: string) => JSON.parse(JSON.stringify(logs.extractResponseMessage(body)));
const requestMessages = (body: unknown) => JSON.parse(JSON.stringify(logs.requestMessages(body)));
it('preserves Anthropic, OpenAI and Responses messages, thinking and tool streams', () => {
  assert.deepEqual(
    requestMessages({ instructions: 'Be concise.', input: 'Hello.' }),
    [
      { role: 'system', content: 'Be concise.' },
      { role: 'user', content: 'Hello.' },
    ],
    'Responses instructions must not suppress the input',
  );
  assert.deepEqual(
    requestMessages({
      input: [
        { type: 'function_call', call_id: 'call-1', name: 'lookup', arguments: '{"q":"x"}' },
        { type: 'function_call_output', call_id: 'call-1', output: 'Found it.' },
      ],
    }),
    [
      {
        role: 'assistant',
        content: '',
        tool_calls: [
          {
            id: 'call-1',
            type: 'function',
            function: { name: 'lookup', arguments: '{"q":"x"}' },
          },
        ],
      },
      { role: 'tool', tool_call_id: 'call-1', content: 'Found it.' },
    ],
    'Responses tool inputs must retain their call and result IDs',
  );
  const reasoningDiff = logs.buildMessageDiffOps(
    [{ role: 'assistant', content: 'Answer', reasoning_content: 'First' }],
    [{ role: 'assistant', content: 'Answer', reasoning_content: 'Second' }],
  );
  assert.equal(reasoningDiff[0].kind, 'modified');
  const anthropicRequest = {
    model: 'claude-sonnet-4-6',
    system: [{ type: 'text', text: 'Use the screen carefully.' }],
    messages: [
      {
        role: 'user',
        content: [
          { type: 'text', text: 'What is shown?' },
          { type: 'image', source: { type: 'base64', media_type: 'image/png', data: 'aGVsbG8=' } },
        ],
      },
      {
        role: 'assistant',
        content: [
          { type: 'thinking', thinking: 'Inspect before calling.', signature: 'sig' },
          { type: 'text', text: 'I will inspect it.' },
          { type: 'tool_use', id: 'tool-1', name: 'inspect_screen', input: { detail: 'high' } },
        ],
      },
      {
        role: 'user',
        content: [{ type: 'tool_result', tool_use_id: 'tool-1', content: 'Screen inspected.' }],
      },
    ],
  };

  assert.deepEqual(
    requestMessages(anthropicRequest),
    [
      { role: 'system', content: [{ type: 'text', text: 'Use the screen carefully.' }] },
      {
        role: 'user',
        content: [
          { type: 'text', text: 'What is shown?' },
          { type: 'image_url', image_url: { url: 'data:image/png;base64,aGVsbG8=' } },
        ],
      },
      {
        role: 'assistant',
        content: [{ type: 'text', text: 'I will inspect it.' }],
        reasoning_content: 'Inspect before calling.',
        tool_calls: [
          {
            id: 'tool-1',
            type: 'function',
            function: { name: 'inspect_screen', arguments: '{"detail":"high"}' },
          },
        ],
      },
      {
        role: 'user',
        content: [{ type: 'tool_result', tool_use_id: 'tool-1', content: 'Screen inspected.' }],
      },
    ],
    'Anthropic requests should expose system, images, tool calls, and tool results to the log UI',
  );

  const anthropicRequestHtml = context.renderMessages(
    requestMessages(anthropicRequest),
    true,
    'anthropic-request',
  );
  assert.match(anthropicRequestHtml, /Use the screen carefully\./);
  assert.match(anthropicRequestHtml, /data:image\/png;base64,aGVsbG8=/);
  assert.match(anthropicRequestHtml, /inspect_screen/);
  assert.match(anthropicRequestHtml, /Screen inspected\./);

  assert.deepEqual(
    requestMessages({
      system: 'Keep the spoken response concise.',
      messages: [{ role: 'user', content: [{ type: 'text', text: 'Describe the screen.' }] }],
    }),
    [
      { role: 'system', content: 'Keep the spoken response concise.' },
      { role: 'user', content: [{ type: 'text', text: 'Describe the screen.' }] },
    ],
    'Anthropic requests with string system prompts should retain the system message',
  );

  const remoteImageRequest = {
    messages: [
      {
        role: 'user',
        content: [
          {
            type: 'image',
            source: { type: 'url', url: 'https://example.com/screen.png' },
          },
        ],
      },
    ],
  };
  const remoteImageRequestHtml = context.renderMessages(
    requestMessages(remoteImageRequest),
    true,
    'anthropic-remote-image-request',
  );
  assert.match(
    remoteImageRequestHtml,
    /src="https:\/\/example\.com\/screen\.png"/,
    'Anthropic URL image sources should render as images',
  );
  assert.match(remoteImageRequestHtml, />screen\.png</);
  assert.equal(
    context.imageUrlFromPart({
      type: 'image',
      source: { type: 'url', url: 'javascript:alert(1)' },
    }),
    '',
    'Non-HTTP image URLs should not render',
  );

  const openAIRequestMessages = [
    {
      role: 'assistant',
      content: [],
      tool_calls: [{ id: 'call-1', type: 'function', function: { name: 'tool', arguments: '{}' } }],
    },
  ];
  assert.deepEqual(
    requestMessages({ messages: openAIRequestMessages }),
    openAIRequestMessages,
    'OpenAI request messages should retain their existing shape',
  );

  assert.deepEqual(
    extract(
      JSON.stringify({
        type: 'message',
        role: 'assistant',
        content: [
          { type: 'thinking', thinking: 'Plan the answer.', signature: 'sig' },
          { type: 'text', text: 'Hello ' },
          { type: 'tool_use', id: 'tool-1', name: 'echo', input: { value: 'test' } },
          { type: 'text', text: 'world' },
        ],
      }),
    ),
    {
      role: 'assistant',
      content: 'Hello world',
      reasoning_content: 'Plan the answer.',
      tool_calls: [
        {
          id: 'tool-1',
          type: 'function',
          function: { name: 'echo', arguments: '{"value":"test"}' },
        },
      ],
    },
    'Anthropic JSON responses should preserve text and tool calls',
  );

  assert.deepEqual(
    extract(
      JSON.stringify({
        id: 'msg_live',
        model: 'claude-sonnet-4-6',
        role: '',
        content: [
          { type: 'text', text: 'Live response' },
          { type: 'tool_use', id: 'tool-live', name: 'echo', input: { value: 'ok' } },
        ],
        stop_reason: 'tool_use',
        usage: { input_tokens: 10, output_tokens: 4 },
      }),
    ),
    {
      role: 'assistant',
      content: 'Live response',
      tool_calls: [
        {
          id: 'tool-live',
          type: 'function',
          function: { name: 'echo', arguments: '{"value":"ok"}' },
        },
      ],
    },
    'Aggregated Anthropic streaming responses should render as assistant messages',
  );

  assert.deepEqual(
    extract(
      JSON.stringify({
        error: { type: 'upstream_error', message: 'Provider failed' },
        content: [],
      }),
    ),
    {
      role: 'assistant',
      content: 'Error: {\n  "type": "upstream_error",\n  "message": "Provider failed"\n}',
    },
    'Error responses should take precedence over content arrays',
  );

  assert.deepEqual(
    extract(
      JSON.stringify({
        choices: [
          { message: { role: 'assistant', content: 'Answer', reasoning_content: 'Think first.' } },
        ],
      }),
    ),
    { role: 'assistant', content: 'Answer', reasoning_content: 'Think first.', tool_calls: [] },
    'OpenAI-compatible responses should preserve reasoning_content',
  );

  assert.deepEqual(
    extract(
      JSON.stringify({
        id: 'resp_1',
        output: [
          { type: 'reasoning', summary: [{ type: 'summary_text', text: 'Compare the options.' }] },
          { type: 'message', content: [{ type: 'output_text', text: 'Choose option A.' }] },
        ],
      }),
    ),
    {
      role: 'assistant',
      content: 'Choose option A.',
      reasoning_content: 'Compare the options.',
      tool_calls: [],
    },
    'Responses API output should expose reasoning summaries',
  );

  assert.deepEqual(
    extract(
      [
        'data: {"choices":[{"delta":{"reasoning_content":"Think "}}]}',
        'data: {"choices":[{"delta":{"reasoning_content":"first.","content":"Answer"}}]}',
        'data: [DONE]',
        '',
      ].join('\n'),
    ),
    { role: 'assistant', content: 'Answer', reasoning_content: 'Think first.', tool_calls: [] },
    'OpenAI-compatible SSE should aggregate reasoning_content and visible content independently',
  );

  assert.deepEqual(
    extract(
      JSON.stringify({
        choices: [
          {
            message: {
              role: 'assistant',
              content: '<think>Check the premise.</think>\nVisible answer.',
            },
          },
        ],
      }),
    ),
    {
      role: 'assistant',
      content: 'Visible answer.',
      reasoning_content: 'Check the premise.',
      tool_calls: [],
    },
    'Complete think tags should render separately from visible content',
  );

  assert.deepEqual(
    extract(
      JSON.stringify({
        choices: [{ message: { role: 'assistant', content: '<think>unfinished' } }],
      }),
    ),
    { role: 'assistant', content: '<think>unfinished', tool_calls: [] },
    'Incomplete think tags should remain intact for diagnostics',
  );

  assert.deepEqual(
    extract(
      [
        'data: {"type":"response.reasoning_summary_text.delta","delta":"Check "}',
        'data: {"type":"response.reasoning_summary_text.delta","delta":"facts."}',
        'data: {"type":"response.output_text.delta","delta":"Done"}',
        '',
      ].join('\n'),
    ),
    { role: 'assistant', content: 'Done', reasoning_content: 'Check facts.', tool_calls: [] },
    'Responses API SSE should aggregate reasoning summary and output text deltas',
  );

  const reasoningHtml = context.renderMessages(
    [{ role: 'assistant', content: 'Answer', reasoning_content: '<private reasoning>' }],
    false,
    'reasoning-response',
  );
  assert.match(reasoningHtml, /msg-reasoning/);
  assert.match(reasoningHtml, />Reasoning</);
  assert.match(reasoningHtml, /&lt;private reasoning&gt;/);

  assert.equal(
    context.extractResponseMessage(
      JSON.stringify({
        content: [{ type: 'text', text: 'Provider diagnostic' }],
        error_code: 123,
      }),
    ),
    null,
    'Unrecognized content-array envelopes should remain raw diagnostics',
  );

  assert.equal(context.formatLogTimestamp('12:34:56'), '12:34:56');
  assert.equal(context.formatLogTimestamp('2026-08-12T12:34:56Z'), '12:34:56');
  assert.equal(context.formatLogTimestamp(''), '');

  const anthropicSse = [
    'event: message_start',
    'data: {"type":"message_start","message":{"role":"assistant"}}',
    '',
    'event: content_block_delta',
    'data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Inspect the request."}}',
    '',
    'event: content_block_delta',
    'data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}',
    '',
    'event: content_block_delta',
    'data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"stream"}}',
    '',
    'event: content_block_start',
    'data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tool-2","name":"echo","input":{}}}',
    '',
    'event: content_block_delta',
    'data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\\"value\\":"}}',
    '',
    'event: content_block_delta',
    'data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\\"test\\"}"}}',
    '',
    'event: message_stop',
    'data: {"type":"message_stop"}',
    '',
  ].join('\n');

  assert.deepEqual(
    extract(anthropicSse),
    {
      role: 'assistant',
      content: 'Hello stream',
      reasoning_content: 'Inspect the request.',
      tool_calls: [
        {
          id: 'tool-2',
          type: 'function',
          function: { name: 'echo', arguments: '{"value":"test"}' },
        },
      ],
    },
    'Anthropic SSE responses should aggregate text and partial tool JSON',
  );

  assert.deepEqual(
    extract('data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}\n'),
    { role: 'assistant', content: 'Overloaded', tool_calls: [] },
    'Anthropic SSE error-only responses should render the error message',
  );

  const anthropicPartialErrorSse = [
    'data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Partial output"}}',
    'data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}',
    '',
  ].join('\n');

  assert.deepEqual(
    extract(anthropicPartialErrorSse),
    { role: 'assistant', content: 'Partial output\nOverloaded', tool_calls: [] },
    'Anthropic SSE errors should preserve partial output',
  );
});
