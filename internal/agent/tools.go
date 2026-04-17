package agent

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// native_tools.json was copied verbatim from the working app's captured
// OpenAI request body (the 7 built-in Cursor tools: Shell, Glob, Grep,
// Read, Delete, StrReplace, Write). Using the exact schemas keeps the
// model's argument format identical to what Cursor's IDE client expects
// when it later receives our ExecServerMessage frame.
//
//go:embed toolspec/native_tools.json
var nativeToolsJSON []byte

// openAITool is the minimal subset of OpenAI's tool spec we need to pass
// through untouched.
type openAITool struct {
	Type     string          `json:"type"`
	Function json.RawMessage `json:"function"`
}

// Tools that are safe to expose in this MVP iteration. Everything in
// native_tools.json is safe in theory; limiting to Shell/Read/Write at
// first lets us validate the end-to-end flow without chasing edge cases
// in the more specialised tools.
var mvpToolAllowlist = map[string]struct{}{
	"Shell":            {},
	"Read":             {},
	"Write":            {},
	"Grep":             {},
	"Glob":             {},
	"Delete":           {},
	"StrReplace":       {},
	"SwitchMode":       {},
	"CreatePlan":       {},
	"AddTodo":          {},
	"UpdateTodo":       {},
	"CallMcpTool":      {},
	"FetchMcpResource": {},
	"ListMcpResources": {},
}

// mcpNameSafe strips characters OpenAI's function-name validator rejects
// (only [a-zA-Z0-9_-] up to 64 chars allowed). Keeps the human-readable
// tool slug intact where possible so the model's decision-making stays
// predictable.
var mcpNameSafe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func sanitizeMcpName(s string) string {
	s = mcpNameSafe.ReplaceAllString(s, "_")
	if len(s) > 48 {
		s = s[:48]
	}
	return s
}

// openAIToolsForRequest returns the allowlisted native tools plus every
// MCP tool from sess.Run.McpTools, exposed as its own OpenAI function.
// Populates sess.McpMap so buildToolCallProto can translate a call on
// "mcp_<idx>__<slug>" back into CallMcpTool with the real server identifier.
func openAIToolsForRequest(sess *Session) []openAITool {
	var all []struct {
		Type     string          `json:"type"`
		Function json.RawMessage `json:"function"`
	}
	if err := json.Unmarshal(nativeToolsJSON, &all); err != nil {
		return nil
	}
	out := make([]openAITool, 0, len(all))
	for _, t := range all {
		var meta struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(t.Function, &meta)
		if _, ok := mvpToolAllowlist[meta.Name]; !ok {
			continue
		}
		out = append(out, openAITool{Type: "function", Function: t.Function})
	}
	if sess == nil {
		return out
	}
	if sess.McpMap == nil {
		sess.McpMap = map[string]*McpRef{}
	}
	type mcpEntry struct {
		serverName string
		serverID   string
		tool       string
		desc       string
		schema     json.RawMessage
	}
	var collected []mcpEntry
	seen := map[string]bool{}
	add := func(serverName, serverID, tool, desc string, schema json.RawMessage) {
		if serverID == "" && serverName == "" {
			return
		}
		if tool == "" {
			return
		}
		if serverID == "" {
			serverID = serverName
		}
		if serverName == "" {
			serverName = serverID
		}
		key := serverID + "\x00" + tool
		if seen[key] {
			return
		}
		seen[key] = true
		collected = append(collected, mcpEntry{serverName: serverName, serverID: serverID, tool: tool, desc: desc, schema: schema})
	}
	// Source 1: AgentRunRequest.McpTools (rich form with input schema). The
	// McpToolDefinition only exposes provider_identifier; there's no short
	// server_name here, so we reuse the id for both fields.
	if sess.Run != nil {
		if mcpTools := sess.Run.GetMcpTools(); mcpTools != nil {
			for _, d := range mcpTools.GetMcpTools() {
				var schema json.RawMessage
				if sch := d.GetInputSchema(); sch != nil {
					if raw, err := sch.MarshalJSON(); err == nil {
						schema = raw
					}
				}
				id := d.GetProviderIdentifier()
				add(id, id, d.GetToolName(), d.GetDescription(), schema)
			}
		}
	}
	// Source 2: RequestContext.McpFileSystemOptions.McpDescriptors (the
	// actual place Cursor ships MCP tool lists in agent turns — the earlier
	// field is only populated on prewarm). This source has BOTH server_name
	// and server_identifier, which McpArgs needs on two separate fields.
	// The McpToolDescriptor only carries the tool name + a definition_path
	// pointing at a JSON file Cursor writes with the full MCP tool def
	// (description + inputSchema). We load that file when present so the
	// model gets the real arg names instead of guessing at "libraryName".
	if sess.Action != nil {
		if uma := sess.Action.GetUserMessageAction(); uma != nil {
			if rc := uma.GetRequestContext(); rc != nil {
				if fs := rc.GetMcpFileSystemOptions(); fs != nil {
					for _, srv := range fs.GetMcpDescriptors() {
						serverName := srv.GetServerName()
						serverID := srv.GetServerIdentifier()
						instr := srv.GetServerUseInstructions()
						for _, t := range srv.GetTools() {
							desc, schema := loadMcpDefinition(t.GetDefinitionPath())
							if desc == "" {
								desc = instr
							}
							if desc == "" {
								desc = fmt.Sprintf("Tool %q on MCP server %q.", t.GetToolName(), serverName)
							}
							add(serverName, serverID, t.GetToolName(), desc, schema)
						}
					}
				}
			}
		}
	}
	for i, e := range collected {
		fnName := fmt.Sprintf("mcp_%d__%s", i, sanitizeMcpName(e.tool))
		sess.McpMap[fnName] = &McpRef{
			ServerName: e.serverName,
			ServerID:   e.serverID,
			ToolName:   e.tool,
		}
		desc := strings.TrimSpace(e.desc)
		if desc == "" {
			desc = fmt.Sprintf("MCP tool %q on server %q.", e.tool, e.serverName)
		}
		desc = fmt.Sprintf("[MCP server: %s (id: %s) | real tool: %s]\n%s", e.serverName, e.serverID, e.tool, desc)
		params := normalizeMcpSchema(e.schema)
		fnBody, err := json.Marshal(map[string]any{
			"name":        fnName,
			"description": desc,
			"parameters":  params,
		})
		if err != nil {
			continue
		}
		out = append(out, openAITool{Type: "function", Function: fnBody})
	}
	return out
}

// loadMcpDefinition reads the MCP tool definition Cursor writes to disk
// for each registered tool. Returns (description, inputSchema) — either
// may be empty if the file is missing/malformed. Cursor's file layout
// follows the MCP spec: { "name": ..., "description": ..., "inputSchema": {...} },
// but tolerates camelCase and snake_case variants.
func loadMcpDefinition(path string) (string, json.RawMessage) {
	if path == "" {
		return "", nil
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return "", nil
	}
	var def struct {
		Description  string          `json:"description"`
		Arguments    json.RawMessage `json:"arguments"`
		InputSchema  json.RawMessage `json:"inputSchema"`
		InputSchema2 json.RawMessage `json:"input_schema"`
		Schema       json.RawMessage `json:"schema"`
		Parameters   json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &def); err != nil {
		return "", nil
	}
	// Cursor writes the JSON-Schema into "arguments"; upstream MCP uses
	// "inputSchema" / "input_schema". Try them in order.
	schema := def.Arguments
	if len(schema) == 0 {
		schema = def.InputSchema
	}
	if len(schema) == 0 {
		schema = def.InputSchema2
	}
	if len(schema) == 0 {
		schema = def.Schema
	}
	if len(schema) == 0 {
		schema = def.Parameters
	}
	return def.Description, schema
}

// normalizeMcpSchema patches an MCP tool's input schema so it satisfies
// OpenAI's function-parameters validator. OpenAI rejects "type: object"
// without a "properties" key; many MCP servers ship schemas without
// properties (either a bare object or nil). We also coerce top-level types
// we can't handle into a permissive object.
func normalizeMcpSchema(raw json.RawMessage) json.RawMessage {
	fallback := json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`)
	if len(raw) == 0 {
		return fallback
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return fallback
	}
	if obj == nil {
		return fallback
	}
	// Force top-level type to object (OpenAI requires this at the
	// parameters root).
	obj["type"] = "object"
	if _, ok := obj["properties"]; !ok {
		obj["properties"] = map[string]any{}
	}
	if _, ok := obj["additionalProperties"]; !ok {
		obj["additionalProperties"] = true
	}
	patched, err := json.Marshal(obj)
	if err != nil {
		return fallback
	}
	return patched
}
