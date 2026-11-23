package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// EmptyObjectSchema is the JSON Schema for a no-argument function: an object
// with no properties. Schema-strict providers (notably Gemini, which rejects a
// null parameters with "expected record, received null") require an object.
//
// The value is a shared template. Every accessor below returns a clone, so a
// caller mutating its result cannot corrupt the package-level template.
var EmptyObjectSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// StrictEmptyObjectSchema is EmptyObjectSchema made strict-mode compatible:
// OpenAI's structured-output validator requires additionalProperties: false
// and a required array listing every key (none, here) even for empty objects.
var StrictEmptyObjectSchema = json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`)

// emptyObjectShellKeys is the whitelist of keys a no-argument object schema may
// carry. Anything else (anyOf, patternProperties, $ref, enum, ...) makes the
// schema a real constraint that must never be rewritten.
var emptyObjectShellKeys = map[string]struct{}{
	"type":                 {},
	"properties":           {},
	"required":             {},
	"additionalProperties": {},
	"description":          {},
	"title":                {},
	"$schema":              {},
	"$id":                  {},
	"$comment":             {},
	"strict":               {},
	"examples":             {},
	"default":              {},
}

// emptyObjectShellStructuralKeys are the fields that describe the lack of
// arguments. Their presence alone does not make a schema "decorated".
var emptyObjectShellStructuralKeys = map[string]struct{}{
	"type":                 {},
	"properties":           {},
	"required":             {},
	"additionalProperties": {},
}

// NormalizeEmptySchema returns a clone of EmptyObjectSchema when schema is nil,
// blank, the literal JSON null, or an empty object; it also fills the missing
// properties of a no-argument object shell. Clients frequently declare
// no-argument tools with "parameters": null or with the field omitted;
// forwarding that verbatim breaks upstreams (e.g. OpenAI-compat relays in
// front of Gemini) whose validation expects a record.
func NormalizeEmptySchema(schema json.RawMessage) json.RawMessage {
	return NormalizeFunctionParameters(schema, nil)
}

// NormalizeFunctionParameters normalizes an OpenAI-style function parameters
// schema for outbound requests. nil, blank, "null", and "{}" become a clone of
// EmptyObjectSchema — or of StrictEmptyObjectSchema when the tool is in strict
// mode, whose validator rejects a schema lacking additionalProperties: false.
// A no-argument object shell such as {"type":"object"} gains an empty
// properties map. Any other schema is returned as the original slice.
func NormalizeFunctionParameters(schema json.RawMessage, strict *bool) json.RawMessage {
	if isEmptySchema(schema) {
		return cloneEmptyObjectSchema(strict)
	}
	if normalized, ok := normalizeEmptyObjectShell(schema, strict); ok {
		return normalized
	}
	return schema
}

// cloneEmptyObjectSchema returns a fresh copy of the empty object template so
// callers cannot mutate the package-level value.
func cloneEmptyObjectSchema(strict *bool) json.RawMessage {
	if strict != nil && *strict {
		return slices.Clone(StrictEmptyObjectSchema)
	}
	return slices.Clone(EmptyObjectSchema)
}

// isEmptySchema reports whether schema is nil, blank, the literal JSON null, or
// the empty object literal.
func isEmptySchema(schema json.RawMessage) bool {
	trimmed := bytes.TrimSpace(schema)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("{}"))
}

// isNullOrBlankSchema reports whether schema is nil, blank, or literal null.
func isNullOrBlankSchema(schema json.RawMessage) bool {
	trimmed := bytes.TrimSpace(schema)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// isBlankObjectSchema reports whether schema is the empty object literal.
func isBlankObjectSchema(schema json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(schema), []byte("{}"))
}

// normalizeEmptyObjectShell detects a no-argument object schema and returns the
// normalized form. The second result reports whether schema was a shell; when
// it is false the caller must keep the original slice untouched.
//
// Detection is deliberately conservative: only objects whose every key is on
// emptyObjectShellKeys, whose type is absent or "object", whose properties are
// absent/null/empty, whose required is absent/null/empty, and whose
// additionalProperties is absent or false qualify. Dynamic dictionaries
// (additionalProperties as a schema or true) and constraint-bearing schemas
// therefore pass through byte-for-byte.
func normalizeEmptyObjectShell(schema json.RawMessage, strict *bool) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(schema)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' || !gjson.ValidBytes(trimmed) {
		return nil, false
	}

	root := gjson.ParseBytes(trimmed)
	if !isEmptyObjectShell(root) {
		return nil, false
	}

	if isCompleteEmptyObjectShell(root, strict) {
		return nil, false
	}

	if shouldDecorateEmptyObjectShell(root) {
		return decorateEmptyObjectShell(trimmed, strict)
	}

	return cloneEmptyObjectSchema(strict), true
}

// isCompleteEmptyObjectShell reports whether root already satisfies the empty
// object schema contract (including strict-mode fields when requested). Keeping
// the original slice preserves key order, whitespace, and upstream prompt caches.
func isCompleteEmptyObjectShell(root gjson.Result, strict *bool) bool {
	typeField := root.Get("type")
	props := root.Get("properties")
	required := root.Get("required")

	explicitObjectType := typeField.Exists() && typeField.Type == gjson.String && typeField.String() == "object"
	propsEmptyObject := props.Exists() && props.IsObject() && len(props.Map()) == 0
	requiredIsNull := required.Exists() && required.Type == gjson.Null
	if !explicitObjectType || !propsEmptyObject || requiredIsNull {
		return false
	}

	if strict == nil || !*strict {
		return true
	}

	ap := root.Get("additionalProperties")
	return ap.Exists() && ap.Type == gjson.False && required.Exists() && required.IsArray() && len(required.Array()) == 0
}

// shouldDecorateEmptyObjectShell reports whether root carries metadata,
// additionalProperties: false, or a stale required: null that must be patched
// in place rather than collapsed to the bare empty template.
func shouldDecorateEmptyObjectShell(root gjson.Result) bool {
	for key := range root.Map() {
		if _, structural := emptyObjectShellStructuralKeys[key]; !structural {
			return true
		}
	}

	ap := root.Get("additionalProperties")
	required := root.Get("required")
	return (ap.Exists() && ap.Type == gjson.False) || (required.Exists() && required.Type == gjson.Null)
}

// isEmptyObjectShell reports whether root is a no-argument object schema: an
// object whose type is absent or "object", whose properties/required are
// absent/null/empty, whose additionalProperties is absent or false, and whose
// every key is on the metadata whitelist.
func isEmptyObjectShell(root gjson.Result) bool {
	if !root.IsObject() {
		return false
	}
	if !isEmptyObjectShellChild(root.Get("type"), "object") {
		return false
	}

	if props := root.Get("properties"); props.Exists() && props.Type != gjson.Null && (!props.IsObject() || len(props.Map()) != 0) {
		return false
	}

	if required := root.Get("required"); required.Exists() && required.Type != gjson.Null && (!required.IsArray() || len(required.Array()) != 0) {
		return false
	}

	if ap := root.Get("additionalProperties"); ap.Exists() && ap.Type != gjson.False {
		return false
	}

	for key := range root.Map() {
		if _, ok := emptyObjectShellKeys[key]; !ok {
			return false
		}
	}

	return true
}

// isEmptyObjectShellChild reports whether an optional "type" field is absent or
// the expected string value. A present non-string or different type is a real
// constraint.
func isEmptyObjectShellChild(typ gjson.Result, want string) bool {
	if !typ.Exists() {
		return true
	}
	return typ.Type == gjson.String && typ.String() == want
}

// decorateEmptyObjectShell fills in the structural fields of a decorated
// no-argument shell, preserving its description/title metadata and any
// additionalProperties: false constraint. It deletes a stale required: null.
func decorateEmptyObjectShell(schema json.RawMessage, strict *bool) (json.RawMessage, bool) {
	out, err := sjson.SetRawBytes(slices.Clone(schema), "type", []byte(`"object"`))
	if err != nil {
		return nil, false
	}
	out, err = sjson.SetRawBytes(out, "properties", []byte(`{}`))
	if err != nil {
		return nil, false
	}
	if strict != nil && *strict {
		out, err = sjson.SetRawBytes(out, "required", []byte(`[]`))
		if err != nil {
			return nil, false
		}
		out, err = sjson.SetBytes(out, "additionalProperties", false)
		if err != nil {
			return nil, false
		}
	} else if required := gjson.GetBytes(out, "required"); required.Exists() && required.Type == gjson.Null {
		out, err = sjson.DeleteBytes(out, "required")
		if err != nil {
			return nil, false
		}
	}
	return out, true
}

// IsEmptyObjectSchema reports whether schema is a no-argument object shell:
// nil, blank, the literal null, the empty object, or an object with no
// properties. Providers such as Gemini reject such a schema as a response
// schema, so callers use this to fall back or drop it.
func IsEmptyObjectSchema(schema json.RawMessage) bool {
	if isEmptySchema(schema) {
		return true
	}
	trimmed := bytes.TrimSpace(schema)
	if !gjson.ValidBytes(trimmed) {
		return false
	}
	return isEmptyObjectShell(gjson.ParseBytes(trimmed))
}

// Tool represents a function tool.
type Tool struct {
	// Type is the type of the tool.
	// Any of "function", "image_generation", "web_search", or "google" (for Google-specific tools).
	Type string `json:"type"`

	// Function is the function definition, will be used when Type is "function".
	Function Function `json:"function"`

	// ImageGeneration is the image generation definition, will be used when Type is "image_generation".
	ImageGeneration *ImageGeneration `json:"image_generation,omitempty"`

	// WebSearch is the web search definition, will be used when Type is "web_search".
	WebSearch *WebSearch `json:"web_search,omitempty"`

	// Google contains Google/Gemini-specific grounding tools.
	// This namespace isolates Google's tools from other providers.
	Google *GoogleTools `json:"google,omitempty"`

	// ResponseCustomTool is the custom tool definition for OpenAI Responses API.
	// Will be used when Type is "custom".
	ResponseCustomTool *ResponseCustomTool `json:"response_custom_tool,omitempty"`

	// CacheControl is used for provider-specific cache control (e.g., Anthropic).
	// This field is not serialized in JSON.
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// Function represents a function definition.
type Function struct {
	// Namespace preserves the original namespace for transformers that need it,
	// such as OpenAI Responses. Flat-name transformers use Name and ignore it.
	Namespace string `json:"namespace,omitempty"`
	// Name is the unified flat name: Namespace + "__" + the original local name
	// when Namespace is nonempty, otherwise the unchanged original name.
	// Namespace-aware transformers remove that exact prefix once, never infer
	// Namespace by splitting Name. A local name may itself contain the prefix.
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	// ParametersJsonSchema is the newer Gemini format that supports full JSON Schema Draft 2020-12
	// including const, enum, and other advanced features. This field is mutually exclusive with Parameters.
	ParametersJsonSchema json.RawMessage `json:"parametersJsonSchema,omitempty"`
	Strict               *bool           `json:"strict,omitempty"`
	// AllowedCallers restricts which callers may invoke this function tool,
	// e.g. ["direct"] or ["code_execution_20250825"]. Anthropic-only today;
	// other providers have no equivalent and keep dropping it.
	AllowedCallers []string `json:"allowed_callers,omitempty"`
}

// EffectiveParameters returns the function's parameters schema from whichever
// representation actually carries it. Gemini's newer inbound format populates
// ParametersJsonSchema while the classic format uses Parameters, so a
// transformer converting Gemini to another protocol must consult both.
//
// An empty "{}" ParametersJsonSchema yields to a usable Parameters schema. When
// neither field holds a value, a non-nil ParametersJsonSchema is returned even
// if it is the literal "null", so callers can still tell an explicit null from
// an omitted field.
func (f Function) EffectiveParameters() json.RawMessage {
	if !isNullOrBlankSchema(f.ParametersJsonSchema) {
		if isBlankObjectSchema(f.ParametersJsonSchema) && !isNullOrBlankSchema(f.Parameters) && !isBlankObjectSchema(f.Parameters) {
			return f.Parameters
		}
		return f.ParametersJsonSchema
	}
	if !isNullOrBlankSchema(f.Parameters) {
		return f.Parameters
	}
	if f.ParametersJsonSchema != nil {
		return f.ParametersJsonSchema
	}
	return f.Parameters
}

// FunctionCall represents a function call (deprecated).
type FunctionCall struct {
	// Name follows Function.Name's flat-name convention.
	Name string `json:"name"`

	// Namespace is used only by namespace-aware transformers such as Responses.
	// Flat-protocol responses may omit it; Responses can recover it from the
	// originating request's declarations and historical calls.
	Namespace string `json:"namespace,omitempty"`

	// The arguments to call the function with, as generated by the model in JSON
	// format. Note that the model does not always generate valid JSON, and may
	// hallucinate parameters not defined by your function schema. Validate the
	// arguments in your code before calling your function.
	Arguments string `json:"arguments"`
}

// ToolCall represents a tool call in the response.
type ToolCall struct {
	ID string `json:"id,omitempty"`

	// The type of the tool. Currently, only `function` is supported.
	Type string `json:"type,omitempty"`

	Function FunctionCall `json:"function"`

	// ResponseCustomToolCall holds the custom tool call data for OpenAI Responses API.
	// Will be used when Type is "custom".
	ResponseCustomToolCall *ResponseCustomToolCall `json:"response_custom_tool_call,omitempty"`

	// Index is the index of the tool call in the list of tool calls.
	// Cannot use omitempty, as an index of 0 would be omitted, which can break consumers.
	Index int `json:"index"`

	// CacheControl is used for provider-specific cache control (e.g., Anthropic).
	CacheControl *CacheControl `json:"cache_control,omitempty"`

	// TransformerMetadata is used for provider-specific metadata (e.g., Gemini).
	TransformerMetadata map[string]any `json:"transformer_metadata,omitempty"`
}

// ToolFunction references a function by name.
type ToolFunction struct {
	Name string `json:"name"`
}

// ToolOption represents a specific tool within a multi-tool choice.
type ToolOption struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// ToolChoice represents the tool choice parameter for function calling.
//
// Tool choice can be a string or a struct.
type ToolChoice struct {
	ToolChoice      *string          `json:"tool_choice,omitempty"`
	NamedToolChoice *NamedToolChoice `json:"named_tool_choice,omitempty"`
	Tools           []ToolOption     `json:"tools,omitempty"`
}

type NamedToolChoice struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// allowedToolsChoiceJSON keeps the discriminator, mode, and permitted tools
// together. A mode alone would lose the restriction during request persistence.
type allowedToolsChoiceJSON struct {
	Type  string       `json:"type"`
	Mode  *string      `json:"mode,omitempty"`
	Tools []ToolOption `json:"tools,omitempty"`
}

func (t ToolChoice) MarshalJSON() ([]byte, error) {
	if t.NamedToolChoice != nil && t.NamedToolChoice.Type == "allowed_tools" {
		return json.Marshal(allowedToolsChoiceJSON{
			Type: t.NamedToolChoice.Type, Mode: t.ToolChoice, Tools: t.Tools,
		})
	}
	if t.ToolChoice != nil {
		return json.Marshal(t.ToolChoice)
	}

	if len(t.Tools) > 0 {
		return json.Marshal(t.Tools)
	}

	return json.Marshal(t.NamedToolChoice)
}

func (t *ToolChoice) UnmarshalJSON(data []byte) error {
	var str string

	err := json.Unmarshal(data, &str)
	if err == nil {
		*t = ToolChoice{ToolChoice: &str}
		return nil
	}

	var named NamedToolChoice

	err = json.Unmarshal(data, &named)
	if err == nil {
		if named.Type == "allowed_tools" {
			var allowed allowedToolsChoiceJSON
			if err := json.Unmarshal(data, &allowed); err != nil {
				return err
			}
			*t = ToolChoice{ToolChoice: allowed.Mode, NamedToolChoice: &named, Tools: allowed.Tools}
			return nil
		}
		*t = ToolChoice{NamedToolChoice: &named}
		return nil
	}

	var options []ToolOption

	err = json.Unmarshal(data, &options)
	if err == nil {
		*t = ToolChoice{Tools: options}
		return nil
	}

	return errors.New("invalid tool choice type")
}

// ImageGeneration is a permissive structure to carry image generation tool
// parameters. It mirrors the OpenRouter/OpenAI Responses API fields we care
// about, but is intentionally loose to allow forward-compatibility.
type ImageGeneration struct {
	Model string `json:"model,omitempty"`
	// One of opaque, transparent.
	Background     string         `json:"background,omitempty"`
	InputFidelity  string         `json:"input_fidelity,omitempty"`
	InputImageMask map[string]any `json:"input_image_mask,omitempty"`
	// One of low, auto.
	Moderation string `json:"moderation,omitempty"`
	// The compression level (0-100%) for the generated images. Default: 100.
	OutputCompression *int64 `json:"output_compression,omitempty"`
	// One of png, webp, or jpeg. Default: png.
	OutputFormat string `json:"output_format,omitempty"`
	// The number of images to generate. Default: 1.
	PartialImages  *int64 `json:"partial_images,omitempty"`
	N              *int64 `json:"n,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
	// The quality of the image that will be generated.
	// auto (default value) will automatically select the best quality for the given model.
	// high, medium and low are supported for gpt-image-1.
	// hd and standard are supported for dall-e-3.
	// standard is the only option for dall-e-2.
	Quality string `json:"quality,omitempty"`
	// One of 256x256, 512x512, or 1024x1024. Default: 1024x1024.
	Size  string `json:"size,omitempty"`
	Style string `json:"style,omitempty"`

	// Whether to add a watermark to the generated image. Default: false.
	// It only works for the models support watermark, it will be ignored otherwise.
	Watermark bool `json:"watermark,omitempty"`
}

// GoogleTools contains Google/Gemini-specific grounding tools.
// This namespace isolates Google's tools from other providers,
// allowing for provider-specific implementations without naming conflicts.
type GoogleTools struct {
	// Search enables Google Search grounding for real-time web searches.
	Search *GoogleSearch `json:"search,omitempty"`
	// CodeExecution enables code execution as part of generation.
	CodeExecution *GoogleCodeExecution `json:"code_execution,omitempty"`
	// UrlContext enables URL context grounding for Gemini 2.0+.
	UrlContext *GoogleUrlContext `json:"url_context,omitempty"`
}

// GoogleSearch represents Google Search grounding tool for Gemini.
// This enables the model to perform real-time web searches.
type GoogleSearch struct{}

// GoogleCodeExecution represents code execution tool for Gemini.
// This enables the model to execute code as part of generation.
type GoogleCodeExecution struct{}

// GoogleUrlContext represents URL context grounding tool for Gemini 2.0+.
// This allows the model to fetch and process content from specified URLs.
type GoogleUrlContext struct{}

// ContainsGoogleNativeTools checks if the tools slice contains any Google native tools.
// Google native tools include GoogleSearch, GoogleCodeExecution, and GoogleUrlContext.
// These tools are only supported by native Gemini API format (gemini/gemini_vertex),
// not by OpenAI-compatible endpoints (gemini_openai).
func ContainsGoogleNativeTools(tools []Tool) bool {
	return slices.ContainsFunc(tools, IsGoogleNativeTool)
}

// IsGoogleNativeTool checks if a single tool is a Google native tool.
// Google native tools follow the naming convention "google_*".
func IsGoogleNativeTool(tool Tool) bool {
	return strings.HasPrefix(tool.Type, "google_")
}

// FilterGoogleNativeTools removes Google native tools from the tools slice.
// This is useful as a fallback when routing to channels that don't support native tools.
func FilterGoogleNativeTools(tools []Tool) []Tool {
	if len(tools) == 0 {
		return tools
	}

	filtered := make([]Tool, 0, len(tools))

	for _, tool := range tools {
		if !IsGoogleNativeTool(tool) {
			filtered = append(filtered, tool)
		}
	}

	return filtered
}

type WebSearch struct {
	MaxUses        *int64                    `json:"max_uses,omitempty"`
	Strict         *bool                     `json:"strict,omitempty"`
	AllowedDomains []string                  `json:"allowed_domains,omitzero"`
	BlockedDomains []string                  `json:"blocked_domains,omitzero"`
	UserLocation   WebSearchToolUserLocation `json:"user_location,omitzero"`
}

type WebSearchToolUserLocation struct {
	// The city of the user.
	City string `json:"city,omitempty"`
	// The two letter
	// [ISO country code](https://en.wikipedia.org/wiki/ISO_3166-1_alpha-2) of the
	// user.
	Country string `json:"country,omitempty"`
	// The region of the user.
	Region string `json:"region,omitempty"`
	// The [IANA timezone](https://nodatime.org/TimeZones) of the user.
	Timezone string `json:"timezone,omitempty"`
	// This field can be elided, and will marshal its zero value as "approximate".
	Type string `json:"type"`
}

// ResponseCustomTool represents a custom tool definition for the OpenAI Responses API.
// Custom tools use freeform input (not JSON arguments) and a grammar-based format definition.
// This is a Responses API-specific tool type.
type ResponseCustomTool struct {
	// Name is the name of the custom tool.
	Name string `json:"name"`
	// Description is the description of the custom tool.
	Description string `json:"description,omitempty"`
	// Format defines the grammar-based format for the custom tool's input.
	Format *ResponseCustomToolFormat `json:"format,omitempty"`
}

// ResponseCustomToolFormat represents the format definition for a custom tool.
type ResponseCustomToolFormat struct {
	// Type is the format type, e.g. "grammar".
	Type string `json:"type"`
	// Syntax is the grammar syntax, e.g. "lark".
	Syntax string `json:"syntax,omitempty"`
	// Definition is the grammar definition string.
	Definition string `json:"definition,omitempty"`
}

// ResponseCustomToolCall represents a custom tool call from the OpenAI Responses API.
// Unlike function calls which use JSON arguments, custom tool calls use freeform input text.
type ResponseCustomToolCall struct {
	// CallID is the identifier used to map this custom tool call to a tool call output.
	CallID string `json:"call_id"`
	// Name is the name of the custom tool being called.
	Name string `json:"name"`
	// Input is the freeform input for the custom tool call generated by the model.
	Input string `json:"input"`
}
