package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// syncCommands registers the command menu, but only when it differs from what
// the bot already advertises. getMyCommands returns the current set; if it
// already matches, no setMyCommands call is made.
func (t *Telegram) syncCommands(ctx context.Context) error {
	current, err := t.call(ctx, "getMyCommands", url.Values{})
	if err != nil {
		return err
	}
	var existing []botCommand
	if err := json.Unmarshal(current, &existing); err != nil {
		// A decode failure shouldn't block registration — fall through and set.
		existing = nil
	}
	if sameCommands(existing, botCommands) {
		t.log.Debug("command menu already current")
		return nil
	}

	encoded, err := json.Marshal(botCommands)
	if err != nil {
		return err
	}
	if _, err := t.call(ctx, "setMyCommands", url.Values{"commands": {string(encoded)}}); err != nil {
		return err
	}
	t.log.Info("registered telegram command menu", "commands", len(botCommands))
	return nil
}

// sameCommands compares two command sets by name and description, order
// included — Telegram returns them in the order they were set.
func sameCommands(a, b []botCommand) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Command != b[i].Command || a[i].Description != b[i].Description {
			return false
		}
	}
	return true
}

// clearChat deletes the tracked messages and resets the conversation. Telegram
// only lets a bot delete messages from the last 48h, and only ones it has the
// ID for (since this process started), so this clears the recent visible chat
// rather than the entire history — the most a bot can do. Deletion is best
// effort: a message too old or already gone is skipped silently.
func (t *Telegram) clearChat(ctx context.Context, chatID int64) {
	t.mu.Lock()
	ids := t.deletable[chatID]
	delete(t.deletable, chatID)
	delete(t.sessions, chatID)
	t.mu.Unlock()

	for _, id := range ids {
		t.deleteMessage(ctx, chatID, id)
	}
}

func (t *Telegram) deleteMessage(ctx context.Context, chatID, msgID int64) {
	params := url.Values{
		"chat_id":    {fmt.Sprint(chatID)},
		"message_id": {fmt.Sprint(msgID)},
	}
	if _, err := t.call(ctx, "deleteMessage", params); err != nil {
		// Expected for anything older than 48h or already deleted.
		t.log.Debug("delete message skipped", "chat_id", chatID, "message_id", msgID, "error", err)
	}
}

// track records a message ID as deletable by a future /new, bounding the list.
func (t *Telegram) track(chatID, msgID int64) {
	if msgID == 0 {
		return
	}
	t.mu.Lock()
	ids := append(t.deletable[chatID], msgID)
	if len(ids) > maxTrackedMessages {
		ids = ids[len(ids)-maxTrackedMessages:]
	}
	t.deletable[chatID] = ids
	t.mu.Unlock()
}

// trackSent records the ID of a message the bot just sent, from the
// sendMessage response, so /new can delete it too.
func (t *Telegram) trackSent(chatID int64, result json.RawMessage) {
	var m struct {
		MessageID int64 `json:"message_id"`
	}
	if err := json.Unmarshal(result, &m); err == nil {
		t.track(chatID, m.MessageID)
	}
}

// modelReport describes the configured provider chain for /model: which model
// runs and what it falls back to.
func (t *Telegram) modelReport() string {
	if len(t.modelChain) == 0 {
		return "No model configured."
	}
	if len(t.modelChain) == 1 {
		return "Model: " + t.modelChain[0] + "\n(no fallback)"
	}
	return "Model: " + t.modelChain[0] +
		"\nFallback: " + strings.Join(t.modelChain[1:], " → ") +
		"\n(restarts from the primary each turn; falls back on error)"
}

func (t *Telegram) session(chatID int64) *session {
	t.mu.Lock()
	defer t.mu.Unlock()

	sess, ok := t.sessions[chatID]
	if ok && sess.idleSince(time.Now()) < t.sessionTTL {
		return sess
	}
	if ok {
		// Expired sessions are dropped to bound memory and cost.
		t.log.Info("session expired, starting fresh", "chat_id", chatID)
	}
	sess = &session{lastSeen: time.Now()}
	t.sessions[chatID] = sess
	return sess
}
