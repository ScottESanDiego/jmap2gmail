package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Fastmail   Fastmail          `json:"fastmail"`
	Gmail      Gmail             `json:"gmail"`
	StateFile  string            `json:"state_file"`
	Delete     bool              `json:"delete"`
	RoleLabels map[string]string `json:"role_labels,omitempty"`
}

type Fastmail struct {
	TokenFile        string   `json:"token_file"`
	SessionEndpoint  string   `json:"session_endpoint,omitempty"`
	IncludeMailboxes []string `json:"include_mailboxes,omitempty"`
}

type Gmail struct {
	CredentialsFile string `json:"credentials_file"`
	TokenFile       string `json:"token_file"`
	User            string `json:"user,omitempty"`
}

func Load(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()
	var config Config
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if config.StateFile == "" {
		config.StateFile = "~/.local/state/jmap2gmail/state.json"
	}
	if config.Gmail.TokenFile == "" {
		config.Gmail.TokenFile = "~/.config/jmap2gmail/google-token.json"
	}
	if config.Gmail.User == "" {
		config.Gmail.User = "me"
	}
	base := filepath.Dir(path)
	paths := []*string{
		&config.Fastmail.TokenFile,
		&config.Gmail.CredentialsFile,
		&config.Gmail.TokenFile,
		&config.StateFile,
	}
	for _, value := range paths {
		expanded, err := expandPath(*value, base)
		if err != nil {
			return Config{}, err
		}
		*value = expanded
	}
	return config, nil
}

func (c Config) ValidateForAuth() error {
	if c.Gmail.CredentialsFile == "" {
		return errors.New("gmail.credentials_file is required")
	}
	if c.Gmail.TokenFile == "" {
		return errors.New("gmail.token_file is required")
	}
	return nil
}

func (c Config) ValidateForSync() error {
	var missing []string
	if c.Fastmail.TokenFile == "" {
		missing = append(missing, "fastmail.token_file")
	}
	if c.Gmail.CredentialsFile == "" {
		missing = append(missing, "gmail.credentials_file")
	}
	if c.Gmail.TokenFile == "" {
		missing = append(missing, "gmail.token_file")
	}
	if c.StateFile == "" {
		missing = append(missing, "state_file")
	}
	if len(missing) != 0 {
		return fmt.Errorf("missing required config: %s", strings.Join(missing, ", "))
	}
	return nil
}

func expandPath(path, base string) (string, error) {
	if path == "" {
		return "", nil
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	return filepath.Clean(filepath.Join(base, path)), nil
}
