package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zalando/go-keyring"
)

// ReadStoredToken reads a token from the configured store (keyring or file).
func ReadStoredToken(service, username string) (string, error) {
	if os.Getenv("ENTIRE_TOKEN_STORE") == "file" {
		return readFileToken(fileTokenPath(), service, username)
	}
	token, err := keyring.Get(service, username)
	if err != nil {
		return "", fmt.Errorf("read keyring token: %w", err)
	}
	return token, nil
}

// WriteStoredToken writes a token to the configured store.
func WriteStoredToken(service, username, password string) error {
	if os.Getenv("ENTIRE_TOKEN_STORE") == "file" {
		return writeFileToken(fileTokenPath(), service, username, password)
	}
	if err := keyring.Set(service, username, password); err != nil {
		return fmt.Errorf("write keyring token: %w", err)
	}
	return nil
}

func fileTokenPath() string {
	path := os.Getenv("ENTIRE_TOKEN_STORE_PATH")
	if path != "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "entiredb", "tokens.json")
}

func readFileToken(path, service, username string) (string, error) {
	if path == "" {
		return "", keyring.ErrNotFound
	}
	// Acquire shared lock for concurrent read safety (issue #10).
	unlock, err := flockShared(path)
	if err != nil {
		// If we can't lock (e.g., file doesn't exist yet), fall through to direct read.
		return readFileTokenDirect(path, service, username)
	}
	defer unlock()
	return readFileTokenDirect(path, service, username)
}

func readFileTokenDirect(path, service, username string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", keyring.ErrNotFound
		}
		return "", fmt.Errorf("read token store file: %w", err)
	}
	var store map[string]map[string]string
	if err := json.Unmarshal(data, &store); err != nil {
		return "", fmt.Errorf("unmarshal token store: %w", err)
	}
	users := store[service]
	if users == nil {
		return "", keyring.ErrNotFound
	}
	password, ok := users[username]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return password, nil
}

// writeFileToken writes a token to the file store with exclusive file locking
// to prevent corruption from concurrent processes (issue #10).
func writeFileToken(path, service, username, password string) error {
	if path == "" {
		return os.ErrInvalid
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	// Acquire exclusive lock for the write.
	unlock, err := flockExclusive(path)
	if err != nil {
		return err
	}
	defer unlock()

	// Re-read under lock to avoid lost updates.
	store := map[string]map[string]string{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &store); err != nil {
			return fmt.Errorf("unmarshal token store: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read token store file: %w", err)
	}
	if store[service] == nil {
		store[service] = map[string]string{}
	}
	store[service][username] = password
	data, err := json.Marshal(store)
	if err != nil {
		return fmt.Errorf("marshal token store: %w", err)
	}
	// Atomic write: write to temp file then rename to prevent corruption on crash.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write token store temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename token store temp file: %w", err)
	}
	return nil
}

// flockShared / flockExclusive are defined per-platform: tokenstore_lock_unix.go
// uses syscall.Flock, tokenstore_lock_windows.go provides a compiling fallback
// (syscall.Flock and the LOCK_* constants do not exist on Windows).
