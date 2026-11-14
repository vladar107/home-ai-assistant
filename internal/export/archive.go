package export

import (
	"archive/zip"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"runtime/debug"
	"slices"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/locale"
)

const (
	// FormatVersion is the newest layout of the file this code reads and the one it writes.
	FormatVersion = 1
	// MaximumFileBytes bounds the file as sent, which is also the most a bot may download.
	MaximumFileBytes = 20 << 20
	// MaximumEntries, MaximumEntryBytes, and MaximumTotalBytes bound the file once inflated; they
	// are checked while inflating, never from the zip headers.
	MaximumEntries    = 64
	MaximumEntryBytes = 10 << 20
	MaximumTotalBytes = 20 << 20

	manifestName = "manifest.json"
	signatureTag = "home-assistant-export\x00"
)

// entryName is the only shape of a name the file may hold: flat, with no directory, so nothing in
// it can name a path.
var entryName = regexp.MustCompile(`^[a-z0-9_]{1,64}\.json$`)

// Refusal is a file that is not restored at all, with the sentence that tells the user why. Text is
// that sentence in English, for the model and the log; Message is it in both languages, for the
// user.
type Refusal struct {
	Reason  string
	Text    string
	Message locale.Text
}

func newRefusal(reason string, message locale.Text) *Refusal {
	return &Refusal{Reason: reason, Text: message.In(locale.English), Message: message}
}

func (r *Refusal) Error() string { return "export refused: " + r.Reason }

var (
	ErrUnreadable = newRefusal("unreadable", locale.T("This is not an export file I can read; nothing was restored.", "Это не файл экспорта, который я могу прочитать; ничего не восстановлено."))
	ErrTooLarge   = newRefusal("too_large", locale.T("This file is larger than an export can be; nothing was restored.", "Этот файл больше, чем может быть экспорт; ничего не восстановлено."))
	ErrTooMany    = newRefusal("too_many_entries", locale.T("This file holds more parts than an export can; nothing was restored.", "В этом файле больше частей, чем может быть в экспорте; ничего не восстановлено."))
	ErrPath       = newRefusal("path", locale.T("This file holds a part with a path, which an export never has; nothing was restored.", "В этом файле есть часть с путём, а в экспорте такого не бывает; ничего не восстановлено."))
	ErrSignature  = newRefusal("signature", locale.T("This file was changed after it was exported, or was not exported here; nothing was restored.", "Этот файл изменили после экспорта или он создан не здесь; ничего не восстановлено."))
	ErrPrincipal  = newRefusal("principal", locale.T("This export belongs to another user, and you can restore only your own; nothing was restored.", "Этот экспорт принадлежит другому пользователю, а восстановить можно только свой; ничего не восстановлено."))
	ErrNewer      = newRefusal("newer", locale.T("This export was made by a newer version of the assistant; nothing was restored.", "Этот экспорт создан более новой версией ассистента; ничего не восстановлено."))
)

// manifest is what the signature covers: who the file is for, when it was made, and each
// section's file with its digest, version, and row counts.
type manifest struct {
	Format      int       `json:"format"`
	ExportedAt  time.Time `json:"exported_at"`
	AppVersion  string    `json:"app_version"`
	PrincipalID string    `json:"principal_id"`
	Sections    []entry   `json:"sections"`
}

type entry struct {
	Name    string         `json:"name"`
	Version int            `json:"version"`
	File    string         `json:"file"`
	SHA256  string         `json:"sha256"`
	Counts  map[string]int `json:"counts"`
}

// signedManifest is manifest.json: the manifest's exact bytes and their HMAC-SHA256 under the
// installation's export key.
type signedManifest struct {
	Manifest  json.RawMessage `json:"manifest"`
	Signature string          `json:"signature"`
}

// sectionFile is one section's file.
type sectionFile struct {
	Section string `json:"section"`
	Version int    `json:"version"`
	Tables  Tables `json:"tables"`
}

// archive is a file that was read within the limits and verified: its manifest and its sections.
type archive struct {
	manifest manifest
	sections map[string]sectionFile
}

func sign(key, manifest []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signatureTag))
	mac.Write(manifest)
	return mac.Sum(nil)
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// appVersion names the build that made the file, for the operator reading one.
func appVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && setting.Value != "" {
			return setting.Value
		}
	}
	if info.Main.Version != "" {
		return info.Main.Version
	}
	return "unknown"
}

// writeArchive builds the file: the signed manifest first, then each section's file. A file that
// could not be read back within the limits is refused here, so no export is unrestorable.
func writeArchive(key []byte, principalID string, at time.Time, sections []sectionFile) ([]byte, error) {
	files := make(map[string][]byte, len(sections))
	m := manifest{Format: FormatVersion, ExportedAt: at.UTC(), AppVersion: appVersion(), PrincipalID: principalID}
	total := 0
	for _, section := range sections {
		body, err := json.Marshal(section)
		if err != nil {
			return nil, fmt.Errorf("encode export section %s: %w", section.Section, err)
		}
		name := section.Section + ".json"
		if !entryName.MatchString(name) || name == manifestName {
			return nil, fmt.Errorf("export section %q has no valid file name", section.Section)
		}
		if len(body) > MaximumEntryBytes {
			return nil, fmt.Errorf("export section %s is too large", section.Section)
		}
		total += len(body)
		counts := make(map[string]int, len(section.Tables))
		for table, rows := range section.Tables {
			counts[table] = len(rows)
		}
		m.Sections = append(m.Sections, entry{Name: section.Section, Version: section.Version, File: name, SHA256: digest(body), Counts: counts})
		files[name] = body
	}
	if len(sections)+1 > MaximumEntries {
		return nil, fmt.Errorf("export has too many sections")
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode export manifest: %w", err)
	}
	signed, err := json.Marshal(signedManifest{Manifest: raw, Signature: hex.EncodeToString(sign(key, raw))})
	if err != nil {
		return nil, fmt.Errorf("encode signed export manifest: %w", err)
	}
	if total+len(signed) > MaximumTotalBytes {
		return nil, fmt.Errorf("export is too large")
	}
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	write := func(name string, body []byte) error {
		part, err := writer.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: at.UTC()})
		if err != nil {
			return err
		}
		_, err = part.Write(body)
		return err
	}
	if err := write(manifestName, signed); err != nil {
		return nil, fmt.Errorf("write export manifest: %w", err)
	}
	for _, section := range m.Sections {
		if err := write(section.File, files[section.File]); err != nil {
			return nil, fmt.Errorf("write export section %s: %w", section.Name, err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("finish export: %w", err)
	}
	if buffer.Len() > MaximumFileBytes {
		return nil, fmt.Errorf("export is too large")
	}
	return buffer.Bytes(), nil
}

// readArchive reads and verifies a file for the principal. Every limit is checked while the file
// inflates; the signature is checked before anything in the manifest is believed, and every part
// must be one the signed manifest lists with its digest.
func readArchive(data []byte, key []byte, principalID string) (*archive, error) {
	if len(data) > MaximumFileBytes {
		return nil, ErrTooLarge
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, ErrUnreadable
	}
	if len(reader.File) > MaximumEntries {
		return nil, ErrTooMany
	}
	contents := make(map[string][]byte, len(reader.File))
	total := int64(0)
	for _, file := range reader.File {
		if !entryName.MatchString(file.Name) {
			return nil, ErrPath
		}
		if _, repeated := contents[file.Name]; repeated {
			return nil, ErrUnreadable
		}
		limit := min(int64(MaximumEntryBytes), MaximumTotalBytes-total)
		body, err := inflate(file, limit)
		if err != nil {
			return nil, err
		}
		total += int64(len(body))
		contents[file.Name] = body
	}
	raw, found := contents[manifestName]
	if !found {
		return nil, ErrUnreadable
	}
	var signed signedManifest
	if err := json.Unmarshal(raw, &signed); err != nil || len(signed.Manifest) == 0 {
		return nil, ErrUnreadable
	}
	signature, err := hex.DecodeString(signed.Signature)
	if err != nil || len(key) == 0 || !hmac.Equal(signature, sign(key, signed.Manifest)) {
		return nil, ErrSignature
	}
	var m manifest
	if err := json.Unmarshal(signed.Manifest, &m); err != nil || m.Format < 1 {
		return nil, ErrUnreadable
	}
	if m.Format > FormatVersion {
		return nil, ErrNewer
	}
	if m.PrincipalID != principalID {
		return nil, ErrPrincipal
	}
	a := &archive{manifest: m, sections: make(map[string]sectionFile, len(m.Sections))}
	listed := map[string]bool{manifestName: true}
	for _, section := range m.Sections {
		body, found := contents[section.File]
		if !found || listed[section.File] || digest(body) != section.SHA256 {
			return nil, ErrSignature
		}
		listed[section.File] = true
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		var file sectionFile
		if err := decoder.Decode(&file); err != nil || file.Section != section.Name || file.Version != section.Version {
			return nil, ErrUnreadable
		}
		if _, repeated := a.sections[section.Name]; repeated {
			return nil, ErrUnreadable
		}
		a.sections[section.Name] = file
	}
	for name := range contents {
		if !listed[name] {
			return nil, ErrSignature
		}
	}
	return a, nil
}

// inflate reads one part, refusing it as soon as it grows past limit, whatever its header says.
func inflate(file *zip.File, limit int64) ([]byte, error) {
	if file.Method != zip.Store && file.Method != zip.Deflate {
		return nil, ErrUnreadable
	}
	body, err := file.Open()
	if err != nil {
		return nil, ErrUnreadable
	}
	defer body.Close()
	read, err := io.ReadAll(io.LimitReader(body, limit+1))
	if int64(len(read)) > limit {
		return nil, ErrTooLarge
	}
	if err != nil {
		// A part that ends early or does not match its checksum is not a file this code wrote.
		return nil, ErrUnreadable
	}
	return read, nil
}

// sectionNames lists the archive's sections in a fixed order.
func (a *archive) sectionNames() []string {
	names := make([]string, 0, len(a.sections))
	for name := range a.sections {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
