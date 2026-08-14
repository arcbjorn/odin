package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const outboxVersion = 1

// Bounds on the outbox. A queue that grows without limit, or that replays a
// day-old briefing as if it were fresh, is worse than the loss it prevents.
const (
	maxOutboxEntries  = 50
	maxOutboxAttempts = 20
	maxOutboxAge      = 24 * time.Hour

	// delayNoticeAfter is how late a message has to be before it is labelled.
	// A scheduled brief that arrives hours after its window must not read as
	// if it were written just now — the scheduler skips stale runs for the
	// same reason.
	delayNoticeAfter = 5 * time.Minute
)

// outbox persists messages that could not be delivered so an outage costs a
// late message rather than the model's work.
//
// It exists because scheduled jobs have no one waiting: an interactive turn
// that fails to send is retried by the user, but a 07:00 brief that fails to
// send is simply gone, tokens already spent. Entries survive a restart, and a
// failed Notify still reports its error so the run is recorded as failed —
// the queue makes the content recoverable, it does not hide the fault.
type outbox struct {
	// path is where entries persist. Empty keeps the queue in memory only,
	// which is what a gateway built without a profile directory gets.
	path string
	log  *slog.Logger

	mu      sync.Mutex
	entries []outboxEntry
}

type outboxEntry struct {
	ChatID   int64     `json:"chat_id"`
	Text     string    `json:"text"`
	QueuedAt time.Time `json:"queued_at"`
	Attempts int       `json:"attempts"`

	// Buttons is the keyboard derived from the original message, kept because
	// the queued text is often only the tail of it — re-deriving from that
	// remainder loses the numbered list and delivers the brief bare. Empty for
	// a message that never had one, and for entries written by an older build.
	Buttons []byte `json:"buttons,omitempty"`
}

type outboxFile struct {
	Version int           `json:"version"`
	Entries []outboxEntry `json:"entries"`
}

// newOutbox loads any persisted queue. A corrupt or unreadable file is
// reported and discarded rather than blocking startup: the agent running
// matters more than one undelivered message.
func newOutbox(path string, log *slog.Logger) *outbox {
	if log == nil {
		log = slog.Default()
	}
	o := &outbox{path: path, log: log}
	if path == "" {
		return o
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warn("could not read outbox; starting empty", "path", path, "error", err)
		}
		return o
	}
	var file outboxFile
	if err := json.Unmarshal(raw, &file); err != nil || file.Version != outboxVersion {
		log.Warn("discarding unreadable outbox", "path", path, "error", err)
		return o
	}
	o.entries = file.Entries
	if len(o.entries) > 0 {
		log.Info("restored undelivered messages", "count", len(o.entries))
	}
	return o
}

// queue stores a message for a later attempt, dropping the oldest entries
// once the queue is full.
func (o *outbox) queue(chatID int64, text string, now time.Time) {
	o.queueWithButtons(chatID, text, nil, now)
}

// queueWithButtons is queue, preserving the keyboard the original message
// carried so a retried remainder is not delivered bare.
func (o *outbox) queueWithButtons(chatID int64, text string, buttons []byte, now time.Time) {
	if text == "" {
		return
	}
	o.mu.Lock()
	o.entries = append(o.entries, outboxEntry{
		ChatID: chatID, Text: text, Buttons: buttons, QueuedAt: now,
	})
	if len(o.entries) > maxOutboxEntries {
		dropped := len(o.entries) - maxOutboxEntries
		o.log.Warn("outbox full, dropping oldest messages", "dropped", dropped)
		o.entries = o.entries[dropped:]
	}
	o.mu.Unlock()
	o.persist()
}

// sender delivers one message, returning whatever was left undelivered. It is
// satisfied by Telegram.sendWithButtons, so a partial multi-chunk send
// re-queues only the chunks that never landed instead of repeating the ones
// that did, and a retry keeps the keyboard the original message carried.
// buttons nil means "derive from text".
type sender func(ctx context.Context, chatID int64, text string, buttons []byte) (undelivered string, err error)

// flush retries every queued message once. Entries that are too old or that
// have failed too often are dropped with a warning: at some point a message
// is stale enough that delivering it is the wrong outcome.
func (o *outbox) flush(ctx context.Context, deliver sender, now time.Time) {
	o.mu.Lock()
	pending := append([]outboxEntry(nil), o.entries...)
	o.mu.Unlock()
	if len(pending) == 0 {
		return
	}

	var keep []outboxEntry
	delivered := 0
	for _, entry := range pending {
		if ctx.Err() != nil {
			keep = append(keep, entry)
			continue
		}
		age := now.Sub(entry.QueuedAt)
		if age > maxOutboxAge || entry.Attempts >= maxOutboxAttempts {
			o.log.Error("dropping undelivered message",
				"chat_id", entry.ChatID, "age", age.Round(time.Minute),
				"attempts", entry.Attempts, "bytes", len(entry.Text))
			continue
		}

		entry.Attempts++
		undelivered, err := deliver(ctx, entry.ChatID, withDelayNotice(entry.Text, age), entry.Buttons)
		if err == nil {
			delivered++
			continue
		}
		if undelivered != "" {
			entry.Text = undelivered
		}
		keep = append(keep, entry)
	}

	o.mu.Lock()
	o.entries = keep
	o.mu.Unlock()
	o.persist()

	if delivered > 0 {
		o.log.Info("delivered queued messages", "count", delivered, "remaining", len(keep))
	}
}

func (o *outbox) pending() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.entries)
}

// persist writes the queue atomically. A write failure is logged and the
// in-memory queue kept, so a full disk costs durability across a restart
// rather than the message itself.
func (o *outbox) persist() {
	if o.path == "" {
		return
	}
	o.mu.Lock()
	file := outboxFile{Version: outboxVersion, Entries: append([]outboxEntry(nil), o.entries...)}
	o.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(o.path), 0o700); err != nil {
		o.log.Warn("could not create outbox directory", "error", err)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(o.path), ".outbox-*.tmp")
	if err != nil {
		o.log.Warn("could not persist outbox", "error", err)
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		o.log.Warn("could not persist outbox", "error", err)
		return
	}
	if err := json.NewEncoder(tmp).Encode(file); err != nil {
		tmp.Close()
		o.log.Warn("could not persist outbox", "error", err)
		return
	}
	if err := tmp.Close(); err != nil {
		o.log.Warn("could not persist outbox", "error", err)
		return
	}
	if err := os.Rename(tmpName, o.path); err != nil {
		o.log.Warn("could not persist outbox", "error", err)
	}
}

// withDelayNotice labels a message that missed its moment, so late output is
// never mistaken for current.
func withDelayNotice(text string, age time.Duration) string {
	if age < delayNoticeAfter {
		return text
	}
	return fmt.Sprintf("_(delayed %s)_\n\n%s", roundAge(age), text)
}

func roundAge(age time.Duration) time.Duration {
	if age >= time.Hour {
		return age.Round(time.Hour)
	}
	return age.Round(time.Minute)
}
