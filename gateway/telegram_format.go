package gateway

import (
	"regexp"
	"strings"
)

// Telegram Bot API 10.1 sendRichMessage renders raw Markdown natively — bold,
// italics, lists, code, and tables (which MarkdownV2 cannot express at all).
// So the gateway sends the model's Markdown as-is; the only preprocessing is
// three fixes for how the native renderer treats plain Markdown:
//
//  1. A single newline is a soft break, so consecutive prose lines collapse
//     into one paragraph. Turn single newlines into hard breaks, leaving
//     fenced code and tables alone — their newlines are structural.
//  2. A bare "---" table column renders its header centered but its cells
//     left. Make every delimiter cell's alignment explicit so header and body
//     agree.
//  3. A table must START a block. GFM tables cannot interrupt a paragraph, so
//     a table written directly under a label line is absorbed into it and
//     rendered as one line of literal pipes — verified against the live API,
//     which returns the whole table as a single paragraph. Surround every
//     table with blank lines.

// richProtectedRegion matches a fenced code block or a GFM table: regions whose
// internal newlines are structural and must not receive hard breaks.
var richProtectedRegion = regexp.MustCompile(
	"(?s:```[^\n]*\n.*?```)" +
		"|(?m:^[^\n]*\\|[^\n]*\n[ \t]*\\|?[ \t]*:?-+:?[ \t]*(?:\\|[ \t]*:?-+:?[ \t]*)+\\|?[ \t]*(?:\n[^\n]*\\|[^\n]*)*)",
)

// tableSeparatorRE matches a GFM delimiter row (|---|:--:|...), requiring at
// least one internal pipe so a lone `---` rule is not treated as a table.
var tableSeparatorRE = regexp.MustCompile(`^\s*\|?\s*:?-+:?\s*(?:\|\s*:?-+:?\s*){1,}\|?\s*$`)

// richMarkdown prepares the model's raw Markdown for sendRichMessage.
func richMarkdown(text string) string {
	if text == "" {
		return text
	}
	var b strings.Builder
	last := 0
	afterTable := false
	// prose is emitted with the blank lines a neighbouring table requires.
	prose := func(s string, beforeTable bool) {
		if afterTable {
			s = withLeadingBlankLine(s)
		}
		if beforeTable {
			s = withTrailingBlankLine(s)
		}
		b.WriteString(s)
	}
	for _, loc := range richProtectedRegion.FindAllStringIndex(text, -1) {
		region := text[loc[0]:loc[1]]
		isTable := isTableRegion(region)
		prose(hardBreaks(text[last:loc[0]]), isTable)
		b.WriteString(explicitAlignment(region))
		afterTable = isTable
		last = loc[1]
	}
	prose(hardBreaks(text[last:]), false)
	return b.String()
}

// isTableRegion distinguishes the two kinds of protected region. Only a fenced
// code block starts with a fence; anything else the pattern matched is a table.
func isTableRegion(region string) bool {
	return !strings.HasPrefix(strings.TrimLeft(region, " \t\n"), "```")
}

// withTrailingBlankLine ensures prose ending immediately before a table is
// separated from it by a blank line.
//
// This is the difference between a table and a wall of pipes. A GFM table
// cannot interrupt a paragraph: a label written directly above one ("Factors"
// then the header row) makes the entire table a continuation of that
// paragraph, and the renderer emits it as a single line of literal pipes.
// hardBreaks alone cannot save it — a hard break is still inside the paragraph.
func withTrailingBlankLine(s string) string {
	trimmed := strings.TrimRight(s, " \t\n")
	if trimmed == "" {
		// Nothing but whitespace before the table (it opens the message, or
		// already sits after a blank line): leave it as it is.
		return s
	}
	return trimmed + "\n\n"
}

// withLeadingBlankLine ensures prose resuming after a table is not swallowed
// into the table block, which is the same failure in the other direction.
func withLeadingBlankLine(s string) string {
	trimmed := strings.TrimLeft(s, " \t\n")
	if trimmed == "" {
		return s
	}
	return "\n\n" + trimmed
}

// hardBreaks turns a single newline between two non-blank lines into a Markdown
// hard break ("  \n"). Blank-line paragraph breaks are left as-is.
func hardBreaks(s string) string {
	lines := strings.Split(s, "\n")
	var b strings.Builder
	for i, line := range lines {
		b.WriteString(line)
		if i == len(lines)-1 {
			break
		}
		if strings.TrimSpace(line) == "" || strings.TrimSpace(lines[i+1]) == "" {
			b.WriteByte('\n') // adjacent to a blank line: real paragraph break
		} else {
			b.WriteString("  \n") // soft break the renderer would collapse
		}
	}
	return b.String()
}

// explicitAlignment rewrites a table's delimiter row so every column carries an
// explicit alignment marker; the native renderer then applies it uniformly to
// the header and the body. A region with no delimiter row (a code block) is
// returned untouched.
func explicitAlignment(region string) string {
	lines := strings.Split(region, "\n")
	for i, line := range lines {
		if tableSeparatorRE.MatchString(line) {
			lines[i] = markDelimiter(line)
			break
		}
	}
	return strings.Join(lines, "\n")
}

func markDelimiter(delimiter string) string {
	trimmed := strings.TrimSpace(delimiter)
	lead := strings.HasPrefix(trimmed, "|")
	trail := strings.HasSuffix(trimmed, "|")
	cells := strings.Split(strings.Trim(trimmed, "|"), "|")
	for i, c := range cells {
		c = strings.TrimSpace(c)
		left, right := strings.HasPrefix(c, ":"), strings.HasSuffix(c, ":")
		dashes := strings.Trim(c, ":")
		if dashes == "" {
			dashes = "---"
		}
		switch {
		case left && right:
			cells[i] = ":" + dashes + ":" // center, already explicit
		case right:
			cells[i] = dashes + ":" // right
		default:
			cells[i] = ":" + dashes // bare or left → explicit left
		}
	}
	out := strings.Join(cells, "|")
	if lead {
		out = "|" + out
	}
	if trail {
		out += "|"
	}
	return out
}
