package telegram

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

const tokenFileSizeLimit = 4 << 10

func LoadTokenFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("Telegram token file metadata could not be read")
	}
	if err := validateTokenFile(info); err != nil {
		return "", err
	}

	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("Telegram token file could not be opened")
	}
	defer file.Close()

	info, err = file.Stat()
	if err != nil {
		return "", fmt.Errorf("Telegram token file metadata could not be read")
	}
	if err := validateTokenFile(info); err != nil {
		return "", err
	}

	contents, err := io.ReadAll(io.LimitReader(file, tokenFileSizeLimit+1))
	if err != nil {
		return "", fmt.Errorf("Telegram token file could not be read")
	}
	if len(contents) > tokenFileSizeLimit {
		return "", fmt.Errorf("Telegram token file is too large")
	}
	if !utf8.Valid(contents) {
		return "", fmt.Errorf("Telegram token file contains an invalid token")
	}

	token := strings.TrimRightFunc(string(contents), unicode.IsSpace)
	if err := validateToken(token); err != nil {
		return "", fmt.Errorf("Telegram token file contains an invalid token")
	}
	return token, nil
}

func validateTokenFile(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("Telegram token file must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("Telegram token file must not grant group or other permissions")
	}
	if info.Size() > tokenFileSizeLimit {
		return fmt.Errorf("Telegram token file is too large")
	}
	return nil
}
