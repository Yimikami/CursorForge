package agent

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	agentv1 "cursorforge/internal/protocodec/gen/agent/v1"
	aiserverv1 "cursorforge/internal/protocodec/gen/aiserver/v1"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func HandleAddAsyncBackgroundComposer(reqBody []byte, contentType string) Result {
	req := &aiserverv1.AddAsyncFollowupBackgroundComposerRequest{}
	if err := decodeUnary(reqBody, contentType, req); err != nil {
		return errResult(400, "decode add background composer followup: "+err.Error())
	}
	bcID := strings.TrimSpace(req.GetBcId())
	if bcID == "" {
		bcID = newBackgroundComposerID()
	}
	prompt := &aiserverv1.HeadlessAgenticComposerPrompt{
		Text:                req.GetFollowup(),
		RichText:            req.GetRichFollowup(),
		BaseConversationMessage: req.GetFollowupMessage(),
	}
	st := &backgroundComposerState{
		BcID:      bcID,
		Prompt:    prompt,
		Request:   req,
		Status:    aiserverv1.BackgroundComposerStatus_BACKGROUND_COMPOSER_STATUS_CREATING,
		CreatedAt: time.Now(),
	}
	if md := req.GetModelDetails(); md != nil {
		st.ModelName = md.GetModelName()
	}
	saveBackgroundComposer(st)
	body, err := proto.Marshal(&aiserverv1.AddAsyncFollowupBackgroundComposerResponse{})
	if err != nil {
		return errResult(500, "marshal add background composer response: "+err.Error())
	}
	return Result{Status: 200, ContentType: "application/proto", Body: body}
}

func HandleGetBackgroundComposerStatus(reqBody []byte, contentType string) Result {
	req := &aiserverv1.GetBackgroundComposerStatusRequest{}
	if err := decodeUnary(reqBody, contentType, req); err != nil {
		return errResult(400, "decode get background composer status: "+err.Error())
	}
	st := getBackgroundComposer(req.GetBcId())
	status := aiserverv1.BackgroundComposerStatus_BACKGROUND_COMPOSER_STATUS_UNSPECIFIED
	if st != nil {
		status = st.Status
	}
	body, err := proto.Marshal(&aiserverv1.GetBackgroundComposerStatusResponse{Status: status})
	if err != nil {
		return errResult(500, "marshal background composer status response: "+err.Error())
	}
	return Result{Status: 200, ContentType: "application/proto", Body: body}
}

func HandleBackgroundComposerAttach(ctx context.Context, reqBody []byte, contentType string, rawWriter io.Writer, resolve AdapterResolver) {
	w := &lockedWriter{w: rawWriter}
	req := &aiserverv1.AttachBackgroundComposerRequest{}
	if err := decodeUnary(reqBody, contentType, req); err != nil {
		_ = writeBackgroundComposerResponse(w, &aiserverv1.AttachBackgroundComposerResponse{
			HeadlessAgenticComposerResponse: &aiserverv1.HeadlessAgenticComposerResponse{Error: &aiserverv1.HeadlessAgenticComposerResponse_Error{Message: "decode attach background composer: " + err.Error()}},
			StatusUpdate:                    aiserverv1.BackgroundComposerStatus_BACKGROUND_COMPOSER_STATUS_ERROR,
		})
		_ = writeEndStream(w)
		return
	}
	st := getBackgroundComposer(req.GetBcId())
	if st == nil {
		_ = writeBackgroundComposerResponse(w, &aiserverv1.AttachBackgroundComposerResponse{
			HeadlessAgenticComposerResponse: &aiserverv1.HeadlessAgenticComposerResponse{Error: &aiserverv1.HeadlessAgenticComposerResponse_Error{Message: "unknown background composer: " + req.GetBcId()}},
			StatusUpdate:                    aiserverv1.BackgroundComposerStatus_BACKGROUND_COMPOSER_STATUS_ERROR,
		})
		_ = writeEndStream(w)
		return
	}
	if req.GetStartingIndex() > 0 {
		for _, upd := range st.updates[req.GetStartingIndex():] {
			if err := writeBackgroundComposerResponse(w, upd); err != nil {
				return
			}
		}
		if st.streamDone {
			_ = writeEndStream(w)
		}
		return
	}
	if st.streaming {
		for _, upd := range st.updates {
			if err := writeBackgroundComposerResponse(w, upd); err != nil {
				return
			}
		}
		if st.streamDone {
			_ = writeEndStream(w)
		}
		return
	}
	st.streaming = true
	st.StartedAt = time.Now()
	st.Status = aiserverv1.BackgroundComposerStatus_BACKGROUND_COMPOSER_STATUS_RUNNING
	_ = appendAndWriteBackgroundComposerUpdate(w, st, &aiserverv1.AttachBackgroundComposerResponse{
		Prompt:       st.Prompt,
		StatusUpdate: aiserverv1.BackgroundComposerStatus_BACKGROUND_COMPOSER_STATUS_RUNNING,
	})

	target, ok := ResolveAdapterForModel(resolve(), st.ModelName)
	if !ok {
		st.Status = aiserverv1.BackgroundComposerStatus_BACKGROUND_COMPOSER_STATUS_ERROR
		st.LastError = "no BYOK adapter configured"
		_ = appendAndWriteBackgroundComposerUpdate(w, st, &aiserverv1.AttachBackgroundComposerResponse{
			HeadlessAgenticComposerResponse: &aiserverv1.HeadlessAgenticComposerResponse{Error: &aiserverv1.HeadlessAgenticComposerResponse_Error{Message: st.LastError}},
			StatusUpdate:                    st.Status,
		})
		st.streamDone = true
		_ = writeEndStream(w)
		return
	}
	messages := []openAIMessage{
		textMessage("system", "You are Cursor background composer. Continue the user's task autonomously, use tools when needed, and stream concise progress updates."),
		textMessage("user", backgroundComposerPromptText(st.Prompt)),
	}
	sess := &Session{RequestID: st.BcID, ConversationID: st.BcID, UserText: backgroundComposerPromptText(st.Prompt)}
	tools := openAIToolsForRequest(sess)
	stream := pickProviderStreamer(target.ProviderType)
	for round := 0; round < 20; round++ {
		res, err := stream(ctx, target.BaseURL, target.APIKey, target.Model, messages, tools, target.Opts, func(chunk string, reasoning string, done bool) error {
			if reasoning != "" {
				return appendAndWriteBackgroundComposerUpdate(w, st, &aiserverv1.AttachBackgroundComposerResponse{HeadlessAgenticComposerResponse: &aiserverv1.HeadlessAgenticComposerResponse{Thinking: &aiserverv1.ConversationMessage_Thinking{Text: reasoning}, ThinkingDurationMs: int32Ptr(0), IsMessageDone: false}, StatusUpdate: st.Status})
			}
			if chunk == "" {
				return nil
			}
			st.LastText += chunk
			return appendAndWriteBackgroundComposerUpdate(w, st, &aiserverv1.AttachBackgroundComposerResponse{HeadlessAgenticComposerResponse: &aiserverv1.HeadlessAgenticComposerResponse{Text: chunk, IsMessageDone: false}, StatusUpdate: st.Status})
		})
		if err != nil {
			st.Status = aiserverv1.BackgroundComposerStatus_BACKGROUND_COMPOSER_STATUS_ERROR
			st.LastError = err.Error()
			_ = appendAndWriteBackgroundComposerUpdate(w, st, &aiserverv1.AttachBackgroundComposerResponse{HeadlessAgenticComposerResponse: &aiserverv1.HeadlessAgenticComposerResponse{Error: &aiserverv1.HeadlessAgenticComposerResponse_Error{Message: err.Error()}}, StatusUpdate: st.Status})
			st.streamDone = true
			_ = writeEndStream(w)
			return
		}
		if len(res.ToolCalls) == 0 {
			break
		}
		assistant := openAIMessage{Role: "assistant"}
		for _, tc := range res.ToolCalls {
			assistant.ToolCalls = append(assistant.ToolCalls, openAIToolCallMsg{ID: tc.ID, Type: "function", Function: openAIToolCallFn{Name: tc.Name, Arguments: tc.Arguments}})
			streamed, err := buildBackgroundComposerStreamedToolCall(sess, tc)
			if err == nil && streamed != nil {
				_ = appendAndWriteBackgroundComposerUpdate(w, st, &aiserverv1.AttachBackgroundComposerResponse{HeadlessAgenticComposerResponse: &aiserverv1.HeadlessAgenticComposerResponse{StreamedBackToolCall: streamed, IsMessageDone: false}, StatusUpdate: st.Status})
			}
		}
		messages = append(messages, assistant)
		for _, tc := range res.ToolCalls {
			resultJSON, finalResult := executeBackgroundComposerTool(w, sess, tc)
			messages = append(messages, toolResultMessage(tc.ID, tc.Name, resultJSON))
			_ = appendAndWriteBackgroundComposerUpdate(w, st, &aiserverv1.AttachBackgroundComposerResponse{HeadlessAgenticComposerResponse: &aiserverv1.HeadlessAgenticComposerResponse{FinalToolResult: &aiserverv1.HeadlessAgenticComposerResponse_FinalToolResult{ToolCallId: tc.ID, Result: finalResult}, IsMessageDone: false}, StatusUpdate: st.Status})
		}
	}
	st.Status = aiserverv1.BackgroundComposerStatus_BACKGROUND_COMPOSER_STATUS_FINISHED
	st.FinishedAt = time.Now()
	_ = appendAndWriteBackgroundComposerUpdate(w, st, &aiserverv1.AttachBackgroundComposerResponse{HeadlessAgenticComposerResponse: &aiserverv1.HeadlessAgenticComposerResponse{Text: "", IsMessageDone: true, Status: &aiserverv1.HeadlessAgenticComposerResponse_Status{Type: aiserverv1.HeadlessAgenticComposerResponse_Status_STATUS_TYPE_GENERIC, Message: "Completed", IsComplete: true}}, StatusUpdate: st.Status})
	st.streamDone = true
	_ = writeEndStream(w)
}

func HandleBackgroundComposerInteractionUpdates(ctx context.Context, reqBody []byte, contentType string, rawWriter io.Writer) {
	w := &lockedWriter{w: rawWriter}
	req := &aiserverv1.StreamInteractionUpdatesRequest{}
	if err := decodeUnary(reqBody, contentType, req); err != nil {
		_ = writeEndStream(w)
		return
	}
	_ = req
	_ = WriteBackgroundComposerInteractionStubStream(ctx, w)
}

func backgroundComposerPromptText(prompt *aiserverv1.HeadlessAgenticComposerPrompt) string {
	if prompt == nil {
		return "Continue the background task."
	}
	if s := strings.TrimSpace(prompt.GetRichText()); s != "" {
		return s
	}
	if s := strings.TrimSpace(prompt.GetText()); s != "" {
		return s
	}
	if msg := prompt.GetBaseConversationMessage(); msg != nil {
		if s := strings.TrimSpace(msg.GetText()); s != "" {
			return s
		}
	}
	return "Continue the background task."
}

func appendAndWriteBackgroundComposerUpdate(w io.Writer, st *backgroundComposerState, resp *aiserverv1.AttachBackgroundComposerResponse) error {
	appendBackgroundComposerUpdate(st.BcID, resp)
	return writeBackgroundComposerResponse(w, resp)
}

func writeBackgroundComposerResponse(w io.Writer, resp *aiserverv1.AttachBackgroundComposerResponse) error {
	body, err := proto.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal background composer response: %w", err)
	}
	if err := writeFrame(w, body, false); err != nil {
		return err
	}
	flushIfPossible(w)
	return nil
}

func WriteBackgroundComposerInteractionStubStream(ctx context.Context, w io.Writer) error {
	lw := &lockedWriter{w: w}
	id, ch := subscribeInteractionUpdates()
	defer unsubscribeInteractionUpdates(id)
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return writeEndStream(lw)
		case msg, ok := <-ch:
			if !ok {
				return writeEndStream(lw)
			}
			body, err := proto.Marshal(msg)
			if err != nil {
				return fmt.Errorf("marshal background composer interaction update: %w", err)
			}
			if err := writeFrame(lw, body, false); err != nil {
				return err
			}
			flushIfPossible(lw)
		case <-heartbeat.C:
			msg := &aiserverv1.InteractionUpdate{Message: &aiserverv1.InteractionUpdate_Heartbeat{Heartbeat: &aiserverv1.HeartbeatUpdate{}}}
			body, err := proto.Marshal(msg)
			if err != nil {
				return fmt.Errorf("marshal background composer heartbeat: %w", err)
			}
			if err := writeFrame(lw, body, false); err != nil {
				return err
			}
			flushIfPossible(lw)
		}
	}
}

func int32Ptr(v int32) *int32 { return &v }

func buildBackgroundComposerStreamedToolCall(sess *Session, pc PendingToolCall) (*aiserverv1.StreamedBackToolCall, error) {
	tc, buildErr := buildToolCallProto(sess, pc)
	if buildErr != "" {
		return &aiserverv1.StreamedBackToolCall{ToolCallId: pc.ID, Name: pc.Name, RawArgs: pc.Arguments, Error: &aiserverv1.ToolResultError{ClientVisibleErrorMessage: buildErr, ModelVisibleErrorMessage: buildErr}}, nil
	}
	if tc == nil {
		return nil, fmt.Errorf("unsupported background composer tool %s", pc.Name)
	}
	v2Tool := mapBackgroundComposerTool(tc)
	return &aiserverv1.StreamedBackToolCall{Tool: v2Tool, ToolCallId: pc.ID, Name: pc.Name, RawArgs: pc.Arguments}, nil
}

func executeBackgroundComposerTool(w io.Writer, sess *Session, pc PendingToolCall) (string, *aiserverv1.ClientSideToolV2Result) {
	tc, buildErr := buildToolCallProto(sess, pc)
	v2Tool := mapBackgroundComposerTool(tc)
	if buildErr != "" {
		return `{"error":` + jsonString(buildErr) + `}`, &aiserverv1.ClientSideToolV2Result{Tool: v2Tool, ToolCallId: pc.ID, Error: &aiserverv1.ToolResultError{ClientVisibleErrorMessage: buildErr, ModelVisibleErrorMessage: buildErr}}
	}
	if tc == nil {
		errMsg := "unsupported background composer tool: " + pc.Name
		return `{"error":` + jsonString(errMsg) + `}`, &aiserverv1.ClientSideToolV2Result{Tool: v2Tool, ToolCallId: pc.ID, Error: &aiserverv1.ToolResultError{ClientVisibleErrorMessage: errMsg, ModelVisibleErrorMessage: errMsg}}
	}
	execID := newExecID(execToolName(pc.Name))
	seq := nextExecSeq()
	waitCh := registerToolWait(pc.ID)
	registerExecIDAlias(execID, seq, pc.ID)
	if err := writeExecRequest(w, execID, seq, pc, tc); err != nil {
		errMsg := err.Error()
		return `{"error":` + jsonString(errMsg) + `}`, &aiserverv1.ClientSideToolV2Result{Tool: v2Tool, ToolCallId: pc.ID, Error: &aiserverv1.ToolResultError{ClientVisibleErrorMessage: errMsg, ModelVisibleErrorMessage: errMsg}}
	}
	env := waitForToolResult(waitCh, 2*time.Minute)
	if env == nil {
		errMsg := "background composer tool execution returned no result"
		return `{"error":` + jsonString(errMsg) + `}`, &aiserverv1.ClientSideToolV2Result{Tool: v2Tool, ToolCallId: pc.ID, Error: &aiserverv1.ToolResultError{ClientVisibleErrorMessage: errMsg, ModelVisibleErrorMessage: errMsg}}
	}
	if env.Error != "" {
		return `{"error":` + jsonString(env.Error) + `}`, &aiserverv1.ClientSideToolV2Result{Tool: v2Tool, ToolCallId: pc.ID, Error: &aiserverv1.ToolResultError{ClientVisibleErrorMessage: env.Error, ModelVisibleErrorMessage: env.Error}}
	}
	return env.ResultJSON, buildBackgroundComposerFinalResult(v2Tool, pc, env)
}

func mapBackgroundComposerTool(tc *agentv1.ToolCall) aiserverv1.ClientSideToolV2 {
	if tc == nil {
		return aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_UNSPECIFIED
	}
	switch tc.Tool.(type) {
	case *agentv1.ToolCall_ShellToolCall:
		return aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_RUN_TERMINAL_COMMAND_V2
	case *agentv1.ToolCall_ReadToolCall:
		return aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_READ_FILE_V2
	case *agentv1.ToolCall_GlobToolCall:
		return aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_LIST_DIR_V2
	case *agentv1.ToolCall_GrepToolCall:
		return aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_RIPGREP_RAW_SEARCH
	case *agentv1.ToolCall_EditToolCall:
		return aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_EDIT_FILE_V2
	case *agentv1.ToolCall_DeleteToolCall:
		return aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_DELETE_FILE
	case *agentv1.ToolCall_McpToolCall:
		return aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_CALL_MCP_TOOL
	case *agentv1.ToolCall_ReadMcpResourceToolCall:
		return aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_READ_MCP_RESOURCE
	case *agentv1.ToolCall_ListMcpResourcesToolCall:
		return aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_LIST_MCP_RESOURCES
	case *agentv1.ToolCall_CreatePlanToolCall:
		return aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_CREATE_PLAN
	case *agentv1.ToolCall_SwitchModeToolCall:
		return aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_SWITCH_MODE
	default:
		return aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_UNSPECIFIED
	}
}

func buildBackgroundComposerFinalResult(tool aiserverv1.ClientSideToolV2, pc PendingToolCall, env *toolResultEnvelope) *aiserverv1.ClientSideToolV2Result {
	res := &aiserverv1.ClientSideToolV2Result{Tool: tool, ToolCallId: pc.ID}
	if env == nil {
		res.Error = &aiserverv1.ToolResultError{ClientVisibleErrorMessage: "empty tool result", ModelVisibleErrorMessage: "empty tool result"}
		return res
	}
	raw := env.ResultJSON
	switch tool {
	case aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_RUN_TERMINAL_COMMAND_V2:
		exitCode := int32(0)
		cwd := ""
		if env != nil && env.ShellAccum != nil {
			exitCode = int32(env.ShellAccum.ExitCode)
			cwd = env.ShellAccum.Cwd
		}
		res.Result = &aiserverv1.ClientSideToolV2Result_RunTerminalCommandV2Result{RunTerminalCommandV2Result: &aiserverv1.RunTerminalCommandV2Result{Output: raw, OutputRaw: raw, ExitCode: exitCode, ExitCodeV2: &exitCode, ResultingWorkingDirectory: cwd, EndedReason: aiserverv1.RunTerminalCommandEndedReason_RUN_TERMINAL_COMMAND_ENDED_REASON_EXECUTION_COMPLETED, NotInterrupted: true}}
	case aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_READ_FILE_V2:
		lineCount := int32(strings.Count(raw, "\n") + 1)
		chars := int32(len(raw))
		res.Result = &aiserverv1.ClientSideToolV2Result_ReadFileV2Result{ReadFileV2Result: &aiserverv1.ReadFileV2Result{Contents: stringPtr(raw), TotalLinesInFile: &lineCount, NumCharactersInRequestedRange: chars}}
	case aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_EDIT_FILE_V2:
		res.Result = &aiserverv1.ClientSideToolV2Result_EditFileV2Result{EditFileV2Result: &aiserverv1.EditFileV2Result{ResultForModel: raw, ContentsAfterEdit: stringPtr(raw)}}
	case aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_LIST_DIR_V2:
		res.Result = &aiserverv1.ClientSideToolV2Result_ListDirV2Result{ListDirV2Result: &aiserverv1.ListDirV2Result{DirectoryTreeRoot: &aiserverv1.ListDirV2Result_DirectoryTreeNode{AbsPath: raw, ChildrenWereProcessed: true}}}
	case aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_RIPGREP_RAW_SEARCH:
		res.Result = &aiserverv1.ClientSideToolV2Result_RipgrepRawSearchResult{RipgrepRawSearchResult: &aiserverv1.RipgrepRawSearchResult{Result: &aiserverv1.RipgrepRawSearchResult_Success{Success: &aiserverv1.RipgrepRawSearchSuccess{Pattern: pc.Arguments, Path: "", OutputMode: "content"}}}}
	case aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_CALL_MCP_TOOL:
		st, err := structpb.NewStruct(map[string]any{"result": raw})
		if err != nil {
			res.Error = &aiserverv1.ToolResultError{ClientVisibleErrorMessage: err.Error(), ModelVisibleErrorMessage: err.Error()}
		} else {
			res.Result = &aiserverv1.ClientSideToolV2Result_CallMcpToolResult{CallMcpToolResult: &aiserverv1.CallMcpToolResult{Result: st}}
		}
	case aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_CREATE_PLAN:
		res.Result = &aiserverv1.ClientSideToolV2Result_CreatePlanResult{CreatePlanResult: &aiserverv1.CreatePlanResult{Result: &aiserverv1.CreatePlanResult_Accepted_{Accepted: &aiserverv1.CreatePlanResult_Accepted{}}}}
	case aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_SWITCH_MODE:
		res.Result = &aiserverv1.ClientSideToolV2Result_SwitchModeResult{SwitchModeResult: &aiserverv1.SwitchModeResult{ToModeId: raw, AutoApproved: true, UserApproved: true}}
	case aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_LIST_MCP_RESOURCES:
		res.Result = &aiserverv1.ClientSideToolV2Result_ListMcpResourcesResult{ListMcpResourcesResult: &aiserverv1.ListMcpResourcesResult{}}
	case aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_READ_MCP_RESOURCE:
		res.Result = &aiserverv1.ClientSideToolV2Result_ReadMcpResourceResult{ReadMcpResourceResult: &aiserverv1.ReadMcpResourceResult{Content: &aiserverv1.ReadMcpResourceResult_Text{Text: raw}}}
	default:
		res.Error = &aiserverv1.ToolResultError{ClientVisibleErrorMessage: "result mapping pending for tool", ModelVisibleErrorMessage: "result mapping pending for tool"}
	}
	return res
}

func stringPtr(s string) *string { return &s }
