package htmltext

import (
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// emptyLinks is a 2 MiB page of links whose target is the page's own address, so each 10-byte
// anchor reduces to the whole base address.
var emptyLinks = strings.Repeat(`<a href=?>`, (2<<20)/len(`<a href=?>`))

func TestReduceStopsAtTheTextLimit(t *testing.T) {
	base, err := url.Parse("https://example.com/" + strings.Repeat("a", 2000))
	if err != nil {
		t.Fatal(err)
	}
	_, text, err := Reduce(strings.NewReader(emptyLinks), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(text) > maximumTextBytes {
		t.Fatalf("the reduced text is %d bytes, over the %d-byte limit", len(text), maximumTextBytes)
	}
	if !strings.HasPrefix(text, base.String()+"?") {
		t.Fatalf("the reduced text does not start with the resolved link: %.100q", text)
	}

	document, err := html.Parse(strings.NewReader(emptyLinks))
	if err != nil {
		t.Fatal(err)
	}
	linksToFill := maximumTextBytes / len(base.String())
	allocations := testing.AllocsPerRun(1, func() {
		(&reducer{base: base}).walk(document, false)
	})
	if ceiling := float64(20 * linksToFill); allocations > ceiling {
		t.Fatalf("the walk made %.0f allocations, more than the %.0f that resolving the links up to the limit needs", allocations, ceiling)
	}
}

func TestReduceLeavesOutOverLongLinks(t *testing.T) {
	base, err := url.Parse("https://example.com/" + strings.Repeat("a", maximumLinkBytes))
	if err != nil {
		t.Fatal(err)
	}
	_, text, err := Reduce(strings.NewReader(`<p><a href=?>Read more</a> and <a href="/short">a short one</a></p>`), base)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Read more and a short one (https://example.com/short)"; text != want {
		t.Fatalf("text = %.200q, want %q", text, want)
	}
}

func TestReduceMailStopsAtTheTextLimit(t *testing.T) {
	text, err := ReduceMail(strings.NewReader("<p>" + strings.Repeat("word ", 1<<20) + "</p><p>after the limit</p>"))
	if err != nil {
		t.Fatal(err)
	}
	if len(text) > maximumTextBytes || !strings.HasPrefix(text, "word word") || strings.Contains(text, "after the limit") {
		t.Fatalf("the reduced mail is %d bytes, ending %q", len(text), text[max(len(text)-40, 0):])
	}
}
