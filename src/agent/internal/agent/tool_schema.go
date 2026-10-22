package agent

// Schema Examples Guidelines
//
// All schema helper functions in this file support optional examples via variadic parameters.
// The examples field is a standard JSON Schema field that helps LLMs understand parameter formats.
//
// KEY PRINCIPLE: Only add examples when they prevent common LLM mistakes.
//               Do NOT add examples when the type constraint is already clear.
//
// ============================================================================
// MUST PROVIDE EXAMPLES
// ============================================================================
//
// 1. Coordinate parameters (x, y, width, height):
//    numberArgSchema("X coordinate", 500)
//    numberArgSchema("Y coordinate", 300)
//
//    Why: LLMs may incorrectly pass arrays like [500, 300] or objects like {"value": 500}
//         Examples show that coordinates must be single numbers.
//
// 2. Array parameters:
//    stringArrayArgSchema("Keys to press", []string{"ctrl", "c"})
//    integerArrayArgSchema("Indices", []int{0, 1, 2})
//
//    Why: LLMs may pass comma-separated strings like "ctrl,c" instead of proper arrays.
//         Examples clarify the array structure.
//
// 3. Complex nested objects:
//    Provide examples showing the complete structure when objects have multiple fields.
//
//    Why: Nested structures can be ambiguous without concrete examples.
//
// ============================================================================
// SHOULD NOT PROVIDE EXAMPLES
// ============================================================================
//
// 1. Boolean parameters:
//    boolArgSchema("Enable feature")  // NO examples
//
//    Why: Only two possible values (true/false). Examples add no value.
//
// 2. Enum parameters:
//    stringEnumArgSchema("Platform", "ios", "android", "mac")  // NO examples
//
//    Why: The enum field already lists all valid values explicitly.
//
// 3. Ranged numbers with minimum/maximum:
//    rangedIntegerArgSchema("Volume level", 0, 100)  // NO examples
//
//    Why: The minimum and maximum constraints already make the valid range clear.
//         Adding examples like 50 or 70 doesn't add clarity.
//
// ============================================================================
// MAY PROVIDE EXAMPLES (case-by-case judgment)
// ============================================================================
//
// 1. Simple strings:
//    stringArgSchema("URL")  // Consider: stringArgSchema("URL", "https://example.com")
//
//    Provide examples if the format is non-obvious or has special requirements.
//
// 2. Simple numbers without range constraints:
//    numberArgSchema("Timeout in seconds")  // Consider: numberArgSchema("Timeout", 30)
//
//    Provide examples if the scale/magnitude is unclear.
//
// ============================================================================
// IMPLEMENTATION DETAIL
// ============================================================================
//
// All helper functions use this pattern:
//
//   if len(examples) > 0 {
//       schema["examples"] = examples
//   }
//
// This ensures that:
// - When examples are provided, they appear in the JSON Schema
// - When examples are omitted, NO empty "examples" field is added (avoiding redundancy)
// - The output JSON Schema remains clean and contains only meaningful information
//
// ============================================================================
// EXAMPLES
// ============================================================================
//
// Good usage:
//   "x": numberArgSchema("X coordinate", 500)                    // ✅ Prevents [500, 300] mistake
//   "keys": stringArrayArgSchema("Keys", []string{"ctrl", "c"})  // ✅ Prevents "ctrl,c" mistake
//   "enabled": boolArgSchema("Enable feature")                   // ✅ No example needed
//   "platform": stringEnumArgSchema("Platform", "ios", "android") // ✅ Enum is clear
//   "volume": rangedIntegerArgSchema("Volume", 0, 100)           // ✅ Range is clear
//
// Bad usage:
//   "enabled": boolArgSchema("Enable feature", true)             // ❌ Unnecessary
//   "platform": stringEnumArgSchema("Platform", "ios", "android", "mac")
//               with manual examples: ["ios"]                     // ❌ Redundant with enum
//   "volume": rangedIntegerArgSchema("Volume", 0, 100, 50)       // ❌ Doesn't add clarity
//
// ============================================================================
// LEGACY CODE NOTICE
// ============================================================================
//
// All known legacy hand-written schema definitions have been migrated to use
// helper functions. When adding new tools:
// - DO use helper functions (stringArgSchema, objectArgsSchema, etc.)
// - DO follow the examples guidelines above
// - DO NOT use direct map[string]any{"type": ...} for parameter definitions
//
// When migrating, preserve existing constraints (minItems, maxItems, etc.) and add
// examples only where they add value according to the guidelines above.

func objectArgsSchema(properties map[string]any, required ...string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringArgSchema(description string, examples ...string) map[string]any {
	schema := map[string]any{"type": "string"}
	if description != "" {
		schema["description"] = description
	}
	if len(examples) > 0 {
		schema["examples"] = examples
	}
	return schema
}

func stringEnumArgSchema(description string, values ...string) map[string]any {
	schema := stringArgSchema(description)
	schema["enum"] = values
	return schema
}

func stringArrayArgSchema(description string, examples ...[]string) map[string]any {
	schema := map[string]any{
		"type":  "array",
		"items": map[string]any{"type": "string"},
	}
	if description != "" {
		schema["description"] = description
	}
	if len(examples) > 0 {
		schema["examples"] = examples
	}
	return schema
}

func boolArgSchema(description string, examples ...bool) map[string]any {
	schema := map[string]any{"type": "boolean"}
	if description != "" {
		schema["description"] = description
	}
	if len(examples) > 0 {
		schema["examples"] = examples
	}
	return schema
}

func integerArgSchema(description string, examples ...int) map[string]any {
	schema := map[string]any{"type": "integer"}
	if description != "" {
		schema["description"] = description
	}
	if len(examples) > 0 {
		schema["examples"] = examples
	}
	return schema
}

func numberArgSchema(description string, examples ...float64) map[string]any {
	schema := map[string]any{"type": "number"}
	if description != "" {
		schema["description"] = description
	}
	if len(examples) > 0 {
		schema["examples"] = examples
	}
	return schema
}

func minIntegerArgSchema(description string, minimum int, examples ...int) map[string]any {
	schema := integerArgSchema(description, examples...)
	schema["minimum"] = minimum
	return schema
}

func rangedIntegerArgSchema(description string, minimum, maximum int, examples ...int) map[string]any {
	schema := minIntegerArgSchema(description, minimum, examples...)
	schema["maximum"] = maximum
	return schema
}

func rangedNumberArgSchema(description string, minimum, maximum float64, examples ...float64) map[string]any {
	schema := numberArgSchema(description, examples...)
	schema["minimum"] = minimum
	schema["maximum"] = maximum
	return schema
}

func coordinateSchema(description string, examples ...float64) map[string]any {
	return rangedNumberArgSchema(description, 0, 1000, examples...)
}

func nonNegativeIntegerSchema(description string) map[string]any {
	return minIntegerArgSchema(description, 0)
}
