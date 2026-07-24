package gateway

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Inline keyboards for numbered briefs.
//
// The scout's briefs end by asking for a verdict on a numbered list. Typing
// "1 4" is already short, but on a phone a tap is shorter, and the verdict loop
// is the only thing that teaches the agent what he actually likes — so its
// friction is worth removing.
//
// The gateway derives the buttons from the text the model already wrote rather
// than adding a tool for it. The model's job stays "write a good brief"; it
// cannot forget to attach a keyboard, and no schema grows. If a message is not
// a numbered brief, nothing is attached and the send path is unchanged.

// maxButtons caps the keyboard. Briefs are capped at five items by their job
// prompts; the extra headroom absorbs a model that numbers one line too many
// without turning a runaway list into a wall of buttons.
const maxButtons = 8

// callbackPrefix namespaces our callback data, so a future button type cannot
// be confused for a verdict.
const callbackPrefix = "v:"

// numberedItem matches a line that opens with "1." / "2)" / "3 -" — the shapes
// a model actually produces for a numbered list. Anchored to the line start so
// a date or price mid-sentence is never mistaken for an item number.
var numberedItem = regexp.MustCompile(`(?m)^\s*(\d{1,2})\s*[.)\-]\s+\S`)

// verdictButtons builds a keyboard for a numbered brief, or nil when the text
// is not one. Nil means "send exactly as before".
func verdictButtons(text string) []byte {
	nums := itemNumbers(text)
	if len(nums) < 2 {
		// A single numbered line is a sentence, not a menu.
		return nil
	}

	// Two rows: the item numbers, then a way to say "none of these". Without
	// the second row the only cheap answer is a positive one, which would bias
	// taste_signals toward approval — the loop needs a one-tap "no" too.
	row := make([]map[string]string, 0, len(nums))
	for _, n := range nums {
		row = append(row, map[string]string{
			"text":          fmt.Sprint(n),
			"callback_data": callbackPrefix + strconv.Itoa(n),
		})
	}

	keyboard := [][]map[string]string{
		row,
		{{"text": "none", "callback_data": callbackPrefix + "none"}},
	}

	payload, err := json.Marshal(map[string]any{"inline_keyboard": keyboard})
	if err != nil {
		return nil
	}
	return payload
}

// itemNumbers extracts the leading numbers of a numbered list.
//
// Requires the list to start at 1 and run consecutively. A model that writes
// "1." then "3." has produced something we do not understand well enough to
// turn into buttons, and a keyboard whose numbers disagree with the visible
// text is worse than no keyboard: the tap would record a verdict against the
// wrong item, and verdicts are permanent.
func itemNumbers(text string) []int {
	matches := numberedItem.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}

	out := make([]int, 0, len(matches))
	for i, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil || n != i+1 {
			return nil
		}
		out = append(out, n)
		if len(out) > maxButtons {
			return nil
		}
	}
	return out
}

// verdictReply turns a callback payload into the chat message the agent sees.
//
// The tap is rewritten into the same words he would have typed, so the agent
// needs no callback-specific handling and the scout skill's numbered-reply
// procedure resolves it unchanged. One path through the model, not two.
func verdictReply(data string) (string, bool) {
	if !strings.HasPrefix(data, callbackPrefix) {
		return "", false
	}
	arg := strings.TrimPrefix(data, callbackPrefix)
	if arg == "none" {
		return "none of these", true
	}
	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 || n > maxButtons {
		return "", false
	}
	return fmt.Sprint(n), true
}
