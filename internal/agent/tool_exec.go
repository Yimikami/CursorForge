package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	agentv1 "cursorforge/internal/protocodec/gen/agent/v1"
	aiserverv1 "cursorforge/internal/protocodec/gen/aiserver/v1"

	"google.golang.org/protobuf/types/known/structpb"
)

// execToolName extracts the lowercase tool slug from an OpenAI tool name
// ("Shell" -> "shell") for exec_id composition.
func execToolName(openAIName string) string {
	return strings.ToLower(openAIName)
}

// pendingResult is the channel a RunSSE goroutine blocks on while waiting
// for Cursor's IDE to execute a tool and BidiAppend the result back. We
// key by tool_call_id so concurrent tool calls within the same turn each
// get their own waiter.
type pendingResult struct {
	ch chan *toolResultEnvelope
}

// toolResultEnvelope bundles what BidiAppend hands us when Cursor finishes
// running a tool. ResultJSON is what we'll feed OpenAI as the "tool" role
// message content; ExecClient is the raw proto so we can attach the
// matching Result field onto the ToolCall for pill rendering.
// Error is set instead on failure.
type toolResultEnvelope struct {
	ResultJSON string
	ExecClient *agentv1.ExecClientMessage
	ShellAccum *shellAccumState // populated for Shell so UI render has the pieces
	Error      string
}

var (
	pendingMu sync.Mutex
	pending   = map[string]*pendingResult{}
	// execIDAlias maps Cursor's generated exec_id ("exec-shell-<nanos>")
	// back to the OpenAI tool_call_id we registered the waiter under.
	// Cursor's BidiAppend reply carries the exec_id, not the tool_call_id.
	execIDAlias = map[string]string{}
	// seqAlias maps ExecServerMessage.Id (numeric sequence we emit) to the
	// OpenAI tool_call_id. Cursor correlates by this numeric id on every
	// BidiAppend frame in its streaming reply.
	seqAlias = map[uint32]string{}
	// shellAccum tracks per-seq stdout/stderr/exit as ShellStream events
	// arrive across many BidiAppend frames. Finalised on Exit.
	shellAccum = map[uint32]*shellAccumState{}
	// pendingInteraction holds channels for in-flight InteractionQuery
	// responses, keyed by the InteractionQuery.id we assigned when emitting
	// the frame. Cursor's BidiAppend delivers AgentClientMessage_InteractionResponse
	// once the user accepts/rejects the UI prompt (e.g. the "Switch to X?"
	// dialog), and we forward that verdict back to the model.
	pendingInteraction = map[uint32]chan *agentv1.InteractionResponse{}
)

func registerInteractionWait(id uint32) chan *agentv1.InteractionResponse {
	ch := make(chan *agentv1.InteractionResponse, 1)
	pendingMu.Lock()
	pendingInteraction[id] = ch
	pendingMu.Unlock()
	return ch
}

func deliverInteractionResponse(resp *agentv1.InteractionResponse) {
	if resp == nil {
		return
	}
	id := resp.GetId()
	pendingMu.Lock()
	ch := pendingInteraction[id]
	delete(pendingInteraction, id)
	pendingMu.Unlock()
	if ch != nil {
		select {
		case ch <- resp:
		default:
		}
	}
}

// waitForInteractionResponse blocks until Cursor's IDE delivers the user's
// verdict on the InteractionQuery we emitted (approve / reject), or the
// timeout elapses. Returns nil on timeout.
func waitForInteractionResponse(ch <-chan *agentv1.InteractionResponse, timeout time.Duration) *agentv1.InteractionResponse {
	select {
	case r := <-ch:
		return r
	case <-time.After(timeout):
		return nil
	}
}

type shellAccumState struct {
	Stdout   []byte
	Stderr   []byte
	Started  bool
	Exited   bool
	ExitCode uint32
	Cwd      string
}

// registerExecIDAlias remembers that a future BidiAppend with execID
// (string) OR seq (numeric ExecServerMessage.Id we emitted) should be
// routed to the waiter registered against toolCallID.
func registerExecIDAlias(execID string, seq uint32, toolCallID string) {
	pendingMu.Lock()
	execIDAlias[execID] = toolCallID
	seqAlias[seq] = toolCallID
	pendingMu.Unlock()
}

func registerToolWait(toolCallID string) chan *toolResultEnvelope {
	ch := make(chan *toolResultEnvelope, 1)
	pendingMu.Lock()
	pending[toolCallID] = &pendingResult{ch: ch}
	pendingMu.Unlock()
	return ch
}

func deliverToolResult(id string, env *toolResultEnvelope) {
	pendingMu.Lock()
	// Accept either the OpenAI tool_call_id or Cursor's exec_id alias.
	pr := pending[id]
	if pr == nil {
		if callID, ok := execIDAlias[id]; ok {
			pr = pending[callID]
			delete(pending, callID)
			delete(execIDAlias, id)
		}
	} else {
		delete(pending, id)
	}
	pendingMu.Unlock()
	if pr != nil {
		select {
		case pr.ch <- env:
		default:
		}
	}
}

// handleLocalTool resolves tool calls that don't need to round-trip through
// Cursor's IDE — e.g. SwitchMode, CreatePlan — by mutating session state and
// feeding the result straight back to the model.
//
// pillProto (when non-nil) becomes a ToolCallStarted/ToolCallCompleted pair
// so the call shows up in the chat bubble.
//
// interaction (when non-nil) is fired as an AgentServerMessage.interaction_query
// — this is the ONLY channel that moves Cursor's UI affordances like the
// mode-selector picker. SwitchMode needs this; the pill alone doesn't flip
// the selector.
func handleLocalTool(sess *Session, pc PendingToolCall) (result string, pillProto *agentv1.ToolCall, interaction *agentv1.InteractionQuery, handled bool) {
	switch pc.Name {
	case "SwitchMode":
		var a struct {
			TargetModeId string `json:"target_mode_id"`
			Explanation  string `json:"explanation"`
		}
		if err := json.Unmarshal([]byte(pc.Arguments), &a); err != nil {
			return `{"error":"parse SwitchMode args: ` + jsonStringEscapeRaw(err.Error()) + `"}`, nil, nil, true
		}
		target := agentv1.AgentMode_AGENT_MODE_UNSPECIFIED
		switch strings.ToLower(strings.TrimSpace(a.TargetModeId)) {
		case "agent", "agent_mode_agent":
			target = agentv1.AgentMode_AGENT_MODE_AGENT
		case "ask", "agent_mode_ask":
			target = agentv1.AgentMode_AGENT_MODE_ASK
		case "plan", "agent_mode_plan":
			target = agentv1.AgentMode_AGENT_MODE_PLAN
		case "debug", "agent_mode_debug":
			target = agentv1.AgentMode_AGENT_MODE_DEBUG
		default:
			return `{"error":"unknown target_mode_id (expected: agent, ask, plan, debug)"}`, nil, nil, true
		}
		if sess != nil {
			sess.Mode = target
		}
		// Cursor's UI picker keys options by an internal id that doesn't
		// always match the enum name — confirmed by grepping Cursor's
		// workbench.desktop.main.js for picker definitions:
		//   { id:"agent", name:"Agent" }
		//   { id:"plan",  name:"Plan"  }
		//   { id:"debug", name:"Debug" }
		//   { id:"chat",  name:"Ask"   }  ← the "Ask" label ships as "chat"
		// Send the id, not the enum-derived shortLabel, so every mode
		// flips the picker correctly.
		pickerID := cursorPickerID(target)
		switchArgs := &agentv1.SwitchModeArgs{
			TargetModeId: pickerID,
			ToolCallId:   pc.ID,
		}
		if a.Explanation != "" {
			expl := a.Explanation
			switchArgs.Explanation = &expl
		}
		pill := &agentv1.ToolCall{
			Tool: &agentv1.ToolCall_SwitchModeToolCall{
				SwitchModeToolCall: &agentv1.SwitchModeToolCall{
					Args: switchArgs,
				},
			},
		}
		// The pill alone doesn't move Cursor's mode selector — only an
		// InteractionQuery with SwitchModeRequestQuery does. Fire it so the
		// picker flips and the IDE treats this like a user-approved switch.
		iq := &agentv1.InteractionQuery{
			Id: interactionQueryID(pc.ID),
			Query: &agentv1.InteractionQuery_SwitchModeRequestQuery{
				SwitchModeRequestQuery: &agentv1.SwitchModeRequestQuery{
					Args: switchArgs,
				},
			},
		}
		return fmt.Sprintf(`{"result":"ok","mode":%q,"note":"Mode switched. Continue the task in the new mode without re-asking the user."}`, a.TargetModeId), pill, iq, true
	case "CreatePlan":
		var a struct {
			Name     string   `json:"name"`
			Overview string   `json:"overview"`
			Todos    []string `json:"todos"`
		}
		if err := json.Unmarshal([]byte(pc.Arguments), &a); err != nil {
			return `{"error":"parse CreatePlan args: ` + jsonStringEscapeRaw(err.Error()) + `"}`, nil, nil, true
		}
		if sess == nil {
			return `{"error":"no session"}`, nil, nil, true
		}
		// Build the authoritative todo list. Store goes into the conversation-
		// scoped map (so next turn's session sees the same plan); session's
		// own copy is for the current turn's <active_plan> projection only.
		todos := make([]*TodoEntry, 0, len(a.Todos))
		protoTodos := make([]*agentv1.TodoItem, 0, len(a.Todos))
		for i, t := range a.Todos {
			content := strings.TrimSpace(t)
			id := fmt.Sprintf("t%d", i+1)
			todos = append(todos, &TodoEntry{ID: id, Content: content, Status: "pending"})
			protoTodos = append(protoTodos, &agentv1.TodoItem{
				Id:      id,
				Content: content,
				Status:  agentv1.TodoStatus_TODO_STATUS_PENDING,
			})
		}
		SavePlanState(sess.ConversationID, a.Name, a.Overview, todos)
		sess.PlanName = a.Name
		sess.PlanOverview = a.Overview
		sess.Todos = todos
		planArgs := &agentv1.CreatePlanArgs{
			Name:     a.Name,
			Overview: a.Overview,
			Todos:    protoTodos,
		}
		pill := &agentv1.ToolCall{
			Tool: &agentv1.ToolCall_CreatePlanToolCall{
				CreatePlanToolCall: &agentv1.CreatePlanToolCall{
					Args: planArgs,
					Result: &agentv1.CreatePlanResult{
						Result: &agentv1.CreatePlanResult_Success{
							Success: &agentv1.CreatePlanSuccess{},
						},
					},
				},
			},
		}
		// Gate the InteractionQuery per-conversation: Cursor's native Plan
		// panel creates a fresh .plan.md file for every CreatePlanRequestQuery
		// it receives, so letting the model fire CreatePlan repeatedly within
		// one chat would spawn a dozen plan files and flood <open_files> on
		// the next turn. Keying the gate off conversation_id (not session
		// state) keeps it clean across reconnect-clones and continuation
		// rounds while still giving each new chat a fresh panel.
		var iq *agentv1.InteractionQuery
		if !PlanEmittedFor(sess.ConversationID) {
			iq = &agentv1.InteractionQuery{
				Id: interactionQueryID(pc.ID),
				Query: &agentv1.InteractionQuery_CreatePlanRequestQuery{
					CreatePlanRequestQuery: &agentv1.CreatePlanRequestQuery{
						Args:       planArgs,
						ToolCallId: pc.ID,
					},
				},
			}
			MarkPlanEmitted(sess.ConversationID)
		}
		body, _ := json.Marshal(map[string]any{
			"result": "ok",
			"name":   sess.PlanName,
			"todos":  renderTodosJSON(sess.Todos),
			"note":   "Plan created. Continue work; use UpdateTodo as items progress. The plan persists in this conversation — do NOT call CreatePlan again for the same task.",
		})
		return string(body), pill, iq, true
	case "AddTodo":
		var a struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(pc.Arguments), &a); err != nil {
			return `{"error":"parse AddTodo args: ` + jsonStringEscapeRaw(err.Error()) + `"}`, nil, nil, true
		}
		a.Content = strings.TrimSpace(a.Content)
		if a.Content == "" {
			return `{"error":"content required"}`, nil, nil, true
		}
		if sess == nil {
			return `{"error":"no session"}`, nil, nil, true
		}
		todos, ok := AppendTodo(sess.ConversationID, a.Content)
		if !ok {
			return `{"error":"no active plan — call CreatePlan first"}`, nil, nil, true
		}
		sess.Todos = todos
		body, _ := json.Marshal(map[string]any{
			"result": "ok",
			"id":     todos[len(todos)-1].ID,
			"todos":  renderTodosJSON(todos),
		})
		return string(body), updateTodosPill(todos), nil, true
	case "UpdateTodo":
		var a struct {
			ID      string `json:"id"`
			Content string `json:"content"`
			Status  string `json:"status"`
		}
		if err := json.Unmarshal([]byte(pc.Arguments), &a); err != nil {
			return `{"error":"parse UpdateTodo args: ` + jsonStringEscapeRaw(err.Error()) + `"}`, nil, nil, true
		}
		if sess == nil {
			return `{"error":"no session"}`, nil, nil, true
		}
		allowed := map[string]bool{"pending": true, "in_progress": true, "completed": true, "cancelled": true}
		if !allowed[a.Status] {
			return `{"error":"invalid status (expected: pending, in_progress, completed, cancelled)"}`, nil, nil, true
		}
		todos, ok := UpdateTodoStatus(sess.ConversationID, a.ID, a.Content, a.Status)
		if !ok {
			// Either no plan exists yet, or the lookup missed. Check which.
			if PlanStateFor(sess.ConversationID) == nil {
				return `{"error":"no active plan — call CreatePlan first"}`, nil, nil, true
			}
			return `{"error":"todo not found — pass id or a content prefix"}`, nil, nil, true
		}
		sess.Todos = todos
		// Surface the matched id in the response so the model can chain updates.
		matchedID := a.ID
		if matchedID == "" {
			for _, t := range todos {
				if t.Status == a.Status && (a.Content == "" || strings.HasPrefix(strings.ToLower(t.Content), strings.ToLower(a.Content))) {
					matchedID = t.ID
					break
				}
			}
		}
		body, _ := json.Marshal(map[string]any{
			"result": "ok",
			"id":     matchedID,
			"todos":  renderTodosJSON(todos),
		})
		return string(body), updateTodosPill(todos), nil, true
	}
	return "", nil, nil, false
}

// cursorPickerID returns the Cursor UI picker id for a given AgentMode.
// These ids come from Cursor's bundled workbench JS, not from the proto
// enum: Ask ships as "chat" even though the enum name is AGENT_MODE_ASK.
func cursorPickerID(m agentv1.AgentMode) string {
	switch m {
	case agentv1.AgentMode_AGENT_MODE_AGENT:
		return "agent"
	case agentv1.AgentMode_AGENT_MODE_ASK:
		return "chat"
	case agentv1.AgentMode_AGENT_MODE_PLAN:
		return "plan"
	case agentv1.AgentMode_AGENT_MODE_DEBUG:
		return "debug"
	default:
		return strings.ToLower(strings.TrimPrefix(m.String(), "AGENT_MODE_"))
	}
}

// interactionQueryID derives a deterministic uint32 id from the tool_call_id
// so Cursor's InteractionResponse can correlate a reply back to the query
// we emitted. Full bit collapse is fine — we never send two concurrent
// queries with the same payload.
func interactionQueryID(toolCallID string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(toolCallID); i++ {
		h ^= uint32(toolCallID[i])
		h *= 16777619
	}
	if h == 0 {
		h = 1
	}
	return h
}

func renderTodosJSON(todos []*TodoEntry) []map[string]string {
	out := make([]map[string]string, 0, len(todos))
	for _, t := range todos {
		out = append(out, map[string]string{
			"id":      t.ID,
			"status":  t.Status,
			"content": t.Content,
		})
	}
	return out
}

// todosToProto converts the session's local TodoEntry slice into the proto
// TodoItem form Cursor's Plan panel renders.
func todosToProto(ts []*TodoEntry) []*agentv1.TodoItem {
	out := make([]*agentv1.TodoItem, 0, len(ts))
	for _, t := range ts {
		out = append(out, &agentv1.TodoItem{
			Id:      t.ID,
			Content: t.Content,
			Status:  todoStatusProto(t.Status),
		})
	}
	return out
}

func todoStatusProto(s string) agentv1.TodoStatus {
	switch s {
	case "in_progress":
		return agentv1.TodoStatus_TODO_STATUS_IN_PROGRESS
	case "completed":
		return agentv1.TodoStatus_TODO_STATUS_COMPLETED
	case "cancelled":
		return agentv1.TodoStatus_TODO_STATUS_CANCELLED
	default:
		return agentv1.TodoStatus_TODO_STATUS_PENDING
	}
}

// updateTodosPill builds an UpdateTodosToolCall pill carrying the CURRENT full
// todo list (Merge=false replaces the panel's state). AddTodo and UpdateTodo
// emit this after mutating sess.Todos so Cursor's native Plan panel refreshes
// its checkbox state live — without it the panel only reflects the initial
// CreatePlan snapshot and TODOs never appear to advance in the UI.
func updateTodosPill(todos []*TodoEntry) *agentv1.ToolCall {
	proto := todosToProto(todos)
	return &agentv1.ToolCall{
		Tool: &agentv1.ToolCall_UpdateTodosToolCall{
			UpdateTodosToolCall: &agentv1.UpdateTodosToolCall{
				Args: &agentv1.UpdateTodosArgs{Todos: proto, Merge: false},
				Result: &agentv1.UpdateTodosResult{
					Result: &agentv1.UpdateTodosResult_Success{
						Success: &agentv1.UpdateTodosSuccess{Todos: proto},
					},
				},
			},
		},
	}
}

func jsonStringEscapeRaw(s string) string {
	b, _ := json.Marshal(s)
	out := string(b)
	if len(out) >= 2 {
		return out[1 : len(out)-1]
	}
	return out
}

// buildToolCallProto maps a PendingToolCall (the model's function-call
// intent) into agent.v1.ToolCall — the proto wrapper Cursor's UI reads for
// every supported tool. Returns nil for tools we don't handle yet.
// The second return value is a model-visible error string: when non-empty,
// the caller should skip exec and feed the error back via the tool-role
// message (used by StrReplace when read-modify-write fails locally).
func buildToolCallProto(sess *Session, pc PendingToolCall) (*agentv1.ToolCall, string) {
	// Re-route synthesized per-MCP-tool functions (mcp_<idx>__<slug>) back
	// into a proper CallMcpTool with the real server identifier. The model
	// sees each MCP tool as its own OpenAI function, which kills the "let
	// me invent a composite tool name" failure mode.
	if strings.HasPrefix(pc.Name, "mcp_") && sess != nil && sess.McpMap != nil {
		ref := sess.McpMap[pc.Name]
		if ref == nil {
			return nil, "unknown MCP tool: " + pc.Name
		}
		var rawArgs map[string]interface{}
		if strings.TrimSpace(pc.Arguments) != "" {
			if err := json.Unmarshal([]byte(pc.Arguments), &rawArgs); err != nil {
				return nil, "parse MCP args: " + err.Error()
			}
		}
		// Cursor's MCP router keys the tool registry by a prefixed name
		// ("<serverID>-<toolName>") and uses McpArgs.Name as the lookup key
		// — confirmed by a ToolNotFound response that echoed our sent Name
		// unchanged while listing prefixed "available_tools". ToolName stays
		// the bare MCP tool name; upstream MCP receives it verbatim.
		qualifiedKey := ref.ToolName
		if ref.ServerID != "" && !strings.HasPrefix(ref.ToolName, ref.ServerID+"-") {
			qualifiedKey = ref.ServerID + "-" + ref.ToolName
		}
		mcpArgs := &agentv1.McpArgs{
			Name:               qualifiedKey,
			ToolName:           ref.ToolName,
			ToolCallId:         pc.ID,
			ProviderIdentifier: ref.ServerID,
		}
		if len(rawArgs) > 0 {
			mcpArgs.Args = make(map[string]*structpb.Value)
			for k, v := range rawArgs {
				if sv, err := structpb.NewValue(v); err == nil {
					mcpArgs.Args[k] = sv
				}
			}
		}
		return &agentv1.ToolCall{
			Tool: &agentv1.ToolCall_McpToolCall{
				McpToolCall: &agentv1.McpToolCall{
					Args: mcpArgs,
				},
			},
		}, ""
	}
	switch pc.Name {
	case "CallMcpTool":
		var a struct {
			Server    string                 `json:"server"`
			ToolName  string                 `json:"toolName"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(pc.Arguments), &a); err != nil {
			return nil, "parse CallMcpTool args: " + err.Error()
		}
		mcpArgs := &agentv1.McpArgs{
			Name:               a.Server,
			ToolName:           a.ToolName,
			ToolCallId:         pc.ID,
			ProviderIdentifier: a.Server,
		}
		if len(a.Arguments) > 0 {
			mcpArgs.Args = make(map[string]*structpb.Value)
			for k, v := range a.Arguments {
				sv, err := structpb.NewValue(v)
				if err == nil {
					mcpArgs.Args[k] = sv
				}
			}
		}
		return &agentv1.ToolCall{
			Tool: &agentv1.ToolCall_McpToolCall{
				McpToolCall: &agentv1.McpToolCall{
					Args: mcpArgs,
				},
			},
		}, ""
	case "FetchMcpResource":
		var a struct {
			Server       string `json:"server"`
			URI          string `json:"uri"`
			DownloadPath string `json:"downloadPath"`
		}
		if err := json.Unmarshal([]byte(pc.Arguments), &a); err != nil {
			return nil, "parse FetchMcpResource args: " + err.Error()
		}
		readArgs := &agentv1.ReadMcpResourceExecArgs{
			Server: a.Server,
			Uri:    a.URI,
		}
		if a.DownloadPath != "" {
			readArgs.DownloadPath = &a.DownloadPath
		}
		return &agentv1.ToolCall{
			Tool: &agentv1.ToolCall_ReadMcpResourceToolCall{
				ReadMcpResourceToolCall: &agentv1.ReadMcpResourceToolCall{
					Args: readArgs,
				},
			},
		}, ""
	case "ListMcpResources":
		var a struct {
			Server string `json:"server"`
		}
		_ = json.Unmarshal([]byte(pc.Arguments), &a)
		args := &agentv1.ListMcpResourcesExecArgs{}
		if a.Server != "" {
			args.Server = &a.Server
		}
		return &agentv1.ToolCall{
			Tool: &agentv1.ToolCall_ListMcpResourcesToolCall{
				ListMcpResourcesToolCall: &agentv1.ListMcpResourcesToolCall{
					Args: args,
				},
			},
		}, ""
	case "StrReplace":
		// Cursor has no dedicated StrReplace proto — working app implements
		// it server-side as read → string replace → EditArgs with the full
		// updated file as stream_content. We do the same locally because
		// the file lives on the same filesystem Cursor can access.
		var a struct {
			Path       string `json:"path"`
			OldString  string `json:"old_string"`
			NewString  string `json:"new_string"`
			ReplaceAll bool   `json:"replace_all"`
		}
		if err := json.Unmarshal([]byte(pc.Arguments), &a); err != nil {
			return nil, "parse StrReplace args: " + err.Error()
		}
		if a.Path == "" {
			return nil, "StrReplace: path is required"
		}
		raw, err := os.ReadFile(a.Path)
		if err != nil {
			return nil, "StrReplace read file: " + err.Error()
		}
		before := string(raw)
		// Tolerate \r\n vs \n mismatch (schema note says the system does):
		// try literal match first, then normalise line endings if needed.
		var after string
		if strings.Contains(before, a.OldString) {
			if a.ReplaceAll {
				after = strings.ReplaceAll(before, a.OldString, a.NewString)
			} else {
				// Require unique match when replace_all=false, matching
				// Cursor's documented behaviour.
				if strings.Count(before, a.OldString) > 1 {
					return nil, "StrReplace: old_string matches multiple locations — use replace_all or provide more context"
				}
				after = strings.Replace(before, a.OldString, a.NewString, 1)
			}
		} else {
			normBefore := strings.ReplaceAll(before, "\r\n", "\n")
			normOld := strings.ReplaceAll(a.OldString, "\r\n", "\n")
			if !strings.Contains(normBefore, normOld) {
				return nil, "StrReplace: old_string not found in " + a.Path
			}
			if !a.ReplaceAll && strings.Count(normBefore, normOld) > 1 {
				return nil, "StrReplace: old_string matches multiple locations — use replace_all or provide more context"
			}
			normNew := strings.ReplaceAll(a.NewString, "\r\n", "\n")
			if a.ReplaceAll {
				after = strings.ReplaceAll(normBefore, normOld, normNew)
			} else {
				after = strings.Replace(normBefore, normOld, normNew, 1)
			}
			// Preserve original line-ending style on write.
			if strings.Contains(before, "\r\n") {
				after = strings.ReplaceAll(after, "\n", "\r\n")
			}
		}
		return &agentv1.ToolCall{
			Tool: &agentv1.ToolCall_EditToolCall{
				EditToolCall: &agentv1.EditToolCall{
					Args: &agentv1.EditArgs{
						Path:          a.Path,
						StreamContent: &after,
					},
				},
			},
		}, ""
	case "Shell":
		var a struct {
			Command          string `json:"command"`
			WorkingDirectory string `json:"working_directory"`
			BlockUntilMs     int32  `json:"block_until_ms"`
			Description      string `json:"description"`
		}
		_ = json.Unmarshal([]byte(pc.Arguments), &a)
		timeoutMs := a.BlockUntilMs
		if timeoutMs == 0 {
			timeoutMs = 5000
		}
		fileOutputThreshold := uint64(40000)
		hardTimeout := int32(86400000)
		return &agentv1.ToolCall{
			Tool: &agentv1.ToolCall_ShellToolCall{
				ShellToolCall: &agentv1.ShellToolCall{
					Args: &agentv1.ShellArgs{
						Command:                  a.Command,
						WorkingDirectory:         a.WorkingDirectory,
						Timeout:                  timeoutMs,
						ToolCallId:               pc.ID,
						SimpleCommands:           []string{a.Command},
						ParsingResult: &agentv1.ShellCommandParsingResult{
							ExecutableCommands: []*agentv1.ShellCommandParsingResult_ExecutableCommand{
								{Name: a.Command, FullText: a.Command},
							},
						},
						FileOutputThresholdBytes: &fileOutputThreshold,
						SkipApproval:             true,
						TimeoutBehavior:          agentv1.TimeoutBehavior_TIMEOUT_BEHAVIOR_BACKGROUND,
						HardTimeout:              &hardTimeout,
					},
				},
			},
		}, ""
	case "Read":
		var a struct {
			Path               string `json:"path"`
			Offset             *int32 `json:"offset,omitempty"`
			Limit              *int32 `json:"limit,omitempty"`
			IncludeLineNumbers *bool  `json:"include_line_numbers,omitempty"`
		}
		_ = json.Unmarshal([]byte(pc.Arguments), &a)
		return &agentv1.ToolCall{
			Tool: &agentv1.ToolCall_ReadToolCall{
				ReadToolCall: &agentv1.ReadToolCall{
					Args: &agentv1.ReadToolArgs{
						Path:               a.Path,
						Offset:             a.Offset,
						Limit:              a.Limit,
						IncludeLineNumbers: a.IncludeLineNumbers,
					},
				},
			},
		}, ""
	case "Write":
		// The working app maps Write to EditToolCall with stream_content
		// set — same proto handles both "create file" and "overwrite file"
		// intents, and stream_content carries the final file body.
		// Schema uses `contents` (plural); we accept `content` too just in
		// case another provider defaults the singular name.
		var a struct {
			Path     string `json:"path"`
			Contents string `json:"contents"`
			Content  string `json:"content"`
		}
		_ = json.Unmarshal([]byte(pc.Arguments), &a)
		body := a.Contents
		if body == "" {
			body = a.Content
		}
		return &agentv1.ToolCall{
			Tool: &agentv1.ToolCall_EditToolCall{
				EditToolCall: &agentv1.EditToolCall{
					Args: &agentv1.EditArgs{
						Path:          a.Path,
						StreamContent: &body,
					},
				},
			},
		}, ""
	case "Delete":
		var a struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal([]byte(pc.Arguments), &a)
		return &agentv1.ToolCall{
			Tool: &agentv1.ToolCall_DeleteToolCall{
				DeleteToolCall: &agentv1.DeleteToolCall{
					Args: &agentv1.DeleteArgs{
						Path:       a.Path,
						ToolCallId: pc.ID,
					},
				},
			},
		}, ""
	case "Glob":
		var a struct {
			GlobPattern     string  `json:"glob_pattern"`
			TargetDirectory *string `json:"target_directory,omitempty"`
		}
		_ = json.Unmarshal([]byte(pc.Arguments), &a)
		return &agentv1.ToolCall{
			Tool: &agentv1.ToolCall_GlobToolCall{
				GlobToolCall: &agentv1.GlobToolCall{
					Args: &agentv1.GlobToolArgs{
						GlobPattern:     a.GlobPattern,
						TargetDirectory: a.TargetDirectory,
					},
				},
			},
		}, ""
	case "Grep":
		var a struct {
			Pattern         string  `json:"pattern"`
			Path            *string `json:"path,omitempty"`
			Glob            *string `json:"glob,omitempty"`
			OutputMode      *string `json:"output_mode,omitempty"`
			ContextBefore   *int32  `json:"-B,omitempty"`
			ContextAfter    *int32  `json:"-A,omitempty"`
			Context         *int32  `json:"-C,omitempty"`
			CaseInsensitive *bool   `json:"-i,omitempty"`
			Type            *string `json:"type,omitempty"`
			HeadLimit       *int32  `json:"head_limit,omitempty"`
			Multiline       *bool   `json:"multiline,omitempty"`
		}
		_ = json.Unmarshal([]byte(pc.Arguments), &a)
		return &agentv1.ToolCall{
			Tool: &agentv1.ToolCall_GrepToolCall{
				GrepToolCall: &agentv1.GrepToolCall{
					Args: &agentv1.GrepArgs{
						Pattern:         a.Pattern,
						Path:            a.Path,
						Glob:            a.Glob,
						OutputMode:      a.OutputMode,
						ContextBefore:   a.ContextBefore,
						ContextAfter:    a.ContextAfter,
						Context:         a.Context,
						CaseInsensitive: a.CaseInsensitive,
						Type:            a.Type,
						HeadLimit:       a.HeadLimit,
						Multiline:       a.Multiline,
					},
				},
			},
		}, ""
	}
	return nil, ""
}

// writeToolCallStarted emits the UI-facing "started" frame Cursor draws as
// a spinning tool pill while the real work is executing. call_id must
// match the tool_call_id the model picked; Cursor uses it to correlate
// the following Completed frame.
func writeToolCallStarted(w io.Writer, callID string, tc *agentv1.ToolCall) error {
	msg := &agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_ToolCallStarted{
					ToolCallStarted: &agentv1.ToolCallStartedUpdate{
						CallId:   callID,
						ToolCall: tc,
					},
				},
			},
		},
	}
	return writeAgentServerMessage(w, msg)
}

// writeToolCallCompleted emits the paired completion frame once Cursor
// finished running the tool and fed us a result.
func writeToolCallCompleted(w io.Writer, callID string, tc *agentv1.ToolCall) error {
	msg := &agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_ToolCallCompleted{
					ToolCallCompleted: &agentv1.ToolCallCompletedUpdate{
						CallId:   callID,
						ToolCall: tc,
					},
				},
			},
		},
	}
	return writeAgentServerMessage(w, msg)
}

// writeExecRequest fires the ExecServerMessage that actually tells Cursor's
// IDE to run the tool. The real work happens inside Cursor; we just hand
// over the args and then block on BidiAppend for the ExecClientMessage
// result carrying stdout/stderr/exit/etc.
//
// Wire format was reverse-engineered from the working app (RunSSE capture
// frame 5): shell runs use field 14 (shell_stream_args, uses ShellArgs
// type) — NOT field 2 (shell_args) — Cursor's IDE client ignores the
// deprecated field 2 path entirely. The exec_id must follow the
// "exec-<tool>-<nanos>" naming; the assistant's OpenAI tool_call_id goes
// into ShellArgs.ToolCallId so the completion frame can correlate.
// execSeq is the monotonic id field Cursor uses to order concurrent exec
// requests within a turn.
func writeExecRequest(w io.Writer, execID string, execSeq uint32, pc PendingToolCall, tc *agentv1.ToolCall) error {
	esm := &agentv1.ExecServerMessage{Id: execSeq, ExecId: execID}
	switch inner := tc.Tool.(type) {
	case *agentv1.ToolCall_ShellToolCall:
		esm.Message = &agentv1.ExecServerMessage_ShellStreamArgs{ShellStreamArgs: inner.ShellToolCall.Args}
	case *agentv1.ToolCall_ReadToolCall:
		esm.Message = &agentv1.ExecServerMessage_ReadArgs{ReadArgs: &agentv1.ReadArgs{
			Path:       inner.ReadToolCall.Args.Path,
			ToolCallId: pc.ID,
		}}
	case *agentv1.ToolCall_DeleteToolCall:
		esm.Message = &agentv1.ExecServerMessage_DeleteArgs{DeleteArgs: inner.DeleteToolCall.Args}
	case *agentv1.ToolCall_EditToolCall:
		// Write/StrReplace tools map to EditToolCall. The exec channel for
		// edits lives on ExecServerMessage.write_args (field 3).
		args := &agentv1.WriteArgs{ToolCallId: pc.ID, ReturnFileContentAfterWrite: true}
		if a := inner.EditToolCall.Args; a != nil {
			args.Path = a.Path
			if a.StreamContent != nil {
				args.FileText = *a.StreamContent
			}
		}
		esm.Message = &agentv1.ExecServerMessage_WriteArgs{WriteArgs: args}
	case *agentv1.ToolCall_GlobToolCall:
		dir := "."
		if inner.GlobToolCall.Args.TargetDirectory != nil {
			dir = *inner.GlobToolCall.Args.TargetDirectory
		}
		esm.Message = &agentv1.ExecServerMessage_LsArgs{LsArgs: &agentv1.LsArgs{
			Path:       dir,
			ToolCallId: pc.ID,
		}}
	case *agentv1.ToolCall_GrepToolCall:
		esm.Message = &agentv1.ExecServerMessage_GrepArgs{GrepArgs: inner.GrepToolCall.Args}
	case *agentv1.ToolCall_McpToolCall:
		esm.Message = &agentv1.ExecServerMessage_McpArgs{McpArgs: inner.McpToolCall.Args}
	case *agentv1.ToolCall_ReadMcpResourceToolCall:
		esm.Message = &agentv1.ExecServerMessage_ReadMcpResourceExecArgs{
			ReadMcpResourceExecArgs: inner.ReadMcpResourceToolCall.Args,
		}
	case *agentv1.ToolCall_ListMcpResourcesToolCall:
		esm.Message = &agentv1.ExecServerMessage_ListMcpResourcesExecArgs{
			ListMcpResourcesExecArgs: inner.ListMcpResourcesToolCall.Args,
		}
	default:
		return fmt.Errorf("unsupported exec tool %T", inner)
	}
	msg := &agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_ExecServerMessage{
			ExecServerMessage: esm,
		},
	}
	return writeAgentServerMessage(w, msg)
}

// newExecID mints an exec_id in the format Cursor's IDE client recognises.
// Working-app captures consistently show "exec-<tool>-<nanoseconds>" — the
// tool prefix ("shell", "write", "read", etc.) helps Cursor route the
// completion callback to the right tool pill.
func newExecID(tool string) string {
	return fmt.Sprintf("exec-%s-%d", tool, time.Now().UnixNano())
}

// routeExecClientResult dispatches an incoming ExecClientMessage to the
// RunSSE goroutine waiting on its tool result. Cursor streams ShellStream
// events across many BidiAppend calls (Start -> Stdout chunks -> Exit);
// for shells we accumulate until the Exit event lands and only then
// deliver a composite result. Non-shell results (Write, Read, Ls, Grep,
// Delete) arrive as one complete message, so we deliver them immediately.
func routeExecClientResult(acm *agentv1.AgentClientMessage) {
	ecm := acm.GetExecClientMessage()
	if ecm == nil {
		return
	}
	seq := ecm.GetId()
	pendingMu.Lock()
	callID := seqAlias[seq]
	pendingMu.Unlock()

	// Shell streaming: accumulate chunks, deliver on Exit.
	if ss := ecm.GetShellStream(); ss != nil {
		pendingMu.Lock()
		state := shellAccum[seq]
		if state == nil {
			state = &shellAccumState{}
			shellAccum[seq] = state
		}
		pendingMu.Unlock()

		switch evt := ss.GetEvent().(type) {
		case *agentv1.ShellStream_Start:
			state.Started = true
			publishInteractionUpdate(&aiserverv1.InteractionUpdate{Message: &aiserverv1.InteractionUpdate_ShellOutputDelta{ShellOutputDelta: &aiserverv1.ShellOutputDeltaUpdate{Event: &aiserverv1.ShellOutputDeltaUpdate_Start{Start: &aiserverv1.ShellStreamStart{}}}}})
		case *agentv1.ShellStream_Stdout:
			state.Stdout = append(state.Stdout, []byte(evt.Stdout.GetData())...)
			publishInteractionUpdate(&aiserverv1.InteractionUpdate{Message: &aiserverv1.InteractionUpdate_ShellOutputDelta{ShellOutputDelta: &aiserverv1.ShellOutputDeltaUpdate{Event: &aiserverv1.ShellOutputDeltaUpdate_Stdout{Stdout: &aiserverv1.ShellStreamStdout{Data: evt.Stdout.GetData()}}}}})
		case *agentv1.ShellStream_Stderr:
			state.Stderr = append(state.Stderr, []byte(evt.Stderr.GetData())...)
			publishInteractionUpdate(&aiserverv1.InteractionUpdate{Message: &aiserverv1.InteractionUpdate_ShellOutputDelta{ShellOutputDelta: &aiserverv1.ShellOutputDeltaUpdate{Event: &aiserverv1.ShellOutputDeltaUpdate_Stderr{Stderr: &aiserverv1.ShellStreamStderr{Data: evt.Stderr.GetData()}}}}})
		case *agentv1.ShellStream_Exit:
			state.Exited = true
			state.ExitCode = evt.Exit.GetCode()
			state.Cwd = evt.Exit.GetCwd()
			publishInteractionUpdate(&aiserverv1.InteractionUpdate{Message: &aiserverv1.InteractionUpdate_ShellOutputDelta{ShellOutputDelta: &aiserverv1.ShellOutputDeltaUpdate{Event: &aiserverv1.ShellOutputDeltaUpdate_Exit{Exit: &aiserverv1.ShellStreamExit{Code: evt.Exit.GetCode(), Cwd: evt.Exit.GetCwd()}}}}})
		}
		if state.Exited && callID != "" {
			body, _ := json.Marshal(map[string]any{
				"exit_code": state.ExitCode,
				"cwd":       state.Cwd,
				"stdout":    string(state.Stdout),
				"stderr":    string(state.Stderr),
			})
			finalState := *state
			pendingMu.Lock()
			delete(shellAccum, seq)
			delete(seqAlias, seq)
			pendingMu.Unlock()
			deliverToolResult(callID, &toolResultEnvelope{
				ResultJSON: string(body),
				ShellAccum: &finalState,
			})
		}
		return
	}

	// Non-shell result — marshal whole ExecClientMessage and deliver.
	body, err := json.Marshal(ecm)
	if err != nil {
		return
	}
	env := &toolResultEnvelope{ResultJSON: string(body), ExecClient: ecm}
	if id := ecm.GetExecId(); id != "" {
		deliverToolResult(id, env)
		return
	}
	if callID != "" {
		pendingMu.Lock()
		delete(seqAlias, seq)
		pendingMu.Unlock()
		deliverToolResult(callID, env)
		return
	}
	// Legacy fallback: single pending waiter gets everything.
	pendingMu.Lock()
	var only string
	count := 0
	for id := range pending {
		only = id
		count++
	}
	pendingMu.Unlock()
	if count == 1 {
		deliverToolResult(only, env)
	}
}

// attachToolResultToProto writes the accumulated tool result onto the
// matching field of the ToolCall proto so the ToolCallCompleted frame
// carries it. Cursor's UI reads the embedded result to render the tool
// output inside the pill — without this the pill shows just the command
// header and leaves the result section blank. Best-effort: silently
// no-ops on unsupported combinations (model still sees the result via
// the tool-role message it gets fed afterwards).
func attachToolResultToProto(tc *agentv1.ToolCall, toolName string, env *toolResultEnvelope) {
	if tc == nil || env == nil {
		return
	}
	if strings.HasPrefix(toolName, "mcp_") {
		attachMcpResult(tc, env)
		return
	}
	switch toolName {
	case "Shell":
		attachShellResult(tc, env)
	case "Write":
		attachWriteResult(tc, env)
	case "Read":
		attachReadResult(tc, env)
	case "Glob":
		attachGlobResult(tc, env)
	case "Grep":
		attachGrepResult(tc, env)
	case "Delete":
		attachDeleteResult(tc, env)
	case "StrReplace":
		attachWriteResult(tc, env)
	case "CallMcpTool":
		attachMcpResult(tc, env)
	case "FetchMcpResource":
		attachFetchMcpResult(tc, env)
	case "ListMcpResources":
		attachListMcpResult(tc, env)
	}
}

func attachShellResult(tc *agentv1.ToolCall, env *toolResultEnvelope) {
	shell := tc.GetShellToolCall()
	if shell == nil || env.ShellAccum == nil {
		return
	}
	cmd := ""
	if shell.Args != nil {
		cmd = shell.Args.GetCommand()
	}
	shell.Result = &agentv1.ShellResult{
		Result: &agentv1.ShellResult_Success{
			Success: &agentv1.ShellSuccess{
				Command:          cmd,
				WorkingDirectory: env.ShellAccum.Cwd,
				ExitCode:         int32(env.ShellAccum.ExitCode),
				Stdout:           string(env.ShellAccum.Stdout),
				Stderr:           string(env.ShellAccum.Stderr),
			},
		},
	}
}

func attachWriteResult(tc *agentv1.ToolCall, env *toolResultEnvelope) {
	edit := tc.GetEditToolCall()
	if edit == nil || env.ExecClient == nil {
		return
	}
	wr := env.ExecClient.GetWriteResult()
	if wr == nil {
		return
	}
	// Map WriteResult variants onto EditResult so the pill can render
	// success/permission-denied/etc consistently.
	er := &agentv1.EditResult{}
	switch v := wr.GetResult().(type) {
	case *agentv1.WriteResult_Success:
		path := ""
		afterContent := ""
		if edit.Args != nil {
			path = edit.Args.GetPath()
			if edit.Args.StreamContent != nil {
				afterContent = *edit.Args.StreamContent
			}
		}
		linesAdded := int32(0)
		for _, ch := range afterContent {
			if ch == '\n' {
				linesAdded++
			}
		}
		if afterContent != "" {
			linesAdded++
		}
		er.Result = &agentv1.EditResult_Success{
			Success: &agentv1.EditSuccess{
				Path:                 path,
				LinesAdded:           &linesAdded,
				AfterFullFileContent: afterContent,
			},
		}
		_ = v
	case *agentv1.WriteResult_PermissionDenied:
		er.Result = &agentv1.EditResult_WritePermissionDenied{
			WritePermissionDenied: &agentv1.EditWritePermissionDenied{
				Path: v.PermissionDenied.GetPath(),
			},
		}
	case *agentv1.WriteResult_Error:
		er.Result = &agentv1.EditResult_Error{
			Error: &agentv1.EditError{Error: v.Error.GetError()},
		}
	case *agentv1.WriteResult_Rejected:
		er.Result = &agentv1.EditResult_Rejected{
			Rejected: &agentv1.EditRejected{
				Path:   v.Rejected.GetPath(),
				Reason: v.Rejected.GetReason(),
			},
		}
	}
	edit.Result = er
}

func attachReadResult(tc *agentv1.ToolCall, env *toolResultEnvelope) {
	read := tc.GetReadToolCall()
	if read == nil || env.ExecClient == nil {
		return
	}
	rr := env.ExecClient.GetReadResult()
	if rr == nil {
		return
	}
	// ReadResult (from Cursor) uses ReadSuccess/ReadError; ReadToolCall
	// wants ReadToolSuccess/ReadToolError which are separate types with
	// partially overlapping fields. Map success via the path+content we
	// care about; errors via the single error_message field.
	rtr := &agentv1.ReadToolResult{}
	switch v := rr.GetResult().(type) {
	case *agentv1.ReadResult_Success:
		rtr.Result = &agentv1.ReadToolResult_Success{
			Success: &agentv1.ReadToolSuccess{
				Path:       v.Success.GetPath(),
				TotalLines: uint32(v.Success.GetTotalLines()),
				Output:     &agentv1.ReadToolSuccess_Content{Content: v.Success.GetContent()},
			},
		}
	case *agentv1.ReadResult_Error:
		rtr.Result = &agentv1.ReadToolResult_Error{
			Error: &agentv1.ReadToolError{ErrorMessage: v.Error.GetError()},
		}
	default:
		return
	}
	read.Result = rtr
}

func attachGlobResult(tc *agentv1.ToolCall, env *toolResultEnvelope) {
	glob := tc.GetGlobToolCall()
	if glob == nil || env.ExecClient == nil {
		return
	}
	ls := env.ExecClient.GetLsResult()
	if ls == nil {
		return
	}
	pattern := ""
	rootPath := "."
	if glob.Args != nil {
		pattern = glob.Args.GetGlobPattern()
		if glob.Args.TargetDirectory != nil {
			rootPath = *glob.Args.TargetDirectory
		}
	}
	switch v := ls.GetResult().(type) {
	case *agentv1.LsResult_Success:
		files := collectGlobMatches(v.Success.GetDirectoryTreeRoot(), rootPath, pattern)
		total := int32(len(files))
		glob.Result = &agentv1.GlobToolResult{
			Result: &agentv1.GlobToolResult_Success{
				Success: &agentv1.GlobToolSuccess{
					Pattern:    pattern,
					Path:       rootPath,
					Files:      files,
					TotalFiles: total,
				},
			},
		}
	case *agentv1.LsResult_Error:
		glob.Result = &agentv1.GlobToolResult{
			Result: &agentv1.GlobToolResult_Error{
				Error: &agentv1.GlobToolError{Error: v.Error.GetError()},
			},
		}
	}
}

// collectGlobMatches walks the Ls directory tree Cursor returned and yields
// every file whose relative path matches the glob pattern. Uses the
// minimal globToRegex matcher below (supports *, **, ? — the 99% case).
// An empty pattern returns every file.
func collectGlobMatches(root *agentv1.LsDirectoryTreeNode, rootPath, pattern string) []string {
	if root == nil {
		return nil
	}
	re := globToRegex(pattern)
	var out []string
	var walk func(n *agentv1.LsDirectoryTreeNode)
	walk = func(n *agentv1.LsDirectoryTreeNode) {
		if n == nil {
			return
		}
		base := n.GetAbsPath()
		for _, f := range n.GetChildrenFiles() {
			full := base
			if full != "" && !strings.HasSuffix(full, "/") && !strings.HasSuffix(full, "\\") {
				full += "/"
			}
			full += f.GetName()
			rel := full
			if rootPath != "" && rootPath != "." && strings.HasPrefix(rel, rootPath) {
				rel = strings.TrimPrefix(rel, rootPath)
				rel = strings.TrimPrefix(rel, "/")
				rel = strings.TrimPrefix(rel, "\\")
			}
			if re == nil || re.MatchString(rel) || re.MatchString(full) {
				out = append(out, full)
			}
		}
		for _, c := range n.GetChildrenDirs() {
			walk(c)
		}
	}
	walk(root)
	return out
}

// globToRegex compiles a minimal glob pattern into a regexp. Supports the
// three operators models overwhelmingly emit: `**` (any dirs), `*` (any
// chars except separators), and `?` (single char). Returns nil for empty
// patterns so the caller can short-circuit to "match all".
func globToRegex(pattern string) *regexp.Regexp {
	if pattern == "" {
		return nil
	}
	var b strings.Builder
	b.WriteString("^")
	// Normalise backslashes to forward so the same regex works for both.
	p := strings.ReplaceAll(pattern, "\\", "/")
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '/':
			b.WriteString("[/\\\\]")
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}

func attachGrepResult(tc *agentv1.ToolCall, env *toolResultEnvelope) {
	grep := tc.GetGrepToolCall()
	if grep == nil || env.ExecClient == nil {
		return
	}
	if r := env.ExecClient.GetGrepResult(); r != nil {
		grep.Result = r
	}
}

func attachMcpResult(tc *agentv1.ToolCall, env *toolResultEnvelope) {
	mcp := tc.GetMcpToolCall()
	if mcp == nil || env.ExecClient == nil {
		return
	}
	if r := env.ExecClient.GetMcpResult(); r != nil {
		mcp.Result = &agentv1.McpToolResult{}
		switch v := r.GetResult().(type) {
		case *agentv1.McpResult_Success:
			mcp.Result.Result = &agentv1.McpToolResult_Success{
				Success: v.Success,
			}
		case *agentv1.McpResult_Error:
			mcp.Result.Result = &agentv1.McpToolResult_Error{
				Error: &agentv1.McpToolError{Error: v.Error.GetError()},
			}
		}
	}
}

func attachFetchMcpResult(tc *agentv1.ToolCall, env *toolResultEnvelope) {
	fetch := tc.GetReadMcpResourceToolCall()
	if fetch == nil || env.ExecClient == nil {
		return
	}
	if r := env.ExecClient.GetReadMcpResourceExecResult(); r != nil {
		fetch.Result = r
	}
}

func attachListMcpResult(tc *agentv1.ToolCall, env *toolResultEnvelope) {
	list := tc.GetListMcpResourcesToolCall()
	if list == nil || env.ExecClient == nil {
		return
	}
	if r := env.ExecClient.GetListMcpResourcesExecResult(); r != nil {
		list.Result = r
	}
}

func attachDeleteResult(tc *agentv1.ToolCall, env *toolResultEnvelope) {
	del := tc.GetDeleteToolCall()
	if del == nil || env.ExecClient == nil {
		return
	}
	if r := env.ExecClient.GetDeleteResult(); r != nil {
		del.Result = r
	}
}

// waitForToolResult blocks until Cursor posts a matching tool_call_id
// result via BidiAppend (see bidi.go -> HandleBidiAppend). Returns a
// timeout envelope after 2 minutes so a stuck IDE doesn't hang the server
// goroutine forever.
func waitForToolResult(ch chan *toolResultEnvelope, timeout time.Duration) *toolResultEnvelope {
	select {
	case env := <-ch:
		return env
	case <-time.After(timeout):
		return &toolResultEnvelope{Error: "tool execution timed out after " + timeout.String()}
	}
}
