package telegram

import "testing"

func TestRenderMarkupRendersASafeSubset(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want string
	}{
		{"escapes markup characters", "5 < 6 & 7 > 6 <b>not a tag</b>", "5 &lt; 6 &amp; 7 &gt; 6 &lt;b&gt;not a tag&lt;/b&gt;"},
		{"bold", "**Confirmed** already", "<b>Confirmed</b> already"},
		{"bold escapes its content", "**a < b**", "<b>a &lt; b</b>"},
		{"inline code", "call `notion_update_page` twice", "call <code>notion_update_page</code> twice"},
		{"inline code stays literal", "`**a** <b> [x](https://x)`", "<code>**a** &lt;b&gt; [x](https://x)</code>"},
		{"fenced code", "```json\n{\"a\": 1 < 2}\n```", "<pre><code>{\"a\": 1 &lt; 2}</code></pre>"},
		{"fenced code without a language", "```\nline one\nline two\n```", "<pre><code>line one\nline two</code></pre>"},
		{"link", "see [the page](https://notion.so/p?a=1&b=2)", `see <a href="https://notion.so/p?a=1&amp;b=2">the page</a>`},
		{"link target escaped for an attribute", `[q](https://x/?a="1")`, `<a href="https://x/?a=&quot;1&quot;">q</a>`},
		{"link label rendered", "[**bold**](https://x/y)", `<a href="https://x/y"><b>bold</b></a>`},
		{"non-HTTP link", "[click](javascript:alert(1))", "[click](javascript:alert(1))"},
		{"link target with a space", "[click](https://x y)", "[click](https://x y)"},
		{"identifiers untouched", "notion_update_page needs data_source_id and 2 * 3", "notion_update_page needs data_source_id and 2 * 3"},
		{"unbalanced bold", "**almost bold and *lonely*", "**almost bold and *lonely*"},
		{"unbalanced inline code", "an `unclosed span", "an `unclosed span"},
		{"unbalanced fence", "```\nunclosed < block", "```\nunclosed &lt; block"},
		{"unbalanced link", "[label](https://x", "[label](https://x"},
		{"empty markers", "**** `` []()", "**** `` []()"},
		{"single-line quote", "> 🚆 **On time**", "<blockquote expandable>🚆 <b>On time</b></blockquote>"},
		{"multi-line quote", "Trains:\n> line one\n>\n> line two\nDone.", "Trains:\n<blockquote expandable>line one\n\nline two</blockquote>\nDone."},
		{"separate quotes", "> one\n\n> two", "<blockquote expandable>one</blockquote>\n\n<blockquote expandable>two</blockquote>"},
		{"markup inside a quote", "> **a < b** see [x](https://x/y) and `c > d`", `<blockquote expandable><b>a &lt; b</b> see <a href="https://x/y">x</a> and <code>c &gt; d</code></blockquote>`},
		{"quote inside fenced code", "```\n> not a quote\n```\n> a quote", "<pre><code>&gt; not a quote</code></pre>\n<blockquote expandable>a quote</blockquote>"},
		{"quote at the start", "> first\nthen text", "<blockquote expandable>first</blockquote>\nthen text"},
		{"quote at the end", "text first\n> last\n", "text first\n<blockquote expandable>last</blockquote>\n"},
		{"quote with CRLF", "intro\r\n> one\r\n> two\r\noutro", "intro\n<blockquote expandable>one\ntwo</blockquote>\noutro"},
		{"greater-than mid-line", "a > b\nc >d\n>e", "a &gt; b\nc &gt;d\n&gt;e"},
		{"empty quote", "> \ntext", "&gt; \ntext"},
		{"bold across lines", "**a\nb**", "<b>a\nb</b>"},
		{"plain text unchanged", "Notion writes are on.", "Notion writes are on."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderMarkup(tc.text); got != tc.want {
				t.Fatalf("renderMarkup(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}
