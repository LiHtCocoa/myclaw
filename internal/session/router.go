package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Router struct {
	path     string
	mu       sync.Mutex
	sessions map[string]string
}

func New(path string) (*Router, error) {
	r := &Router{
		path:     strings.TrimSpace(path),
		sessions: map[string]string{},
	}
	if r.path == "" {
		return r, nil
	}

	data, err := os.ReadFile(r.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return r, nil
		}
		return nil, fmt.Errorf("read session router: %w", err)
	}
	if len(data) == 0 {
		return r, nil
	}
	if err := json.Unmarshal(data, &r.sessions); err != nil {
		return nil, fmt.Errorf("decode session router: %w", err)
	}
	return r, nil
}

func (r *Router) Resolve(key, fallback string) string {
	key = strings.TrimSpace(key)
	fallback = strings.TrimSpace(fallback)
	if r == nil {
		if fallback != "" {
			return fallback
		}
		return key
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if sessionID := strings.TrimSpace(r.sessions[key]); sessionID != "" {
		return sessionID
	}
	if fallback != "" {
		return fallback
	}
	return key
}

func (r *Router) Current(key string) string {
	return r.Resolve(key, "")
}

func (r *Router) Rotate(key string) (oldSessionID, newSessionID string, err error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", "", errors.New("session key is empty")
	}
	if r == nil {
		newSessionID = nextSessionID(key)
		return key, newSessionID, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	oldSessionID = strings.TrimSpace(r.sessions[key])
	if oldSessionID == "" {
		oldSessionID = key
	}
	newSessionID = nextSessionID(key)
	r.sessions[key] = newSessionID
	if err := r.persistLocked(); err != nil {
		return "", "", err
	}
	return oldSessionID, newSessionID, nil
}

func (r *Router) Set(key, sessionID string) error {
	key = strings.TrimSpace(key)
	sessionID = strings.TrimSpace(sessionID)
	if key == "" || r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if sessionID == "" || sessionID == key {
		delete(r.sessions, key)
	} else {
		r.sessions[key] = sessionID
	}
	return r.persistLocked()
}

func (r *Router) Reset(key string) error {
	key = strings.TrimSpace(key)
	if key == "" || r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, key)
	return r.persistLocked()
}

func (r *Router) persistLocked() error {
	if r == nil || r.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return fmt.Errorf("mkdir session router dir: %w", err)
	}
	data, err := json.MarshalIndent(r.sessions, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session router: %w", err)
	}
	if err := os.WriteFile(r.path, data, 0o600); err != nil {
		return fmt.Errorf("write session router: %w", err)
	}
	return nil
}

func nextSessionID(base string) string {
	return fmt.Sprintf("%s#%d", base, time.Now().UnixNano())
}
