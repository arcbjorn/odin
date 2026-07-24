package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A numbered brief gets one button per item plus an explicit "none".
func TestVerdictButtonsForNumberedBrief(t *testing.T) {
	brief := strings.Join([]string{
		"Closes Aug 20 — Tadao Ando retrospective, MAAT. Only European stop.",
		"",
		"1. Ando retrospective, MAAT — closes Aug 20",
		"2. Fabrica coffee, Príncipe Real — new roaster",
		"3. Sintra ride, N247 — coast road",
		"",
		`reply with numbers — worth it? (e.g. "1 4", or "skip 2")`,
	}, "\n")

	raw := verdictButtons(brief)
	if raw == nil {
		t.Fatal("expected a keyboard for a numbered brief")
	}

	var parsed struct {
		InlineKeyboard [][]struct {
			Text string `json:"text"`
			Data string `json:"callback_data"`
		} `json:"inline_keyboard"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("keyboard is not valid json: %v", err)
	}

	if len(parsed.InlineKeyboard) != 2 {
		t.Fatalf("expected an item row and a none row, got %d rows", len(parsed.InlineKeyboard))
	}
	if got := len(parsed.InlineKeyboard[0]); got != 3 {
		t.Fatalf("expected 3 item buttons, got %d", got)
	}
	if parsed.InlineKeyboard[0][0].Data != "v:1" {
		t.Errorf("first button data = %q, want v:1", parsed.InlineKeyboard[0][0].Data)
	}
	// Without a one-tap negative, the cheapest answer is always a positive
	// one, which would bias the learned taste toward approval.
	if parsed.InlineKeyboard[1][0].Data != "v:none" {
		t.Errorf("expected a none button, got %q", parsed.InlineKeyboard[1][0].Data)
	}
}

// Ordinary prose must not sprout buttons.
func TestVerdictButtonsIgnoresProse(t *testing.T) {
	for _, text := range []string{
		"Nothing worth reporting this week.",
		"Booked for 2 people at 19.30 on the 4th.",
		"1. Only one item, which is a sentence and not a menu.",
		"",
	} {
		if raw := verdictButtons(text); raw != nil {
			t.Errorf("unexpected keyboard for %q", text)
		}
	}
}

// The critical safety property: if the visible numbering is not a clean
// 1..N run, no keyboard is attached. A button whose number disagrees with the
// text would record a permanent verdict against the wrong opportunity.
func TestVerdictButtonsRefusesInconsistentNumbering(t *testing.T) {
	skipped := "1. First\n3. Third\n"
	if raw := verdictButtons(skipped); raw != nil {
		t.Error("expected no keyboard when numbering skips a value")
	}

	notStartingAtOne := "2. Second\n3. Third\n"
	if raw := verdictButtons(notStartingAtOne); raw != nil {
		t.Error("expected no keyboard when numbering does not start at 1")
	}
}

// A runaway list is dropped rather than rendered as a wall of buttons.
func TestVerdictButtonsCapsRunawayLists(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= maxButtons+2; i++ {
		b.WriteString(itoa(int64(i)) + ". item\n")
	}
	if raw := verdictButtons(b.String()); raw != nil {
		t.Errorf("expected no keyboard beyond %d items", maxButtons)
	}
}

// A tap is rewritten into the words he would have typed, so the agent needs no
// callback-aware code path.
func TestVerdictReplyRewritesTapAsText(t *testing.T) {
	cases := map[string]string{
		"v:1":    "1",
		"v:5":    "5",
		"v:none": "none of these",
	}
	for data, want := range cases {
		got, ok := verdictReply(data)
		if !ok || got != want {
			t.Errorf("verdictReply(%q) = %q, %v; want %q", data, got, ok, want)
		}
	}
}

// Anything not ours, or out of range, is rejected rather than guessed at.
func TestVerdictReplyRejectsForeignPayloads(t *testing.T) {
	for _, data := range []string{"", "1", "other:1", "v:", "v:abc", "v:0", "v:99"} {
		if _, ok := verdictReply(data); ok {
			t.Errorf("expected %q to be rejected", data)
		}
	}
}

// makeCallback builds a button-tap update.
func makeCallback(updateID, userID, chatID int64, data string) update {
	var u update
	raw := `{
		"update_id": ` + itoa(updateID) + `,
		"callback_query": {
			"id": "cb1",
			"from": {"id": ` + itoa(userID) + `, "username": "test"},
			"message": {"message_id": 7, "chat": {"id": ` + itoa(chatID) + `}},
			"data": ` + quote(data) + `
		}
	}`
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		panic(err)
	}
	return u
}

// A tap reaches the agent as the text he would have typed, so the scout's
// numbered-reply procedure resolves it with no callback-specific code.
func TestCallbackReachesAgentAsText(t *testing.T) {
	done := make(chan struct{})
	agent := &fakeAgent{reply: "noted", onRun: func() { close(done) }}
	g, _ := newGateway(t, agent, []int64{42})

	g.handle(context.Background(), makeCallback(1, 42, 99, "v:2"))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("agent was never called for a button tap")
	}

	agent.mu.Lock()
	defer agent.mu.Unlock()
	last := agent.calls[0][len(agent.calls[0])-1]
	if last.Content != "2" {
		t.Fatalf("agent saw %q, want %q", last.Content, "2")
	}
}

// The allowlist covers taps, not just typed messages: a button in a forwarded
// message must not become an open door.
func TestCallbackFromStrangerIsIgnored(t *testing.T) {
	agent := &fakeAgent{reply: "should not happen"}
	g, _ := newGateway(t, agent, []int64{42})

	g.handle(context.Background(), makeCallback(1, 1337, 99, "v:1"))

	time.Sleep(300 * time.Millisecond)
	if n := agent.callCount(); n != 0 {
		t.Fatalf("an unauthorized tap reached the agent (%d calls)", n)
	}
}
