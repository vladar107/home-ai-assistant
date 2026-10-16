package conversation

import (
	"strings"
	"testing"
)

func TestParseTabNameAcceptsOnlyPlainShortNames(t *testing.T) {
	invisible := func(code rune) string { return string(code) }
	clock := "⏰" + invisible(0xFE0F)
	icons := []string{"🌱", clock}
	for answer, want := range map[string]TabName{
		`{"name":"Garden","icon":"🌱"}`:                      {Name: "Garden", Icon: "🌱"},
		`{"name":"Jardin d'été","icon":""}`:                 {Name: "Jardin d'été"},
		`{"name":"Garden","icon":"🚀"}`:                      {Name: "Garden"},
		"```json\n{\"name\":\"Alarm\",\"icon\":\"⏰\"}\n```": {Name: "Alarm", Icon: clock},
		`{"name":"St. Petersburg trip","icon":""}`:          {Name: "St. Petersburg trip"},
	} {
		if got, err := parseTabName(answer, icons); err != nil || got != want {
			t.Errorf("parseTabName(%q) = %+v %v, want %+v", answer, got, err, want)
		}
	}
	for _, name := range []string{
		"",
		"   ",
		strings.Repeat("long ", 7),
		"Garden\nnotes",
		"Garden" + invisible(0x2028) + "notes",
		"Garden" + invisible(0x2029) + "notes",
		invisible(0x202E) + "neddrag",
		"Garden" + invisible(0x2066) + "notes" + invisible(0x2069),
		"Gar" + invisible(0x200B) + "den",
		"Gar" + invisible(0x200D) + "den",
		"Garden" + invisible(0x00AD) + "notes",
		"Garden" + invisible(0x00A0) + "notes",
		"Ask @helper",
		"Read example.com",
		"Notes on 1.io",
		"https://garden",
		"www.garden",
		"t.me/garden",
	} {
		answer := `{"name":"` + strings.NewReplacer("\n", `\n`, `"`, `\"`).Replace(name) + `","icon":"🌱"}`
		if got, err := parseTabName(answer, icons); err == nil {
			t.Errorf("name %q was taken as %+v", name, got)
		}
	}
	for _, answer := range []string{`Garden`, `{"name":"Garden","icon":"🌱","extra":1}`, `{"icon":"🌱"}`, `{"name":"Garden"} {}`} {
		if got, err := parseTabName(answer, icons); err == nil {
			t.Errorf("answer %q was taken as %+v", answer, got)
		}
	}
}
