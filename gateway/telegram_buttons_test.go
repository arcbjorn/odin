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

// The gateway sends rich Markdown, so a model writes "**1.**" as readily as
// "1.". Every emphasis style must still produce a keyboard — otherwise the
// feature fails silently on exactly the messages it exists for, and the text
// still looks correct so nobody notices.
func TestVerdictButtonsToleratesMarkdownEmphasis(t *testing.T) {
	for name, text := range map[string]string{
		"bold dot":   "**1.** Ando, MAAT\n**2.** Fabrica coffee",
		"bold paren": "**1)** Ando, MAAT\n**2)** Fabrica coffee",
		"italic":     "*1* Ando, MAAT\n*2* Fabrica coffee",
		"underscore": "_1_ Ando, MAAT\n_2_ Fabrica coffee",
		"bold body":  "1. **Ando** — closes Aug 20\n2. **Fabrica** — new roaster",
	} {
		if verdictButtons(text) == nil {
			t.Errorf("expected a keyboard for %s numbering", name)
		}
	}
}

// Prose that merely contains numbers must never sprout a keyboard.
func TestVerdictButtonsIgnoresIncidentalNumbers(t *testing.T) {
	for name, text := range map[string]string{
		"years":  "2026 was busy.\n2027 looks busier.",
		"prices": "12 euros for the ticket.\n30 euros for dinner.",
		"times":  "09:00 swim\n18:00 cardio",
	} {
		if verdictButtons(text) != nil {
			t.Errorf("unexpected keyboard for %s", name)
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

// splitBrief is a numbered brief long enough to split, with the list in the
// first chunk and the closing question in the second. It reproduces the case
// verdictButtons is deliberately derived from the whole reply for: the
// keyboard's items are not present in the chunk that carries the keyboard.
func splitBrief(t *testing.T) string {
	t.Helper()
	brief := strings.Join([]string{
		"1. Ando retrospective, MAAT — closes Aug 20",
		"2. Fabrica coffee, Príncipe Real — new roaster",
		"3. Sintra ride, N247 — coast road",
		strings.Repeat("context ", 700),
		"",
		"reply with numbers — worth it?",
	}, "\n")
	if got := len(splitMessage(brief, maxMessageRunes)); got != 2 {
		t.Fatalf("fixture split into %d chunks, want exactly 2", got)
	}
	return brief
}

// markupCount reports how many sends carried a keyboard.
func markupCount(markups []string) int {
	n := 0
	for _, m := range markups {
		if m != "" {
			n++
		}
	}
	return n
}

// The keyboard belongs on the last chunk only. Attached to the first, it would
// sit stranded mid-brief above text the user has not read yet; attached to
// every chunk, the same verdict could be recorded twice.
func TestVerdictButtonsRideOnTheFinalChunkOnly(t *testing.T) {
	g, fake := newGatewayWithOutbox(t, "")

	if _, err := g.send(context.Background(), 1, splitBrief(t)); err != nil {
		t.Fatalf("send: %v", err)
	}

	markups := fake.sentMarkups()
	if len(markups) != 2 {
		t.Fatalf("sent %d chunks, want 2", len(markups))
	}
	if markups[0] != "" {
		t.Errorf("first chunk carried a keyboard: %s", markups[0])
	}
	if markups[1] == "" {
		t.Fatal("final chunk carried no keyboard")
	}

	// Derived from the whole reply: the items are in chunk one, but the
	// keyboard on chunk two must still offer all three.
	var kb struct {
		InlineKeyboard [][]struct {
			Text string `json:"text"`
		} `json:"inline_keyboard"`
	}
	if err := json.Unmarshal([]byte(markups[1]), &kb); err != nil {
		t.Fatalf("decode markup: %v", err)
	}
	if len(kb.InlineKeyboard) == 0 || len(kb.InlineKeyboard[0]) != 3 {
		t.Fatalf("keyboard did not reflect the whole brief: %s", markups[1])
	}
}

// A brief that is not a numbered list must send exactly as it did before the
// keyboard existed.
func TestPlainReplyCarriesNoKeyboard(t *testing.T) {
	g, fake := newGatewayWithOutbox(t, "")

	if _, err := g.send(context.Background(), 1, "just a sentence, nothing to rank"); err != nil {
		t.Fatalf("send: %v", err)
	}

	if n := markupCount(fake.sentMarkups()); n != 0 {
		t.Fatalf("%d sends carried a keyboard, want 0", n)
	}
}

// The interaction the two rebased lines create: a multi-chunk brief whose final
// chunk fails is requeued, and the retry must still arrive with its keyboard.
// Losing it would leave a brief the user can only answer by typing — the exact
// friction the buttons exist to remove — and the failure would be silent,
// because the text still looks correct.
func TestRequeuedFinalChunkKeepsItsKeyboard(t *testing.T) {
	g, fake := newGatewayWithOutbox(t, "")
	brief := splitBrief(t)

	// The first chunk lands, then Telegram goes down on the one holding the
	// keyboard.
	fake.setFailSends(true, 1)
	if err := g.Notify(context.Background(), 1, brief); err == nil {
		t.Fatal("partial delivery must still report failure")
	}

	fake.setFailSends(false, 0)
	g.outbox.flush(context.Background(), g.sendWithButtons, time.Now())

	markups := fake.sentMarkups()
	if n := markupCount(markups); n != 1 {
		t.Fatalf("%d delivered sends carried a keyboard, want exactly 1: %v", n, markups)
	}
	if last := markups[len(markups)-1]; last == "" {
		t.Fatal("the retried final chunk arrived without its keyboard")
	}
}

// A requeued tail inherits the keyboard the whole brief earned, even when the
// tail itself is pure prose.
//
// The numbered list is not in the retried text, but it did reach the chat in
// the chunk that landed — the user can scroll up and read items 1 and 2. So the
// keyboard still refers to something visible, and attaching it is what lets the
// verdict be a tap. Re-deriving from the fragment instead would drop it.
func TestRequeuedTailInheritsTheBriefsKeyboard(t *testing.T) {
	g, fake := newGatewayWithOutbox(t, "")

	// Numbered list in the first chunk, prose in the second.
	brief := strings.Join([]string{
		"1. first item",
		"2. second item",
		strings.Repeat("prose ", 700),
	}, "\n")
	if got := len(splitMessage(brief, maxMessageRunes)); got != 2 {
		t.Fatalf("fixture split into %d chunks, want exactly 2", got)
	}
	// The tail alone yields no keyboard — the point of the fix.
	tail := splitMessage(brief, maxMessageRunes)[1]
	if verdictButtons(tail) != nil {
		t.Fatal("fixture tail must not derive a keyboard on its own")
	}

	fake.setFailSends(true, 1)
	if err := g.Notify(context.Background(), 1, brief); err == nil {
		t.Fatal("partial delivery must still report failure")
	}

	fake.setFailSends(false, 0)
	g.outbox.flush(context.Background(), g.sendWithButtons, time.Now())

	markups := fake.sentMarkups()
	if n := markupCount(markups); n != 1 {
		t.Fatalf("%d sends carried a keyboard, want exactly 1: %v", n, markups)
	}
	if last := markups[len(markups)-1]; last == "" {
		t.Fatal("the retried tail lost the keyboard the brief earned")
	}
}

// A message that never had a keyboard must not acquire one on retry.
func TestRequeuedPlainMessageStaysPlain(t *testing.T) {
	g, fake := newGatewayWithOutbox(t, "")

	long := strings.Repeat("alpha ", 500) + "\n\n" + strings.Repeat("omega ", 500)
	if got := len(splitMessage(long, maxMessageRunes)); got != 2 {
		t.Fatalf("fixture split into %d chunks, want exactly 2", got)
	}

	fake.setFailSends(true, 1)
	if err := g.Notify(context.Background(), 1, long); err == nil {
		t.Fatal("partial delivery must still report failure")
	}

	fake.setFailSends(false, 0)
	g.outbox.flush(context.Background(), g.sendWithButtons, time.Now())

	if n := markupCount(fake.sentMarkups()); n != 0 {
		t.Fatalf("%d sends carried a keyboard, want 0 — this brief never had one", n)
	}
}
