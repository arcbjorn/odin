package gateway

import (
	"regexp"
	"strconv"
	"strings"
)

// This file converts the CommonMark-ish prose the model writes into Telegram
// MarkdownV2, so bold, italics, inline code, fenced code, links and headers
// render instead of showing raw asterisks. The old send path shipped plain
// text precisely because a naive parse_mode rejects a message outright on one
// unbalanced character; the fix is not to avoid formatting but to escape every
// literal special character and keep a plain-text fallback for the rare case
// Telegram still refuses. See send() in telegram.go.
//
// Modeled on the Hermes adapter's format_message/_strip_mdv2 pair, adapted to
// Go's RE2 (no lookaround, no backreferences).

// mdv2Special is every character Telegram MarkdownV2 requires to be
// backslash-escaped when it appears as literal text.
// https://core.telegram.org/bots/api#markdownv2-style
const mdv2Special = "_*[]()~`>#+-=|{}.!\\"

// escapeMarkdownV2 backslash-escapes every literal MarkdownV2 special rune.
func escapeMarkdownV2(s string) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/8 + 8)
	for _, r := range s {
		if strings.ContainsRune(mdv2Special, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

var (
	reFence   = regexp.MustCompile("(?s)```.*?```")
	reInline  = regexp.MustCompile("`[^`\n]+`")
	reLink    = regexp.MustCompile(`\[([^\]]+)\]\(([^()\s]+)\)`)
	reHeader  = regexp.MustCompile(`(?m)^[ \t]*#{1,6}[ \t]+(.+?)[ \t]*#*$`)
	reBoldAst = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reBoldUnd = regexp.MustCompile(`__(.+?)__`)
	reStrike  = regexp.MustCompile(`~~(.+?)~~`)
	reItalic  = regexp.MustCompile(`\*([^*\n]+?)\*`)
)

// formatMarkdownV2 translates common Markdown to Telegram MarkdownV2.
//
// Entities (code, links, and each converted emphasis run) are stashed behind
// NUL-delimited placeholders before the blanket escape, so their internal
// markup is never double-escaped, then restored in reverse so nested entities
// (inline code inside bold, say) resolve correctly.
func formatMarkdownV2(text string) string {
	if text == "" {
		return text
	}

	// Telegram has no table syntax; a pipe table renders as escaped-pipe noise.
	// Rewrite tables to bullet groups before anything else, exactly as Hermes does.
	text = tableToBullets(text)

	var stash []string
	ph := func(v string) string {
		stash = append(stash, v)
		return "\x00" + strconv.Itoa(len(stash)-1) + "\x00"
	}

	// 1) Fenced code blocks. Only \ and ` need escaping inside a code entity.
	text = reFence.ReplaceAllStringFunc(text, func(m string) string {
		inner := m[3 : len(m)-3]
		inner = strings.ReplaceAll(inner, "\\", "\\\\")
		inner = strings.ReplaceAll(inner, "`", "\\`")
		return ph("```" + inner + "```")
	})

	// 2) Inline code.
	text = reInline.ReplaceAllStringFunc(text, func(m string) string {
		inner := m[1 : len(m)-1]
		inner = strings.ReplaceAll(inner, "\\", "\\\\")
		return ph("`" + inner + "`")
	})

	// 3) Links [display](url): escape the display text; inside the URL only
	//    ) and \ are special.
	text = reLink.ReplaceAllStringFunc(text, func(m string) string {
		g := reLink.FindStringSubmatch(m)
		disp := escapeMarkdownV2(g[1])
		u := strings.ReplaceAll(g[2], "\\", "\\\\")
		u = strings.ReplaceAll(u, ")", "\\)")
		return ph("[" + disp + "](" + u + ")")
	})

	// 4) ATX headers (# Title) → bold. Telegram has no heading syntax.
	text = reHeader.ReplaceAllStringFunc(text, func(m string) string {
		inner := reBoldAst.ReplaceAllString(reHeader.FindStringSubmatch(m)[1], "$1")
		return ph("*" + escapeMarkdownV2(inner) + "*")
	})

	// 5) Bold: **x** and __x__ → *x* (MarkdownV2 bold). Before italics so a
	//    doubled asterisk is never mistaken for two empty italics.
	bold := func(open int) func(string) string {
		return func(m string) string { return ph("*" + escapeMarkdownV2(m[open:len(m)-open]) + "*") }
	}
	text = reBoldAst.ReplaceAllStringFunc(text, bold(2))
	text = reBoldUnd.ReplaceAllStringFunc(text, bold(2))

	// 6) Strikethrough: ~~x~~ → ~x~.
	text = reStrike.ReplaceAllStringFunc(text, func(m string) string {
		return ph("~" + escapeMarkdownV2(m[2:len(m)-2]) + "~")
	})

	// 7) Italic: *x* → _x_. Single underscores are left alone on purpose so
	//    snake_case identifiers are not italicized; they escape to literals.
	text = reItalic.ReplaceAllStringFunc(text, func(m string) string {
		return ph("_" + escapeMarkdownV2(m[1:len(m)-1]) + "_")
	})

	// 8) Escape every remaining literal special character. Placeholder tokens
	//    (NUL + digits) contain nothing special, so they pass through intact.
	text = escapeMarkdownV2(text)

	// 9) Restore, reverse order for correct nesting.
	for i := len(stash) - 1; i >= 0; i-- {
		text = strings.ReplaceAll(text, "\x00"+strconv.Itoa(i)+"\x00", stash[i])
	}
	return text
}

var (
	reUnescape    = regexp.MustCompile(`\\([_*\[\]()~` + "`" + `>#+\-=|{}.!\\])`)
	reHeaderPlain = regexp.MustCompile(`(?m)^[ \t]*#{1,6}[ \t]+(.+?)[ \t]*#*$`)
	reInlinePlain = regexp.MustCompile("`([^`\n]+)`")
	reFencePlain  = regexp.MustCompile("(?s)```[^\n]*\n?(.*?)```")
	reLinkPlain   = regexp.MustCompile(`\[([^\]]+)\]\([^()\s]+\)`)
)

// --- GFM tables → bullet groups (ported from Hermes convert_table_to_bullets) ---
//
// Telegram MarkdownV2 has no table syntax and its proportional font destroys any
// column alignment, so a pipe table is unreadable. Each data row is rewritten as
// a bold heading (the row's label/first cell) followed by "header: value"
// bullets, which reflow cleanly on a phone.

// tableSeparatorRE matches a GFM delimiter row (|---|:--:|...), requiring at
// least one internal pipe so a lone `---` rule is not treated as a table.
var tableSeparatorRE = regexp.MustCompile(`^\s*\|?\s*:?-+:?\s*(?:\|\s*:?-+:?\s*){1,}\|?\s*$`)

func isTableRow(line string) bool {
	s := strings.TrimSpace(line)
	return s != "" && strings.Contains(s, "|")
}

func splitTableRow(line string) []string {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")
	parts := strings.Split(s, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func tableToBullets(text string) string {
	if !strings.Contains(text, "|") || !strings.Contains(text, "-") {
		return text
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	inFence := false
	for i := 0; i < len(lines); {
		line := lines[i]
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			out = append(out, line)
			i++
			continue
		}
		if !inFence && strings.Contains(line, "|") &&
			i+1 < len(lines) && tableSeparatorRE.MatchString(lines[i+1]) {
			block := []string{line, lines[i+1]}
			j := i + 2
			for j < len(lines) && isTableRow(lines[j]) {
				block = append(block, lines[j])
				j++
			}
			out = append(out, renderTableBlock(block))
			i = j
			continue
		}
		out = append(out, line)
		i++
	}
	return strings.Join(out, "\n")
}

func renderTableBlock(block []string) string {
	if len(block) < 3 {
		return strings.Join(block, "\n")
	}
	headers := splitTableRow(block[0])
	if len(headers) < 2 {
		return strings.Join(block, "\n")
	}
	// A leading row-label column shows up as one more cell than there are headers.
	hasRowLabel := len(splitTableRow(block[2])) == len(headers)+1

	groups := make([]string, 0, len(block)-2)
	for idx, row := range block[2:] {
		cells := splitTableRow(row)
		var heading string
		var data []string
		if hasRowLabel {
			if len(cells) > 0 && cells[0] != "" {
				heading = cells[0]
			} else {
				heading = "Row " + strconv.Itoa(idx+1)
			}
			data = cells[1:]
		} else {
			heading = "Row " + strconv.Itoa(idx+1)
			for _, c := range cells {
				if c != "" {
					heading = c
					break
				}
			}
			data = cells
		}
		for len(data) < len(headers) {
			data = append(data, "")
		}
		data = data[:len(headers)]

		lines := []string{"**" + heading + "**"}
		for k, header := range headers {
			if !hasRowLabel && data[k] == heading {
				continue // the heading already stands in for its own column
			}
			lines = append(lines, "• "+header+": "+data[k])
		}
		groups = append(groups, strings.Join(lines, "\n"))
	}
	return strings.Join(groups, "\n\n")
}

// stripMarkdown removes Markdown markers to produce clean plain text for the
// fallback path — a message that reads naturally rather than one littered with
// asterisks and backticks. It runs on the original prose, not the MarkdownV2
// conversion, so it is a straightforward marker strip.
func stripMarkdown(text string) string {
	text = tableToBullets(text)
	text = reFencePlain.ReplaceAllString(text, "$1")
	text = reInlinePlain.ReplaceAllString(text, "$1")
	text = reLinkPlain.ReplaceAllString(text, "$1")
	text = reHeaderPlain.ReplaceAllString(text, "$1")
	text = reBoldAst.ReplaceAllString(text, "$1")
	text = reBoldUnd.ReplaceAllString(text, "$1")
	text = reStrike.ReplaceAllString(text, "$1")
	text = reItalic.ReplaceAllString(text, "$1")
	text = reUnescape.ReplaceAllString(text, "$1")
	return text
}
