package nas

import (
	"strings"
	"testing"
)

func TestParseLocationRefusesAnythingButAPlainRelativePath(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"", "a path is required"},
		{"/home/Documents", "not with /"},
		{`home\Documents`, "separate folders with /"},
		{"home/../../etc/passwd", "cannot contain . or .."},
		{"home/./Documents", "cannot contain . or .."},
		{"home//Documents", "empty folder name"},
		{"home/doc:ument", `cannot contain < > : " | ? or *`},
		{"home/Documents/report.", "cannot end with a space or a dot"},
		{"home/Documents /notes.md", "cannot end with a space or a dot"},
		{"home/doc\x00ument", "control characters"},
		{"home/" + strings.Repeat("a", 256), "too long"},
		{strings.Repeat("a/", 600) + "b", "too long"},
	} {
		if _, err := parseLocation(tc.path); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("parseLocation(%q) = %v, want a refusal naming %q", tc.path, err, tc.want)
		}
	}
}

func TestParseLocationResolvesRootAndPath(t *testing.T) {
	for _, tc := range []struct{ raw, root, path string }{
		{"home", "home", ""},
		{"home/", "home", ""},
		{"home/Documents", "home", "Documents"},
		{" home/Documents/notes.md ", "home", "Documents/notes.md"},
		{"shared-inbox/2026/photos", "shared-inbox", "2026/photos"},
	} {
		location, err := parseLocation(tc.raw)
		if err != nil {
			t.Fatalf("parseLocation(%q) = %v", tc.raw, err)
		}
		if location.Root != tc.root || location.Path != tc.path {
			t.Fatalf("parseLocation(%q) = %+v, want root %q path %q", tc.raw, location, tc.root, tc.path)
		}
	}
}

// parseRelative must apply the same rules, because a save's path is model input too; only the
// root is out of the model's reach.
func TestParseRelativeKeepsTheConfiguredRoot(t *testing.T) {
	location, err := parseRelative("inbox", "receipts/2026/scan.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if location.Root != "inbox" || location.Path != "receipts/2026/scan.pdf" {
		t.Fatalf("parseRelative = %+v", location)
	}
	if _, err := parseRelative("inbox", "../home/notes.md"); err == nil {
		t.Fatal("parseRelative accepted a traversal")
	}
}

func TestAncestorsParentAndName(t *testing.T) {
	location := Location{Root: "inbox", Path: "receipts/2026/scan.pdf"}
	ancestors := location.Ancestors()
	if len(ancestors) != 2 || ancestors[0].String() != "inbox/receipts" || ancestors[1].String() != "inbox/receipts/2026" {
		t.Fatalf("ancestors = %+v", ancestors)
	}
	if parent := location.Parent(); parent.String() != "inbox/receipts/2026" {
		t.Fatalf("parent = %s", parent)
	}
	if name := location.Name(); name != "scan.pdf" {
		t.Fatalf("name = %s", name)
	}
	top := Location{Root: "inbox"}
	if top.Ancestors() != nil || top.Parent().String() != "inbox" || top.Name() != "inbox" {
		t.Fatalf("root = %+v", top)
	}
}

// safeName has to accept whatever name the sender's device chose, without letting it become a
// path or an unusable segment.
func TestSafeNameProducesOneUsableSegment(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"scan.pdf", "scan.pdf"},
		{"../../etc/passwd", "..-..-etc-passwd"},
		{`report:final?.pdf`, "report-final-.pdf"},
		{"trailing dot.", "trailing dot"},
		{"   ", "file"},
		{"..", "file"},
		{strings.Repeat("a", 300) + ".pdf", strings.Repeat("a", 255)},
	} {
		if got := safeName(tc.raw); got != tc.want {
			t.Fatalf("safeName(%q) = %q, want %q", tc.raw, got, tc.want)
		}
		if err := validateSegment(safeName(tc.raw)); err != nil {
			t.Fatalf("safeName(%q) is not a valid segment: %v", tc.raw, err)
		}
	}
}
