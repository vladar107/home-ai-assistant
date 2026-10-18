package runtime

import (
	"fmt"
	"slices"
	"strings"
)

// Efforts lists the reasoning levels Pi accepts, from the least to the most.
var Efforts = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// DefaultEffort is the level a model setting without one runs on.
const DefaultEffort = "medium"

// ValidEffort reports whether the name is a level Pi accepts.
func ValidEffort(effort string) bool {
	return slices.Contains(Efforts, effort)
}

// ModelEffort is a model id with the reasoning effort it runs on.
type ModelEffort struct {
	Model  string
	Effort string
}

// ParseModelEffort reads "<model>" or "<model>:<effort>"; a missing effort is DefaultEffort. Like
// Pi, it takes the text after the last colon as the effort only when that is a known level, so a
// model id may contain colons.
func ParseModelEffort(value string) (ModelEffort, error) {
	model, effort := value, DefaultEffort
	if index := strings.LastIndex(value, ":"); index >= 0 && ValidEffort(value[index+1:]) {
		model, effort = value[:index], value[index+1:]
	}
	if model == "" {
		return ModelEffort{}, fmt.Errorf("model is required")
	}
	return ModelEffort{Model: model, Effort: effort}, nil
}

func (m ModelEffort) String() string {
	return m.Model + ":" + m.Effort
}
