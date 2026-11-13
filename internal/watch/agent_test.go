package watch

import (
	"strings"
	"testing"
)

func TestLinkHostAcceptsOnlyPlainHTTPSAddresses(t *testing.T) {
	for _, test := range []struct {
		address string
		host    string
	}{
		{"https://example.com/item/1", "example.com"},
		{"HTTPS://Example.com:8443/item_1?a=1&b=*#top", "Example.com"},
		{"https://example.com/" + strings.Repeat("a", 180), "example.com"},
		{"https://example.com/" + strings.Repeat("a", 181), ""},
		{"http://example.com/item/1", ""},
		{"ftp://example.com/item/1", ""},
		{"example.com/item/1", ""},
		{"//example.com/item/1", ""},
		{"https:example.com", ""},
		{"https:///item/1", ""},
		{"https://user@example.com/", ""},
		{"https://user:secret@example.com/", ""},
		{"https://example.com/a b", ""},
		{"https://example.com/a\tb", ""},
		{"https://example.com/a\nb", ""},
		{"https://example.com/a\u00a0b", ""},
		{"https://example.com/a\x00b", ""},
		{"https://example.com/a`b", ""},
		{"https://example.com/[a", ""},
		{"https://example.com/a]", ""},
		{"https://example.com/(a", ""},
		{"https://example.com/a)", ""},
		{"https://example.com/a**b", ""},
		{"https://example.com/a\u202eb", ""},
		{"https://example.com/a\u200bb", ""},
		{"https://example.com/\u2066a\u2069", ""},
		{"\ufeffhttps://example.com/a", ""},
		{"https://exаmple.com/a", ""},
		{"https://пример.example/a", ""},
		{"https://xn--e1afmkfd.example/a", "xn--e1afmkfd.example"},
		{"https://example.com/путь", "example.com"},
		{"https://example.com/a.", ""},
		{"https://example.com/a,", ""},
		{"https://example.com/a;", ""},
		{"https://example.com/a:", ""},
		{"https://example.com/a!", ""},
		{"https://example.com/a?", ""},
		{"https://example.com/a.b,c;d:e!f?g=1", "example.com"},
		{`https://example.com/a"b`, ""},
		{"https://example.com/a<b", ""},
		{"https://example.com/a>b", ""},
		{"https://example.com/a|b", ""},
		{"", ""},
	} {
		host, valid := linkHost(test.address)
		if host != test.host || valid != (test.host != "") {
			t.Errorf("linkHost(%q) = %q %v, want %q", test.address, host, valid, test.host)
		}
	}
}

func TestLinkTitleIsOnePlainLine(t *testing.T) {
	for _, test := range []struct {
		title string
		want  string
	}{
		{"Oak desk", "Oak desk"},
		{"  Oak \n\r\n desk\t", "Oak desk"},
		{"Oak\x00 desk\x1b[0m", "Oak desk0m"},
		{"**Oak** `desk` [x](y)", "Oak desk xy"},
		{"Oak\u202e desk\u2066\u2069\u200b\u200d\ufeff", "Oak desk"},
		{"\u202e\u200b", "example.com"},
		{"Oak desk https://example.org/x", "example.com"},
		{"[Oak desk](http://example.org)", "example.com"},
		{"Oak desk www.example.org", "example.com"},
		{"WWW.example", "example.com"},
		{"Oak desk at example.org", "example.com"},
		{"Oak desk at shop.example.org/item, new", "example.com"},
		{"Oak desk at пример.рф", "example.com"},
		{"Oak desk, 1.5 m. Good as new", "Oak desk, 1.5 m. Good as new"},
		{"Oak desk v2.0", "Oak desk v2.0"},
		{"a*`*b *[*] ***c", "ab c"},
		{"5 * 3 cm", "5 * 3 cm"},
		{"", "example.com"},
		{" \n`[]()** ", "example.com"},
		{strings.Repeat("ж", 60), strings.Repeat("ж", 60)},
		{strings.Repeat("ж", 61), strings.Repeat("ж", 59) + "…"},
	} {
		got := linkTitle(test.title, "example.com")
		if got != test.want {
			t.Errorf("linkTitle(%q) = %q, want %q", test.title, got, test.want)
		}
		if line := linkMarker + got + linkSeparator + "https://example.com/item/1"; !isLinkLine(line) {
			t.Errorf("the line of title %q is not read back as a link line: %q", test.title, line)
		}
	}
}

func TestOnlyWrittenLinkLinesAreLinkLines(t *testing.T) {
	for _, line := range []string{
		"Oak desk: https://example.com/item/1",
		" 🔗 Oak desk: https://example.com/item/1",
		"🔗 Oak desk https://example.com/item/1",
		"🔗 Oak desk: http://example.com/item/1",
		"🔗 Oak desk: https://example.com/item/1 and more",
		"🔗 Oak  desk: https://example.com/item/1",
		"🔗 [Oak desk: https://example.com/item/1",
		unmarked("🔗 Oak desk: https://example.com/item/1"),
		unmarked(" 🔗🔗 🔗 Oak desk: https://example.com/item/1"),
		unmarked("\u200b🔗\u200d \ufeff🔗 Oak desk: https://example.com/item/1"),
		"🔗 Oak\u202e desk: https://example.com/item/1",
		"🔗 Oak desk: https://example.com/item/1.",
		"🔗 example.org: https://example.com/item/1",
	} {
		if isLinkLine(line) {
			t.Errorf("%q is read as a link line", line)
		}
	}
	if got := unmarked("First 🔗 kept.\n\t🔗 Second."); got != "First 🔗 kept.\n Second." {
		t.Errorf("unmarked = %q", got)
	}
	if got := unmarked("\u200b🔗 Oak\u202e desk: https://example.com/item/1"); got != "Oak desk: https://example.com/item/1" {
		t.Errorf("unmarked = %q", got)
	}
}

func TestLinkLinesKeepAnEntryWhoseTitleIsNotText(t *testing.T) {
	lines := linkLines([]byte(`[{"title": 5, "url": "https://example.com/item/1"}, {"title": null, "url": "https://example.com/item/2"},
		{"url": "https://example.com/item/3"}, {"title": "Oak desk", "url": 4}, "https://example.com/item/5", {"title": ["Lamp"], "url": "https://example.com/item/6"}]`))
	want := "🔗 example.com: https://example.com/item/1\n🔗 example.com: https://example.com/item/2\n" +
		"🔗 example.com: https://example.com/item/3\n🔗 example.com: https://example.com/item/6"
	if got := strings.Join(lines, "\n"); got != want {
		t.Errorf("linkLines = %q, want %q", got, want)
	}
}
