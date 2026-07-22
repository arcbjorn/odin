// Package gateway connects the agent to a chat transport.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arcbjorn/odin/model"
)

const telegramAPI = "https://api.telegram.org"

// maxMessageRunes is Telegram's per-message limit. Longer replies are split on
// paragraph boundaries rather than truncated — a debrief cut mid-sentence is
// worse than two messages.
const maxMessageRunes = 4096

// Agent is the conversation driver the gateway calls. Implemented by
// profile.Runtime's loop; kept as an interface so the gateway can be tested
// without a provider.
type Agent interface {
	Run(ctx context.Context, history []model.Message) (text string, updated []model.Message, err error)
}

// Telegram is a long-polling Telegram gateway.
//
// Long-poll rather than webhooks: no inbound port, no TLS certificate, no
// reverse proxy. The agent runs on a box that only needs outbound HTTPS.
type Telegram struct {
	token   string
	allowed map[int64]bool
	agent   Agent
	log     *slog.Logger
	http    *http.Client

	// baseURL is the API root, overridden in tests to point at a stub.
	baseURL string

	// sessionTTL resets a conversation after inactivity, so a stale morning
	// context is not still in the prompt at midnight.
	sessionTTL time.Duration

	// modelChain is the configured provider chain ("name/model" each), primary
	// first. Static — reported by /model, never mutated.
	modelChain []string

	mu       sync.Mutex
	sessions map[int64]*session
	// deletable tracks message IDs per chat that /new may delete to clear the
	// visible chat. Populated as messages come and go; bounded by
	// maxTrackedMessages. Guarded by mu.
	deletable map[int64][]int64
	offset    int64

	// richDisabled latches once sendRichMessage proves unavailable, so later
	// sends skip the doomed rich attempt and go straight to plain text.
	richDisabled atomic.Bool
}

// session is one chat's conversation state.
//
// Two locks with distinct jobs. busy serializes whole turns so two messages
// arriving together cannot interleave into one history. mu guards the fields
// themselves, because session() reads lastSeen for the TTL check while a turn
// may be writing it — that read happens under the gateway's map lock, not
// under busy, so busy alone does not order it.
type session struct {
	busy sync.Mutex

	mu       sync.Mutex
	history  []model.Message
	lastSeen time.Time
}

func (s *session) snapshot() []model.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Message(nil), s.history...)
}

func (s *session) commit(history []model.Message, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = history
	s.lastSeen = now
}

func (s *session) idleSince(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return now.Sub(s.lastSeen)
}

// TelegramConfig configures the gateway.
type TelegramConfig struct {
	Token string

	// AllowedUsers is a strict allowlist of Telegram user IDs. Enforced at
	// the transport, before the message reaches the agent — an empty list is
	// a configuration error, never "allow everyone".
	AllowedUsers []int64

	Agent      Agent
	Logger     *slog.Logger
	SessionTTL time.Duration

	// ModelChain is the configured provider chain, "name/model" each, primary
	// first. Reported by the /model command.
	ModelChain []string
}

// NewTelegram builds the gateway.
func NewTelegram(cfg TelegramConfig) (*Telegram, error) {
	if cfg.Token == "" {
		return nil, errors.New("telegram token is required")
	}
	if len(cfg.AllowedUsers) == 0 {
		// Never expose an agent gateway without an explicit allowlist.
		return nil, errors.New("allowed_users is empty; refusing to run an open gateway")
	}
	if cfg.Agent == nil {
		return nil, errors.New("gateway requires an agent")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	ttl := cfg.SessionTTL
	if ttl == 0 {
		ttl = 4 * time.Hour
	}

	allowed := make(map[int64]bool, len(cfg.AllowedUsers))
	for _, id := range cfg.AllowedUsers {
		allowed[id] = true
	}

	return &Telegram{
		token:      cfg.Token,
		allowed:    allowed,
		agent:      cfg.Agent,
		log:        cfg.Logger,
		baseURL:    telegramAPI,
		sessionTTL: ttl,
		modelChain: cfg.ModelChain,
		sessions:   make(map[int64]*session),
		deletable:  make(map[int64][]int64),
		// Slightly longer than the poll timeout so the request itself does not
		// time out mid-long-poll.
		http: &http.Client{Timeout: 70 * time.Second},
	}, nil
}

type update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID int64 `json:"message_id"`
		From      *struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		} `json:"from"`
		Chat *struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
		Date int64  `json:"date"`
	} `json:"message"`
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
}

// Run polls for updates until ctx is cancelled.
// botCommands is the menu Telegram shows when a user types "/". It is static —
// the commands are compiled in — so it only changes across deploys.
var botCommands = []botCommand{
	{Command: "new", Description: "Clear the chat and start fresh"},
	{Command: "model", Description: "Show the running model and fallbacks"},
}

// maxTrackedMessages bounds the per-chat list of message IDs /new may delete,
// so a long-running chat that never clears cannot grow it without limit. The
// oldest are dropped first — and Telegram refuses to delete anything older than
// 48h anyway, so the tail is the only useful part.
const maxTrackedMessages = 500

type botCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

func (t *Telegram) Run(ctx context.Context) error {
	t.log.Info("telegram gateway started", "allowed_users", len(t.allowed))

	// Register the command menu so the commands are discoverable, but only if
	// it differs from what the bot already advertises — setMyCommands replaces
	// the whole menu, so re-sending an identical set on every restart is a
	// wasted call. Non-fatal: a bot that can't set its menu still works.
	if err := t.syncCommands(ctx); err != nil {
		t.log.Warn("could not sync command menu", "error", err)
	}

	backoff := time.Second
	for {
		if ctx.Err() != nil {
			t.log.Info("telegram gateway stopped")
			return ctx.Err()
		}

		updates, err := t.poll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Network blips are expected on a box that runs for months.
			// Back off, but never give up — silence is the failure mode.
			t.log.Warn("poll failed", "error", err, "retry_in", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			if backoff < time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second

		for _, u := range updates {
			t.handle(ctx, u)
		}
	}
}

func (t *Telegram) poll(ctx context.Context) ([]update, error) {
	t.mu.Lock()
	offset := t.offset
	t.mu.Unlock()

	params := url.Values{
		"timeout": {"60"},
		"offset":  {fmt.Sprint(offset)},
		// Only messages matter; ignore edits, callbacks, channel posts.
		"allowed_updates": {`["message"]`},
	}

	raw, err := t.call(ctx, "getUpdates", params)
	if err != nil {
		return nil, err
	}
	var updates []update
	if err := json.Unmarshal(raw, &updates); err != nil {
		return nil, fmt.Errorf("decode updates: %w", err)
	}

	// Advance the offset even for updates we drop, or a message from a
	// stranger would be redelivered forever.
	for _, u := range updates {
		t.mu.Lock()
		if u.UpdateID >= t.offset {
			t.offset = u.UpdateID + 1
		}
		t.mu.Unlock()
	}
	return updates, nil
}

func (t *Telegram) handle(ctx context.Context, u update) {
	msg := u.Message
	if msg == nil || msg.From == nil || msg.Chat == nil {
		return
	}
	if strings.TrimSpace(msg.Text) == "" {
		return // non-text: photos, stickers, voice
	}

	// The allowlist is enforced here, before the agent or the database is
	// touched. An unknown sender gets no reply at all — a bot that answers
	// strangers confirms it exists.
	if !t.allowed[msg.From.ID] {
		t.log.Warn("ignoring message from unauthorized user",
			"user_id", msg.From.ID, "username", msg.From.Username)
		return
	}

	// Track the incoming message so /new can delete it along with the rest.
	t.track(msg.Chat.ID, msg.MessageID)

	go t.respond(ctx, msg.Chat.ID, msg.Text)
}

func (t *Telegram) respond(ctx context.Context, chatID int64, text string) {
	sess := t.session(chatID)

	// One turn at a time per chat. Rapid follow-ups queue rather than
	// interleaving into a corrupted history.
	sess.busy.Lock()
	defer sess.busy.Unlock()

	// Long turns should still show that the bot is working.
	t.sendChatAction(ctx, chatID, "typing")

	if cmd := strings.TrimSpace(text); strings.HasPrefix(cmd, "/") {
		switch strings.Fields(cmd)[0] {
		// /new is the single "clear the chat" command; /start is what Telegram
		// sends when a chat is first opened. Both delete the tracked messages
		// and reset the conversation.
		case "/start", "/new":
			t.clearChat(ctx, chatID)
			t.send(ctx, chatID, "Cleared.")
			return
		case "/model":
			t.send(ctx, chatID, t.modelReport())
			return
		}
		// Any other slash command falls through to the agent as ordinary input.
	}

	// Work on a copy: the turn runs outside the session lock because it can
	// take tens of seconds, and holding a lock that long would block the TTL
	// check for every other chat.
	history := append(sess.snapshot(), model.Message{Role: model.RoleUser, Content: text})

	reply, updated, err := t.agent.Run(ctx, history)
	if err != nil {
		t.log.Error("agent run failed", "chat_id", chatID, "error", err)
		// Report the blocker rather than going quiet. A guardrail stop or a
		// dead provider must be visible to the user, not swallowed.
		if reply == "" {
			reply = "Something went wrong: " + err.Error()
		}
		// The failed turn is simply never committed, so a broken exchange is
		// not replayed on every subsequent message.
		t.send(ctx, chatID, reply)
		return
	}

	sess.commit(updated, time.Now())
	t.send(ctx, chatID, reply)
}

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

// send delivers a reply, splitting it if it exceeds Telegram's limit.
func (t *Telegram) send(ctx context.Context, chatID int64, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	for _, chunk := range splitMessage(text, maxMessageRunes) {
		if err := t.sendChunk(ctx, chatID, chunk); err != nil {
			t.log.Error("send failed", "chat_id", chatID, "error", err)
			return
		}
	}
}

// sendChunk delivers one chunk via Bot API 10.1 sendRichMessage, which renders
// the model's raw Markdown natively — bold, lists, code, and the tables that
// MarkdownV2 cannot express. richMarkdown only fixes soft breaks and table
// alignment; nothing is escaped. If the endpoint is unavailable (an older
// server), it latches off and every send after uses plain text.
func (t *Telegram) sendChunk(ctx context.Context, chatID int64, chunk string) error {
	if !t.richDisabled.Load() {
		payload, _ := json.Marshal(map[string]string{"markdown": richMarkdown(chunk)})
		res, err := t.call(ctx, "sendRichMessage", url.Values{
			"chat_id":      {fmt.Sprint(chatID)},
			"rich_message": {string(payload)},
		})
		if err == nil {
			t.trackSent(chatID, res)
			return nil
		}
		if isRichUnavailable(err) {
			t.richDisabled.Store(true)
		}
		t.log.Warn("sendRichMessage failed, sending plain text", "chat_id", chatID, "error", err)
	}

	res, err := t.call(ctx, "sendMessage", url.Values{
		"chat_id": {fmt.Sprint(chatID)},
		"text":    {chunk},
	})
	if err == nil {
		t.trackSent(chatID, res)
	}
	return err
}

// isRichUnavailable reports whether the error means the sendRichMessage
// endpoint does not exist (an older Bot API), as opposed to a per-message or
// transient failure — only then is it worth latching rich off for good.
func isRichUnavailable(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not found") ||
		strings.Contains(s, "no such method") ||
		strings.Contains(s, "unknown method")
}

func (t *Telegram) sendChatAction(ctx context.Context, chatID int64, action string) {
	params := url.Values{"chat_id": {fmt.Sprint(chatID)}, "action": {action}}
	if _, err := t.call(ctx, "sendChatAction", params); err != nil {
		t.log.Debug("chat action failed", "error", err)
	}
}

// Notify pushes an unsolicited message — how scheduled jobs reach the user.
func (t *Telegram) Notify(ctx context.Context, chatID int64, text string) error {
	if !t.allowed[chatID] {
		// chat_id and user_id match for direct messages; a mismatch means a
		// misconfigured job target.
		return fmt.Errorf("chat %d is not in the allowlist", chatID)
	}
	t.send(ctx, chatID, text)
	return nil
}

func (t *Telegram) call(ctx context.Context, method string, params url.Values) (json.RawMessage, error) {
	endpoint := fmt.Sprintf("%s/bot%s/%s", t.baseURL, t.token, method)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(params.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}

	var parsed apiResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", method, err)
	}
	if !parsed.OK {
		// The token appears in the URL, so never echo the endpoint in an
		// error — these get logged.
		return nil, fmt.Errorf("telegram %s failed: %s", method, parsed.Description)
	}
	return parsed.Result, nil
}

// splitMessage breaks text into chunks under limit runes, preferring paragraph
// then line boundaries so a split never lands mid-sentence.
func splitMessage(text string, limit int) []string {
	if len([]rune(text)) <= limit {
		return []string{text}
	}

	var chunks []string
	remaining := text

	for len([]rune(remaining)) > limit {
		runes := []rune(remaining)
		window := string(runes[:limit])

		cut := strings.LastIndex(window, "\n\n")
		if cut < limit/2 {
			cut = strings.LastIndex(window, "\n")
		}
		if cut < limit/2 {
			cut = strings.LastIndex(window, " ")
		}
		if cut <= 0 {
			cut = len(window) // no boundary; hard split
		}

		chunks = append(chunks, strings.TrimSpace(remaining[:cut]))
		remaining = strings.TrimSpace(remaining[cut:])
	}
	if remaining != "" {
		chunks = append(chunks, remaining)
	}
	return chunks
}
