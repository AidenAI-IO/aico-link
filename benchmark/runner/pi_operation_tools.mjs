// Load only the experiment's explicit business tools. Pi owns its agent loop,
// context, retries and built-in tools; this extension only translates I/O.
import { readFileSync, writeFileSync } from "node:fs";

export function toolResult(response) {
  let output = response.output;
  try { output = JSON.parse(output); } catch { /* Plain text is a valid result. */ }
  // The HTTP endpoint can report successful invocation while the business
  // action failed. Preserve the explicit action outcome, including when it is
  // wrapped with a screenshot; do not infer failure from screen stability.
  let action = output?.action_output ?? output;
  try { action = JSON.parse(action); } catch { /* Plain text is valid. */ }
  const isError = !!response.is_error || response.tool_error != null || action?.ok === false;
  const content = [];
  if (output && typeof output === "object" && typeof output.data === "string"
      && output.width > 0 && output.height > 0
      && ["jpeg", "jpg", "png", "webp"].includes(output.format)) {
    const { data, ...metadata } = output;
    content.push({ type: "text", text: JSON.stringify(metadata) });
    content.push({ type: "image", mimeType: `image/${output.format === "jpg" ? "jpeg" : output.format}`, data });
  } else {
    content.push({ type: "text", text: typeof output === "string" ? output : JSON.stringify(output ?? response.error ?? "") });
  }
  return { content, details: { source: "aiden-tool-api", raw_input: response.raw_input, tool_error: response.tool_error,
                               is_error: isError, duration_ms: response.duration_ms } };
}

export default function operationTools(pi) {
  const config = JSON.parse(readFileSync(process.env.AIDEN_OPERATION_CONFIG, "utf8"));
  for (const tool of config.tools) {
    pi.registerTool({
      name: tool.name,
      label: tool.name,
      description: tool.description,
      parameters: tool.args_schema,
      async execute(_id, args, signal) {
        const response = await fetch(`${config.endpoint}/api/tools/${encodeURIComponent(tool.name)}`, {
          method: "POST", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ input: args }), signal,
        });
        if (!response.ok) throw new Error(`Business tool ${tool.name}: HTTP ${response.status}`);
        return toolResult(await response.json());
      },
    });
  }
  // Preserve error status AND any screenshot returned alongside a failed action.
  pi.on("tool_result", event => {
    if (config.tools.some(tool => tool.name === event.toolName) && event.details?.is_error) {
      return { isError: true };
    }
  });
  writeFileSync(config.ready_file, JSON.stringify(config.tools.map(tool => tool.name)));
}
