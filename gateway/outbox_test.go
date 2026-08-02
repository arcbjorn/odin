package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newGatewayWithOutbox(t *testing.T, path string) (*Telegram, *fakeTelegram) {
	t.Helper()
	fake := &fakeTelegram{}
	srv := fake.server(t)

	g, err := NewTelegram(TelegramConfig{
		Token:        "test-token",
		AllowedUsers: []int64{1},
		Agent:        &fakeAgent{reply: "x"},
		Logger:       quiet(),
		OutboxPath:   path,
	})
	if err != nil {
		t.Fatalf("NewTelegram: %v", err)
	}
	g.http = srv.Client()
	g.baseURL = srv.URL
	return g, fake
}

// The bug this whole mechanism exists for: a failed delivery used to return
// nil, so the scheduler recorded the run as a success, status said "last run
// ok", and the watchdog had nothing to alert on while the output was gone.
func TestNotifyReportsDeliveryFailure(t *testing.T) {
	g, fake := newGatewayWithOutbox(t, "")
	fake.setFailSends(true, 0)

	err := g.Notify(context.Background(), 1, "the morning brief")
	if err == nil {
		t.Fatal("a failed delivery must be reported, not swallowed")
	}
	if !strings.Contains(err.Error(), "queued") {
		t.Fatalf("error should say the message was kept: %v", err)
	}
}

func TestNotifyQueuesUndeliveredMessage(t *testing.T) {
	g, fake := newGatewayWithOutbox(t, "")
	fake.setFailSends(true, 0)

	_ = g.Notify(context.Background(), 1, "the morning brief")
	if got := g.outbox.pending(); got != 1 {
		t.Fatalf("pending = %d, want the message queued for retry", got)
	}
}

func TestOutboxRetriesUntilDelivered(t *testing.T) {
	g, fake := newGatewayWithOutbox(t, "")
	fake.setFailSends(true, 0)

	_ = g.Notify(context.Background(), 1, "the morning brief")

	// Telegram comes back.
	fake.setFailSends(false, 0)
	g.outbox.flush(context.Background(), g.send, time.Now())

	if got := g.outbox.pending(); got != 0 {
		t.Fatalf("pending = %d, want the queue drained", got)
	}
	msgs := fake.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[len(msgs)-1], "the morning brief") {
		t.Fatalf("message not delivered on retry: %v", msgs)
	}
}

// Tokens are already spent when delivery fails, so the content has to outlive
// the process that produced it.
func TestOutboxSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "outbox.json")

	first, firstFake := newGatewayWithOutbox(t, path)
	firstFake.setFailSends(true, 0)
	_ = first.Notify(context.Background(), 1, "the morning brief")
	if first.outbox.pending() != 1 {
		t.Fatal("message was not queued")
	}

	// A new process reads the same state directory.
	second, secondFake := newGatewayWithOutbox(t, path)
	if got := second.outbox.pending(); got != 1 {
		t.Fatalf("restored pending = %d, want 1", got)
	}
	second.outbox.flush(context.Background(), second.send, time.Now())

	if got := second.outbox.pending(); got != 0 {
		t.Fatalf("pending after flush = %d", got)
	}
	msgs := secondFake.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "the morning brief") {
		t.Fatalf("restored message not delivered: %v", msgs)
	}
}

// A brief that arrives hours late must not read as if it were written now.
func TestOutboxLabelsDelayedMessages(t *testing.T) {
	g, fake := newGatewayWithOutbox(t, "")
	queued := time.Now()
	g.outbox.queue(1, "the morning brief", queued)

	g.outbox.flush(context.Background(), g.send, queued.Add(3*time.Hour))

	msgs := fake.messages()
	if len(msgs) != 1 {
		t.Fatalf("want one message, got %v", msgs)
	}
	if !strings.Contains(msgs[0], "delayed") {
		t.Fatalf("late message should be labelled: %q", msgs[0])
	}
	if !strings.Contains(msgs[0], "the morning brief") {
		t.Fatalf("label must not replace the content: %q", msgs[0])
	}
}

func TestOutboxDeliversPromptRetryWithoutALabel(t *testing.T) {
	g, fake := newGatewayWithOutbox(t, "")
	queued := time.Now()
	g.outbox.queue(1, "the morning brief", queued)

	g.outbox.flush(context.Background(), g.send, queued.Add(30*time.Second))

	if msgs := fake.messages(); len(msgs) != 1 || strings.Contains(msgs[0], "delayed") {
		t.Fatalf("a prompt retry needs no delay label: %v", msgs)
	}
}

// Past a point, delivering is the wrong outcome — the scheduler skips stale
// runs for the same reason.
func TestOutboxDropsStaleMessages(t *testing.T) {
	g, fake := newGatewayWithOutbox(t, "")
	queued := time.Now()
	g.outbox.queue(1, "yesterday's brief", queued)

	g.outbox.flush(context.Background(), g.send, queued.Add(maxOutboxAge+time.Minute))

	if got := g.outbox.pending(); got != 0 {
		t.Fatalf("pending = %d, want the stale entry dropped", got)
	}
	if msgs := fake.messages(); len(msgs) != 0 {
		t.Fatalf("a stale message must not be delivered: %v", msgs)
	}
}

func TestOutboxDropsAfterTooManyAttempts(t *testing.T) {
	g, fake := newGatewayWithOutbox(t, "")
	fake.setFailSends(true, 0)
	now := time.Now()
	g.outbox.queue(1, "the morning brief", now)

	for i := 0; i < maxOutboxAttempts+1; i++ {
		g.outbox.flush(context.Background(), g.send, now)
	}
	if got := g.outbox.pending(); got != 0 {
		t.Fatalf("pending = %d, want the entry abandoned after %d attempts", got, maxOutboxAttempts)
	}
}

// An outage that lasts must not grow the queue without bound.
func TestOutboxBoundsQueueSize(t *testing.T) {
	g, fake := newGatewayWithOutbox(t, "")
	fake.setFailSends(true, 0)
	now := time.Now()

	for i := 0; i < maxOutboxEntries+10; i++ {
		g.outbox.queue(1, "brief", now)
	}
	if got := g.outbox.pending(); got != maxOutboxEntries {
		t.Fatalf("pending = %d, want it capped at %d", got, maxOutboxEntries)
	}
}

// Re-sending chunks the user already received would duplicate them, so only
// the remainder is queued.
func TestOutboxQueuesOnlyUndeliveredChunks(t *testing.T) {
	g, fake := newGatewayWithOutbox(t, "")

	// Two blocks, each under the limit, separated by the paragraph break
	// splitMessage prefers — so the split lands exactly between them.
	long := strings.Repeat("alpha ", 500) + "\n\n" + strings.Repeat("omega ", 500)
	if got := len(splitMessage(long, maxMessageRunes)); got != 2 {
		t.Fatalf("fixture split into %d chunks, want exactly 2", got)
	}

	// The first chunk lands, then Telegram goes down.
	fake.setFailSends(true, 1)
	if err := g.Notify(context.Background(), 1, long); err == nil {
		t.Fatal("partial delivery must still report failure")
	}

	fake.setFailSends(false, 0)
	g.outbox.flush(context.Background(), g.send, time.Now())

	msgs := fake.messages()
	withAlpha, withOmega := 0, 0
	for _, m := range msgs {
		if strings.Contains(m, "alpha") {
			withAlpha++
		}
		if strings.Contains(m, "omega") {
			withOmega++
		}
	}
	if withOmega != 1 {
		t.Fatalf("undelivered chunk sent %d times, want exactly 1: %d messages", withOmega, len(msgs))
	}
	if withAlpha != 1 {
		t.Fatalf("delivered chunk sent %d times, want exactly 1 — the retry duplicated it", withAlpha)
	}
}

// No retry fixes a message addressed to a chat that is not allowed to receive
// it, so it must fail loudly and never enter the queue.
func TestNotifyDoesNotQueueForDisallowedChat(t *testing.T) {
	g, _ := newGatewayWithOutbox(t, "")

	if err := g.Notify(context.Background(), 999, "brief"); err == nil {
		t.Fatal("an unallowed chat must be rejected")
	}
	if got := g.outbox.pending(); got != 0 {
		t.Fatalf("pending = %d, want nothing queued", got)
	}
}

// Interactive replies are not queued: the user is present, and answering a
// question they have moved on from is worse than not answering.
func TestChatReplyFailureIsNotQueued(t *testing.T) {
	g, fake := newGatewayWithOutbox(t, "")
	fake.setFailSends(true, 0)

	g.respond(context.Background(), 1, "hello")

	if got := g.outbox.pending(); got != 0 {
		t.Fatalf("pending = %d, want chat replies left out of the outbox", got)
	}
}

// A corrupt state file must not stop the agent from starting.
func TestOutboxDiscardsUnreadableState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outbox.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := newOutbox(path, quiet())
	if o.pending() != 0 {
		t.Fatal("a corrupt outbox should start empty rather than fail")
	}
}
