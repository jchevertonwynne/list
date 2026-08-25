// Package live is an in-process publish/subscribe hub for Server-Sent Events.
//
// `list` deploys as a single replica: SQLite lives on a ReadWriteOnce volume
// under a Recreate strategy, so there is never more than one process to fan
// events out from in the first place. That makes an in-memory hub the whole
// correct answer rather than a stopgap — there is no second replica for a
// Redis pub/sub channel or a NATS subject to coordinate with, and polling the
// database for change events would just be a slower, noisier version of the
// same single-process broadcast this package does directly.
//
// Callers publish an Event to a topic; every subscriber on that topic gets
// the same marshalled bytes over a small buffered channel. Two topic shapes
// are used throughout the app — see CollectionTopic and UserTopic — but the
// hub itself treats a topic as an opaque string.
package live

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"list/internal/metrics"
)

// sendBuffer is the per-subscriber channel capacity. A publish is a
// non-blocking send (see Publish), so this is how much slack a slow reader
// gets before it is dropped rather than stalling the publisher.
const sendBuffer = 16

// maxSubsPerUser caps how many live connections a single user may hold at
// once, across every topic. It exists so a stuck tab or a reconnect loop
// cannot accumulate goroutines and channels without bound.
const maxSubsPerUser = 8

// CollectionTopic is the topic everyone with a given collection page open
// subscribes to.
func CollectionTopic(id int64) string { return fmt.Sprintf("collection:%d", id) }

// UserTopic is the topic every page a given user has open, anywhere,
// subscribes to.
func UserTopic(id int64) string { return fmt.Sprintf("user:%d", id) }

// Event is the payload delivered to subscribers, marshalled to JSON once per
// Publish and fanned out as the same bytes to everyone on the topic.
type Event struct {
	Kind       string `json:"kind"`                 // "item" | "members" | "collection" | "collections" | "access"
	ID         int64  `json:"id,omitempty"`         // item id, for Kind=="item"
	Collection int64  `json:"collection,omitempty"` // for Kind=="access"
	Action     string `json:"action,omitempty"`     // "created"|"updated"|"deleted"|"renamed"|"revoked"
	Origin     string `json:"origin,omitempty"`     // originating tab id, so a browser can ignore its own echo
}

// subscriber is one live connection. It tracks its own topic list so that
// unsubscribing is O(len(topics)) rather than a scan of every topic the hub
// knows about, and its own userID so the connection cap and Evict's
// per-user filter don't need a separate lookup.
//
// Its channel is closed from four different places — a slow-consumer drop
// in Publish, Evict, Hub.Close, and the cancel func Subscribe hands back to
// the caller — so closeOnce is what stands between that and a double-close
// panic. This is the single detail in this package most worth getting
// right.
type subscriber struct {
	ch        chan []byte
	userID    int64
	topics    []string
	closeOnce sync.Once
}

// Hub is a topic → subscriber registry. The zero value is not usable;
// construct one with New. A Hub is safe for concurrent use.
type Hub struct {
	mu        sync.Mutex
	topics    map[string]map[*subscriber]struct{}
	userConns map[int64]map[*subscriber]struct{}
	closed    bool
}

// New returns a ready-to-use Hub.
func New() *Hub {
	return &Hub{
		topics:    make(map[string]map[*subscriber]struct{}),
		userConns: make(map[int64]map[*subscriber]struct{}),
	}
}

// Subscribe registers a new subscriber for every named topic. The returned
// channel receives the raw JSON bytes of each Event published to any of
// those topics; it is closed (readable-until-drained, then closed) when the
// subscriber is dropped for being slow, evicted, unsubscribed, or the hub
// itself is closed.
//
// The returned func unsubscribes; it is safe to call more than once and
// safe to call even when ok is false.
//
// ok is false, with a nil channel and a no-op func, if the hub is closed or
// if userID already holds maxSubsPerUser connections — the caller should
// respond 429 rather than evict an older connection, so a hoard of stale
// tabs can't be used to force out a live one.
func (h *Hub) Subscribe(userID int64, topics ...string) (<-chan []byte, func(), bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed || len(h.userConns[userID]) >= maxSubsPerUser {
		return nil, func() {}, false
	}

	sub := &subscriber{
		ch:     make(chan []byte, sendBuffer),
		userID: userID,
		topics: append([]string(nil), topics...),
	}

	for _, topic := range topics {
		if h.topics[topic] == nil {
			h.topics[topic] = make(map[*subscriber]struct{})
		}
		h.topics[topic][sub] = struct{}{}
	}

	if h.userConns[userID] == nil {
		h.userConns[userID] = make(map[*subscriber]struct{})
	}
	h.userConns[userID][sub] = struct{}{}

	return sub.ch, func() { h.unsubscribe(sub) }, true
}

// unsubscribe removes sub from every topic and from the per-user connection
// set, then closes its channel. Map deletes on an already-absent key are
// no-ops and closeOnce guards the close, so calling this twice for the same
// subscriber (e.g. the caller's cancel func racing a slow-consumer drop) is
// harmless.
func (h *Hub) unsubscribe(sub *subscriber) {
	h.mu.Lock()
	for _, topic := range sub.topics {
		if subs, ok := h.topics[topic]; ok {
			delete(subs, sub)
			if len(subs) == 0 {
				delete(h.topics, topic)
			}
		}
	}
	if conns, ok := h.userConns[sub.userID]; ok {
		delete(conns, sub)
		if len(conns) == 0 {
			delete(h.userConns, sub.userID)
		}
	}
	h.mu.Unlock()

	sub.closeOnce.Do(func() { close(sub.ch) })
}

// Publish marshals ev once and fans the same bytes out to every subscriber
// on topic. The send to each subscriber is non-blocking: a stalled reader
// must never be able to block a publisher, since the publisher is an HTTP
// handler goroutine that a caller (and, transitively, whoever is waiting on
// their own response) is blocked behind. A subscriber whose buffer is full
// is dropped instead — its channel is closed, ending its handler — which is
// safe because the client re-syncs from scratch on reconnect, making a
// dropped event self-healing rather than a permanent inconsistency.
func (h *Hub) Publish(topic string, ev Event) {
	data, err := json.Marshal(ev)
	if err != nil {
		// Event is a flat struct of strings and ints; this cannot fail in
		// practice. Nothing sensible to do but drop the publish.
		return
	}

	h.mu.Lock()
	var drop []*subscriber
	for sub := range h.topics[topic] {
		select {
		case sub.ch <- data:
			metrics.LiveEventSent()
		default:
			drop = append(drop, sub)
		}
	}
	h.mu.Unlock()

	for _, sub := range drop {
		metrics.LiveEventDropped()
		h.unsubscribe(sub)
	}
}

// Evict closes subscribers on topic. If userID is non-zero only that user's
// subscribers go; if zero, all of them do.
//
// Evict never drains a subscriber's channel before closing it, only stops
// accepting new sends into it — so a Publish immediately followed by an
// Evict of the same subscriber reliably delivers that last event: it is
// already sitting in the channel buffer, and a reader can still receive
// everything buffered before it sees the channel close. This is what lets
// callers queue a final "you were removed" event and then evict in the same
// breath.
func (h *Hub) Evict(topic string, userID int64) {
	h.mu.Lock()
	var drop []*subscriber
	for sub := range h.topics[topic] {
		if userID != 0 && sub.userID != userID {
			continue
		}
		drop = append(drop, sub)
	}
	h.mu.Unlock()

	for _, sub := range drop {
		h.unsubscribe(sub)
	}
}

// HasUserSubscribers reports whether any "user:" topic currently has a
// subscriber. It lets a hot-path handler (e.g. a checkbox toggle) skip an
// extra membership query to build a fan-out list when nobody has an index
// page open to notify. The scan is over the set of distinct topics, not
// subscribers, so it stays cheap regardless of connection count.
func (h *Hub) HasUserSubscribers() bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	for topic := range h.topics {
		if strings.HasPrefix(topic, "user:") {
			return true
		}
	}
	return false
}

// Close closes every subscriber's channel and marks the hub closed, so
// later Subscribe calls fail with ok==false instead of registering a
// subscriber nobody will ever evict. Close is idempotent — calling it more
// than once (e.g. a shutdown path racing a test) is safe.
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true

	// Walk userConns rather than topics: each subscriber appears exactly
	// once there regardless of how many topics it's on, so this naturally
	// dedupes without needing a seen-set.
	var all []*subscriber
	for _, conns := range h.userConns {
		for sub := range conns {
			all = append(all, sub)
		}
	}
	h.topics = make(map[string]map[*subscriber]struct{})
	h.userConns = make(map[int64]map[*subscriber]struct{})
	h.mu.Unlock()

	for _, sub := range all {
		sub.closeOnce.Do(func() { close(sub.ch) })
	}
}
