package agent

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"

	agentv1 "cursorforge/internal/protocodec/gen/agent/v1"

	"google.golang.org/protobuf/proto"
)

// Connect SSE envelope flag bits — same values Cursor / connect-go use.
const (
	flagCompressed = 0x01
	flagEndStream  = 0x02
)

// writeFrame writes a single Connect SSE envelope to w. Header + body
// are serialized into a single buffer and written atomically so a
// background keepalive goroutine can share the writer without interleaving
// bytes between the header and payload.
func writeFrame(w io.Writer, body []byte, endStream bool) error {
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(body); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	flags := byte(flagCompressed)
	if endStream {
		flags |= flagEndStream
	}
	frame := make([]byte, 5+compressed.Len())
	frame[0] = flags
	binary.BigEndian.PutUint32(frame[1:5], uint32(compressed.Len()))
	copy(frame[5:], compressed.Bytes())
	if _, err := w.Write(frame); err != nil {
		return err
	}
	return nil
}

// flushIfPossible flushes the underlying http.ResponseWriter if it
// implements http.Flusher, so each SSE frame leaves the server immediately.
func flushIfPossible(w io.Writer) {
	if f, ok := w.(interface{ Flush() }); ok {
		f.Flush()
	}
}

// writeConversationCheckpoint emits a ConversationStateStructure replay so
// Cursor can sync its conversation state. Working app captures show one of
// these (a re-broadcast of the carry_forward block) sandwiched between
// every user-visible action — Cursor's IDE seems to depend on them as
// state-sync barriers; without them tool execution result frames never
// arrive on BidiAppend.
//
// The `replay` argument is the JSON byte block Cursor originally sent us as
// the user's first message (workspace context + carry-forward); we wrap it
// as the only entry of root_prompt_messages_json.
func writeConversationCheckpoint(w io.Writer, replay []byte) error {
	if len(replay) == 0 {
		return nil
	}
	state := &agentv1.ConversationStateStructure{
		RootPromptMessagesJson: [][]byte{replay},
	}
	msg := &agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_ConversationCheckpointUpdate{
			ConversationCheckpointUpdate: state,
		},
	}
	body, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	if err := writeFrame(w, body, false); err != nil {
		return err
	}
	flushIfPossible(w)
	return nil
}

// writeInteractionQuery emits an InteractionQuery frame (AgentServerMessage
// field 7). This is the channel Cursor's IDE listens to for client-side
// interactions — mode switching, web-search approval, plan creation
// prompts, etc. SwitchModeToolCall pills only render in the chat bubble;
// the model-selector UI picker only updates when it receives an
// InteractionQuery with a SwitchModeRequestQuery inside.
func writeInteractionQuery(w io.Writer, query *agentv1.InteractionQuery) error {
	if query == nil {
		return nil
	}
	msg := &agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionQuery{
			InteractionQuery: query,
		},
	}
	return writeAgentServerMessage(w, msg)
}

// writeAgentServerMessage marshals msg and writes it as a single compressed
// Connect SSE envelope. Lower-level than the specialised writers below —
// used by tool-call frame emitters in tool_exec.go.
func writeAgentServerMessage(w io.Writer, msg *agentv1.AgentServerMessage) error {
	body, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal server message: %w", err)
	}
	if err := writeFrame(w, body, false); err != nil {
		return err
	}
	flushIfPossible(w)
	return nil
}

// writeTextDeltaFrame emits one streaming text chunk in the format Cursor
// renders directly into the chat bubble:
//
//	AgentServerMessage.interaction_update (field 1)
//	  -> InteractionUpdate.text_delta     (field 1)
//	     -> TextDeltaUpdate.text          (field 1)
//	        = chunk
func writeTextDeltaFrame(w io.Writer, chunk string) error {
	msg := &agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_TextDelta{
					TextDelta: &agentv1.TextDeltaUpdate{Text: chunk},
				},
			},
		},
	}
	body, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal text delta: %w", err)
	}
	if err := writeFrame(w, body, false); err != nil {
		return err
	}
	flushIfPossible(w)
	return nil
}

// writeKeepaliveFrame emits a zero-token update. Cursor closes the SSE
// pipe after ~60-90s of silence, which interrupts long-running tool loops
// and forces a reconnect storm. Firing a 0-delta keepalive every few
// seconds keeps the write side active without changing any visible UI.
func writeKeepaliveFrame(w io.Writer) error {
	return writeTokenDeltaFrame(w, 0)
}

// writeTurnEndedFrame emits the empty TurnEndedUpdate frame Cursor expects
// just before the END-STREAM marker so the chat bubble flips out of the
// "thinking…" state. The TurnEndedUpdate proto we ship has no scalar
// fields, so the body is just the nested empty wrappers — captured working
// app responses include extra varints inside this frame, but Cursor renders
// fine with the minimal form too.
func writeTurnEndedFrame(w io.Writer) error {
	msg := &agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_TurnEnded{
					TurnEnded: &agentv1.TurnEndedUpdate{},
				},
			},
		},
	}
	body, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal turn ended: %w", err)
	}
	if err := writeFrame(w, body, false); err != nil {
		return err
	}
	flushIfPossible(w)
	return nil
}

// writeThinkingDeltaFrame emits a thinking/reasoning chunk Cursor renders
// in a collapsible "Thinking…" section above the regular chat bubble.
func writeThinkingDeltaFrame(w io.Writer, text string) error {
	msg := &agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_ThinkingDelta{
					ThinkingDelta: &agentv1.ThinkingDeltaUpdate{Text: text},
				},
			},
		},
	}
	body, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal thinking delta: %w", err)
	}
	if err := writeFrame(w, body, false); err != nil {
		return err
	}
	flushIfPossible(w)
	return nil
}

// writeThinkingCompletedFrame emits the signal that thinking is done.
func writeThinkingCompletedFrame(w io.Writer, durationMs int32) error {
	msg := &agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_ThinkingCompleted{
					ThinkingCompleted: &agentv1.ThinkingCompletedUpdate{
						ThinkingDurationMs: durationMs,
					},
				},
			},
		},
	}
	body, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal thinking completed: %w", err)
	}
	if err := writeFrame(w, body, false); err != nil {
		return err
	}
	flushIfPossible(w)
	return nil
}

// writeTokenDeltaFrame emits a token-usage update Cursor uses to populate
// its "tokens used" counter in the chat UI.
func writeTokenDeltaFrame(w io.Writer, tokens int32) error {
	msg := &agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_TokenDelta{
					TokenDelta: &agentv1.TokenDeltaUpdate{Tokens: tokens},
				},
			},
		},
	}
	body, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal token delta: %w", err)
	}
	if err := writeFrame(w, body, false); err != nil {
		return err
	}
	flushIfPossible(w)
	return nil
}

// writeEndStream sends the Connect protocol terminator: a frame with the
// end-stream flag set whose payload is the JSON object "{}". Without it
// Cursor keeps the SSE connection open expecting more frames.
func writeEndStream(w io.Writer) error {
	if err := writeFrame(w, []byte("{}"), true); err != nil {
		return err
	}
	flushIfPossible(w)
	return nil
}
