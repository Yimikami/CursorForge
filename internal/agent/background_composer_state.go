package agent

import (
	"fmt"
	"sync"
	"time"

	aiserverv1 "cursorforge/internal/protocodec/gen/aiserver/v1"
)

type backgroundComposerState struct {
	BcID        string
	Prompt      *aiserverv1.HeadlessAgenticComposerPrompt
	Request     *aiserverv1.AddAsyncFollowupBackgroundComposerRequest
	Status      aiserverv1.BackgroundComposerStatus
	CreatedAt   time.Time
	StartedAt   time.Time
	FinishedAt  time.Time
	LastError   string
	LastText    string
	ModelName   string
	streaming   bool
	streamDone  bool
	streamIndex int32
	updates     []*aiserverv1.AttachBackgroundComposerResponse
}

var backgroundComposers = struct {
	sync.RWMutex
	byID map[string]*backgroundComposerState
}{byID: map[string]*backgroundComposerState{}}

func newBackgroundComposerID() string {
	return fmt.Sprintf("bc-%d", time.Now().UnixNano())
}

func saveBackgroundComposer(st *backgroundComposerState) {
	if st == nil || st.BcID == "" {
		return
	}
	backgroundComposers.Lock()
	backgroundComposers.byID[st.BcID] = st
	backgroundComposers.Unlock()
}

func getBackgroundComposer(id string) *backgroundComposerState {
	backgroundComposers.RLock()
	defer backgroundComposers.RUnlock()
	return backgroundComposers.byID[id]
}

func appendBackgroundComposerUpdate(id string, resp *aiserverv1.AttachBackgroundComposerResponse) int32 {
	backgroundComposers.Lock()
	defer backgroundComposers.Unlock()
	st := backgroundComposers.byID[id]
	if st == nil {
		return 0
	}
	st.updates = append(st.updates, resp)
	st.streamIndex = int32(len(st.updates))
	return st.streamIndex
}
