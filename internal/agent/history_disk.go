package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Disk layout mirrors the closed-source working app so anyone comparing
// artifact trees side by side sees the same shape:
//
//	<history_root>/
//	  <conversation_id>/
//	    conversation.json            // live log of entries (user_message, assistant_text, ...)
//	    turns/
//	      000000/
//	        request.json             // exact body POSTed to the BYOK provider
//	        sse.jsonl                // every streamed chunk that came back
//	        summary.json             // tokens + timing snapshot
//
// We don't try to match every working-app field — only what we can fill
// honestly from our own state. Additional metadata/tool entries land here
// once Phase 3c tool calls are implemented.

var (
	historyDirMu sync.RWMutex
	historyDir   string
)

// InitHistoryDir wires the root directory for persisted conversation turns
// and restores any previously-saved sessions back into the in-memory store
// so picker history survives restart.
func InitHistoryDir(dir string) {
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	historyDirMu.Lock()
	historyDir = dir
	historyDirMu.Unlock()
	loadAllHistory()
}

// conversationEntry is one row of conversation.json. Kinds we currently
// emit: "user_message", "assistant_text". Tool kinds will slot in the same
// envelope once Phase 3c lands.
type conversationEntry struct {
	Seq         int             `json:"seq"`
	TurnSeq     int             `json:"turn_seq"`
	RequestID   string          `json:"request_id,omitempty"`
	Role        string          `json:"role"`
	Kind        string          `json:"kind"`
	Payload     json.RawMessage `json:"payload"`
	ToolCallID  string          `json:"tool_call_id,omitempty"`
	CreatedAt   string          `json:"created_at"`
}

type conversationFile struct {
	ConversationID          string              `json:"conversation_id"`
	RootConversationID      string              `json:"root_conversation_id"`
	ParentConversationID    string              `json:"parent_conversation_id"`
	ParentToolCallID        string              `json:"parent_tool_call_id"`
	Mode                    string              `json:"mode"`
	TokenDetailsUsedTokens  int                 `json:"token_details_used_tokens"`
	TokenDetailsMaxTokens   int                 `json:"token_details_max_tokens"`
	CreatedAt               string              `json:"created_at"`
	UpdatedAt               string              `json:"updated_at"`
	NextTurnSeq             int                 `json:"next_turn_seq"`
	NextEntrySeq            int                 `json:"next_entry_seq"`
	Entries                 []conversationEntry `json:"entries"`
}

func historyRoot() string {
	historyDirMu.RLock()
	defer historyDirMu.RUnlock()
	return historyDir
}

func sanitizeID(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, id)
}

func conversationDir(conversationID string) string {
	root := historyRoot()
	if root == "" {
		return ""
	}
	return filepath.Join(root, sanitizeID(conversationID))
}

func readConversationFile(conversationID string) *conversationFile {
	dir := conversationDir(conversationID)
	if dir == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, "conversation.json"))
	if err != nil {
		return nil
	}
	var cf conversationFile
	if err := json.Unmarshal(raw, &cf); err != nil {
		return nil
	}
	return &cf
}

func writeConversationFile(cf *conversationFile) {
	dir := conversationDir(cf.ConversationID)
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	cf.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	raw, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return
	}
	tmp := filepath.Join(dir, "conversation.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, filepath.Join(dir, "conversation.json"))
}

// persistTurn appends a user_message + assistant_text pair to
// conversation.json and writes a matching turns/NNNNNN directory with the
// request/response artifacts the caller already has.
//
// artifacts are optional: pass nil and only the conversation entries update.
type turnArtifacts struct {
	RequestBody     []byte
	SSEJSONL        []byte
	SummaryJSON     []byte
	TotalTokens     int
}

// earlyPersistUserTurn creates conversation.json with the user message entry
// immediately when a turn starts, before streaming begins. This ensures the
// history directory exists even if the stream fails or gets interrupted.
// The full turn (with assistant reply + artifacts) is written later by persistTurn.
func earlyPersistUserTurn(conversationID, requestID, userText string) {
	if historyRoot() == "" || conversationID == "" || userText == "" {
		return
	}
	cf := readConversationFile(conversationID)
	if cf != nil {
		return // already exists — persistTurn will update it
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	cf = &conversationFile{
		ConversationID:        conversationID,
		RootConversationID:    conversationID,
		Mode:                  "agent",
		TokenDetailsMaxTokens: 200000,
		CreatedAt:             now,
		NextTurnSeq:           1,
		NextEntrySeq:          1,
	}
	userPayload, _ := json.Marshal(map[string]string{"text": userText})
	cf.Entries = append(cf.Entries, conversationEntry{
		Seq:       cf.NextEntrySeq,
		TurnSeq:   cf.NextTurnSeq,
		RequestID: requestID,
		Role:      "user",
		Kind:      "user_message",
		Payload:   userPayload,
		CreatedAt: now,
	})
	writeConversationFile(cf)
}

func persistTurn(conversationID, requestID, userText, assistantText, mode string, art *turnArtifacts, messages []StoredMessage) {
	if historyRoot() == "" || conversationID == "" {
		return
	}
	if mode == "" {
		mode = "agent"
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	cf := readConversationFile(conversationID)
	if cf == nil {
		cf = &conversationFile{
			ConversationID:        conversationID,
			RootConversationID:    conversationID,
			Mode:                  mode,
			TokenDetailsMaxTokens: 200000,
			CreatedAt:             now,
			NextTurnSeq:           1,
			NextEntrySeq:          1,
		}
	} else {
		// Always track the latest mode — the chat may switch mid-conversation.
		cf.Mode = mode
	}
	turnSeq := cf.NextTurnSeq

	userPayload, _ := json.Marshal(map[string]string{"text": userText})
	cf.Entries = append(cf.Entries, conversationEntry{
		Seq:       cf.NextEntrySeq,
		TurnSeq:   turnSeq,
		RequestID: requestID,
		Role:      "user",
		Kind:      "user_message",
		Payload:   userPayload,
		CreatedAt: now,
	})
	cf.NextEntrySeq++

	if assistantText != "" {
		assPayload, _ := json.Marshal(map[string]string{"text": assistantText})
		cf.Entries = append(cf.Entries, conversationEntry{
			Seq:       cf.NextEntrySeq,
			TurnSeq:   turnSeq,
			RequestID: requestID,
			Role:      "assistant",
			Kind:      "assistant_text",
			Payload:   assPayload,
			CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
		cf.NextEntrySeq++
	}
	// turn_messages carries the exact OpenAI message slice this turn added —
	// assistant messages with tool_calls plus tool-role results. Replaying
	// these on the next turn lets the model see intermediate tool state
	// instead of "[tool-only turn: ...]" and re-running every tool.
	if len(messages) > 0 {
		msgPayload, _ := json.Marshal(messages)
		cf.Entries = append(cf.Entries, conversationEntry{
			Seq:       cf.NextEntrySeq,
			TurnSeq:   turnSeq,
			RequestID: requestID,
			Role:      "meta",
			Kind:      "turn_messages",
			Payload:   msgPayload,
			CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
		cf.NextEntrySeq++
	}
	cf.NextTurnSeq++
	if art != nil && art.TotalTokens > 0 {
		cf.TokenDetailsUsedTokens += art.TotalTokens
	}
	writeConversationFile(cf)

	if art != nil {
		turnDir := filepath.Join(conversationDir(conversationID), "turns", fmt.Sprintf("%06d", turnSeq-1))
		_ = os.MkdirAll(turnDir, 0o755)
		if len(art.RequestBody) > 0 {
			_ = os.WriteFile(filepath.Join(turnDir, "request.json"), art.RequestBody, 0o644)
		}
		if len(art.SSEJSONL) > 0 {
			_ = os.WriteFile(filepath.Join(turnDir, "sse.jsonl"), art.SSEJSONL, 0o644)
		}
		if len(art.SummaryJSON) > 0 {
			_ = os.WriteFile(filepath.Join(turnDir, "summary.json"), art.SummaryJSON, 0o644)
		}
	}
}

// UsageSnapshot is the flat aggregate the bridge layer maps into its public
// UsageStats type. Keeping it in the agent package avoids an import cycle
// (bridge imports agent; agent can't import bridge).
type UsageSnapshot struct {
	TotalPromptTokens     int64
	TotalCompletionTokens int64
	TotalTokens           int64
	ConversationCount     int
	TurnCount             int
	PerModel              []ModelUsage
	Last7Days             []DailyUsage
}

type ModelUsage struct {
	Model            string
	Provider         string
	PromptTokens     int64
	CompletionTokens int64
	TurnCount        int
}

type DailyUsage struct {
	Date             string // YYYY-MM-DD, local time
	PromptTokens     int64
	CompletionTokens int64
}

// ComputeUsageStats walks every conversation under historyRoot() and sums up
// the per-turn summary.json artifacts RunSSE writes at turn completion. The
// result is the raw token counters the user's Stats tab shows — no pricing
// math applied (intentional design decision: BYOK providers have wildly
// different billing models and we don't want to mislead).
//
// Safe to call repeatedly and from any goroutine; does not mutate in-memory
// session state.
func ComputeUsageStats() UsageSnapshot {
	root := historyRoot()
	if root == "" {
		return UsageSnapshot{}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return UsageSnapshot{}
	}
	var snap UsageSnapshot
	perModel := map[string]*ModelUsage{} // key: model|provider
	perDay := map[string]*DailyUsage{}   // key: YYYY-MM-DD

	// Build the rolling 7-day window in local time up front so even days
	// with zero usage get a zero bar on the chart.
	now := time.Now()
	for i := 6; i >= 0; i-- {
		d := now.AddDate(0, 0, -i).Format("2006-01-02")
		perDay[d] = &DailyUsage{Date: d}
	}

	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		convDir := filepath.Join(root, ent.Name())
		turnsDir := filepath.Join(convDir, "turns")
		turns, err := os.ReadDir(turnsDir)
		if err != nil {
			continue
		}
		convCounted := false
		for _, tn := range turns {
			if !tn.IsDir() {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(turnsDir, tn.Name(), "summary.json"))
			if err != nil {
				continue
			}
			var s struct {
				PromptTokens     int64  `json:"prompt_tokens"`
				CompletionTokens int64  `json:"completion_tokens"`
				TotalTokens      int64  `json:"total_tokens"`
				Model            string `json:"model"`
				Provider         string `json:"provider"`
				FinishedAt       string `json:"finished_at"`
			}
			if err := json.Unmarshal(raw, &s); err != nil {
				continue
			}
			snap.TotalPromptTokens += s.PromptTokens
			snap.TotalCompletionTokens += s.CompletionTokens
			snap.TotalTokens += s.TotalTokens
			snap.TurnCount++
			if !convCounted {
				snap.ConversationCount++
				convCounted = true
			}
			key := s.Model + "|" + s.Provider
			m := perModel[key]
			if m == nil {
				m = &ModelUsage{Model: s.Model, Provider: s.Provider}
				perModel[key] = m
			}
			m.PromptTokens += s.PromptTokens
			m.CompletionTokens += s.CompletionTokens
			m.TurnCount++

			if s.FinishedAt != "" {
				if t, err := time.Parse(time.RFC3339Nano, s.FinishedAt); err == nil {
					d := t.Local().Format("2006-01-02")
					if bucket, ok := perDay[d]; ok {
						bucket.PromptTokens += s.PromptTokens
						bucket.CompletionTokens += s.CompletionTokens
					}
				}
			}
		}
	}

	snap.PerModel = make([]ModelUsage, 0, len(perModel))
	for _, m := range perModel {
		snap.PerModel = append(snap.PerModel, *m)
	}
	// Stable display order: largest total first.
	sort.Slice(snap.PerModel, func(i, j int) bool {
		return snap.PerModel[i].PromptTokens+snap.PerModel[i].CompletionTokens >
			snap.PerModel[j].PromptTokens+snap.PerModel[j].CompletionTokens
	})

	// Chronological order for the 7-day bar chart.
	snap.Last7Days = make([]DailyUsage, 0, 7)
	for i := 6; i >= 0; i-- {
		d := now.AddDate(0, 0, -i).Format("2006-01-02")
		if b, ok := perDay[d]; ok {
			snap.Last7Days = append(snap.Last7Days, *b)
		}
	}
	return snap
}

// loadAllHistory walks history_root and rehydrates the in-memory turn log
// so picker history survives restart. Any conversation.json that fails to
// parse is skipped silently.
func loadAllHistory() {
	root := historyRoot()
	if root == "" {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		cf := readConversationFile(ent.Name())
		if cf == nil || len(cf.Entries) == 0 {
			continue
		}
		// Reassemble turns from user_message + assistant_text + turn_messages.
		turnsByIdx := map[int]*ConvTurn{}
		var order []int
		for _, e := range cf.Entries {
			if e.Kind != "user_message" && e.Kind != "assistant_text" && e.Kind != "turn_messages" {
				continue
			}
			t := turnsByIdx[e.TurnSeq]
			if t == nil {
				t = &ConvTurn{}
				turnsByIdx[e.TurnSeq] = t
				order = append(order, e.TurnSeq)
			}
			switch e.Kind {
			case "user_message":
				var p struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(e.Payload, &p)
				t.User = p.Text
			case "assistant_text":
				var p struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(e.Payload, &p)
				t.Assistant = p.Text
			case "turn_messages":
				var msgs []StoredMessage
				_ = json.Unmarshal(e.Payload, &msgs)
				t.Messages = msgs
			}
		}
		var turns []*ConvTurn
		for _, idx := range order {
			if t := turnsByIdx[idx]; t != nil && t.User != "" {
				turns = append(turns, t)
			}
		}
		if len(turns) == 0 {
			continue
		}
		store.mu.Lock()
		store.history[ent.Name()] = turns
		store.mu.Unlock()
	}
}
