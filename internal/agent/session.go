package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	agentv1 "cursorforge/internal/protocodec/gen/agent/v1"
)

// Session captures the conversation state Cursor sent through BidiAppend.
// We key sessions by RequestId (the BidiRequestId.request_id field) so the
// follow-up RunSSE call can look them back up without re-parsing the body.
//
// The Cursor agent loop normally streams BidiAppend (input) and RunSSE
// (output) on separate HTTP connections that share the same RequestId; the
// store is the only thing that bridges them.
type Session struct {
	RequestID      string
	ConversationID string
	UserText       string
	Mode           agentv1.AgentMode
	ModelDetails   *agentv1.ModelDetails
	Action         *agentv1.ConversationAction
	State          *agentv1.ConversationStateStructure
	Run            *agentv1.AgentRunRequest
	// McpMap maps the OpenAI-visible function name (e.g. "mcp_0__search") to
	// the real (server_identifier, mcp_tool_name) pair. We expose every MCP
	// tool as a standalone OpenAI function so the model stops hallucinating
	// composite names — then translate back when it calls one.
	McpMap map[string]*McpRef

	// Plan state — populated by the local CreatePlan/AddTodo/UpdateTodo
	// tools. Projected into the system prompt on every turn so the model
	// can track progress without us having to serialize it through
	// ConversationState.
	PlanName     string
	PlanOverview string
	Todos        []*TodoEntry
}

type McpRef struct {
	// ServerName is the user-facing short name (McpDescriptor.server_name,
	// e.g. "context7"). Cursor writes this into McpArgs.name and renders it
	// in the UI pill.
	ServerName string
	// ServerID is the full routing identifier (McpDescriptor.server_identifier,
	// e.g. "plugin-context7-context7"). Goes into McpArgs.provider_identifier;
	// Cursor's MCP router uses this to pick the actual MCP server process.
	ServerID string
	ToolName string
}

// TodoEntry is a single item on a session's active plan. We keep these in
// the Session (not proto) because CreatePlan/AddTodo/UpdateTodo resolve
// locally — there's no ExecServer round-trip. The list is projected back
// into the system prompt on every turn so the model can track progress.
type TodoEntry struct {
	ID      string
	Content string
	Status  string
}

// ConvTurn is one user/assistant pair we've seen on a conversation. Cursor
// doesn't ship its own chat history in BidiAppend (it keeps it behind a
// server-side blob store the real cursor.sh backend implements), so we
// maintain our own per-conversation_id log and replay it whenever the user
// sends a new message with the same conversation_id.
//
// Messages (when non-nil) carries the exact OpenAI-format messages the turn
// added to the prompt — assistant-with-tool_calls + tool-role results. This
// is what lets the agent loop replay a tool-heavy turn without losing the
// intermediate tool state; plain string User/Assistant alone can't represent
// it. Old turns saved before rich history was introduced have nil Messages
// and fall back to the User/Assistant replay path.
type ConvTurn struct {
	User      string
	Assistant string
	Messages  []StoredMessage
}

// StoredMessage is a disk-friendly mirror of openAIMessage. We keep it here
// (not in openai.go) because history_disk.go marshals it into conversation.json
// and openai.go's openAIMessage uses json.RawMessage for Content which doesn't
// round-trip cleanly. Content here is always the string form.
type StoredMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
	ToolCalls  []StoredToolCall `json:"tool_calls,omitempty"`
}

type StoredToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type sessionStore struct {
	mu          sync.RWMutex
	byID        map[string]*Session
	byConv      map[string]*Session
	lastPut     *Session               // fallback when RunSSE ids don't match any BidiAppend id
	lastConv    *Session               // most recent session with UserText, for continuation rounds
	history     map[string][]*ConvTurn // conversation_id -> turns in arrival order
	planEmitted map[string]bool        // conversation_id -> did we already open Cursor's Plan panel?
	planByConv  map[string]*PlanState  // conversation_id -> authoritative plan state
}

// PlanState is the conversation-scoped plan snapshot. Cursor does not ship
// the plan back inside BidiAppend (it lives purely in our state), so we have
// to keep it keyed off conversation_id so that the next turn's session — a
// brand-new object built from a fresh BidiAppend — can still see the plan
// the previous turn created. Before this was session-scoped, and every new
// turn arrived with an empty Todos slice: UpdateTodo responded
// "no active plan — call CreatePlan first" and forced the model to rebuild
// the plan from scratch on every message.
type PlanState struct {
	Name     string
	Overview string
	Todos    []*TodoEntry
}

var store = &sessionStore{
	byID:        map[string]*Session{},
	byConv:      map[string]*Session{},
	history:     map[string][]*ConvTurn{},
	planEmitted: map[string]bool{},
	planByConv:  map[string]*PlanState{},
}

// PlanStateFor returns a copy of the plan for a conversation (nil when none).
// Callers must not mutate the returned slice — use Save*/Append*/Update* helpers.
func PlanStateFor(conversationID string) *PlanState {
	if conversationID == "" {
		return nil
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	p := store.planByConv[conversationID]
	if p == nil {
		return nil
	}
	// Deep copy the todos so the caller can iterate without locking.
	todos := make([]*TodoEntry, len(p.Todos))
	for i, t := range p.Todos {
		cp := *t
		todos[i] = &cp
	}
	return &PlanState{Name: p.Name, Overview: p.Overview, Todos: todos}
}

// SavePlanState replaces (or creates) the plan for a conversation. Called by
// CreatePlan — drops any previous plan and installs the new one verbatim.
func SavePlanState(conversationID, name, overview string, todos []*TodoEntry) {
	if conversationID == "" {
		return
	}
	copied := make([]*TodoEntry, len(todos))
	for i, t := range todos {
		cp := *t
		copied[i] = &cp
	}
	store.mu.Lock()
	store.planByConv[conversationID] = &PlanState{Name: name, Overview: overview, Todos: copied}
	store.mu.Unlock()
}

// AppendTodo atomically adds a todo to the conversation's plan and returns
// the full updated list. Returns nil + false when there's no active plan
// (caller should tell the model to call CreatePlan first).
func AppendTodo(conversationID, content string) ([]*TodoEntry, bool) {
	if conversationID == "" {
		return nil, false
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	p := store.planByConv[conversationID]
	if p == nil {
		return nil, false
	}
	id := fmt.Sprintf("t%d", len(p.Todos)+1)
	p.Todos = append(p.Todos, &TodoEntry{ID: id, Content: content, Status: "pending"})
	out := make([]*TodoEntry, len(p.Todos))
	for i, t := range p.Todos {
		cp := *t
		out[i] = &cp
	}
	return out, true
}

// UpdateTodoStatus finds a todo by id (or content prefix when id is empty),
// flips its status, and returns the full updated list. Second return value is
// true on success, false when either the plan doesn't exist or no todo matches.
func UpdateTodoStatus(conversationID, id, contentPrefix, status string) ([]*TodoEntry, bool) {
	if conversationID == "" {
		return nil, false
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	p := store.planByConv[conversationID]
	if p == nil || len(p.Todos) == 0 {
		return nil, false
	}
	var target *TodoEntry
	if id != "" {
		for _, t := range p.Todos {
			if t.ID == id {
				target = t
				break
			}
		}
	}
	if target == nil && contentPrefix != "" {
		low := strings.ToLower(contentPrefix)
		for _, t := range p.Todos {
			if strings.HasPrefix(strings.ToLower(t.Content), low) {
				target = t
				break
			}
		}
	}
	if target == nil {
		return nil, false
	}
	target.Status = status
	out := make([]*TodoEntry, len(p.Todos))
	for i, t := range p.Todos {
		cp := *t
		out[i] = &cp
	}
	return out, true
}

// PlanEmittedFor reports whether Cursor's Plan panel has already been
// opened for this conversation. Conversation-scoped (not session-scoped)
// so continuation rounds or reconnect-clones don't accidentally reuse or
// reset the gate; a new chat always gets a fresh panel.
func PlanEmittedFor(conversationID string) bool {
	if conversationID == "" {
		return false
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.planEmitted[conversationID]
}

// MarkPlanEmitted records that we fired a CreatePlanRequestQuery for this
// conversation so subsequent CreatePlan calls in the same chat silently
// update state instead of spawning a second .plan.md file.
func MarkPlanEmitted(conversationID string) {
	if conversationID == "" {
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.planEmitted[conversationID] = true
}

func (s *sessionStore) Put(sess *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.RequestID != "" {
		s.byID[sess.RequestID] = sess
	}
	if conv := sess.ConversationID; conv != "" {
		s.byConv[conv] = sess
	}
	s.lastPut = sess
	if sess.UserText != "" {
		s.lastConv = sess
	}
}

func (s *sessionStore) Get(id string) *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if sess := s.byID[id]; sess != nil {
		return sess
	}
	if sess := s.byConv[id]; sess != nil {
		return sess
	}
	return nil
}

func (s *sessionStore) Drop(id string) {
	s.mu.Lock()
	if sess := s.byID[id]; sess != nil {
		delete(s.byID, id)
		// Keep byConv mapping alive so follow-up RunSSE requests for the
		// same conversation (different request_id) can still find context.
		if s.lastPut == sess {
			s.lastPut = nil
		}
	}
	s.mu.Unlock()
}

func PutSession(s *Session)         { store.Put(s) }
func GetSession(id string) *Session { return store.Get(id) }
func DropSession(id string)         { store.Drop(id) }

// HistoryFor returns a copy of the stored turns for a conversation_id so
// the caller can iterate without holding the store lock. Callers are free
// to mutate the returned slice; the internal slice is not shared.
func HistoryFor(conversationID string) []*ConvTurn {
	store.mu.RLock()
	defer store.mu.RUnlock()
	src := store.history[conversationID]
	if len(src) == 0 {
		return nil
	}
	out := make([]*ConvTurn, len(src))
	copy(out, src)
	return out
}

// RecordTurn appends a completed (user, assistant) pair to the conversation
// history (in-memory + on-disk). Called after RunSSE finishes streaming so
// we have the full assistant reply. In-memory is capped at 50 turns per
// conversation to avoid unbounded memory growth — beyond that we drop the
// oldest pair from the live window, but the disk log keeps every turn
// intact so a future restart with a bigger cap can replay everything.
//
// When art != nil the caller supplies the exact request/response bytes so
// they get written into turns/NNNNNN/ alongside conversation.json — this
// matches the working app's artifact layout.
//
// messages (when non-nil) is the OpenAI-format message list the turn added
// to the prompt. Persisting it lets buildMessageHistory replay intermediate
// tool_calls + tool results on the next turn instead of losing them to the
// User/Assistant-string fallback — which is what used to trigger the
// "reconnecting → re-run every tool from scratch" loop.
func RecordTurn(conversationID, requestID, user, assistant, mode string, art *turnArtifacts, messages []StoredMessage) {
	if conversationID == "" || user == "" {
		return
	}
	turn := &ConvTurn{User: user, Assistant: assistant, Messages: messages}
	store.mu.Lock()
	turns := store.history[conversationID]
	turns = append(turns, turn)
	if len(turns) > 50 {
		turns = turns[len(turns)-50:]
	}
	store.history[conversationID] = turns
	store.mu.Unlock()
	persistTurn(conversationID, requestID, user, assistant, mode, art, messages)
}

// modeString maps the proto AgentMode to the short label we record in
// conversation.json / stats panel. Kept here so every call site agrees.
func modeString(m agentv1.AgentMode) string {
	switch m {
	case agentv1.AgentMode_AGENT_MODE_AGENT:
		return "agent"
	case agentv1.AgentMode_AGENT_MODE_ASK:
		return "ask"
	case agentv1.AgentMode_AGENT_MODE_PLAN:
		return "plan"
	case agentv1.AgentMode_AGENT_MODE_DEBUG:
		return "debug"
	default:
		return "agent"
	}
}

// WaitForSession blocks until a session with usable UserText exists for id
// (or until the context expires). Cursor occasionally fires RunSSE before
// the matching BidiAppend lands, AND continuation rounds ship a fresh
// request_id with no user-facing text of their own — in that case we clone
// the last session that had UserText so the model keeps working on the
// same conversation instead of crashing with "no session/user text".
//
// Priority on exit:
//  1. A session with matching id AND non-empty UserText (happy path).
//  2. A lastConv clone (continuation rounds or reconnect storms — without
//     this we'd reject the retry and Cursor shows "Internal Error").
//  3. Any session at all (heartbeat skeleton) — last resort; caller still
//     decides whether UserText is usable.
func WaitForSession(ctx context.Context, id string) *Session {
	if id == "" {
		return nil
	}
	deadline := time.Now().Add(5 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	for {
		if sess := GetSession(id); sess != nil && sess.UserText != "" {
			return sess
		}
		if time.Now().After(deadline) {
			// Continuation / reconnect: we have no matching session (or
			// only a heartbeat skeleton with no UserText). Clone the most
			// recent session that DID carry user text so the RunSSE retry
			// can resume instead of erroring out.
			store.mu.RLock()
			fallback := store.lastConv
			store.mu.RUnlock()
			if fallback != nil && fallback.UserText != "" {
				clone := *fallback
				clone.RequestID = id
				// Clear the McpMap — it's request-scoped state rebuilt on
				// every new turn by openAIToolsForRequest. Sharing the
				// previous map across requests would stick stale entries.
				clone.McpMap = nil
				PutSession(&clone)
				return &clone
			}
			return GetSession(id)
		}
		select {
		case <-ctx.Done():
			if sess := GetSession(id); sess != nil && sess.UserText != "" {
				return sess
			}
			store.mu.RLock()
			fallback := store.lastConv
			store.mu.RUnlock()
			if fallback != nil && fallback.UserText != "" {
				clone := *fallback
				clone.RequestID = id
				clone.McpMap = nil
				PutSession(&clone)
				return &clone
			}
			return nil
		case <-time.After(25 * time.Millisecond):
		}
	}
}
