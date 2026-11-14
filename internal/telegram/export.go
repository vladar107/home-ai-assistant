package telegram

import (
	"context"
	"errors"

	"github.com/vladar107/home-ai-assistant/internal/export"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

var (
	ExportFailedReply  = locale.T("Your export could not be made. Try again later.", "Не удалось подготовить экспорт. Попробуй позже.")
	RestoreFailedReply = locale.T("That file could not be checked right now. Try again later.", "Сейчас не удаётся проверить этот файл. Попробуй позже.")
	ExportReply        = locale.T("Here is your export: what you set up and keep here, such as your memory, jobs, watches, places, and projects. "+
		"It holds private data, and Telegram keeps the files it carries on its servers, as it keeps this chat, so keep the file somewhere safe. "+
		"Secrets, connections, and conversations are never in it. To restore from it, send it back with /restore as its caption.", "Вот твой экспорт: то, что настроено и хранится у тебя здесь, например память, задания, наблюдения, места и проекты. В нём личные данные, а Telegram хранит файлы, которые передаёт, на своих серверах, как хранит и этот чат, поэтому сохрани файл в надёжном месте. Секретов, подключений и разговоров в нём никогда нет. Чтобы восстановить данные из него, отправь его обратно с подписью /restore.")
	RestoreHelpReply = locale.T("To restore from an export, send me the export file with /restore as its caption, or reply /restore to the message that holds it. I show everything it would add before anything changes.", "Чтобы восстановить данные из экспорта, отправь мне файл экспорта с подписью /restore или ответь /restore на сообщение с этим файлом. Прежде чем что-либо изменится, я покажу всё, что будет добавлено.")
)

// ExportController makes a user's export and restores one; nil leaves /export and /restore out.
type ExportController interface {
	// SendExport builds the principal's export and sends it to their own chat as a document.
	SendExport(ctx context.Context, principalID string) error
	// PreviewRestore verifies the principal's file and plans restoring it; a file that is refused
	// is an *export.Refusal.
	PreviewRestore(ctx context.Context, principalID string, data []byte) (export.Preview, error)
	// OfferRestore asks the principal to confirm restoring the previewed file.
	OfferRestore(ctx context.Context, principalID string, chatID, threadID int64, preview export.Preview) error
}

// processExport answers /export: what the file holds and where Telegram keeps it, then the file.
func (s *Service) processExport(ctx context.Context, message *Message, principalID string) (processingResult, string, error) {
	result, failure, err := s.sendCommandReply(ctx, message, principalID, "export", ExportReply.Format(ctx), nil)
	if err != nil || result.event != "telegram.command.handled" {
		return result, failure, err
	}
	if err := s.exports.SendExport(ctx, principalID); err != nil {
		s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.export.failed", "principal_id", principalID, "error", err)
		return s.sendCommandReply(ctx, message, principalID, "export", ExportFailedReply.Format(ctx), nil)
	}
	return result, "", nil
}

// restoreDocument is the file a /restore names: the message's own, or the one it replies to.
func restoreDocument(message *Message) *MediaFile {
	if message.Document != nil {
		return message.Document
	}
	if message.ReplyToMessage != nil {
		return message.ReplyToMessage.Document
	}
	return nil
}

// processRestore answers /restore with a file: it downloads the file within the export's limit,
// shows everything restoring it would add, and asks to confirm when it adds anything.
func (s *Service) processRestore(ctx context.Context, message *Message, principalID string) (processingResult, string, error) {
	document := restoreDocument(message)
	if document == nil || document.FileID == "" {
		return s.sendCommandReply(ctx, message, principalID, "restore", RestoreHelpReply.Format(ctx), nil)
	}
	if document.FileSize > export.MaximumFileBytes {
		return s.sendCommandReply(ctx, message, principalID, "restore", export.ErrTooLarge.Message.Format(ctx), nil)
	}
	reference, err := s.client.GetFile(ctx, document.FileID)
	if err != nil {
		if isPermanentFileError(err) {
			return s.sendCommandReply(ctx, message, principalID, "restore", AttachmentUnavailableText.Format(ctx), nil)
		}
		return processingResult{}, "fetch_restore", err
	}
	data, err := s.client.DownloadFile(ctx, reference.FilePath, export.MaximumFileBytes)
	if err != nil {
		var oversized *oversizedFileError
		if errors.As(err, &oversized) {
			return s.sendCommandReply(ctx, message, principalID, "restore", export.ErrTooLarge.Message.Format(ctx), nil)
		}
		if isPermanentFileError(err) {
			return s.sendCommandReply(ctx, message, principalID, "restore", AttachmentUnavailableText.Format(ctx), nil)
		}
		return processingResult{}, "fetch_restore", err
	}
	preview, err := s.exports.PreviewRestore(ctx, principalID, data)
	if err != nil {
		var refusal *export.Refusal
		if errors.As(err, &refusal) {
			return s.sendCommandReply(ctx, message, principalID, "restore", refusal.Message.Format(ctx), nil)
		}
		s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.restore.failed", "principal_id", principalID, "error", err)
		return s.sendCommandReply(ctx, message, principalID, "restore", RestoreFailedReply.Format(ctx), nil)
	}
	// The preview retells the user's file, so it is sent as plain text, where nothing in it can hide
	// behind markup.
	for _, page := range preview.Pages {
		if err := s.client.SendPlainReply(ctx, message.Chat.ID, message.MessageID, page); err != nil {
			if isPermanentAPIError(err) {
				return processingResult{event: "telegram.command.undeliverable", reason: "send_rejected", principalID: principalID, command: "restore", err: err}, "", nil
			}
			return processingResult{}, "send_message", err
		}
	}
	result := processingResult{event: "telegram.command.handled", principalID: principalID, command: "restore"}
	if preview.Additions == 0 {
		return result, "", nil
	}
	if err := s.exports.OfferRestore(ctx, principalID, message.Chat.ID, thread.From(ctx), preview); err != nil {
		s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.restore.offer_failed", "principal_id", principalID, "error", err)
		return s.sendCommandReply(ctx, message, principalID, "restore", RestoreFailedReply.Format(ctx), nil)
	}
	return result, "", nil
}
