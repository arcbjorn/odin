package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/arcbjorn/odin/model"
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

// maxListedModels bounds how much of one provider's catalog /model list
// prints. A full catalog can run past a hundred entries and turn a chat
// command into several screens; any model can still be selected by name
// whether or not it was listed.
const maxListedModels = 30

// handleModel implements /model. Bare shows the active target, "list" prints
// the live catalogs, "reset" restores config.toml, and anything else is a
// switch target.
//
// Deliberately message-driven rather than an inline keyboard: this gateway
// polls for messages only, and adding callback queries would widen the
// transport surface for a command that reads fine as text.
func (t *Telegram) handleModel(ctx context.Context, args string) string {
	if t.switcher == nil {
		return t.modelReport()
	}
	switch strings.ToLower(args) {
	case "":
		return t.modelReport()
	case "list", "ls":
		options, err := t.switcher.Options(ctx)
		if err != nil {
			return "Could not list models: " + err.Error()
		}
		return t.modelOptions(options)
	case "reset", "default":
		target, err := t.switcher.Reset()
		if err != nil {
			return "Restored " + code(target) + ", but: " + err.Error()
		}
		return "Restored the configured chain.\n**Model:** " + code(target)
	}

	change, err := t.switcher.Switch(ctx, args)
	if err != nil {
		return "Cannot switch: " + err.Error()
	}
	t.log.Info("model switched",
		"target", change.Target, "previous", change.Previous, "resolved_via", change.ResolvedVia)

	var b strings.Builder
	b.WriteString("**Model:** " + code(change.Target))
	if change.ResolvedVia != "" && change.ResolvedVia != "catalog" {
		b.WriteString("  (" + change.ResolvedVia + ")")
	}
	if change.Previous != "" && change.Previous != change.Target {
		b.WriteString("\nWas: " + code(change.Previous))
	}
	if change.ProviderChanged {
		b.WriteString("\nProvider changed — the rest of the chain stays as fallback.")
	}
	if change.Warning != "" {
		b.WriteString("\n⚠️ " + change.Warning)
	}
	b.WriteString("\n\nThis applies to chat only; scheduled jobs keep the configured model.")
	return b.String()
}

// modelReport describes what runs now and what it falls back to.
func (t *Telegram) modelReport() string {
	if t.switcher == nil {
		return t.staticModelReport()
	}
	target, overridden := t.switcher.Current()
	if target == "" {
		return "No model configured."
	}

	var b strings.Builder
	b.WriteString("**Model:** " + code(target))
	if overridden {
		b.WriteString("  (override)")
	}

	// The active provider is promoted to primary on a switch, so the fallback
	// order is the rest of the configured chain in its committed order.
	activeProvider, _, _ := strings.Cut(target, "/")
	configured := t.switcher.Configured()
	fallback := make([]string, 0, len(configured))
	for _, entry := range configured {
		if name, _, _ := strings.Cut(entry, "/"); name == activeProvider {
			continue
		}
		fallback = append(fallback, entry)
	}
	if len(fallback) > 0 {
		b.WriteString("\n**Fallback:** " + strings.Join(fallback, " → "))
		b.WriteString("\n(restarts from the primary each turn; falls back on error)")
	} else {
		b.WriteString("\n(no fallback)")
	}
	if overridden && len(configured) > 0 {
		b.WriteString("\n**config.toml:** " + code(configured[0]))
	}
	b.WriteString("\n\n`/model NAME` switch · `/model list` catalog · `/model reset` restore")
	return b.String()
}

// staticModelReport is the read-only view used when no switcher is wired.
func (t *Telegram) staticModelReport() string {
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

// modelOptions renders the per-provider catalogs, marking the active model.
func (t *Telegram) modelOptions(options []model.ProviderModels) string {
	if len(options) == 0 {
		return "No providers configured."
	}
	target, _ := t.switcher.Current()

	var b strings.Builder
	for i, option := range options {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("**" + option.Provider + "**\n")
		if option.Err != "" {
			// Still switchable at its configured model, so say so rather than
			// leaving the provider looking unusable.
			b.WriteString("· " + code(option.Provider+"/"+option.Configured) + " (configured)\n")
			b.WriteString("_" + option.Err + "_\n")
			continue
		}
		listed := option.Models
		if len(listed) > maxListedModels {
			listed = listed[:maxListedModels]
		}
		for _, id := range listed {
			line := "· " + code(id)
			if option.Provider+"/"+id == target {
				line += " ← active"
			} else if id == option.Configured {
				line += " (configured)"
			}
			b.WriteString(line + "\n")
		}
		if rest := len(option.Models) - len(listed); rest > 0 {
			b.WriteString(fmt.Sprintf("_…%d more; `odin models` lists them all_\n", rest))
		}
	}
	b.WriteString("\n`/model NAME` to switch — any catalog model works, listed or not.")
	return b.String()
}

// code wraps a value in backticks so Telegram renders it monospace and
// tap-to-copy, which is how a model id gets reused in the next command.
func code(s string) string {
	if s == "" {
		return "(none)"
	}
	return "`" + s + "`"
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
