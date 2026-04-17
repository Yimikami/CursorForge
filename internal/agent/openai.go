package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// openAIRequest is the minimal subset of the chat-completions schema we use.
type openAIRequest struct {
	Model           string              `json:"model"`
	Messages        []openAIMessage     `json:"messages"`
	Stream          bool                `json:"stream"`
	Tools           []openAITool        `json:"tools,omitempty"`
	StreamOptions   *openAIStreamOpts   `json:"stream_options,omitempty"`
	MaxTokens       int                 `json:"max_tokens,omitempty"`
	ReasoningEffort string              `json:"reasoning_effort,omitempty"`
	ServiceTier     string              `json:"service_tier,omitempty"`
}

type openAIStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

// openAIMessage covers user/system/assistant and tool role messages. When
// Role == "tool", ToolCallID links the result back to the assistant turn
// that issued the call. When Role == "assistant" with tool calls, ToolCalls
// carries the function invocations the model asked for.
// openAIMessage covers user/system/assistant and tool role messages.
// Content can be a plain string OR a multipart array (for images).
// We use json.RawMessage so the marshaller emits whichever form we set.
type openAIMessage struct {
	Role       string              `json:"role"`
	Content    json.RawMessage     `json:"content,omitempty"`
	ToolCallID string              `json:"tool_call_id,omitempty"`
	Name       string              `json:"name,omitempty"`
	ToolCalls  []openAIToolCallMsg `json:"tool_calls,omitempty"`
}

func textMessage(role, text string) openAIMessage {
	raw, _ := json.Marshal(text)
	return openAIMessage{Role: role, Content: raw}
}

type openAIContentPart struct {
	Type     string              `json:"type"`
	Text     string              `json:"text,omitempty"`
	ImageURL *openAIImageURL     `json:"image_url,omitempty"`
}

type openAIImageURL struct {
	URL string `json:"url"`
}

func multipartMessage(role string, parts []openAIContentPart) openAIMessage {
	raw, _ := json.Marshal(parts)
	return openAIMessage{Role: role, Content: raw}
}

func toolResultMessage(toolCallID, name, content string) openAIMessage {
	raw, _ := json.Marshal(content)
	return openAIMessage{Role: "tool", ToolCallID: toolCallID, Name: name, Content: raw}
}

// openAIToolCallMsg is the shape assistant messages take when the model
// fired off a function call; carried in history to satisfy OpenAI's
// "assistant-with-tool-calls must appear before tool-role result" rule.
type openAIToolCallMsg struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function openAIToolCallFn `json:"function"`
}

type openAIToolCallFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// openAIStreamChunk is one delta from the chat-completion stream. The
// delta may carry either plain text content OR a partial tool_call — the
// stream interleaves these across chunks so the caller must accumulate
// arguments by index until finish_reason==tool_calls lands.
type openAIStreamChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content   string `json:"content"`
			Reasoning string `json:"reasoning_content"` // Anthropic/DeepSeek thinking
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage,omitempty"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// PendingToolCall is the accumulated tool invocation request the agent
// needs to hand off to Cursor. Built by streamOpenAI from partial tool_call
// deltas across multiple chunks.
type PendingToolCall struct {
	ID        string
	Name      string
	Arguments string // complete JSON arguments object
}

// openAIDelta is the callback shape for streaming. err non-nil terminates
// the stream; the caller should propagate it to the SSE writer.
// openAIDelta is the callback shape for streaming. content is normal text,
// reasoning is thinking/reasoning text (from reasoning_content field).
// done=true signals stream end.
type openAIDelta func(content string, reasoning string, done bool) error

// streamResult captures the raw request/response bytes so the caller can
// persist them in the working-app-style turns/NNNNNN/ directory layout.
// RequestBody is the JSON posted to the provider; SSERaw is the concatenated
// SSE text (one `data: {...}\n\n` chunk per line, Cursor-agnostic).
// ToolCalls are the fully-accumulated function invocations the model
// emitted in this stream — empty when the model answered with plain text.
// FinishReason is the last non-empty finish_reason seen ("stop",
// "tool_calls", "length", etc.) so the agent loop knows whether to expect
// another round after running the tools.
type streamResult struct {
	RequestBody  []byte
	SSERaw       []byte
	ToolCalls    []PendingToolCall
	FinishReason string
	Usage        *openAIUsage
}

// streamOpenAI sends a chat-completion request to baseURL and invokes onDelta
// for each content chunk. Compatible with OpenAI proper, OpenRouter, Groq,
// Together, and any vendor that speaks the same chat-completions wire.
//
// The messages slice is sent verbatim — caller is responsible for assembling
// system prompt + history + current user query in the right order.
func streamOpenAI(
	ctx context.Context,
	baseURL, apiKey, model string,
	messages []openAIMessage,
	tools []openAITool,
	opts AdapterOpts,
	onDelta openAIDelta,
) (*streamResult, error) {
	if baseURL == "" {
		return nil, errors.New("missing baseURL")
	}
	if apiKey == "" {
		return nil, errors.New("missing apiKey")
	}
	if model == "" {
		return nil, errors.New("missing model")
	}
	if len(messages) == 0 {
		return nil, errors.New("empty message list")
	}

	req := openAIRequest{
		Model:         model,
		Stream:        true,
		Messages:      messages,
		Tools:         tools,
		StreamOptions: &openAIStreamOpts{IncludeUsage: true},
	}
	if effort := normalizeReasoningEffort(opts.ReasoningEffort); effort != "" {
		req.ReasoningEffort = effort
	}
	if opts.ServiceTier != "" {
		req.ServiceTier = opts.ServiceTier
	}
	if opts.MaxOutputTokens > 0 {
		req.MaxTokens = opts.MaxOutputTokens
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	result := &streamResult{RequestBody: body}
	// Per-choice accumulators keyed by tool_call index. OpenAI streams a
	// tool_call across many chunks — first a chunk with the id+name+type,
	// then N chunks appending fragments of the JSON arguments string. We
	// reassemble by index and emit the final struct in result.ToolCalls.
	toolAccs := map[int]*accTC{}

	url := strings.TrimRight(baseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(httpReq)
	if err != nil {
		return result, fmt.Errorf("call upstream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return result, fmt.Errorf("upstream %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}

	var sseBuf bytes.Buffer
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		sseBuf.WriteString(line)
		sseBuf.WriteByte('\n')
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			flushToolCalls(toolAccs, result)
			result.SSERaw = sseBuf.Bytes()
			return result, onDelta("", "", true)
		}
		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			result.Usage = chunk.Usage
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" || c.Delta.Reasoning != "" {
				if err := onDelta(c.Delta.Content, c.Delta.Reasoning, false); err != nil {
					result.SSERaw = sseBuf.Bytes()
					return result, err
				}
			}
			for _, tc := range c.Delta.ToolCalls {
				acc := toolAccs[tc.Index]
				if acc == nil {
					acc = &accTC{}
					toolAccs[tc.Index] = acc
				}
				if tc.ID != "" {
					acc.ID = tc.ID
				}
				if tc.Type != "" {
					acc.Type = tc.Type
				}
				if tc.Function.Name != "" {
					acc.Name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					acc.ArgsBuf.WriteString(tc.Function.Arguments)
				}
			}
			if c.FinishReason != "" {
				result.FinishReason = c.FinishReason
				flushToolCalls(toolAccs, result)
			}
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		flushToolCalls(toolAccs, result)
		result.SSERaw = sseBuf.Bytes()
		return result, err
	}
	flushToolCalls(toolAccs, result)
	result.SSERaw = sseBuf.Bytes()
	return result, onDelta("", "", true)
}

// accTC is the per-tool_call accumulator streamOpenAI fills while parsing
// the chat-completion delta stream. Tool calls span many chunks, so we key
// them by OpenAI's stable per-choice index.
type accTC struct {
	ID      string
	Name    string
	Type    string
	ArgsBuf strings.Builder
}

// flushToolCalls moves the per-index accumulator map into result.ToolCalls
// in stable index order (OpenAI tools come back indexed 0..N-1 regardless
// of arrival sequence, so sort for determinism).

func flushToolCalls(accs map[int]*accTC, result *streamResult) {
	if len(accs) == 0 || len(result.ToolCalls) > 0 {
		return
	}
	indices := make([]int, 0, len(accs))
	for idx := range accs {
		indices = append(indices, idx)
	}
	// Tiny insertion sort — typical N is 1-3.
	for i := 1; i < len(indices); i++ {
		for j := i; j > 0 && indices[j-1] > indices[j]; j-- {
			indices[j-1], indices[j] = indices[j], indices[j-1]
		}
	}
	for _, idx := range indices {
		acc := accs[idx]
		if acc.Name == "" {
			continue
		}
		result.ToolCalls = append(result.ToolCalls, PendingToolCall{
			ID:        acc.ID,
			Name:      acc.Name,
			Arguments: acc.ArgsBuf.String(),
		})
	}
}

var validReasoningEfforts = map[string]string{
	"low":     "low",
	"medium":  "medium",
	"high":    "high",
	"xhigh":  "xhigh",
	"none":    "low",
	"extreme": "xhigh",
	"max":     "xhigh",
}

func normalizeReasoningEffort(s string) string {
	if s == "" {
		return ""
	}
	if v, ok := validReasoningEfforts[strings.ToLower(s)]; ok {
		return v
	}
	return ""
}
