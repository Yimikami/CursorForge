package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "cursorforge/internal/protocodec/gen/agent/v1"
	aiserverv1 "cursorforge/internal/protocodec/gen/aiserver/v1"
)

var BugBotSSEHeaders = http.Header{
	"Content-Type":             {"text/event-stream"},
	"Cache-Control":            {"no-cache"},
	"Connect-Content-Encoding": {"gzip"},
	"Connect-Accept-Encoding":  {"gzip"},
}

const bugBotMaxLoopRounds = 20

type bugBotReportItem struct {
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Severity    string  `json:"severity"`
	Confidence  float32 `json:"confidence"`
	Rationale   string  `json:"rationale"`
	File        string  `json:"file"`
	StartLine   int32   `json:"start_line"`
	EndLine     int32   `json:"end_line"`
}

func HandleWriteGitCommitMessage(ctx context.Context, reqBody []byte, contentType string, resolve AdapterResolver) Result {
	req := &aiserverv1.WriteGitCommitMessageRequest{}
	if err := decodeUnary(reqBody, contentType, req); err != nil {
		return errResult(http.StatusBadRequest, "decode write git commit message: "+err.Error())
	}
	target, ok := ResolveAdapterForModel(resolve(), "")
	if !ok {
		return errResult(http.StatusBadRequest, "no BYOK adapter configured")
	}
	stream := pickProviderStreamer(target.ProviderType)
	message, err := generateCommitMessage(ctx, req, target.BaseURL, target.APIKey, target.Model, target.Opts, stream)
	if err != nil {
		return errResult(http.StatusBadGateway, "generate commit message: "+err.Error())
	}
	body, err := proto.Marshal(&aiserverv1.WriteGitCommitMessageResponse{CommitMessage: message})
	if err != nil {
		return errResult(http.StatusInternalServerError, "marshal commit message response: "+err.Error())
	}
	return Result{Status: http.StatusOK, ContentType: "application/proto", Body: body}
}

func HandleBugBotRunSSE(ctx context.Context, reqBody []byte, contentType string, rawWriter io.Writer, resolve AdapterResolver) {
	w := &lockedWriter{w: rawWriter}
	bid := &aiserverv1.BidiRequestId{}
	if err := decodeUnary(reqBody, contentType, bid); err != nil {
		writeBugBotEndStreamError(w, "decode bugbot request id: "+err.Error())
		return
	}
	requestID := bid.GetRequestId()
	sess := WaitForSession(ctx, requestID)
	if sess == nil || sess.BugBotRequest == nil {
		writeBugBotEndStreamError(w, "no bugbot session for request_id="+requestID)
		return
	}
	target, ok := ResolveAdapterForBugBotRequest(resolve(), sess.BugBotRequest)
	if !ok {
		writeBugBotEndStreamError(w, "no BYOK adapter configured")
		return
	}
	stream := pickProviderStreamer(target.ProviderType)

	keepaliveCtx, stopKeepalive := context.WithCancel(ctx)
	defer stopKeepalive()
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-keepaliveCtx.Done():
				return
			case <-t.C:
				_ = writeBugBotStatusFrame(w, aiserverv1.BugBotStatus_STATUS_IN_PROGRESS, "Review in progress")
			}
		}
	}()

	resp, err := runBugBotReview(ctx, sess, target.BaseURL, target.APIKey, target.Model, target.Opts, stream, w)
	if err != nil {
		writeBugBotEndStreamError(w, err.Error())
		DropSession(requestID)
		return
	}
	if err := writeBugBotResponseFrame(w, resp); err != nil {
		writeBugBotEndStreamError(w, err.Error())
		DropSession(requestID)
		return
	}
	if err := writeEndStream(w); err != nil {
		DropSession(requestID)
		return
	}
	DropSession(requestID)
}

func pickProviderStreamer(providerType string) providerStreamer {
	if strings.EqualFold(providerType, "anthropic") {
		return streamAnthropic
	}
	return streamOpenAI
}

func generateCommitMessage(ctx context.Context, req *aiserverv1.WriteGitCommitMessageRequest, baseURL, apiKey, model string, opts AdapterOpts, stream providerStreamer) (string, error) {
	prompt := buildCommitMessagePrompt(req)
	fastOpts := opts
	fastOpts.ReasoningEffort = ""
	fastOpts.ServiceTier = ""
	if fastOpts.MaxOutputTokens <= 0 || fastOpts.MaxOutputTokens > 120 {
		fastOpts.MaxOutputTokens = 120
	}
	result, err := collectSingleResponse(ctx, stream, baseURL, apiKey, model, fastOpts, []openAIMessage{
		textMessage("system", "Write a concise conventional commit message. Default to a single subject line in the form type: summary, where type is usually feat, fix, refactor, docs, chore, or test. Add a body only if absolutely necessary. Return only the commit message text."),
		textMessage("user", prompt),
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result), nil
}

func buildCommitMessagePrompt(req *aiserverv1.WriteGitCommitMessageRequest) string {
	var b strings.Builder
	b.WriteString("Write a conventional commit message for these changes. Prefer `feat:`, `fix:`, `refactor:`, `docs:`, `test:`, or `chore:` as appropriate. Focus on intent and user-visible impact, not a file-by-file changelog.\n\n")
	if prev := req.GetPreviousCommitMessages(); len(prev) > 0 {
		b.WriteString("Match this style:\n")
		for _, m := range prev {
			if s := strings.TrimSpace(m); s != "" {
				b.WriteString("- ")
				b.WriteString(s)
				b.WriteString("\n")
			}
		}
		b.WriteString("\n")
	}
	if ec := req.GetExplicitContext(); ec != nil {
		if ctx := strings.TrimSpace(ec.GetContext()); ctx != "" {
			b.WriteString("Explicit context:\n")
			b.WriteString(ctx)
			b.WriteString("\n\n")
		}
		if repo := strings.TrimSpace(ec.GetRepoContext()); repo != "" {
			b.WriteString("Repository context:\n")
			b.WriteString(repo)
			b.WriteString("\n\n")
		}
		if mode := strings.TrimSpace(ec.GetModeSpecificContext()); mode != "" {
			b.WriteString("Mode-specific context:\n")
			b.WriteString(mode)
			b.WriteString("\n\n")
		}
	}
	b.WriteString("Diffs:\n")
	for i, d := range req.GetDiffs() {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(d)
	}
	return b.String()
}

func runBugBotReview(ctx context.Context, sess *Session, baseURL, apiKey, model string, opts AdapterOpts, stream providerStreamer, w io.Writer) (*aiserverv1.StreamBugBotResponse, error) {
	if sess == nil || sess.BugBotRequest == nil {
		return nil, fmt.Errorf("missing bugbot request")
	}
	messages := []openAIMessage{
		textMessage("system", "You are a strict code reviewer. Find only real bugs or meaningful regressions in the provided diff. Use tools when needed. Return either 'NO_ISSUES' or a JSON array. Each array item must contain: title, description, severity, confidence, rationale, file, start_line, end_line."),
		textMessage("user", sess.UserText),
	}
	tools := openAIToolsForRequest(sess)
	var answer strings.Builder
	totalIterations := int32(bugBotMaxLoopRounds)
	iterationsCompleted := int32(0)
	for round := 0; round < bugBotMaxLoopRounds; round++ {
		iterationsCompleted = int32(round + 1)
		_ = writeBugBotResponseFrame(w, &aiserverv1.StreamBugBotResponse{
			Status: &aiserverv1.BugBotStatus{
				Status:              aiserverv1.BugBotStatus_STATUS_IN_PROGRESS_ITERATIONS,
				Message:             fmt.Sprintf("Review iteration %d", round+1),
				IterationsCompleted: &iterationsCompleted,
				TotalIterations:     &totalIterations,
			},
			NumTurns: &iterationsCompleted,
		})
		result, err := stream(ctx, baseURL, apiKey, model, messages, tools, opts, func(chunk string, reasoning string, done bool) error {
			if chunk != "" {
				answer.WriteString(chunk)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if len(result.ToolCalls) == 0 {
			break
		}
		assistantMsg := openAIMessage{Role: "assistant"}
		for _, tc := range result.ToolCalls {
			assistantMsg.ToolCalls = append(assistantMsg.ToolCalls, openAIToolCallMsg{ID: tc.ID, Type: "function", Function: openAIToolCallFn{Name: tc.Name, Arguments: tc.Arguments}})
		}
		messages = append(messages, assistantMsg)
		for _, pc := range result.ToolCalls {
			tc, buildErr := buildToolCallProto(sess, pc)
			if buildErr != "" {
				messages = append(messages, toolResultMessage(pc.ID, pc.Name, `{"error":`+jsonString(buildErr)+`}`))
				continue
			}
			if tc == nil {
				messages = append(messages, toolResultMessage(pc.ID, pc.Name, `{"error":"tool not implemented"}`))
				continue
			}
			execID := newExecID(execToolName(pc.Name))
			seq := nextExecSeq()
			waitCh := registerToolWait(pc.ID)
			registerExecIDAlias(execID, seq, pc.ID)
			if err := writeBugBotExecRequest(w, execID, seq, pc, tc); err != nil {
				return nil, err
			}
			env := waitForToolResult(waitCh, 30*time.Second)
			content := env.ResultJSON
			if env.Error != "" {
				content = `<bugbot-tool-error>` + env.Error + `</bugbot-tool-error>`
			}
			if len(content) > 24*1024 {
				content = content[:24*1024] + "\n…[bugbot tool output truncated]"
			}
			messages = append(messages, toolResultMessage(pc.ID, pc.Name, content))
		}
		if answer.Len() > 0 {
			break
		}
	}
	bugs, summary := parseBugBotResponse(answer.String())
	status := &aiserverv1.BugBotStatus{Status: aiserverv1.BugBotStatus_STATUS_DONE, IterationsCompleted: &iterationsCompleted, TotalIterations: &totalIterations}
	if len(bugs) == 0 {
		status.Message = "No issues found"
		return &aiserverv1.StreamBugBotResponse{Status: status, Summary: protoString(summary), NumTurns: &iterationsCompleted}, nil
	}
	status.Message = fmt.Sprintf("Found %d issue(s)", len(bugs))
	return &aiserverv1.StreamBugBotResponse{
		BugReports: &aiserverv1.BugReports{BugReports: bugs},
		Status:     status,
		Summary:    protoString(summary),
		NumTurns:   &iterationsCompleted,
	}, nil
}

func writeBugBotExecRequest(w io.Writer, execID string, execSeq uint32, pc PendingToolCall, tc any) error {
	var msg proto.Message
	agentTC, ok := tc.(*agentv1.ToolCall)
	if !ok {
		return fmt.Errorf("bugbot exec request: expected *agentv1.ToolCall")
	}
	esm := &agentv1.ExecServerMessage{Id: execSeq, ExecId: execID}
	switch inner := agentTC.Tool.(type) {
	case *agentv1.ToolCall_ShellToolCall:
		esm.Message = &agentv1.ExecServerMessage_ShellStreamArgs{ShellStreamArgs: inner.ShellToolCall.Args}
	case *agentv1.ToolCall_ReadToolCall:
		esm.Message = &agentv1.ExecServerMessage_ReadArgs{ReadArgs: &agentv1.ReadArgs{Path: inner.ReadToolCall.Args.Path, ToolCallId: pc.ID}}
	case *agentv1.ToolCall_DeleteToolCall:
		esm.Message = &agentv1.ExecServerMessage_DeleteArgs{DeleteArgs: inner.DeleteToolCall.Args}
	case *agentv1.ToolCall_EditToolCall:
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
		esm.Message = &agentv1.ExecServerMessage_LsArgs{LsArgs: &agentv1.LsArgs{Path: dir, ToolCallId: pc.ID}}
	case *agentv1.ToolCall_GrepToolCall:
		esm.Message = &agentv1.ExecServerMessage_GrepArgs{GrepArgs: inner.GrepToolCall.Args}
	case *agentv1.ToolCall_McpToolCall:
		esm.Message = &agentv1.ExecServerMessage_McpArgs{McpArgs: inner.McpToolCall.Args}
	case *agentv1.ToolCall_ReadMcpResourceToolCall:
		esm.Message = &agentv1.ExecServerMessage_ReadMcpResourceExecArgs{ReadMcpResourceExecArgs: inner.ReadMcpResourceToolCall.Args}
	case *agentv1.ToolCall_ListMcpResourcesToolCall:
		esm.Message = &agentv1.ExecServerMessage_ListMcpResourcesExecArgs{ListMcpResourcesExecArgs: inner.ListMcpResourcesToolCall.Args}
	default:
		return fmt.Errorf("unsupported bugbot exec tool %T", inner)
	}
	raw, err := proto.Marshal(esm)
	if err != nil {
		return err
	}
	out := &aiserverv1.ExecServerMessage{}
	if err := proto.Unmarshal(raw, out); err != nil {
		return err
	}
	msg = &aiserverv1.StreamBugBotAgenticServerMessage{Message: &aiserverv1.StreamBugBotAgenticServerMessage_ExecServerMessage{ExecServerMessage: out}}
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

func collectSingleResponse(ctx context.Context, stream providerStreamer, baseURL, apiKey, model string, opts AdapterOpts, messages []openAIMessage) (string, error) {
	var mu sync.Mutex
	var out strings.Builder
	_, err := stream(ctx, baseURL, apiKey, model, messages, nil, opts, func(chunk string, reasoning string, done bool) error {
		mu.Lock()
		defer mu.Unlock()
		if reasoning != "" {
			return nil
		}
		if chunk != "" {
			out.WriteString(chunk)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	res := strings.TrimSpace(out.String())
	if res == "" {
		return "", fmt.Errorf("empty model response")
	}
	return res, nil
}

func parseBugBotResponse(answer string) ([]*aiserverv1.BugReport, string) {
	trimmed := strings.TrimSpace(answer)
	if trimmed == "" || strings.EqualFold(trimmed, "NO_ISSUES") {
		return nil, "No issues found"
	}
	type wrapped struct {
		Summary string `json:"summary"`
		Bugs    []bugBotReportItem `json:"bugs"`
		Issues  []bugBotReportItem `json:"issues"`
		Reports []bugBotReportItem `json:"reports"`
	}
	var parsed []bugBotReportItem
	if err := json.Unmarshal([]byte(extractJSONArray(trimmed)), &parsed); err != nil || len(parsed) == 0 {
		var wrap wrapped
		if err2 := json.Unmarshal([]byte(extractJSONObject(trimmed)), &wrap); err2 == nil {
			switch {
			case len(wrap.Bugs) > 0:
				parsed = wrap.Bugs
			case len(wrap.Issues) > 0:
				parsed = wrap.Issues
			case len(wrap.Reports) > 0:
				parsed = wrap.Reports
			}
			if len(parsed) > 0 {
				summary := strings.TrimSpace(wrap.Summary)
				if summary == "" {
					summary = fmt.Sprintf("Found %d issue(s)", len(parsed))
				}
				return toBugReports(parsed), summary
			}
		}
		return []*aiserverv1.BugReport{{
			Id:          "bug-1",
			Title:       protoString("Potential issue"),
			Description: trimmed,
			Severity:    protoString("medium"),
		}}, "Found 1 issue"
	}
	return toBugReports(parsed), fmt.Sprintf("Found %d issue(s)", len(parsed))
}

func toBugReports(parsed []bugBotReportItem) []*aiserverv1.BugReport {
	out := make([]*aiserverv1.BugReport, 0, len(parsed))
	for i, p := range parsed {
		bug := &aiserverv1.BugReport{
			Id:          fmt.Sprintf("bug-%d", i+1),
			Description: p.Description,
			Title:       protoString(defaultString(p.Title, p.Description)),
			Severity:    protoString(defaultString(p.Severity, "medium")),
			Rationale:   protoString(p.Rationale),
		}
		if p.Confidence > 0 {
			bug.Confidence = &p.Confidence
		}
		if p.File != "" {
			loc := &aiserverv1.BugLocation{File: p.File, StartLine: maxInt32(p.StartLine, 1), EndLine: maxInt32(maxInt32(p.EndLine, p.StartLine), 1)}
			bug.Locations = []*aiserverv1.BugLocation{loc}
		}
		out = append(out, bug)
	}
	return out
}

func extractJSONArray(s string) string {
	s = stripCodeFence(strings.TrimSpace(s))
	start := strings.IndexByte(s, '[')
	end := strings.LastIndexByte(s, ']')
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}

func extractJSONObject(s string) string {
	s = stripCodeFence(strings.TrimSpace(s))
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}

func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) == 0 {
		return s
	}
	if strings.HasPrefix(lines[0], "```") {
		lines = lines[1:]
	}
	if n := len(lines); n > 0 && strings.TrimSpace(lines[n-1]) == "```" {
		lines = lines[:n-1]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func defaultString(v, fallback string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return fallback
	}
	return v
}

func protoString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func maxInt32(v, min int32) int32 {
	if v < min {
		return min
	}
	return v
}

func writeBugBotResponseFrame(w io.Writer, resp *aiserverv1.StreamBugBotResponse) error {
	msg := &aiserverv1.StreamBugBotAgenticServerMessage{
		Message: &aiserverv1.StreamBugBotAgenticServerMessage_BugbotResponse{BugbotResponse: resp},
	}
	body, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal bugbot response: %w", err)
	}
	if err := writeFrame(w, body, false); err != nil {
		return err
	}
	flushIfPossible(w)
	return nil
}

func writeBugBotStatusFrame(w io.Writer, status aiserverv1.BugBotStatus_Status, message string) error {
	return writeBugBotResponseFrame(w, &aiserverv1.StreamBugBotResponse{
		Status: &aiserverv1.BugBotStatus{Status: status, Message: message},
	})
}

func writeBugBotEndStreamError(w io.Writer, msg string) {
	_ = writeBugBotResponseFrame(w, &aiserverv1.StreamBugBotResponse{
		Status: &aiserverv1.BugBotStatus{Status: aiserverv1.BugBotStatus_STATUS_ERROR, Message: msg},
		Summary: protoString(msg),
	})
	_ = writeEndStream(w)
}

func routeBugBotExecClientResult(msg *aiserverv1.ExecClientMessage) {
	if msg == nil {
		return
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return
	}
	env := &toolResultEnvelope{ResultJSON: string(body), Error: ""}
	if execID := msg.GetExecId(); execID != "" {
		deliverToolResult(execID, env)
		return
	}
	pendingMu.Lock()
	callID := seqAlias[msg.GetId()]
	if callID != "" {
		delete(seqAlias, msg.GetId())
	}
	pendingMu.Unlock()
	if callID != "" {
		deliverToolResult(callID, env)
	}
}
