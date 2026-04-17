package agent

// Native Anthropic Messages API transport.
//
// streamOpenAI is the chat-completions transport used by every
// OpenAI-compatible provider; streamAnthropic is the sibling that speaks
// Anthropic's `/v1/messages` wire format and normalises results back into
// the shared streamResult / PendingToolCall / openAIUsage types the rest
// of the agent loop consumes.
//
// The agent loop in runsse.go is provider-agnostic: once we hand it a
// *streamResult, it doesn't care which transport produced it. So everything
// interesting here is message-shape conversion (OpenAI → Anthropic → OpenAI)
// and SSE event parsing.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// providerStreamer is the shared signature of streamOpenAI / streamAnthropic.
// The agent loop calls this after picking a streamer by adapter type; the
// OpenAI-flavoured message / tool shapes are the lingua franca between the
// agent loop and each transport, even though the Anthropic transport
// converts them on the wire.
type providerStreamer func(
	ctx context.Context,
	baseURL, apiKey, model string,
	messages []openAIMessage,
	tools []openAITool,
	opts AdapterOpts,
	onDelta openAIDelta,
) (*streamResult, error)

// Compile-time assertion that both transports satisfy the shared signature.
var (
	_ providerStreamer = streamOpenAI
	_ providerStreamer = streamAnthropic
)

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

// anthropicRequest is the minimum subset of the Messages API we send.
// `max_tokens` is REQUIRED by Anthropic — missing / 0 returns 400. We
// default to a conservative 8192 when the user didn't pick a value.
type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	Stream    bool               `json:"stream"`
	Thinking  *anthropicThinking `json:"thinking,omitempty"`
}

type anthropicThinking struct {
	Type         string `json:"type"` // always "enabled"
	BudgetTokens int    `json:"budget_tokens"`
}

type anthropicMessage struct {
	Role    string                  `json:"role"` // "user" | "assistant"
	Content []anthropicContentBlock `json:"content"`
}

// anthropicContentBlock is the union of every content block variant we
// care about on input *and* output. A single struct with tag-on-demand
// omitempty keeps marshalling simple; the Type field tells the reader
// which subset of fields is populated.
type anthropicContentBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// tool_use (assistant → tool call)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result (user → tool output)
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`

	// image
	Source *anthropicImageSource `json:"source,omitempty"`

	// thinking (round-trip summary of extended-thinking output we emitted
	// in a previous turn — not something we actually send back, but left
	// here so we can deserialise if a provider echoes it)
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"`       // "base64" | "url"
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ---------------------------------------------------------------------------
// Stream event shapes (for decoding)
// ---------------------------------------------------------------------------

type anthropicStreamEvent struct {
	Type    string                     `json:"type"`
	Index   int                        `json:"index"`
	Message *anthropicStreamMessage    `json:"message,omitempty"`
	Delta   *anthropicStreamDelta      `json:"delta,omitempty"`
	Content *anthropicContentBlockInit `json:"content_block,omitempty"`
	Usage   *anthropicUsage            `json:"usage,omitempty"`
	Error   *anthropicError            `json:"error,omitempty"`
}

type anthropicStreamMessage struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"`
	Role         string            `json:"role"`
	Model        string            `json:"model"`
	StopReason   string            `json:"stop_reason"`
	StopSequence string            `json:"stop_sequence"`
	Usage        *anthropicUsage   `json:"usage,omitempty"`
}

type anthropicStreamDelta struct {
	// content_block_delta fields
	Type        string `json:"type"` // "text_delta" | "input_json_delta" | "thinking_delta" | "signature_delta"
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`

	// message_delta fields
	StopReason   string `json:"stop_reason,omitempty"`
	StopSequence string `json:"stop_sequence,omitempty"`
}

type anthropicContentBlockInit struct {
	Type  string          `json:"type"` // "text" | "tool_use" | "thinking"
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	Text  string          `json:"text,omitempty"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type anthropicError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// ---------------------------------------------------------------------------
// Message / tool conversion
// ---------------------------------------------------------------------------

// convertMessagesToAnthropic remaps OpenAI-shaped messages onto the shape
// Anthropic accepts on `/v1/messages`. The rule list is:
//
//   - role:"system" → collected into the top-level `system` string.
//   - role:"tool"   → a `user` message with a `tool_result` content block,
//     tool_use_id mirroring tool_call_id.
//   - assistant with tool_calls → `assistant` message whose content is
//     [optional text block, tool_use blocks…].
//   - Consecutive same-role messages get coalesced (Anthropic requires
//     strict user/assistant alternation).
//   - Empty text blocks are dropped; empty assistant messages with no
//     tool_use are dropped entirely.
func convertMessagesToAnthropic(msgs []openAIMessage) (system string, out []anthropicMessage, err error) {
	var systemParts []string

	for _, m := range msgs {
		switch m.Role {
		case "system":
			if s, ok := decodeAsString(m.Content); ok && s != "" {
				systemParts = append(systemParts, s)
			}
		case "tool":
			// tool_result lives under the "user" role in Anthropic.
			content := "{}"
			if s, ok := decodeAsString(m.Content); ok {
				content = s
			}
			// tool_result.content accepts either a string or an array of
			// content blocks; the string form is easier and works everywhere.
			contentJSON, _ := json.Marshal(content)
			block := anthropicContentBlock{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   contentJSON,
			}
			out = appendCoalesced(out, anthropicMessage{
				Role:    "user",
				Content: []anthropicContentBlock{block},
			})
		case "assistant":
			var blocks []anthropicContentBlock
			if s, ok := decodeAsString(m.Content); ok && strings.TrimSpace(s) != "" {
				blocks = append(blocks, anthropicContentBlock{
					Type: "text",
					Text: s,
				})
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(tc.Function.Arguments)
				if !json.Valid(input) {
					// Anthropic requires input to be a valid JSON object.
					// Wrap unparseable OpenAI-emitted arg strings so the
					// replay doesn't trip validation.
					wrapped, _ := json.Marshal(map[string]string{"_raw": tc.Function.Arguments})
					input = wrapped
				}
				blocks = append(blocks, anthropicContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: input,
				})
			}
			if len(blocks) == 0 {
				// Skip assistant messages that would be entirely empty after
				// conversion — Anthropic rejects empty content arrays.
				continue
			}
			out = appendCoalesced(out, anthropicMessage{
				Role:    "assistant",
				Content: blocks,
			})
		case "user":
			blocks := openAIContentToAnthropic(m.Content)
			if len(blocks) == 0 {
				continue
			}
			out = appendCoalesced(out, anthropicMessage{
				Role:    "user",
				Content: blocks,
			})
		default:
			// Unknown roles — skip rather than error.
			continue
		}
	}

	system = strings.Join(systemParts, "\n\n")
	return system, out, nil
}

// openAIContentToAnthropic turns the OpenAI `content` payload (a JSON string
// OR an array of {type,text,image_url}) into one or more Anthropic content
// blocks.
func openAIContentToAnthropic(raw json.RawMessage) []anthropicContentBlock {
	if len(raw) == 0 {
		return nil
	}
	// Try as plain string first — the common case.
	if s, ok := decodeAsString(raw); ok {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return []anthropicContentBlock{{Type: "text", Text: s}}
	}
	// Array form.
	var parts []openAIContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		// Last-resort fallback: stringify the raw JSON.
		return []anthropicContentBlock{{Type: "text", Text: string(raw)}}
	}
	out := make([]anthropicContentBlock, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text":
			if strings.TrimSpace(p.Text) == "" {
				continue
			}
			out = append(out, anthropicContentBlock{Type: "text", Text: p.Text})
		case "image_url":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				continue
			}
			block := anthropicContentBlock{Type: "image"}
			if strings.HasPrefix(p.ImageURL.URL, "data:") {
				// data:<mime>;base64,<data> → source.type=base64
				media, data, okData := parseDataURL(p.ImageURL.URL)
				if okData {
					block.Source = &anthropicImageSource{
						Type: "base64", MediaType: media, Data: data,
					}
				} else {
					continue
				}
			} else {
				block.Source = &anthropicImageSource{Type: "url", URL: p.ImageURL.URL}
			}
			out = append(out, block)
		}
	}
	return out
}

// parseDataURL splits "data:image/png;base64,AAA…" into ("image/png", "AAA…").
// Returns ok=false for non-base64 or malformed data URLs.
func parseDataURL(s string) (mediaType, data string, ok bool) {
	if !strings.HasPrefix(s, "data:") {
		return "", "", false
	}
	rest := s[len("data:"):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", "", false
	}
	meta := rest[:comma]
	body := rest[comma+1:]
	if !strings.Contains(meta, "base64") {
		return "", "", false
	}
	// Strip the ";base64" tag to isolate the media type.
	mediaType = strings.Split(meta, ";")[0]
	return mediaType, body, true
}

// decodeAsString returns the JSON-decoded string form of raw when raw is
// a JSON string literal. Otherwise returns ok=false so the caller can fall
// back to the array path.
func decodeAsString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", true // empty content is valid — return empty string
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// appendCoalesced appends msg to out, merging its content blocks into the
// previous message when both share the same role. Anthropic requires strict
// user/assistant alternation, so two consecutive user messages (common after
// sequential tool_result blocks) must be folded into a single user message
// with all the tool_results as siblings.
func appendCoalesced(out []anthropicMessage, msg anthropicMessage) []anthropicMessage {
	if n := len(out); n > 0 && out[n-1].Role == msg.Role {
		out[n-1].Content = append(out[n-1].Content, msg.Content...)
		return out
	}
	return append(out, msg)
}

// convertToolsToAnthropic remaps OpenAI tool definitions onto Anthropic's
// flatter schema. The JSON-Schema body under `parameters` is reused verbatim
// as `input_schema` — normalizeMcpSchema (tools.go) already guarantees every
// schema has a top-level `type: object` with a `properties` key, which is
// exactly what Anthropic requires too.
func convertToolsToAnthropic(in []openAITool) []anthropicTool {
	if len(in) == 0 {
		return nil
	}
	out := make([]anthropicTool, 0, len(in))
	for _, t := range in {
		var fn struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		}
		if err := json.Unmarshal(t.Function, &fn); err != nil || fn.Name == "" {
			continue
		}
		// Anthropic requires a non-empty input_schema object; fall back to
		// `{"type":"object","properties":{}}` when the OpenAI-side schema
		// was absent or failed to normalise.
		if len(fn.Parameters) == 0 {
			fn.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, anthropicTool{
			Name:        fn.Name,
			Description: fn.Description,
			InputSchema: fn.Parameters,
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// Streaming
// ---------------------------------------------------------------------------

// streamAnthropic posts to /v1/messages and invokes onDelta for each text /
// thinking fragment, returning a *streamResult with tool-call accumulators
// flushed on message_stop. The external contract matches streamOpenAI so
// runsse.go can dispatch between the two without threading providers through.
func streamAnthropic(
	ctx context.Context,
	baseURL, apiKey, model string,
	messages []openAIMessage,
	tools []openAITool,
	opts AdapterOpts,
	onDelta openAIDelta,
) (*streamResult, error) {
	if apiKey == "" {
		return nil, errors.New("missing apiKey")
	}
	if model == "" {
		return nil, errors.New("missing model")
	}
	if len(messages) == 0 {
		return nil, errors.New("empty message list")
	}

	// Empty baseURL defaults to Anthropic's public endpoint. Strip any
	// trailing slash; the path segment is always /v1/messages.
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	baseURL = strings.TrimRight(baseURL, "/")

	system, convMsgs, err := convertMessagesToAnthropic(messages)
	if err != nil {
		return nil, fmt.Errorf("convert messages: %w", err)
	}

	maxTokens := opts.MaxOutputTokens
	if maxTokens <= 0 {
		// Anthropic requires max_tokens on every request. Pick something
		// comfortably above typical agent-turn output sizes; models will
		// still stop earlier on natural end_turn.
		maxTokens = 8192
	}

	req := anthropicRequest{
		Model:     model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  convMsgs,
		Tools:     convertToolsToAnthropic(tools),
		Stream:    true,
	}
	if opts.ThinkingBudget > 0 {
		req.Thinking = &anthropicThinking{
			Type:         "enabled",
			BudgetTokens: opts.ThinkingBudget,
		}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	result := &streamResult{RequestBody: body}

	url := baseURL + "/v1/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	// Models with extended-thinking + tool_use need the interleaved-thinking
	// beta flag; cheap to send unconditionally when Thinking is enabled.
	if req.Thinking != nil {
		httpReq.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(httpReq)
	if err != nil {
		return result, fmt.Errorf("call upstream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return result, fmt.Errorf("upstream %d: %s", resp.StatusCode, anthropicErrorMessage(buf))
	}

	// Per-index content block accumulators. Text / thinking blocks stream
	// their deltas straight through onDelta; tool_use blocks buffer the
	// partial_json until the block closes.
	type contentAcc struct {
		kind string // "text" | "tool_use" | "thinking"
		id   string
		name string
		args strings.Builder
	}
	blocks := map[int]*contentAcc{}

	var sseBuf bytes.Buffer
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		// Mirror SSE bytes verbatim so turn artifacts can be replayed /
		// inspected later.
		sseBuf.WriteString(line)
		sseBuf.WriteByte('\n')
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || !strings.HasPrefix(trimmed, "data:") {
			// event: lines and blank separators — the data: payload is all
			// we need since the event type is repeated in the JSON body.
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "[DONE]" {
			break
		}
		var ev anthropicStreamEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil && ev.Message.Usage != nil {
				if result.Usage == nil {
					result.Usage = &openAIUsage{}
				}
				result.Usage.PromptTokens = ev.Message.Usage.InputTokens
				// message_start's output_tokens is usually 0 or 1; the
				// final count arrives on message_delta.
			}
		case "content_block_start":
			acc := &contentAcc{}
			if ev.Content != nil {
				acc.kind = ev.Content.Type
				acc.id = ev.Content.ID
				acc.name = ev.Content.Name
				// Intentionally do NOT seed acc.args from ev.Content.Input:
				// Anthropic always emits `{}` as a placeholder on tool_use
				// content_block_start events and delivers the real arguments
				// through subsequent input_json_delta frames. Seeding with
				// `{}` would produce `{}{"real":"args"}` once appended.
			}
			blocks[ev.Index] = acc
		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					if err := onDelta(ev.Delta.Text, "", false); err != nil {
						result.SSERaw = sseBuf.Bytes()
						return result, err
					}
				}
			case "thinking_delta":
				if ev.Delta.Thinking != "" {
					if err := onDelta("", ev.Delta.Thinking, false); err != nil {
						result.SSERaw = sseBuf.Bytes()
						return result, err
					}
				}
			case "input_json_delta":
				if acc := blocks[ev.Index]; acc != nil {
					acc.args.WriteString(ev.Delta.PartialJSON)
				}
			case "signature_delta":
				// Extended-thinking signature for the preceding thinking
				// block. We don't surface it to the user; record-only.
			}
		case "content_block_stop":
			// Nothing to do; accumulators stay alive until flushToolCalls.
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				result.FinishReason = mapAnthropicStopReason(ev.Delta.StopReason)
			}
			if ev.Usage != nil {
				if result.Usage == nil {
					result.Usage = &openAIUsage{}
				}
				result.Usage.CompletionTokens = ev.Usage.OutputTokens
			}
		case "message_stop":
			// fall through to flush after the loop
		case "error":
			msg := "anthropic stream error"
			if ev.Error != nil && ev.Error.Message != "" {
				msg = ev.Error.Message
			}
			result.SSERaw = sseBuf.Bytes()
			return result, errors.New(msg)
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		result.SSERaw = sseBuf.Bytes()
		return result, err
	}

	// Flush tool_use accumulators into the shared PendingToolCall format so
	// the agent loop can dispatch them the same way it handles OpenAI
	// tool_calls. We preserve the Anthropic `toolu_…` id verbatim — it's
	// opaque to every downstream consumer and keeping it stable is what
	// lets the next turn replay `tool_result.tool_use_id = <same id>`.
	indices := make([]int, 0, len(blocks))
	for i := range blocks {
		indices = append(indices, i)
	}
	for i := 1; i < len(indices); i++ {
		for j := i; j > 0 && indices[j-1] > indices[j]; j-- {
			indices[j-1], indices[j] = indices[j], indices[j-1]
		}
	}
	for _, idx := range indices {
		acc := blocks[idx]
		if acc == nil || acc.kind != "tool_use" || acc.name == "" {
			continue
		}
		args := strings.TrimSpace(acc.args.String())
		if args == "" {
			args = "{}"
		}
		result.ToolCalls = append(result.ToolCalls, PendingToolCall{
			ID:        acc.id,
			Name:      acc.name,
			Arguments: args,
		})
	}

	// Anthropic occasionally skips message_delta.stop_reason on very short
	// replies; synthesise a sensible finish_reason so the agent loop can
	// decide whether to keep running.
	if result.FinishReason == "" {
		if len(result.ToolCalls) > 0 {
			result.FinishReason = "tool_calls"
		} else {
			result.FinishReason = "stop"
		}
	}

	result.SSERaw = sseBuf.Bytes()
	return result, onDelta("", "", true)
}

// mapAnthropicStopReason normalises Anthropic's stop_reason vocabulary onto
// OpenAI's finish_reason values the rest of the agent loop branches on.
func mapAnthropicStopReason(s string) string {
	switch s {
	case "end_turn", "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return s
	}
}

// anthropicErrorMessage extracts `error.message` from Anthropic's JSON error
// envelope, falling back to the trimmed raw body when the shape is off.
func anthropicErrorMessage(body []byte) string {
	var env struct {
		Error *anthropicError `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error != nil && env.Error.Message != "" {
		if env.Error.Type != "" {
			return env.Error.Type + ": " + env.Error.Message
		}
		return env.Error.Message
	}
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 512 {
		snippet = snippet[:512] + "…"
	}
	if snippet == "" {
		snippet = "(empty body, status " + strconv.Itoa(0) + ")"
	}
	return snippet
}
