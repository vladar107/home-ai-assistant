// Package imagegen lets the model draw a picture with the requesting principal's own ChatGPT
// subscription. The picture goes straight to that principal's chat; the model only learns
// whether it arrived.
package imagegen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/attachment"
	"github.com/vladar107/home-ai-assistant/internal/chatgpt"
	"github.com/vladar107/home-ai-assistant/internal/connection"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	toolGenerateImage = "generate_image"

	maximumPromptRunes = 4000
	maximumReferences  = 4
	// generateTimeout covers drawing and uploading; drawing alone often takes most of a minute.
	generateTimeout = 100 * time.Second

	guidance = "generate_image draws a picture from a description and shows it to the user in this chat. " +
		"Use it when the user asks you to draw, create, or make an image, or to change a photo they sent; pass that photo's attachment id as a reference. " +
		"Write the prompt in English as a complete visual description, keeping the user's details and any text that should appear in the picture. " +
		"Generate one picture per request unless the user asked for several. The picture reaches the user directly, so do not describe it afterwards; " +
		"a short line is enough. Generating spends the user's ChatGPT usage, so do not retry a failure unless the user asks."

	deliveredText    = "The picture was generated and is now shown to the user in the chat. Do not describe it; at most add a short line."
	usageLimitText   = "The user's ChatGPT usage limit is reached, so no picture can be generated until it resets. Tell the user."
	unauthorizedText = "The user's ChatGPT subscription refused the request. Tell the user to reconnect it in /settings."
	timedOutText     = "Generating the picture took too long and was stopped. Tell the user; do not retry unless they ask."
	failedText       = "The picture could not be generated. Tell the user; do not retry unless they ask."
	undeliveredText  = "The picture was generated but could not be shown in the chat. Tell the user it did not arrive; do not describe it instead."
)

var schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "prompt": {"type": "string", "description": "A complete description of the picture to draw, or of the change to make to the reference photos."},
    "reference_attachment_ids": {
      "type": "array",
      "items": {"type": "string"},
      "maxItems": 4,
      "description": "Ids of photos the user sent in this conversation to edit or draw from."
    }
  },
  "required": ["prompt"],
  "additionalProperties": false
}`)

// Generator draws with a principal's own subscription.
type Generator interface {
	GenerateImage(ctx context.Context, principalID string, request chatgpt.ImageRequest) (chatgpt.Image, error)
}

// Sender shows a picture in the principal's own chat; no argument the model chooses can send it
// anywhere else.
type Sender interface {
	SendImage(ctx context.Context, principalID, name string, image []byte) error
}

// Attachments returns the photos a principal sent.
type Attachments interface {
	Content(ctx context.Context, principalID, attachmentID string) (attachment.Record, []byte, bool, error)
}

type Config struct {
	// Model is the conversation model that carries the hosted image tool.
	Model string
}

type Service struct {
	model       string
	generator   Generator
	sender      Sender
	attachments Attachments
	logger      *slog.Logger
}

var _ tools.Provider = (*Service)(nil)

func NewService(cfg Config, generator Generator, sender Sender, attachments Attachments, logger *slog.Logger) (*Service, error) {
	if generator == nil || sender == nil || attachments == nil || logger == nil {
		return nil, fmt.Errorf("image generation dependencies are required")
	}
	model := carrierModel(cfg.Model)
	if model == "" {
		return nil, fmt.Errorf("image generation model is required")
	}
	return &Service{model: model, generator: generator, sender: sender, attachments: attachments, logger: logger}, nil
}

// carrierModel drops the thinking level Pi accepts after the model id, which the Responses
// endpoint does not.
func carrierModel(model string) string {
	id, _, _ := strings.Cut(strings.TrimSpace(model), ":")
	return id
}

func (s *Service) Contribute(context.Context, string) (tools.Contribution, error) {
	return tools.Contribution{
		Guidance: guidance,
		Definitions: []tools.Definition{{
			Name:        toolGenerateImage,
			Description: "Draw a picture from a description, optionally from photos the user sent, and show it to the user in this chat.",
			InputSchema: schema,
			Risk:        tools.RiskRead,
			Timeout:     generateTimeout,
			Activity:    tools.ActivityImage,
		}},
	}, nil
}

func (s *Service) Execute(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if call.Name != toolGenerateImage {
		return tools.Result{Text: "This tool is not available.", IsError: true}, nil
	}
	var input struct {
		Prompt       string   `json:"prompt"`
		ReferenceIDs []string `json:"reference_attachment_ids"`
	}
	if err := tools.DecodeArguments(call.Arguments, &input); err != nil {
		return tools.Result{Text: "The arguments could not be read.", IsError: true}, nil
	}
	prompt := strings.TrimSpace(input.Prompt)
	if prompt == "" || !utf8.ValidString(prompt) || utf8.RuneCountInString(prompt) > maximumPromptRunes {
		return tools.Result{Text: fmt.Sprintf("The prompt must be between 1 and %d characters.", maximumPromptRunes), IsError: true}, nil
	}
	if len(input.ReferenceIDs) > maximumReferences {
		return tools.Result{Text: fmt.Sprintf("At most %d reference photos can be used.", maximumReferences), IsError: true}, nil
	}

	references := make([]chatgpt.Image, 0, len(input.ReferenceIDs))
	for _, id := range input.ReferenceIDs {
		record, data, found, err := s.attachments.Content(ctx, principalID, id)
		if err != nil {
			return tools.Result{}, err
		}
		if !found {
			return tools.Result{Text: fmt.Sprintf("No photo with id %q is available in this conversation. Use list_attachments to see the ids.", id), IsError: true}, nil
		}
		if record.Kind != attachment.KindImage {
			return tools.Result{Text: fmt.Sprintf("%s is not a photo, so it cannot be used as a reference.", record.FileName), IsError: true}, nil
		}
		references = append(references, chatgpt.Image{MimeType: record.MimeType, Data: data})
	}

	image, err := s.generator.GenerateImage(ctx, principalID, chatgpt.ImageRequest{Model: s.model, Prompt: prompt, References: references})
	if err != nil {
		return s.failure(ctx, principalID, err), nil
	}
	if err := s.sender.SendImage(ctx, principalID, "image"+extension(image.MimeType), image.Data); err != nil {
		s.logger.WarnContext(ctx, "imagegen.undelivered", "principal_id", principalID, "error", err)
		return tools.Result{Text: undeliveredText, IsError: true}, nil
	}
	s.logger.InfoContext(ctx, "imagegen.delivered", "principal_id", principalID, "references", len(references), "size_bytes", len(image.Data))
	return tools.Result{Text: deliveredText}, nil
}

func (s *Service) failure(ctx context.Context, principalID string, err error) tools.Result {
	var text, reason string
	switch {
	case errors.Is(err, chatgpt.ErrNoImage):
		reason = "no_image"
		text = "No picture was generated."
		detail := strings.TrimPrefix(strings.TrimPrefix(err.Error(), chatgpt.ErrNoImage.Error()), ":")
		if detail = strings.TrimSpace(detail); detail != "" {
			text += " The image model said: " + detail
		}
		text += " Tell the user why, and suggest a change if the request was refused."
	case errors.Is(err, chatgpt.ErrUsageLimit):
		reason, text = "usage_limit", usageLimitText
	case errors.Is(err, chatgpt.ErrUnauthorized), errors.Is(err, connection.ErrNotConnected):
		reason, text = "unauthorized", unauthorizedText
	case errors.Is(err, context.DeadlineExceeded):
		reason, text = "timeout", timedOutText
	default:
		reason, text = "failed", failedText
	}
	s.logger.WarnContext(ctx, "imagegen.failed", "principal_id", principalID, "reason", reason, "error", err)
	return tools.Result{Text: text, IsError: true}
}

func extension(mimeType string) string {
	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}
