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
		{"plain text unchanged", "Notion writes are on.", "Notion writes are on."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderMarkup(tc.text); got != tc.want {
				t.Fatalf("renderMarkup(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}
