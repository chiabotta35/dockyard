package webui

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type EventType string

const (
	EventScanStarted    EventType = "scan_started"
	EventScanComplete   EventType = "scan_complete"
	EventUpdateStarted  EventType = "update_started"
	EventUpdateProgress EventType = "update_progress"
	EventUpdateComplete EventType = "update_complete"
	EventUpdateFailed   EventType = "update_failed"
	EventRollback       EventType = "rollback"
	EventSelfUpdate     EventType = "self_update"
	EventLogLine        EventType = "log_line"
)

type Event struct {
	Type      EventType   `json:"type"`
	Container string      `json:"container,omitempty"`
	Message   string      `json:"message"`
	Data      interface{} `json:"data,omitempty"`
	Timestamp time.Time   `json:"timestamp"`
}

type EventHub struct {
	clients    map[chan Event]struct{}
	mu         sync.RWMutex
	maxHistory int
	history    []Event
	logFunc    func(container, message string) // callback for persistent log storage
}

func NewEventHub(logFunc func(container, message string)) *EventHub {
	return &EventHub{
		clients:    make(map[chan Event]struct{}),
		maxHistory: 200,
		history:    make([]Event, 0, 200),
		logFunc:    logFunc,
	}
}

func (h *EventHub) Subscribe() chan Event {
	ch := make(chan Event, 64)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()

	// Replay structural events (scan/update status) but NOT log_line events.
	// Log lines are persisted separately and replaying them on SSE reconnect
	// causes duplicate "Update available" messages every time the connection drops.
	h.mu.RLock()
	for _, e := range h.history {
		if e.Type == EventLogLine {
			continue
		}
		select {
		case ch <- e:
		default:
		}
	}
	h.mu.RUnlock()

	return ch
}

func (h *EventHub) Unsubscribe(ch chan Event) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
	close(ch)
}

func (h *EventHub) Broadcast(evt Event) {
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now()
	}

	h.mu.Lock()
	h.history = append(h.history, evt)
	if len(h.history) > h.maxHistory {
		h.history = h.history[len(h.history)-h.maxHistory:]
	}
	h.mu.Unlock()

	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.clients {
		select {
		case ch <- evt:
		default:
		}
	}
}

func (h *EventHub) BroadcastLog(container, message string) {
	h.Broadcast(Event{
		Type:      EventLogLine,
		Container: container,
		Message:   message,
	})
	if h.logFunc != nil {
		h.logFunc(container, message)
	}
}

func (h *EventHub) BroadcastUpdate(container, status string) {
	evtType := EventUpdateProgress
	switch status {
	case "started":
		evtType = EventUpdateStarted
	case "complete", "success":
		evtType = EventUpdateComplete
	case "failed", "error":
		evtType = EventUpdateFailed
	case "rollback":
		evtType = EventRollback
	}
	h.Broadcast(Event{
		Type:      evtType,
		Container: container,
		Message:   fmt.Sprintf("Container %s: %s", container, status),
	})
}

func (h *EventHub) HistoryJSON() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	data, _ := json.Marshal(h.history)
	return string(data)
}
