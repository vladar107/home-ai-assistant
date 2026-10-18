package nas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/delivery"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	toolSendFile = "nas_send_file"

	// maximumSendBytes bounds one upload. Telegram accepts 50 MB from a bot, so this is the
	// household's limit rather than the protocol's: a file this size already takes most of a
	// turn to leave a home uplink.
	maximumSendBytes = 20 << 20
	// sendTimeout covers opening the file and uploading it, which the broker's default deadline
	// is too short for.
	sendTimeout = 60 * time.Second

	undeliverableText = "That file could not be delivered to the chat. Tell the user it did not arrive; do not describe its content instead."
	noChatText        = "This user has no chat that can receive files."
	noSenderText      = "This assistant cannot send files."
)

// Sender delivers one file to the principal's own chat. The connector never learns which chat
// that is: a file leaves here named by the principal it belongs to and nothing else, so no
// argument the model chooses can send it anywhere but to the person who asked.
type Sender interface {
	SendFile(ctx context.Context, principalID string, file delivery.File) error
}

const sendGuidance = "nas_send_file delivers a file from the NAS to this chat as a document the user can open or keep, whatever type it is. " +
	"Use it whenever the user asks for the file itself rather than for something it says. " +
	"Sending never needs a read: the user receives the file, not your account of it, so find it by name with nas_list_files or nas_search_files and send that path. " +
	"Files up to 20 MB can be sent. Send one file per request unless the user asked for several, and say what you sent."

var sendDefinitions = []tools.Definition{
	{
		Name:        toolSendFile,
		Description: "Send one file from the NAS to the user in this chat, as a document. Use this when the user asks for the file itself.",
		Risk:        tools.RiskRead,
		Activity:    tools.ActivityFiles,
		Timeout:     sendTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"path":{"type":"string","description":"The full path from a listing, such as home/Documents/report.pdf."}},` +
			`"required":["path"],"additionalProperties":false}`),
	},
}

// executeSend streams one file to the user's own chat. The bytes go from the mounted directory to
// Telegram without passing through the model, which is what keeps sending a file independent of
// reading it.
func (s *Service) executeSend(ctx context.Context, principalID string, connection Session, call tools.Call) (tools.Result, error) {
	// The tool is never contributed without a sender, so this only catches a call that reached
	// here some other way; it must not depend on the path happening to fail.
	if s.sender == nil {
		return tools.Result{Text: noSenderText, IsError: true}, nil
	}
	var arguments struct {
		Path string `json:"path"`
	}
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return s.toolFailure(ctx, principalID, call.Name, err)
	}
	location, err := parseLocation(arguments.Path)
	if err != nil {
		return s.toolFailure(ctx, principalID, call.Name, err)
	}
	if location.Path == "" {
		return s.toolFailure(ctx, principalID, call.Name, fmt.Errorf("%s is a folder, not a file; list it and send one of its files", location))
	}
	body, size, err := connection.Open(ctx, location)
	if err != nil {
		return s.toolFailure(ctx, principalID, call.Name, err)
	}
	switch {
	case size == 0:
		body.Close()
		return s.toolFailure(ctx, principalID, call.Name, fmt.Errorf("%s is empty, so there is nothing to send", location))
	case size > maximumSendBytes:
		body.Close()
		return s.toolFailure(ctx, principalID, call.Name, fmt.Errorf("%s is %s; only files up to %s can be sent", location, formatSize(size), formatSize(maximumSendBytes)))
	}

	// The sender owns the open file from here, including closing it on every failure.
	if err := s.sender.SendFile(ctx, principalID, delivery.File{Name: location.Name(), Size: size, Body: body}); err != nil {
		if errors.Is(err, delivery.ErrNoChat) {
			return tools.Result{Text: noChatText, IsError: true}, nil
		}
		s.logger.WarnContext(ctx, "nas.send.failed", "principal_id", principalID, "size_bytes", size)
		return tools.Result{Text: undeliverableText, IsError: true}, nil
	}
	s.logger.InfoContext(ctx, "nas.send.completed", "principal_id", principalID, "size_bytes", size)
	return tools.Result{Text: fmt.Sprintf("Sent %s (%s) to the user in this chat; they have the file itself.", location, formatSize(size))}, nil
}
