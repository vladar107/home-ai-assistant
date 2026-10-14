package telegram

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/attachment"
)

// inlineImageTypes are the picture formats worth sending to the model as an image. Any other
// image type is kept as a file the model can be told about but cannot look at.
var inlineImageTypes = map[string]struct{}{
	"image/jpeg": {},
	"image/png":  {},
	"image/gif":  {},
	"image/webp": {},
}

// media is one attachment a message carried, before it has been downloaded.
type media struct {
	fileID   string
	kind     attachment.Kind
	name     string
	mimeType string
	size     int64
}

func (m media) limit() int64 {
	if m.kind == attachment.KindImage {
		return attachment.MaximumImageBytes
	}
	return attachment.MaximumFileBytes
}

// classifyMedia reports what a message carried: the files that can be downloaded, or the reason
// the message holds something this application cannot pass to a model at all.
func classifyMedia(message *Message) ([]media, string) {
	switch {
	case message.Voice != nil, message.Audio != nil:
		return nil, "audio"
	case message.Video != nil, message.VideoNote != nil, message.Animation != nil:
		return nil, "video"
	case message.Sticker != nil:
		return nil, "sticker"
	}

	if size := largestPhoto(message.Photo); size != nil {
		return []media{{
			fileID:   size.FileID,
			kind:     attachment.KindImage,
			name:     "photo.jpg",
			mimeType: "image/jpeg",
			size:     size.FileSize,
		}}, ""
	}
	if document := message.Document; document != nil {
		return []media{documentMedia(document)}, ""
	}
	return nil, ""
}

// largestPhoto picks the best rendition the size limit allows. Telegram lists renditions from
// smallest to largest and may omit the size, which is then discovered by the download itself.
func largestPhoto(sizes []PhotoSize) *PhotoSize {
	var chosen *PhotoSize
	for index := range sizes {
		size := &sizes[index]
		if size.FileID == "" || size.FileSize > attachment.MaximumImageBytes {
			continue
		}
		if chosen == nil || size.FileSize >= chosen.FileSize {
			chosen = size
		}
	}
	if chosen == nil && len(sizes) > 0 {
		// Every rendition is too large, which the caller reports rather than silently ignoring.
		return &PhotoSize{FileID: sizes[0].FileID, FileSize: sizes[0].FileSize}
	}
	return chosen
}

func documentMedia(document *MediaFile) media {
	name := strings.TrimSpace(document.FileName)
	if name == "" {
		name = "file"
	}
	mimeType := strings.TrimSpace(document.MimeType)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	kind := attachment.KindFile
	if _, inline := inlineImageTypes[strings.ToLower(mimeType)]; inline && document.FileSize <= attachment.MaximumImageBytes {
		kind = attachment.KindImage
	}
	return media{fileID: document.FileID, kind: kind, name: name, mimeType: mimeType, size: document.FileSize}
}

// fetchMedia downloads what a message carried. It returns a reply for a file this application
// refuses to hold, or an error for a failure that should be retried with the update.
func (s *Service) fetchMedia(ctx context.Context, principalID string, items []media) ([]attachment.Incoming, string, error) {
	if s.attachments == nil {
		return nil, UnsupportedAttachmentReply, nil
	}
	var total int64
	for _, item := range items {
		if item.fileID == "" {
			return nil, AttachmentUnavailableReply, nil
		}
		if item.size > item.limit() {
			return nil, attachmentTooLargeReply(item), nil
		}
		total += item.size
	}
	room, err := s.attachments.Room(ctx, principalID)
	if err != nil {
		return nil, "", err
	}
	if room.Files < len(items) || room.Bytes < total {
		return nil, ConversationFullReply, nil
	}

	files := make([]attachment.Incoming, 0, len(items))
	for _, item := range items {
		reference, err := s.client.GetFile(ctx, item.fileID)
		if err != nil {
			if isPermanentFileError(err) {
				return nil, AttachmentUnavailableReply, nil
			}
			return nil, "", err
		}
		data, err := s.client.DownloadFile(ctx, reference.FilePath, item.limit())
		if err != nil {
			var oversized *oversizedFileError
			if errors.As(err, &oversized) {
				return nil, attachmentTooLargeReply(item), nil
			}
			if isPermanentFileError(err) {
				return nil, AttachmentUnavailableReply, nil
			}
			return nil, "", err
		}
		files = append(files, attachment.Incoming{
			Kind:     item.kind,
			FileName: fileName(item, reference.FilePath),
			MimeType: item.mimeType,
			Data:     data,
		})
	}
	return files, "", nil
}

// fileName prefers the name the sender gave. A photo has none, so the download path supplies its
// extension.
func fileName(item media, filePath string) string {
	if item.kind != attachment.KindImage || item.name != "photo.jpg" {
		return item.name
	}
	if extension := filepath.Ext(filePath); extension != "" {
		return "photo" + extension
	}
	return item.name
}

// attachmentTooLargeReply names the limit the file passed, in the units the sender thinks in.
func attachmentTooLargeReply(item media) string {
	if item.kind == attachment.KindImage {
		return "That image is too large for me to hold; images must stay under 5 MB."
	}
	return "That file is too large for me to hold; files must stay under 20 MB."
}

// unsupportedMediaReply answers a message carrying something no model reachable here can read.
func unsupportedMediaReply(reason string) string {
	switch reason {
	case "audio":
		return UnsupportedAudioReply
	case "video":
		return UnsupportedVideoReply
	default:
		return UnsupportedStickerReply
	}
}

func isPermanentFileError(err error) bool {
	var permanent interface{ Permanent() bool }
	return errors.As(err, &permanent) && permanent.Permanent()
}
