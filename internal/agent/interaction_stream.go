package agent

import (
	"sync"

	aiserverv1 "cursorforge/internal/protocodec/gen/aiserver/v1"
)

var interactionSubscribers = struct {
	sync.Mutex
	nextID int
	chans  map[int]chan *aiserverv1.InteractionUpdate
}{chans: map[int]chan *aiserverv1.InteractionUpdate{}}

func subscribeInteractionUpdates() (int, chan *aiserverv1.InteractionUpdate) {
	interactionSubscribers.Lock()
	defer interactionSubscribers.Unlock()
	id := interactionSubscribers.nextID
	interactionSubscribers.nextID++
	ch := make(chan *aiserverv1.InteractionUpdate, 64)
	interactionSubscribers.chans[id] = ch
	return id, ch
}

func unsubscribeInteractionUpdates(id int) {
	interactionSubscribers.Lock()
	ch := interactionSubscribers.chans[id]
	delete(interactionSubscribers.chans, id)
	interactionSubscribers.Unlock()
	if ch != nil {
		close(ch)
	}
}

func publishInteractionUpdate(msg *aiserverv1.InteractionUpdate) {
	if msg == nil {
		return
	}
	interactionSubscribers.Lock()
	defer interactionSubscribers.Unlock()
	for _, ch := range interactionSubscribers.chans {
		select {
		case ch <- msg:
		default:
		}
	}
}
