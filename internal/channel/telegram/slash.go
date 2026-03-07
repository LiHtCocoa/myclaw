package telegram

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type CommandType string

const (
	CommandTypeInstant      CommandType = "instant"
	CommandTypeOneshot      CommandType = "oneshot"
	CommandTypeConversation CommandType = "conversation"
)

type Command struct {
	Name           string
	Description    string
	Type           CommandType
	Prompt         string
	InitialMessage string
	Handler        string
}

type commandMeta struct {
	Command        string `yaml:"command"`
	Description    string `yaml:"description"`
	Type           string `yaml:"type"`
	Handler        string `yaml:"handler"`
	InitialMessage string `yaml:"initialMessage"`
}

func LoadCommands(dir string) ([]Command, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, nil
	}

	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat slash dir %q: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("slash path is not a directory: %s", dir)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read slash dir %q: %w", dir, err)
	}

	var commands []Command
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		cmdPath := filepath.Join(dir, entry.Name())
		cmd, skip, parseErr := parseCommandFile(cmdPath)
		if parseErr != nil {
			return nil, parseErr
		}
		if skip {
			continue
		}
		commands = append(commands, cmd)
	}

	return commands, nil
}

func parseCommandFile(path string) (Command, bool, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Command{}, true, nil
		}
		return Command{}, false, fmt.Errorf("read command %q: %w", path, err)
	}

	meta, body, err := parseFrontmatter(content)
	if err != nil {
		return Command{}, false, fmt.Errorf("parse command %q: %w", path, err)
	}

	if strings.TrimSpace(meta.Command) == "" {
		return Command{}, false, fmt.Errorf("parse command %q: missing command name", path)
	}

	cmdType := CommandTypeConversation
	if meta.Type != "" {
		cmdType = CommandType(meta.Type)
	}

	return Command{
		Name:           strings.TrimSpace(meta.Command),
		Description:    strings.TrimSpace(meta.Description),
		Type:           cmdType,
		Prompt:         strings.TrimSpace(body),
		InitialMessage: strings.TrimSpace(meta.InitialMessage),
		Handler:        strings.TrimSpace(meta.Handler),
	}, false, nil
}

func parseFrontmatter(content []byte) (commandMeta, string, error) {
	text := strings.TrimPrefix(string(content), "\uFEFF")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return commandMeta{}, "", errors.New("missing YAML frontmatter")
	}

	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return commandMeta{}, "", errors.New("missing closing frontmatter separator")
	}

	frontmatter := strings.Join(lines[1:end], "\n")
	var meta commandMeta
	if err := yaml.Unmarshal([]byte(frontmatter), &meta); err != nil {
		return commandMeta{}, "", fmt.Errorf("invalid YAML: %w", err)
	}

	body := ""
	if end+1 < len(lines) {
		body = strings.Join(lines[end+1:], "\n")
	}

	return meta, body, nil
}
