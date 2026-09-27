package attachment

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/vladar107/home-ai-assistant/internal/pdftext"
)

// textCacheSuffix names the extracted text kept beside a PDF's bytes, so reading a later window
// does not parse the document again. It lives and dies with the bytes it was extracted from.
const textCacheSuffix = ".txt"

var errUnavailable = errors.New("attachment bytes are unavailable")

func (s *Service) pdfText(ctx context.Context, record Record) (string, error) {
	path, err := s.path(record)
	if err != nil {
		return "", err
	}
	if cached, err := os.ReadFile(path + textCacheSuffix); err == nil {
		return string(cached), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errUnavailable
	}
	text, err := pdftext.Extract(ctx, data)
	if err != nil {
		return "", err
	}
	if err := cacheText(path, text); err != nil {
		s.logger.WarnContext(ctx, "attachment.text_cache.failed", "principal_id", record.PrincipalID)
	}
	return text, nil
}

// cacheText writes through a temporary file so a concurrent read never sees half the text.
func cacheText(path, text string) error {
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+textCacheSuffix+".*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	if _, err := file.WriteString(text); err != nil {
		file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, path+textCacheSuffix); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	// A session closed while the text was being extracted has already removed the bytes; the
	// cache must not outlive them until the next start.
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(path + textCacheSuffix)
	}
	return nil
}
