package live

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// recv reads one message from ch or fails the test after a generous
// timeout. Publish is synchronous, so a correct implementation delivers
// near-instantly; a hang here means a bug, not a slow machine.
func recv(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case msg, ok := <-ch:
		if !ok {
			t.Fatal("channel closed while waiting for a message")
		}
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a message")
		return nil
	}
}

func TestPublishFansOutToAllSubscribers(t *testing.T) {
	// If a publish only reached one subscriber, two browser tabs open on the
	// same collection would drift out of sync with each other, which is the
	// exact problem this whole package exists to solve.
	h := New()
	defer h.Close()

	const n = 5
	chans := make([]<-chan []byte, n)
	for i := range chans {
		ch, _, ok := h.Subscribe(int64(i+1), "collection:1")
		if !ok {
			t.Fatalf("Subscribe %d: not ok", i)
		}
		chans[i] = ch
	}

	h.Publish("collection:1", Event{Kind: "item", ID: 42, Action: "created"})

	for i, ch := range chans {
		var ev Event
		if err := json.Unmarshal(recv(t, ch), &ev); err != nil {
			t.Fatalf("subscriber %d: unmarshal: %v", i, err)
		}
		if ev.Kind != "item" || ev.ID != 42 || ev.Action != "created" {
			t.Fatalf("subscriber %d: got %+v, want item/42/created", i, ev)
		}
	}
}

func TestPublishDoesNotCrossTopics(t *testing.T) {
	// Without per-topic isolation, every browser would receive every other
	// collection's edits too — a privacy leak (their existence and activity
	// are visible to people not on that collection) and needless client
	// work re-fetching rows it has no business knowing changed.
	h := New()
	defer h.Close()

	ch1, _, _ := h.Subscribe(1, "collection:1")
	ch2, _, _ := h.Subscribe(2, "collection:2")

	h.Publish("collection:1", Event{Kind: "item", ID: 1})

	recv(t, ch1) // confirms the publish landed at all, before we assert ch2 stayed silent

	select {
	case msg, ok := <-ch2:
		t.Fatalf("collection:2 subscriber received a collection:1 publish (open=%v): %s", ok, msg)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSubscriberReceivesFromEveryTopicItJoined(t *testing.T) {
	// A collection page subscribes to both collection:{id} and user:{id} at
	// once (item events plus access-revoked notices); if the hub only kept
	// one topic per subscriber it could deliver one and silently drop the
	// other.
	h := New()
	defer h.Close()

	ch, _, ok := h.Subscribe(1, "collection:1", "user:1")
	if !ok {
		t.Fatal("Subscribe: not ok")
	}

	h.Publish("collection:1", Event{Kind: "item", ID: 1})
	recv(t, ch)

	h.Publish("user:1", Event{Kind: "collections"})
	recv(t, ch)
}

func TestSlowConsumerIsDroppedWithoutBlockingThePublisher(t *testing.T) {
	// Publish runs inside an HTTP handler goroutine (e.g. handleToggleItem).
	// If a stalled reader could make Publish block, one dead browser tab
	// would hang every request that mutates a collection it was watching —
	// for everyone, not just the stalled tab.
	h := New()
	defer h.Close()

	ch, _, ok := h.Subscribe(1, "collection:1")
	if !ok {
		t.Fatal("Subscribe: not ok")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < sendBuffer+5; i++ {
			h.Publish("collection:1", Event{Kind: "item", ID: int64(i)})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow consumer instead of dropping it")
	}

	// The dropped subscriber's channel is closed once whatever made it into
	// the buffer before the drop has been drained.
	drained := make(chan struct{})
	go func() {
		for {
			if _, ok := <-ch; !ok {
				close(drained)
				return
			}
		}
	}()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber channel never closed after being dropped")
	}
}

func TestUnsubscribeStopsDeliveryAndIsIdempotent(t *testing.T) {
	// The cancel func returned by Subscribe runs from a deferred call in the
	// SSE handler, which can run down more than one path to the same defer;
	// a second call must not double-close the channel, and a publish after
	// unsubscribing must not still reach it.
	h := New()
	defer h.Close()

	ch, cancel, ok := h.Subscribe(1, "collection:1")
	if !ok {
		t.Fatal("Subscribe: not ok")
	}

	cancel()
	cancel() // must not panic

	h.Publish("collection:1", Event{Kind: "item", ID: 1})

	select {
	case msg, ok := <-ch:
		if ok {
			t.Fatalf("received a publish after unsubscribe: %s", msg)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("channel neither closed nor delivered after unsubscribe")
	}
}

func TestEvictClosesOnlyTheGivenUser(t *testing.T) {
	// handleRemoveMember evicts just the removed member so the rest of the
	// collection's viewers keep their live connection uninterrupted.
	h := New()
	defer h.Close()

	chA, _, _ := h.Subscribe(1, "collection:1")
	chB, _, _ := h.Subscribe(2, "collection:1")

	h.Evict("collection:1", 1)

	if _, ok := <-chA; ok {
		t.Fatal("evicted user's channel is still open")
	}

	h.Publish("collection:1", Event{Kind: "members"})
	recv(t, chB) // the untouched subscriber must still be reachable
}

func TestEvictWithZeroUserIDClosesEveryoneOnTheTopic(t *testing.T) {
	// handleDeleteCollection evicts every viewer on collection:{id} at once
	// — there's no single "removed member" to target, so userID==0 must
	// mean "everybody".
	h := New()
	defer h.Close()

	chA, _, _ := h.Subscribe(1, "collection:1")
	chB, _, _ := h.Subscribe(2, "collection:1")

	h.Evict("collection:1", 0)

	if _, ok := <-chA; ok {
		t.Fatal("subscriber A still open after whole-topic evict")
	}
	if _, ok := <-chB; ok {
		t.Fatal("subscriber B still open after whole-topic evict")
	}
}

func TestEvictDeliversAnAlreadyQueuedEventBeforeClosing(t *testing.T) {
	// handleRemoveMember publishes an access/revoked event and evicts the
	// same subscriber right after, relying on the buffered channel to
	// deliver that last event instead of dropping it — Evict must close
	// without draining.
	h := New()
	defer h.Close()

	ch, _, _ := h.Subscribe(1, "user:1")

	h.Publish("user:1", Event{Kind: "access", Collection: 5, Action: "revoked"})
	h.Evict("user:1", 1)

	msg, ok := <-ch
	if !ok {
		t.Fatal("evict discarded the already-queued event instead of delivering it")
	}
	var ev Event
	if err := json.Unmarshal(msg, &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.Kind != "access" || ev.Action != "revoked" {
		t.Fatalf("got %+v, want access/revoked", ev)
	}

	if _, ok := <-ch; ok {
		t.Fatal("channel still open after its one queued event was drained")
	}
}

func TestCloseClosesEveryoneAndIsIdempotent(t *testing.T) {
	// Close runs before srv.Shutdown so live connections don't hold the
	// process open for the shutdown timeout on every deploy. If it panicked
	// on a second call, or left a subscriber open, that ordering couldn't
	// be made safe to retry.
	h := New()

	ch1, _, _ := h.Subscribe(1, "collection:1")
	ch2, _, _ := h.Subscribe(2, "user:2")

	h.Close()
	h.Close() // must not panic

	if _, ok := <-ch1; ok {
		t.Fatal("ch1 still open after Close")
	}
	if _, ok := <-ch2; ok {
		t.Fatal("ch2 still open after Close")
	}

	if _, _, ok := h.Subscribe(3, "collection:1"); ok {
		t.Fatal("Subscribe succeeded after Close")
	}
}

func TestHasUserSubscribersTracksTransitions(t *testing.T) {
	// handleToggleItem uses this to skip a membership query entirely when
	// nobody has an index page open. A stale true just wastes a query, but
	// a stale false would mean an open index page's progress ring never
	// moves — so both directions of the transition matter.
	h := New()
	defer h.Close()

	if h.HasUserSubscribers() {
		t.Fatal("HasUserSubscribers true before anyone subscribed")
	}

	_, cancelCollection, _ := h.Subscribe(1, "collection:1")
	if h.HasUserSubscribers() {
		t.Fatal("HasUserSubscribers true for a collection-only subscriber")
	}

	_, cancelUser, _ := h.Subscribe(1, "user:1")
	if !h.HasUserSubscribers() {
		t.Fatal("HasUserSubscribers false after a user: subscription")
	}

	cancelUser()
	if h.HasUserSubscribers() {
		t.Fatal("HasUserSubscribers true after the only user: subscriber unsubscribed")
	}

	cancelCollection()
}

func TestConnectionCapIsPerUser(t *testing.T) {
	// A tab hoard or a broken reconnect loop must not be able to exhaust
	// goroutines/channels for the whole process. The cap rejects the new
	// connection rather than evicting an old one, so a runaway reconnect
	// loop can't be used to kick a legitimate tab offline; it must also be
	// scoped per user, or one person's hoard would lock everyone else out.
	h := New()
	defer h.Close()

	var cancels []func()
	for i := 0; i < maxSubsPerUser; i++ {
		_, cancel, ok := h.Subscribe(1, "collection:1")
		if !ok {
			t.Fatalf("subscribe %d for user 1: not ok, want ok", i)
		}
		cancels = append(cancels, cancel)
	}

	if _, _, ok := h.Subscribe(1, "collection:1"); ok {
		t.Fatal("9th subscribe for the same user succeeded; want the cap enforced")
	}

	if _, _, ok := h.Subscribe(2, "collection:1"); !ok {
		t.Fatal("a different user was rejected by user 1's cap")
	}

	for _, cancel := range cancels {
		cancel()
	}
}

func TestConcurrentUseIsRaceFree(t *testing.T) {
	// This is the first genuinely concurrent shared state in the app,
	// exercised from unrelated HTTP handler goroutines in production. The
	// other tests establish correctness; this one establishes that the
	// mutex actually protects everything it needs to under real contention
	// — it must pass under `go test -race`.
	h := New()

	const workers = 20
	const iterations = 100

	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(userID int64) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				ch, cancel, ok := h.Subscribe(userID, "collection:1", "user:1")
				if !ok {
					continue // cap hit or hub closed mid-test; both are legitimate under contention
				}
				select {
				case <-ch:
				default:
				}
				cancel()
				cancel() // exercise the idempotent path under contention too
			}
		}(int64(w % 5)) // small user pool so the per-user cap is actually exercised
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				h.Publish("collection:1", Event{Kind: "item", ID: int64(i)})
				h.Publish("user:1", Event{Kind: "collections"})
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			h.Evict("collection:1", int64(i%5))
		}
	}()

	wg.Wait()
	h.Close()
	h.Close() // still must not panic after concurrent use
}
