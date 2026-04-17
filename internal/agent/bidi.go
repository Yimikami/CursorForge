package agent

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"

	agentv1 "cursorforge/internal/protocodec/gen/agent/v1"
	aiserverv1 "cursorforge/internal/protocodec/gen/aiserver/v1"

	"google.golang.org/protobuf/proto"
)

// Result is returned by the unary handlers (BidiAppend) so the MITM bridge
// can build a goproxy http.Response. Body is the marshalled response proto;
// Status is the HTTP status the bridge should set; ContentType is filled
// for non-error replies and left empty otherwise (bridge defaults to JSON).
type Result struct {
	Status      int
	ContentType string
	Body        []byte
}

// HandleBidiAppend parses the incoming BidiAppendRequest, decodes the inner
// AgentClientMessage, extracts the user message + model bindings, and stows
// them in the session store keyed by RequestId so the matching RunSSE call
// can pick them up. Returns an empty BidiAppendResponse — Cursor accepts a
// 0-byte body for unary success on this RPC.
func HandleBidiAppend(reqBody []byte, contentType string) Result {
	bidi := &aiserverv1.BidiAppendRequest{}
	if err := decodeUnary(reqBody, contentType, bidi); err != nil {
		return errResult(http.StatusBadRequest, "decode bidi append: "+err.Error())
	}

	requestID := ""
	if bidi.RequestId != nil {
		requestID = bidi.RequestId.RequestId
	}
	if requestID == "" {
		return errResult(http.StatusBadRequest, "missing request_id")
	}

	// Cursor stuffs the AgentClientMessage proto into BidiAppendRequest.Data
	// as hex (lower-case, no separator). Empty Data means a heartbeat / ack
	// frame — we just record the request_id with no extra state.
	sess := &Session{RequestID: requestID}
	if existing := GetSession(requestID); existing != nil {
		sess = existing
	}

	if bidi.Data != "" {
		raw, err := hex.DecodeString(strings.TrimSpace(bidi.Data))
		if err != nil {
			return errResult(http.StatusBadRequest, "bidi data not hex: "+err.Error())
		}
		acm := &agentv1.AgentClientMessage{}
		if err := proto.Unmarshal(raw, acm); err != nil {
			return errResult(http.StatusBadRequest, "parse agent client message: "+err.Error())
		}
		extractIntoSession(sess, acm)
		// Route tool results back to the RunSSE goroutine waiting on them.
		// Cursor posts ExecClientMessage with the matching tool_call_id
		// (exec_id) once the IDE finishes running a tool.
		routeExecClientResult(acm)
		// Route interaction verdicts (user approved/rejected a SwitchMode
		// prompt, CreatePlan prompt, WebSearch prompt, etc.) back to the
		// RunSSE goroutine that's blocking on the user's decision.
		if ir := acm.GetInteractionResponse(); ir != nil {
			deliverInteractionResponse(ir)
		}
	}

	PutSession(sess)
	body, err := proto.Marshal(&aiserverv1.BidiAppendResponse{})
	if err != nil {
		return errResult(http.StatusInternalServerError, "marshal response: "+err.Error())
	}
	return Result{Status: http.StatusOK, ContentType: "application/proto", Body: body}
}

func errResult(status int, msg string) Result {
	body := []byte(`{"code":"invalid_argument","message":` + jsonString(msg) + `}`)
	return Result{Status: status, ContentType: "application/json", Body: body}
}

// extractIntoSession pulls the bits of AgentClientMessage we care about into
// the session: the user's text, the chosen model, and the conversation
// state (so future iterations can reconstruct full chat history).
func extractIntoSession(sess *Session, acm *agentv1.AgentClientMessage) {
	if run := acm.GetRunRequest(); run != nil {
		sess.Run = run
		sess.ModelDetails = run.GetModelDetails()
		sess.State = run.GetConversationState()
		if cid := run.GetConversationId(); cid != "" {
			sess.ConversationID = cid
		}
		if action := run.GetAction(); action != nil {
			sess.Action = action
			if uma := action.GetUserMessageAction(); uma != nil {
				if msg := uma.GetUserMessage(); msg != nil {
					if msg.GetText() != "" {
						sess.UserText = msg.GetText()
					}
					// Cursor stuffs the real mode (Plan/Ask/Debug/Agent) into
					// UserMessage.mode (field 4). ConversationState.mode stays
					// UNSPECIFIED regardless of UI selection.
					if m := msg.GetMode(); m != agentv1.AgentMode_AGENT_MODE_UNSPECIFIED {
						sess.Mode = m
					}
				}
			}
		}
	}
	if pre := acm.GetPrewarmRequest(); pre != nil && sess.ModelDetails == nil {
		sess.ModelDetails = pre.GetModelDetails()
	}
}

// ---- Connect unary codec helpers ----

// decodeUnary unmarshals a unary Connect request body into msg, transparently
// stripping the optional 5-byte envelope and gzip layer that connect-go
// emits when Content-Type is "application/connect+proto".
func decodeUnary(body []byte, contentType string, msg proto.Message) error {
	payload := body
	if isConnectEnvelope(contentType) && len(payload) >= 5 {
		flags := payload[0]
		length := int(binary.BigEndian.Uint32(payload[1:5]))
		if length >= 0 && len(payload) >= 5+length {
			payload = payload[5 : 5+length]
			if flags&flagCompressed != 0 {
				if up, err := gunzip(payload); err == nil {
					payload = up
				} else {
					return err
				}
			}
		}
	}
	return proto.Unmarshal(payload, msg)
}

func isConnectEnvelope(contentType string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(contentType)), "connect")
}

func gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return out, nil
}
