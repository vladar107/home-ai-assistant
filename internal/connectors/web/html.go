package web

import (
	"io"
	"net/url"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// skippedElements hold no text a reader of the page would see, or only site furniture and
// controls: menus, forms, embedded frames, and code.
var skippedElements = map[atom.Atom]bool{
	atom.Head: true, atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Template: true,
	atom.Svg: true, atom.Math: true, atom.Canvas: true, atom.Nav: true, atom.Form: true,
	atom.Button: true, atom.Select: true, atom.Textarea: true, atom.Input: true, atom.Iframe: true,
	atom.Object: true, atom.Embed: true, atom.Audio: true, atom.Video: true, atom.Dialog: true,
}

// paragraphElements are separated from their neighbours by a blank line, lineElements by a line
// break.
var paragraphElements = map[atom.Atom]bool{
	atom.P: true, atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true,
	atom.H6: true, atom.Blockquote: true, atom.Pre: true, atom.Table: true, atom.Ul: true,
	atom.Ol: true, atom.Dl: true, atom.Figure: true, atom.Section: true, atom.Article: true,
	atom.Header: true, atom.Footer: true, atom.Main: true, atom.Aside: true, atom.Details: true,
}

var lineElements = map[atom.Atom]bool{
	atom.Div: true, atom.Li: true, atom.Tr: true, atom.Br: true, atom.Hr: true, atom.Dt: true,
	atom.Dd: true, atom.Figcaption: true, atom.Address: true, atom.Summary: true, atom.Caption: true,
	atom.Tbody: true, atom.Thead: true, atom.Tfoot: true,
}

// reduceHTML returns a page's title and its visible text, with a link written as its text and
// its absolute address when the two differ.
func reduceHTML(body io.Reader, base *url.URL) (string, string, error) {
	document, err := html.Parse(body)
	if err != nil {
		return "", "", err
	}
	reducer := &reducer{base: base}
	reducer.walk(document, false)
	return findTitle(document), tidy(reducer.text.String()), nil
}

// findTitle returns the document's own title, not one inside an embedded SVG drawing.
func findTitle(node *html.Node) string {
	if node.Type == html.ElementNode && node.DataAtom == atom.Title && node.Namespace == "" {
		return strings.TrimSpace(collapse(textOf(node)))
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if title := findTitle(child); title != "" {
			return title
		}
	}
	return ""
}

type reducer struct {
	base *url.URL
	text strings.Builder
	// breaks is the number of line breaks owed before the next text, so that nested and adjacent
	// block elements produce one separation between them rather than one each.
	breaks int
}

func (r *reducer) write(text string) {
	if strings.TrimSpace(text) == "" && r.breaks > 0 {
		return
	}
	if r.breaks > 0 {
		r.text.WriteString(strings.Repeat("\n", r.breaks))
		r.breaks = 0
	}
	r.text.WriteString(text)
}

func (r *reducer) lineBreak(count int) {
	r.breaks = max(r.breaks, count)
}

func (r *reducer) walk(node *html.Node, preformatted bool) {
	switch node.Type {
	case html.TextNode:
		if preformatted {
			r.write(node.Data)
		} else {
			r.write(collapse(node.Data))
		}
		return
	case html.ElementNode:
		if skippedElements[node.DataAtom] || hidden(node) {
			return
		}
		switch {
		case paragraphElements[node.DataAtom]:
			r.lineBreak(2)
			defer r.lineBreak(2)
		case lineElements[node.DataAtom]:
			r.lineBreak(1)
			defer r.lineBreak(1)
		case node.DataAtom == atom.Td || node.DataAtom == atom.Th:
			r.write(" ")
			defer r.write(" ")
		}
		switch node.DataAtom {
		case atom.Li:
			r.write("- ")
		case atom.Pre:
			preformatted = true
		case atom.A:
			r.link(node, preformatted)
			return
		case atom.Img:
			if alt := strings.TrimSpace(collapse(attribute(node, "alt"))); alt != "" {
				r.write(" [image: " + alt + "] ")
			}
			return
		}
	case html.CommentNode, html.DoctypeNode:
		return
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		r.walk(child, preformatted)
	}
}

func (r *reducer) link(node *html.Node, preformatted bool) {
	start := r.text.Len()
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		r.walk(child, preformatted)
	}
	text := strings.TrimSpace(r.text.String()[start:])
	target := r.resolve(attribute(node, "href"))
	if target == "" || target == text || strings.TrimSuffix(target, "/") == strings.TrimSuffix(text, "/") {
		return
	}
	if text == "" {
		r.write(" " + target + " ")
		return
	}
	r.write(" (" + target + ")")
}

// resolve returns a link's absolute http or https address, or nothing for anchors on the same
// page, script links, mail links, and anything else the model could not fetch.
func (r *reducer) resolve(href string) string {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") {
		return ""
	}
	parsed, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if r.base != nil {
		parsed = r.base.ResolveReference(parsed)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return ""
	}
	parsed.User = nil
	return parsed.String()
}

func hidden(node *html.Node) bool {
	for _, attr := range node.Attr {
		switch {
		case attr.Key == "hidden":
			return true
		case attr.Key == "aria-hidden" && strings.EqualFold(strings.TrimSpace(attr.Val), "true"):
			return true
		}
	}
	return false
}

func attribute(node *html.Node, key string) string {
	for _, attr := range node.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func textOf(node *html.Node) string {
	var builder strings.Builder
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.TextNode {
			builder.WriteString(child.Data)
		}
	}
	return builder.String()
}

// collapse turns every run of whitespace into one space, keeping a leading or trailing space so
// that words in neighbouring inline elements stay apart.
func collapse(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		if text == "" {
			return ""
		}
		return " "
	}
	collapsed := strings.Join(fields, " ")
	if first := text[0]; first == ' ' || first == '\t' || first == '\n' || first == '\r' || first == '\f' {
		collapsed = " " + collapsed
	}
	if last := text[len(text)-1]; last == ' ' || last == '\t' || last == '\n' || last == '\r' || last == '\f' {
		collapsed += " "
	}
	return collapsed
}

// tidy trims every line, collapses the spaces inside it, and keeps at most one blank line in a
// row.
func tidy(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	kept := make([]string, 0, len(lines))
	blank := true
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			if !blank {
				kept = append(kept, "")
			}
			blank = true
			continue
		}
		kept = append(kept, line)
		blank = false
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}
