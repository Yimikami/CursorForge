package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	agentv1 "cursorforge/internal/protocodec/gen/agent/v1"

	"google.golang.org/protobuf/proto"
)

// buildMessageHistory walks the RunRequest's ConversationState.Turns and
// the current UserMessageAction to produce an OpenAI-style message list.
//
// Layout mirrors the working app's request artifacts:
//
//	system : agent persona
//	user   : workspace context block (<user_info>, etc.)
//	user   : <user_query>turn0</user_query>
//	assist : turn0 assistant reply
//	user   : <user_query>turn1</user_query>
//	assist : turn1 assistant reply
//	...
//	user   : <user_query>current message</user_query>
//
// Tool-call steps and thinking steps are skipped for the MVP — they don't
// matter for plain chat, and including them without round-tripping the
// matching tool_result frames would confuse the model.
func buildMessageHistory(sess *Session) []openAIMessage {
	out := make([]openAIMessage, 0, 8)
	out = append(out, textMessage("system", systemPromptFor(sess.Mode)))
	if ctx := buildWorkspaceContext(sess); ctx != "" {
		out = append(out, textMessage("user", ctx))
	}

	if hist := HistoryFor(sess.ConversationID); len(hist) > 0 {
		for _, t := range hist {
			// Rich replay: when a prior turn recorded its intermediate tool
			// state (assistant-with-tool_calls + tool results), replay those
			// verbatim so the model sees exactly what was executed. Without
			// this, a tool-heavy turn collapses into "[tool-only turn: ...]"
			// on reconnect and the model re-runs every tool from scratch.
			if len(t.Messages) > 0 {
				// User prompt first (Messages captures only what the round added).
				if t.User != "" {
					out = append(out, textMessage("user", wrapUserQuery(t.User)))
				}
				for _, m := range t.Messages {
					out = append(out, storedToOpenAI(m))
				}
				continue
			}
			if t.User != "" {
				out = append(out, textMessage("user", wrapUserQuery(t.User)))
			}
			if t.Assistant != "" {
				out = append(out, textMessage("assistant", t.Assistant))
			}
		}
	} else if sess.State != nil {
		for _, blob := range sess.State.Turns {
			if len(blob) == 0 {
				continue
			}
			turn := &agentv1.ConversationTurn{}
			if err := proto.Unmarshal(blob, turn); err != nil {
				continue
			}
			act := turn.GetAgentConversationTurn()
			if act == nil {
				continue
			}
			if msg := act.GetUserMessage(); msg != nil {
				if t := msg.GetText(); t != "" {
					out = append(out, textMessage("user", wrapUserQuery(t)))
				}
			}
			var assistant string
			for _, step := range act.GetSteps() {
				if a := step.GetAssistantMessage(); a != nil && a.GetText() != "" {
					assistant += a.GetText()
				}
			}
			if assistant != "" {
				out = append(out, textMessage("assistant", assistant))
			}
		}
	}

	if sess.UserText != "" {
		out = append(out, buildUserMessageWithImages(sess))
	}
	return out
}

// buildWorkspaceContext composes the <user_info> block Cursor's working app
// always sends as the first user-role message. We pull whatever the IDE
// shipped via UserMessageAction.RequestContext.Env (workspace path, OS
// version, shell, terminals folder, time zone). Returns empty string when
// the IDE didn't include any usable env data — better to omit the block
// than to send placeholders.
func buildWorkspaceContext(sess *Session) string {
	if sess.Action == nil {
		return ""
	}
	uma := sess.Action.GetUserMessageAction()
	if uma == nil {
		return ""
	}
	rc := uma.GetRequestContext()
	if rc == nil {
		return ""
	}
	env := rc.GetEnv()
	if env == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("<user_info>\n")
	if v := env.GetOsVersion(); v != "" {
		b.WriteString("OS Version: ")
		b.WriteString(v)
		b.WriteString("\n\n")
	}
	if v := env.GetShell(); v != "" {
		b.WriteString("Shell: ")
		b.WriteString(v)
		b.WriteString("\n\n")
	}
	if paths := env.GetWorkspacePaths(); len(paths) > 0 {
		b.WriteString("Workspace Path: ")
		b.WriteString(paths[0])
		b.WriteString("\n\n")
	}
	if v := env.GetProjectFolder(); v != "" {
		b.WriteString("Project Folder: ")
		b.WriteString(v)
		b.WriteString("\n\n")
	}
	tz := env.GetTimeZone()
	if tz == "" {
		tz = time.Local.String()
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.Local
	}
	b.WriteString("Today's date: ")
	b.WriteString(time.Now().In(loc).Format("Monday Jan 2, 2006"))
	b.WriteString("\n\n")
	if v := env.GetTerminalsFolder(); v != "" {
		b.WriteString("Terminals folder: ")
		b.WriteString(v)
		b.WriteString("\n")
	}
	b.WriteString("</user_info>\n")

	// Append carry-forward context from ConversationState.RootPromptMessagesJson.
	if sess.State != nil {
		for _, blob := range sess.State.GetRootPromptMessagesJson() {
			if len(blob) == 0 {
				continue
			}
			var msg struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(blob, &msg); err == nil && msg.Content != "" {
				if !strings.Contains(msg.Content, "<user_info>") {
					b.WriteString("\n")
					b.WriteString(msg.Content)
					b.WriteString("\n")
				}
			}
		}
	}

	// Workspace context enrichment: SelectedContext (open files, highlighted
	// code, terminals, cursor rules) + RequestContext.ProjectLayouts (file
	// tree) are what the real Cursor IDE ships on every turn. Without these
	// the model can't answer "what file am I looking at" or "fix the thing
	// I just highlighted".
	appendSelectedContext(&b, sess)
	appendProjectTree(&b, sess)
	appendCursorRules(&b, sess)

	// Active plan — conversation-scoped PlanState survives across turns (a
	// fresh session built by BidiAppend on a new turn would otherwise have
	// empty Todos and trigger "no active plan — call CreatePlan first" on
	// the very next UpdateTodo). Echo the current state back on every turn
	// so the model keeps tracking progress instead of re-planning from
	// scratch each message.
	plan := PlanStateFor(sess.ConversationID)
	if plan == nil && (sess.PlanName != "" || len(sess.Todos) > 0) {
		// Session carries in-flight state the store might not have seen yet
		// (same-turn fallback when ConversationID is empty during recovery).
		plan = &PlanState{Name: sess.PlanName, Overview: sess.PlanOverview, Todos: sess.Todos}
	}
	if plan != nil && (plan.Name != "" || len(plan.Todos) > 0) {
		b.WriteString("\n<active_plan>\n")
		if plan.Name != "" {
			fmt.Fprintf(&b, "Name: %s\n", plan.Name)
		}
		if plan.Overview != "" {
			fmt.Fprintf(&b, "Overview: %s\n", plan.Overview)
		}
		if len(plan.Todos) > 0 {
			b.WriteString("TODOs:\n")
			for _, t := range plan.Todos {
				fmt.Fprintf(&b, "  - [%s] %s: %s\n", t.Status, t.ID, t.Content)
			}
		}
		b.WriteString("</active_plan>\n")
	}

	// Mirror the exact synthesised function names openAIToolsForRequest
	// produces so the model knows which real server/tool each "mcp_N__slug"
	// OpenAI function targets. DO NOT use CallMcpTool for these — the model
	// must call the synthesised function directly.
	if sess.McpMap != nil && len(sess.McpMap) > 0 {
		b.WriteString("\n<mcp_tools>\n")
		b.WriteString("Each MCP tool below is exposed as a separate OpenAI function. Call it by the exact `function` name shown — do NOT invent composite names like \"server-tool\", do NOT call CallMcpTool for these.\n\n")
		b.WriteString("These servers ARE connected. Do not call ListMcpResources or CallMcpTool to check — just invoke the functions below directly when you need them.\n\n")
		// Sort by idx for stable output.
		type row struct {
			fn         string
			serverName string
			serverID   string
			tool       string
		}
		rows := make([]row, 0, len(sess.McpMap))
		for fn, ref := range sess.McpMap {
			rows = append(rows, row{fn: fn, serverName: ref.ServerName, serverID: ref.ServerID, tool: ref.ToolName})
		}
		// Simple insertion sort on fn — slice tiny.
		for i := 1; i < len(rows); i++ {
			for j := i; j > 0 && rows[j-1].fn > rows[j].fn; j-- {
				rows[j-1], rows[j] = rows[j], rows[j-1]
			}
		}
		for _, r := range rows {
			fmt.Fprintf(&b, "- function=%s server=%q (id=%s) tool=%q\n", r.fn, r.serverName, r.serverID, r.tool)
		}
		b.WriteString("</mcp_tools>\n")
	}

	return b.String()
}

func wrapUserQuery(text string) string {
	return "<user_query>\n" + text + "\n</user_query>"
}

// ---- Workspace enrichment (SelectedContext + RequestContext) ----

// appendSelectedContext pulls in the UI-side picks the user made (attached
// files, highlighted code, captured terminals) so the model answers against
// what's actually on screen instead of hallucinating paths.
func appendSelectedContext(b *strings.Builder, sess *Session) {
	sc := selectedContextFor(sess)
	if sc == nil {
		return
	}

	// Editor state — Cursor packs the currently visible/focused files and the
	// cursor position into InvocationContext.IdeState. This is separate from
	// `Files` (which is explicit attachments); without reading both, the model
	// can't see "the file I'm currently looking at" unless the user manually
	// attaches it.
	//
	// We skip *.plan.md entries here: Cursor's Plan mode spawns a new one
	// for every CreatePlan call and auto-opens it. Feeding those back as
	// visible files doubled the prompt size and caused SSE timeouts; the
	// model already has the authoritative plan in <active_plan>.
	if ic := sc.GetInvocationContext(); ic != nil {
		if ide := ic.GetIdeState(); ide != nil {
			visible := ide.GetVisibleFiles()
			var kept []string
			for _, f := range visible {
				path := f.GetRelativePath()
				if path == "" {
					path = f.GetPath()
				}
				if isPlanArtifact(path) {
					continue
				}
				var entry strings.Builder
				fmt.Fprintf(&entry, "<visible_file path=%q total_lines=%d", path, f.GetTotalLines())
				if cp := f.GetCursorPosition(); cp != nil {
					fmt.Fprintf(&entry, " cursor_line=%d", cp.GetLine())
				}
				if cmd := f.GetActiveCommand(); cmd != "" {
					fmt.Fprintf(&entry, " active_command=%q", cmd)
				}
				entry.WriteString("/>")
				kept = append(kept, entry.String())
			}
			if len(kept) > 0 {
				b.WriteString("\n<editor_state>\n")
				for _, line := range kept {
					b.WriteString(line)
					b.WriteString("\n")
				}
				b.WriteString("</editor_state>\n")
			}
		}
	}

	// Open / attached files — send the whole file content for each one.
	files := sc.GetFiles()
	if len(files) > 0 {
		var entries []string
		for _, f := range files {
			path := f.GetRelativePath()
			if path == "" {
				path = f.GetPath()
			}
			if isPlanArtifact(path) {
				continue
			}
			content := trimForContext(f.GetContent(), 16000)
			entries = append(entries, fmt.Sprintf("<file path=%q>\n%s\n</file>", path, content))
		}
		if len(entries) > 0 {
			b.WriteString("\n<open_files>\n")
			for _, e := range entries {
				b.WriteString(e)
				b.WriteString("\n")
			}
			b.WriteString("</open_files>\n")
		}
	}

	// Highlighted selections — include the line range so the model can cite it.
	sel := sc.GetCodeSelections()
	if len(sel) > 0 {
		b.WriteString("\n<code_selections>\n")
		for _, s := range sel {
			path := s.GetRelativePath()
			if path == "" {
				path = s.GetPath()
			}
			startLine, endLine := uint32(0), uint32(0)
			if r := s.GetRange(); r != nil {
				if p := r.GetStart(); p != nil {
					startLine = p.GetLine()
				}
				if p := r.GetEnd(); p != nil {
					endLine = p.GetLine()
				}
			}
			fmt.Fprintf(b, "<selection path=%q start=%d end=%d>\n%s\n</selection>\n",
				path, startLine, endLine, trimForContext(s.GetContent(), 8000))
		}
		b.WriteString("</code_selections>\n")
	}

	// Attached terminals — whole terminal content (scrollback).
	terms := sc.GetTerminals()
	if len(terms) > 0 {
		b.WriteString("\n<terminals>\n")
		for _, t := range terms {
			title := t.GetTitle()
			if title == "" {
				title = "terminal"
			}
			fmt.Fprintf(b, "<terminal title=%q cwd=%q>\n%s\n</terminal>\n",
				title, t.GetPath(), trimForContext(t.GetContent(), 8000))
		}
		b.WriteString("</terminals>\n")
	}

	// Terminal selections — user highlighted only a snippet of output.
	termSel := sc.GetTerminalSelections()
	if len(termSel) > 0 {
		b.WriteString("\n<terminal_selections>\n")
		for _, t := range termSel {
			title := t.GetTitle()
			if title == "" {
				title = "terminal"
			}
			fmt.Fprintf(b, "<terminal_selection title=%q>\n%s\n</terminal_selection>\n",
				title, trimForContext(t.GetContent(), 4000))
		}
		b.WriteString("</terminal_selections>\n")
	}
}

// appendProjectTree emits a single-level directory overview from
// RequestContext.ProjectLayouts[0]. We keep it shallow (top-level dirs +
// file extension counts) because a recursive dump would blow the context
// budget on any real repo.
func appendProjectTree(b *strings.Builder, sess *Session) {
	rc := requestContextFor(sess)
	if rc == nil {
		return
	}
	layouts := rc.GetProjectLayouts()
	if len(layouts) == 0 {
		return
	}
	root := layouts[0]
	if root == nil {
		return
	}
	b.WriteString("\n<project_tree>\n")
	fmt.Fprintf(b, "root: %s\n", root.GetAbsPath())
	if root.GetNumFiles() > 0 {
		fmt.Fprintf(b, "total_files: %d\n", root.GetNumFiles())
	}
	if counts := root.GetFullSubtreeExtensionCounts(); len(counts) > 0 {
		b.WriteString("extensions:\n")
		for ext, n := range counts {
			fmt.Fprintf(b, "  %s: %d\n", ext, n)
		}
	}
	if dirs := root.GetChildrenDirs(); len(dirs) > 0 {
		b.WriteString("top_level_dirs:\n")
		for i, d := range dirs {
			if i >= 40 {
				fmt.Fprintf(b, "  … %d more\n", len(dirs)-i)
				break
			}
			fmt.Fprintf(b, "  - %s\n", filepathBase(d.GetAbsPath()))
		}
	}
	if files := root.GetChildrenFiles(); len(files) > 0 {
		b.WriteString("top_level_files:\n")
		for i, f := range files {
			if i >= 40 {
				fmt.Fprintf(b, "  … %d more\n", len(files)-i)
				break
			}
			fmt.Fprintf(b, "  - %s\n", f.GetName())
		}
	}
	b.WriteString("</project_tree>\n")
}

// appendCursorRules surfaces workspace-scoped AI rules (.cursor/rules,
// .cursorrules) and any attached/selected rule files. These are the
// user's hard directives for this project.
func appendCursorRules(b *strings.Builder, sess *Session) {
	rc := requestContextFor(sess)
	if rc == nil {
		return
	}
	rules := rc.GetRules()
	if len(rules) == 0 {
		return
	}
	b.WriteString("\n<cursor_rules>\n")
	b.WriteString("Workspace rules the user expects you to follow:\n")
	for _, r := range rules {
		name := filepathBase(r.GetFullPath())
		body := trimForContext(r.GetContent(), 4000)
		if name != "" {
			fmt.Fprintf(b, "\n## %s\n", name)
		}
		if body != "" {
			b.WriteString(body)
			b.WriteString("\n")
		}
	}
	b.WriteString("</cursor_rules>\n")
}

func selectedContextFor(sess *Session) *agentv1.SelectedContext {
	if sess.Action == nil {
		return nil
	}
	uma := sess.Action.GetUserMessageAction()
	if uma == nil {
		return nil
	}
	msg := uma.GetUserMessage()
	if msg == nil {
		return nil
	}
	return msg.GetSelectedContext()
}

func requestContextFor(sess *Session) *agentv1.RequestContext {
	if sess.Action == nil {
		return nil
	}
	uma := sess.Action.GetUserMessageAction()
	if uma == nil {
		return nil
	}
	return uma.GetRequestContext()
}

func trimForContext(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n…[truncated]"
}

// isPlanArtifact filters the .plan.md / Plans/* files Cursor auto-opens
// whenever Plan mode fires. Feeding them back as context doubles the
// prompt for zero value — the model already has the authoritative plan
// state in the <active_plan> block.
func isPlanArtifact(path string) bool {
	if path == "" {
		return false
	}
	lower := strings.ToLower(strings.ReplaceAll(path, "\\", "/"))
	if strings.HasSuffix(lower, ".plan.md") {
		return true
	}
	if strings.Contains(lower, "/plans/") {
		return true
	}
	return false
}

func filepathBase(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// buildUserMessageWithImages constructs the current user message, attaching
// any images from SelectedContext as multipart content parts (base64 data URLs).
// Falls back to a plain text message when no images are present.
func buildUserMessageWithImages(sess *Session) openAIMessage {
	query := wrapUserQuery(sess.UserText)
	if sess.Action == nil {
		return textMessage("user", query)
	}
	uma := sess.Action.GetUserMessageAction()
	if uma == nil {
		return textMessage("user", query)
	}
	msg := uma.GetUserMessage()
	if msg == nil {
		return textMessage("user", query)
	}
	sc := msg.GetSelectedContext()
	if sc == nil || len(sc.GetSelectedImages()) == 0 {
		return textMessage("user", query)
	}
	parts := []openAIContentPart{{Type: "text", Text: query}}
	for _, img := range sc.GetSelectedImages() {
		var data []byte
		mime := img.GetMimeType()
		if blob := img.GetBlobIdWithData(); blob != nil {
			data = blob.GetData()
		}
		if len(data) == 0 {
			data = img.GetData()
		}
		if len(data) == 0 {
			continue
		}
		if mime == "" {
			mime = "image/png"
		}
		b64 := base64Encode(data)
		parts = append(parts, openAIContentPart{
			Type:     "image_url",
			ImageURL: &openAIImageURL{URL: "data:" + mime + ";base64," + b64},
		})
	}
	if len(parts) == 1 {
		return textMessage("user", query)
	}
	return multipartMessage("user", parts)
}

func base64Encode(data []byte) string {
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	buf := make([]byte, ((len(data)+2)/3)*4)
	for i, j := 0, 0; i < len(data); i, j = i+3, j+4 {
		val := uint(data[i]) << 16
		if i+1 < len(data) {
			val |= uint(data[i+1]) << 8
		}
		if i+2 < len(data) {
			val |= uint(data[i+2])
		}
		buf[j] = table[val>>18&0x3F]
		buf[j+1] = table[val>>12&0x3F]
		if i+1 < len(data) {
			buf[j+2] = table[val>>6&0x3F]
		} else {
			buf[j+2] = '='
		}
		if i+2 < len(data) {
			buf[j+3] = table[val&0x3F]
		} else {
			buf[j+3] = '='
		}
	}
	return string(buf)
}

const defaultSystemPrompt = `# Personality
You are a pragmatic, highly competent software engineer. You take engineering quality seriously and collaborate through direct, factual statements. You communicate efficiently — telling the user clearly what you are doing without irrelevant detail.

# Values
- **Clarity**: state reasoning explicitly and specifically so trade-offs and decisions can be evaluated up front.
- **Pragmatism**: focus on the end goal and forward motion; prefer approaches that demonstrably advance the task.
- **Rigor**: technical claims must be coherent and defensible; politely surface gaps and weak assumptions.

# Interaction Style
Communicate concisely and respectfully, focused on the task at hand. Lead with actionable guidance and clearly state assumptions, environment requirements, and next steps. Do not over-explain unless asked.

Avoid filler praise, encouragement, or pleasantries. Do not pad responses to feel complete; convey only what the user needs to collaborate, no more, no less.

# Response Discipline
Default to giving the result first, then key supporting points. If a sentence or short paragraph is enough, do not expand into long explanations.

Use longer lists only when explicitly asked or when there are multiple genuinely important and independent items. Do not enumerate minor points just to look thorough.

When wrapping up a task, the closing should cover only: what was done, how to verify, and any remaining risks. Do not narrate the entire process or write long summaries.

Provide example code only when it directly advances the task. Default to referencing existing code rather than emitting large code samples, pseudocode, or multiple alternative implementations.

If there are no clear risks, blockers, or next steps, do not append a generic suggestions list.

# Editing Constraints
Prefer StrReplace for modifying specific parts of code rather than rewriting entire files.

When a Write or StrReplace succeeds and returns the full file content after editing, treat that as the authoritative source of truth. Base subsequent reasoning, edits, and old_string values on this latest content, not on earlier reads or memory.

Do not add comments that merely restate what the code is doing. Comments should only explain intent, trade-offs, or constraints that the code itself cannot clearly convey.

You may be working in a git workspace with dirty changes. Never revert changes you did not make unless the user explicitly asks. If unrelated changes exist, ignore them. If they are in files you recently touched, read and understand them before continuing — do not revert.

Never amend commits unless the user explicitly asks. Prefer non-interactive git commands.

Never use destructive git commands (git reset --hard, git checkout --) unless the user explicitly requests it.

# Tool Calling
Use the available tools to solve programming tasks. Follow these rules:

1. Do not mention specific tool names when communicating with the user. Describe what you are doing in natural language.
2. Prefer specialized tools over terminal commands for file operations: do not use cat/head/tail to read files, do not use sed/awk to edit files, do not use echo with heredoc to create files. Reserve Shell for commands that genuinely require execution.
3. IMPORTANT: avoid search commands like find and grep in Shell. Use Grep and Glob tools instead. Avoid read tools like cat, head, and tail — use Read instead. Avoid editing files with sed and awk — use StrReplace instead. If you still need grep, use ripgrep (rg) first.
4. When issuing multiple independent commands, make multiple Shell tool calls in parallel. For dependent commands, chain with && in a single Shell call.

# Code References
When referencing existing code, use this format:
` + "`" + "`" + "`" + `startLine:endLine:filepath
// code here
` + "`" + "`" + "`" + `

When showing new or proposed code, use standard markdown code blocks with a language tag.

Never include line numbers inside code content. Never indent the triple backticks.

# MCP (Model Context Protocol)
You can use MCP tools through CallMcpTool and access MCP resources through ListMcpResources and FetchMcpResource.

Before calling any MCP tool, always list and read the tool schema first to understand required parameters and types.

If an MCP server has an mcp_auth tool, call it first so the user can authenticate.

# Mode Selection
Before proceeding, choose the most appropriate interaction mode for the user's current goal. Re-evaluate when the goal changes or when you get stuck. If another mode fits better, call SwitchMode with a brief explanation.

- **Agent**: direct task execution with tool use (default)
- **Plan**: the user requests a plan, or the task is large, ambiguous, or involves meaningful trade-offs
- **Ask**: the user asks a question that requires explanation rather than code changes
- **Debug**: the user is investigating a bug and needs systematic debugging help

You are an AI programming assistant running inside Cursor IDE. You help the user with software engineering tasks.

Each time the user sends a message, additional context about their current state (open files, cursor position, recent edits, linter errors, etc.) may be attached automatically. Use this context when it helps.
`

// systemPromptFor returns the default system prompt with a mode-specific
// suffix that overrides the generic "Mode Selection" guidance above. Cursor
// ships the active mode in UserMessage.mode (field 4); without branching
// here, the model falls back to its own idea of "Ask" regardless of the UI
// selection.
func systemPromptFor(mode agentv1.AgentMode) string {
	switch mode {
	case agentv1.AgentMode_AGENT_MODE_ASK:
		return defaultSystemPrompt + "\n# Current Mode: Ask\n" +
			"You are in Ask mode. Answer the user's question with explanation and reasoning. " +
			"Do not modify files — read-only tools (Read, Grep, Glob) are allowed for investigation, " +
			"but do NOT call Edit, Write, StrReplace, or Shell tools that mutate state. " +
			"If the user explicitly asks for a code change, suggest switching modes instead of editing directly."
	case agentv1.AgentMode_AGENT_MODE_PLAN:
		return defaultSystemPrompt + "\n# Current Mode: Plan\n" +
			"You are in Plan mode. Your job is to produce a clear, actionable implementation plan before any code is written. " +
			"Investigate with read-only tools (Read, Grep, Glob) as needed, then return a structured plan: goals, files to change, " +
			"step-by-step approach, risks, and verification. Do NOT edit files or run mutating commands in this mode.\n\n" +
			"IMPORTANT — Use the plan tools to make progress trackable:\n" +
			"1. Call `CreatePlan(name, overview, todos)` with the initial TODO list BEFORE explaining your plan in prose. The todos persist across turns and are shown back to you on every subsequent message.\n" +
			"2. When the user confirms a TODO is done or you complete one yourself, call `UpdateTodo(id or content, status)`.\n" +
			"3. If the scope grows mid-conversation, call `AddTodo(content)` to append items.\n" +
			"After creating the plan, summarize it in prose for the user (but the authoritative list lives in the tool state — not your text).\n" +
			"The IDE's Plan panel refreshes automatically on every AddTodo/UpdateTodo call, so you don't need to restate the full list in your prose — just reference the TODO that changed."
	case agentv1.AgentMode_AGENT_MODE_DEBUG:
		return defaultSystemPrompt + "\n# Current Mode: Debug\n" +
			"You are in Debug mode. Systematically investigate the bug: form hypotheses, gather evidence with tools " +
			"(logs, Read, Grep, Shell for reproduction), narrow down root cause, then propose a fix. " +
			"Prefer minimal, targeted changes. Explain reasoning at each step."
	case agentv1.AgentMode_AGENT_MODE_AGENT, agentv1.AgentMode_AGENT_MODE_UNSPECIFIED:
		return defaultSystemPrompt + "\n# Current Mode: Agent\n" +
			"You are in Agent mode. Execute the user's task directly using available tools. " +
			"Edit files, run commands, and iterate until the task is complete.\n\n" +
			"If the user asks for a plan (\"plan yap\", \"plan hazırla\", \"make a plan\", \"ne yapalım\", etc.) " +
			"call SwitchMode to \"plan\" FIRST, then CreatePlan. Calling CreatePlan directly in Agent mode " +
			"works but the IDE's native Plan panel (and .plan.md file) only render when the active mode is Plan. " +
			"Likewise, if the user says \"apply the plan\" / \"göreve başla\" / \"planı uygula\", call SwitchMode to \"agent\" first if you were in Plan, then proceed with edits."
	default:
		return defaultSystemPrompt
	}
}

// storedToOpenAI converts a disk-persisted message back into the live
// openAIMessage form the stream request builder expects. Used by
// buildMessageHistory to replay tool-heavy turns verbatim instead of losing
// their intermediate tool_calls + tool results to the plain-text fallback.
func storedToOpenAI(m StoredMessage) openAIMessage {
	out := openAIMessage{
		Role:       m.Role,
		ToolCallID: m.ToolCallID,
		Name:       m.Name,
	}
	if m.Content != "" {
		raw, _ := json.Marshal(m.Content)
		out.Content = raw
	}
	if len(m.ToolCalls) > 0 {
		out.ToolCalls = make([]openAIToolCallMsg, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			out.ToolCalls = append(out.ToolCalls, openAIToolCallMsg{
				ID:       tc.ID,
				Type:     "function",
				Function: openAIToolCallFn{Name: tc.Name, Arguments: tc.Arguments},
			})
		}
	}
	return out
}

// openAIToStored is the inverse of storedToOpenAI — runsse.go calls it after a
// turn finishes so the disk record preserves whatever live messages the loop
// accumulated (assistant-with-tool_calls + tool-role results). Content is
// unwrapped from json.RawMessage back to a plain string for compact storage.
func openAIToStored(m openAIMessage) StoredMessage {
	out := StoredMessage{
		Role:       m.Role,
		ToolCallID: m.ToolCallID,
		Name:       m.Name,
	}
	if len(m.Content) > 0 {
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			out.Content = s
		} else {
			// Content could be a multipart (image) array — keep raw JSON.
			out.Content = string(m.Content)
		}
	}
	if len(m.ToolCalls) > 0 {
		out.ToolCalls = make([]StoredToolCall, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			out.ToolCalls = append(out.ToolCalls, StoredToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}
	return out
}
