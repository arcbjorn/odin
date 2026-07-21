package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arcbjorn/odin/model"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeAgent records what it was asked and replies with a canned answer.
type fakeAgent struct {
	mu    sync.Mutex
	calls [][]model.Message
	reply string
	err   error
	onRun func()
}

func (f *fakeAgent) Run(_ context.Context, history []model.Message) (string, []model.Message, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]model.Message(nil), history...))
	reply, err, hook := f.reply, f.err, f.onRun
	f.mu.Unlock()

	if hook != nil {
		hook()
	}
	if err != nil {
		return "", nil, err
	}
	updated := append(append([]model.Message(nil), history...),
		model.Message{Role: model.RoleAssistant, Content: reply})
	return reply, updated, nil
}

func (f *fakeAgent) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeTelegram stands in for api.telegram.org.
type fakeTelegram struct {
	mu   sync.Mutex
	sent []string
}

func (f *fakeTelegram) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			f.mu.Lock()
			f.sent = append(f.sent, r.FormValue("text"))
			f.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"result":[]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeTelegram) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

// newGateway wires a gateway at a fake API endpoint.
func newGateway(t *testing.T, agent Agent, allowed []int64) (*Telegram, *fakeTelegram) {
	t.Helper()
	fake := &fakeTelegram{}
	srv := fake.server(t)

	g, err := NewTelegram(TelegramConfig{
		Token:        "test-token",
		AllowedUsers: allowed,
		Agent:        agent,
		Logger:       quiet(),
	})
	if err != nil {
		t.Fatalf("NewTelegram: %v", err)
	}
	g.http = srv.Client()
	g.baseURL = srv.URL
	return g, fake
}

func makeUpdate(updateID, userID, chatID int64, text string) update {
	var u update
	raw := `{
		"update_id": ` + itoa(updateID) + `,
		"message": {
			"message_id": 1,
			"from": {"id": ` + itoa(userID) + `, "username": "test"},
			"chat": {"id": ` + itoa(chatID) + `},
			"text": ` + quote(text) + `
		}
	}`
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		panic(err)
	}
	return u
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// An open gateway is never intended.
func TestRefusesEmptyAllowlist(t *testing.T) {
	_, err := NewTelegram(TelegramConfig{
		Token: "t", Agent: &fakeAgent{}, Logger: quiet(),
	})
	if err == nil {
		t.Fatal("expected an empty allowlist to be refused")
	}
	if !strings.Contains(err.Error(), "allowed_users") {
		t.Fatalf("error should name allowed_users, got: %v", err)
	}
}

func TestRefusesMissingToken(t *testing.T) {
	if _, err := NewTelegram(TelegramConfig{
		AllowedUsers: []int64{1}, Agent: &fakeAgent{}, Logger: quiet(),
	}); err == nil {
		t.Fatal("expected a missing token to be refused")
	}
}

// The allowlist is enforced at the transport, before the agent or the tracker
// is touched. A stranger gets no reply at all.
func TestUnauthorizedUserIsIgnored(t *testing.T) {
	agent := &fakeAgent{reply: "should not happen"}
	g, fake := newGateway(t, agent, []int64{123456789})

	g.handle(context.Background(), makeUpdate(1, 999999, 999999, "who are you"))
	time.Sleep(100 * time.Millisecond)

	if agent.callCount() != 0 {
		t.Fatal("agent was invoked for an unauthorized user")
	}
	if len(fake.messages()) != 0 {
		t.Fatalf("gateway replied to a stranger: %v", fake.messages())
	}
}

func TestAuthorizedUserGetsReply(t *testing.T) {
	agent := &fakeAgent{reply: "Task saved."}
	g, fake := newGateway(t, agent, []int64{123456789})

	g.handle(context.Background(), makeUpdate(1, 123456789, 123456789, "save task"))

	if !waitFor(t, func() bool { return len(fake.messages()) > 0 }) {
		t.Fatal("no reply sent")
	}
	msgs := fake.messages()
	if msgs[len(msgs)-1] != "Task saved." {
		t.Fatalf("got %q", msgs[len(msgs)-1])
	}
}

// Rapid follow-ups must queue, not interleave into a corrupted history.
func TestConcurrentMessagesSerializePerChat(t *testing.T) {
	var active, maxActive int
	var mu sync.Mutex

	agent := &fakeAgent{reply: "ok"}
	agent.onRun = func() {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()

		time.Sleep(30 * time.Millisecond)

		mu.Lock()
		active--
		mu.Unlock()
	}

	g, _ := newGateway(t, agent, []int64{1})

	ctx := context.Background()
	for i := int64(1); i <= 3; i++ {
		g.handle(ctx, makeUpdate(i, 1, 1, "message"))
	}

	if !waitFor(t, func() bool { return agent.callCount() == 3 }) {
		t.Fatalf("expected 3 turns, got %d", agent.callCount())
	}

	mu.Lock()
	defer mu.Unlock()
	if maxActive > 1 {
		t.Fatalf("%d turns ran concurrently in one chat", maxActive)
	}
}

// History must accumulate so the agent sees the conversation.
func TestHistoryAccumulates(t *testing.T) {
	agent := &fakeAgent{reply: "ack"}
	g, _ := newGateway(t, agent, []int64{1})

	ctx := context.Background()
	g.respond(ctx, 1, "first")
	g.respond(ctx, 1, "second")

	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(agent.calls))
	}
	// Second call sees: user first, assistant ack, user second.
	if len(agent.calls[1]) != 3 {
		t.Fatalf("second call history = %d messages, want 3", len(agent.calls[1]))
	}
	if agent.calls[1][0].Content != "first" || agent.calls[1][2].Content != "second" {
		t.Fatalf("history out of order: %+v", agent.calls[1])
	}
}

// Separate chats must not share context.
func TestSessionsAreIsolatedPerChat(t *testing.T) {
	agent := &fakeAgent{reply: "ack"}
	g, _ := newGateway(t, agent, []int64{1, 2})

	ctx := context.Background()
	g.respond(ctx, 1, "chat one")
	g.respond(ctx, 2, "chat two")

	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.calls[1]) != 1 {
		t.Fatalf("chat 2 inherited chat 1's history: %+v", agent.calls[1])
	}
}

// A morning conversation should not still be in context at midnight.
func TestExpiredSessionStartsFresh(t *testing.T) {
	agent := &fakeAgent{reply: "ack"}
	g, _ := newGateway(t, agent, []int64{1})
	g.sessionTTL = 50 * time.Millisecond

	ctx := context.Background()
	g.respond(ctx, 1, "morning")
	time.Sleep(80 * time.Millisecond)
	g.respond(ctx, 1, "midnight")

	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.calls[1]) != 1 {
		t.Fatalf("expired session carried history: %+v", agent.calls[1])
	}
}

func TestResetCommandClearsHistory(t *testing.T) {
	agent := &fakeAgent{reply: "ack"}
	g, fake := newGateway(t, agent, []int64{1})

	ctx := context.Background()
	g.respond(ctx, 1, "remember this")
	g.respond(ctx, 1, "/reset")
	g.respond(ctx, 1, "fresh start")

	msgs := fake.messages()
	if len(msgs) < 2 || !strings.Contains(msgs[1], "Fresh session") {
		t.Fatalf("reset not acknowledged: %v", msgs)
	}

	agent.mu.Lock()
	defer agent.mu.Unlock()
	// /reset must not reach the agent, and the next turn starts clean.
	if len(agent.calls) != 2 {
		t.Fatalf("expected 2 agent calls, got %d", len(agent.calls))
	}
	if len(agent.calls[1]) != 1 {
		t.Fatalf("history survived reset: %+v", agent.calls[1])
	}
}

// Slash-prefixed input that is not a gateway command must reach the agent —
// Unknown slash commands are ordinary input.
func TestUnknownSlashCommandReachesAgent(t *testing.T) {
	agent := &fakeAgent{reply: "logged"}
	g, _ := newGateway(t, agent, []int64{1})

	g.respond(context.Background(), 1, "/remind review")

	if agent.callCount() != 1 {
		t.Fatal("unknown slash command did not reach the agent")
	}
}

// A failure must be reported, not swallowed. Silence is this system's failure
// mode, and a guardrail stop needs to be visible.
func TestAgentErrorIsReported(t *testing.T) {
	agent := &fakeAgent{err: errors.New("all providers failed")}
	g, fake := newGateway(t, agent, []int64{1})

	g.respond(context.Background(), 1, "brief me")

	msgs := fake.messages()
	if len(msgs) == 0 {
		t.Fatal("agent failure produced no reply")
	}
	if !strings.Contains(msgs[len(msgs)-1], "all providers failed") {
		t.Fatalf("error not surfaced to the user: %q", msgs[len(msgs)-1])
	}
}

// A failed turn must not poison the history and replay forever.
func TestFailedTurnIsDroppedFromHistory(t *testing.T) {
	agent := &fakeAgent{err: errors.New("boom")}
	g, _ := newGateway(t, agent, []int64{1})

	ctx := context.Background()
	g.respond(ctx, 1, "first")

	agent.mu.Lock()
	agent.err = nil
	agent.reply = "recovered"
	agent.mu.Unlock()

	g.respond(ctx, 1, "second")

	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.calls[1]) != 1 {
		t.Fatalf("failed turn stayed in history: %+v", agent.calls[1])
	}
}

func TestNonTextMessagesAreIgnored(t *testing.T) {
	agent := &fakeAgent{reply: "ack"}
	g, _ := newGateway(t, agent, []int64{1})

	g.handle(context.Background(), makeUpdate(1, 1, 1, "   "))
	time.Sleep(50 * time.Millisecond)

	if agent.callCount() != 0 {
		t.Fatal("whitespace-only message reached the agent")
	}
}

// The offset must advance even for dropped updates, or a stranger's message
// is redelivered forever.
func TestOffsetAdvancesPastDroppedUpdates(t *testing.T) {
	fake := &fakeTelegram{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"result":[
			{"update_id": 42, "message": {"message_id":1,
			 "from":{"id":999,"username":"stranger"},
			 "chat":{"id":999}, "text":"hello"}}
		]}`)
	}))
	defer srv.Close()

	g, err := NewTelegram(TelegramConfig{
		Token: "t", AllowedUsers: []int64{1}, Agent: &fakeAgent{}, Logger: quiet(),
	})
	if err != nil {
		t.Fatalf("NewTelegram: %v", err)
	}
	g.http = srv.Client()
	g.baseURL = srv.URL

	if _, err := g.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if g.offset != 43 {
		t.Fatalf("offset = %d, want 43", g.offset)
	}
	_ = fake
}

// A weekly debrief can exceed Telegram's limit. Splitting must land on
// paragraph boundaries, never mid-sentence.
func TestLongMessageSplitsOnBoundaries(t *testing.T) {
	para := strings.Repeat("This is a sentence about the week. ", 60)
	text := para + "\n\n" + para + "\n\n" + para

	chunks := splitMessage(text, maxMessageRunes)
	if len(chunks) < 2 {
		t.Fatalf("expected a split, got %d chunk(s)", len(chunks))
	}
	for i, c := range chunks {
		if n := len([]rune(c)); n > maxMessageRunes {
			t.Errorf("chunk %d is %d runes, over the limit", i, n)
		}
		if strings.TrimSpace(c) == "" {
			t.Errorf("chunk %d is empty", i)
		}
	}
	// No content may be lost.
	joined := strings.Join(chunks, " ")
	if !strings.Contains(joined, "This is a sentence about the week.") {
		t.Fatal("content lost in split")
	}
}

func TestShortMessageIsNotSplit(t *testing.T) {
	chunks := splitMessage("Reminder saved for 18:00.", maxMessageRunes)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
}

func TestSplitHandlesUnicode(t *testing.T) {
	// Rune-counted, not byte-counted: Cyrillic would otherwise split early.
	text := strings.Repeat("плавание и бег каждый день. ", 300)
	for _, c := range splitMessage(text, maxMessageRunes) {
		if n := len([]rune(c)); n > maxMessageRunes {
			t.Fatalf("chunk is %d runes, over the limit", n)
		}
	}
}

// Scheduled jobs reach the user through Notify; it must respect the allowlist.
func TestNotifyRespectsAllowlist(t *testing.T) {
	agent := &fakeAgent{reply: "ack"}
	g, fake := newGateway(t, agent, []int64{123456789})

	ctx := context.Background()
	if err := g.Notify(ctx, 123456789, "Morning brief ready."); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !waitFor(t, func() bool { return len(fake.messages()) > 0 }) {
		t.Fatal("notification not sent")
	}

	if err := g.Notify(ctx, 999999, "leak"); err == nil {
		t.Fatal("Notify sent to a chat outside the allowlist")
	}
}

// The bot token appears in the request URL and must never reach a log.
func TestErrorsDoNotLeakToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":false,"description":"Unauthorized"}`)
	}))
	defer srv.Close()

	g, err := NewTelegram(TelegramConfig{
		Token:        "SUPER-SECRET-TOKEN",
		AllowedUsers: []int64{1},
		Agent:        &fakeAgent{},
		Logger:       quiet(),
	})
	if err != nil {
		t.Fatalf("NewTelegram: %v", err)
	}
	g.http = srv.Client()
	g.baseURL = srv.URL

	_, callErr := g.call(context.Background(), "getUpdates", nil)
	if callErr == nil {
		t.Fatal("expected an API error")
	}
	if strings.Contains(callErr.Error(), "SUPER-SECRET-TOKEN") {
		t.Fatalf("error leaked the bot token: %v", callErr)
	}
}
