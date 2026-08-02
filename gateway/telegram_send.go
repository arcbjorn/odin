package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// send delivers a reply, splitting it if it exceeds Telegram's limit.
//
// It returns the text that never landed alongside the failure. A caller that
// must not lose the message can then queue exactly the remainder instead of
// repeating chunks the user already received.
func (t *Telegram) send(ctx context.Context, chatID int64, text string) (undelivered string, err error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", nil
	}
	chunks := splitMessage(text, maxMessageRunes)
	for i, chunk := range chunks {
		if err := t.sendChunk(ctx, chatID, chunk); err != nil {
			t.log.Error("send failed", "chat_id", chatID, "error", err)
			return strings.Join(chunks[i:], "\n\n"), err
		}
	}
	return "", nil
}

// reply sends an answer to an interactive turn. The failure is already logged
// inside send and there is nothing further to do with it: the user is present
// and will ask again, so queueing a chat reply for later would deliver an
// answer to a question they have moved on from.
func (t *Telegram) reply(ctx context.Context, chatID int64, text string) {
	_, _ = t.send(ctx, chatID, text)
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
//
// A failure is both queued and returned. Queued, because nobody is waiting on
// a 07:00 brief to notice it never arrived and ask again — the tokens are
// already spent, so the content is worth a retry. Returned, because the run
// still failed: swallowing it would record the job as successful and leave the
// watchdog with nothing to alert on, which is the exact silence it exists for.
func (t *Telegram) Notify(ctx context.Context, chatID int64, text string) error {
	if !t.allowed[chatID] {
		// chat_id and user_id match for direct messages; a mismatch means a
		// misconfigured job target. Never queue it: no retry fixes a message
		// addressed to someone who is not allowed to receive it.
		return fmt.Errorf("chat %d is not in the allowlist", chatID)
	}
	undelivered, err := t.send(ctx, chatID, text)
	if err == nil {
		return nil
	}
	if undelivered != "" {
		t.outbox.queue(chatID, undelivered, time.Now())
	}
	return fmt.Errorf("deliver to chat %d (queued for retry): %w", chatID, err)
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
