// Package prompt holds the model-facing text that belongs to no single connector: the system
// prompt's fixed sections, the [system] lines a turn's message starts with, the framing of turns
// nobody is watching, and what the broker and approvals answer a tool call with. It imports no
// domain package; callers pass in what the text needs.
package prompt

// Marker starts every line the application adds to a turn's message, so history can tell those
// lines from what the user wrote.
const Marker = "[system]"

// NothingToAdd is the reply a continuation turn gives when the request is finished, or a turn whose
// reaction is the whole answer; it is never sent.
const NothingToAdd = "[nothing-to-add]"

// Prepend starts the message with a line and a blank line; an empty line leaves it as it is.
func Prepend(line, message string) string {
	if line == "" {
		return message
	}
	return line + "\n\n" + message
}
