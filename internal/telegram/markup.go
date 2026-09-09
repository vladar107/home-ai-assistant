package telegram

import "strings"

const (
	boldMarker  = "**"
	fenceMarker = "```"
)

var (
	textEscaper      = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	attributeEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
)

// renderMarkup converts the Markdown subset the model and the assistant write — fenced code,
// inline code, bold, and HTTP links — into the HTML subset Telegram parses. Every byte is escaped
// first, so arbitrary model, user, and integration content can never open a tag of its own, and a
// marker without its closing counterpart stays literal instead of becoming an entity Telegram
// refuses to parse. Underscores and single asterisks are deliberately not emphasis, so identifiers
// such as notion_update_page reach the chat unchanged.
func renderMarkup(text string) string {
	var rendered strings.Builder
	for index := 0; index < len(text); {
		offset := strings.IndexByte(text[index:], '`')
		if offset < 0 {
			rendered.WriteString(renderInline(text[index:]))
			break
		}
		rendered.WriteString(renderInline(text[index : index+offset]))
		index += offset
		code, width, ok := renderCode(text[index:])
		if !ok {
			// The whole run stays literal; stopping short of it would match its own backticks.
			rendered.WriteString(text[index : index+width])
			index += width
			continue
		}
		rendered.WriteString(code)
		index += width
	}
	return rendered.String()
}

// renderCode renders the code span opening at the start of text and reports its width. A run of
// backticks with no closing run of the same length is not a code span, and the width then covers
// the opening run alone.
func renderCode(text string) (string, int, bool) {
	marker := len(text) - len(strings.TrimLeft(text, "`"))
	closing := strings.Index(text[marker:], text[:marker])
	if closing <= 0 {
		return "", marker, false
	}
	content := text[marker : marker+closing]
	width := marker + closing + marker
	if marker >= len(fenceMarker) {
		return "<pre><code>" + textEscaper.Replace(fencedContent(content)) + "</code></pre>", width, true
	}
	return "<code>" + textEscaper.Replace(content) + "</code>", width, true
}

// fencedContent drops the info string a fence carries on its opening line, so a language tag is
// not shown as the first line of the code.
func fencedContent(content string) string {
	if newline := strings.IndexByte(content, '\n'); newline >= 0 && !strings.ContainsAny(content[:newline], " \t") {
		content = content[newline+1:]
	}
	return strings.TrimSuffix(content, "\n")
}

// renderInline renders bold and links in text that is known to hold no code span, escaping
// everything else.
func renderInline(text string) string {
	var rendered strings.Builder
	for index := 0; index < len(text); {
		offset := strings.IndexAny(text[index:], "*[")
		if offset < 0 {
			rendered.WriteString(textEscaper.Replace(text[index:]))
			break
		}
		rendered.WriteString(textEscaper.Replace(text[index : index+offset]))
		index += offset
		if bold, width, ok := renderBold(text[index:]); ok {
			rendered.WriteString(bold)
			index += width
			continue
		}
		if link, width, ok := renderLink(text[index:]); ok {
			rendered.WriteString(link)
			index += width
			continue
		}
		rendered.WriteString(textEscaper.Replace(text[index : index+1]))
		index++
	}
	return rendered.String()
}

func renderBold(text string) (string, int, bool) {
	if !strings.HasPrefix(text, boldMarker) {
		return "", 0, false
	}
	closing := strings.Index(text[len(boldMarker):], boldMarker)
	if closing <= 0 {
		return "", 0, false
	}
	content := text[len(boldMarker) : len(boldMarker)+closing]
	return "<b>" + renderInline(content) + "</b>", 2*len(boldMarker) + closing, true
}

// renderLink renders [text](url) as an anchor. A target that is not an HTTP or HTTPS URL is not a
// link, and the markers are then escaped as the literal characters the author wrote.
func renderLink(text string) (string, int, bool) {
	if !strings.HasPrefix(text, "[") {
		return "", 0, false
	}
	label := strings.IndexByte(text, ']')
	if label <= 1 || !strings.HasPrefix(text[label+1:], "(") {
		return "", 0, false
	}
	target := strings.IndexByte(text[label+2:], ')')
	if target < 0 {
		return "", 0, false
	}
	address := text[label+2 : label+2+target]
	if !isLinkTarget(address) {
		return "", 0, false
	}
	anchor := `<a href="` + attributeEscaper.Replace(address) + `">` + renderInline(text[1:label]) + "</a>"
	return anchor, label + 3 + target, true
}

func isLinkTarget(address string) bool {
	lowered := strings.ToLower(address)
	if !strings.HasPrefix(lowered, "http://") && !strings.HasPrefix(lowered, "https://") {
		return false
	}
	return strings.IndexFunc(address, func(r rune) bool { return r <= ' ' || r == 0x7f }) < 0
}
