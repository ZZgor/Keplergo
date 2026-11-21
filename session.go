package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	methodOffsets   = "oc"
	methodAppClocks = "ac"
)

// Session describes the overclock that is currently armed. It lives in the
// state directory and is the only thing the watchdog and otval share.
type Session struct {
	PID        int       `json:"pid"`
	Method     string    `json:"method"`
	GPU        int       `json:"gpu"`
	Level      int       `json:"level"`
	Gfx        int       `json:"gfx"`
	Mem        int       `json:"mem"`
	MaxTemp    int       `json:"max_temp"`
	Started    time.Time `json:"started"`
	Deadline   time.Time `json:"deadline"` // zero once the user confirmed
	Display    string    `json:"display"`
	XAuthority string    `json:"xauthority"`
}

func (s *Session) confirmed() bool { return s.Deadline.IsZero() }

func (a *app) sessionPath() string { return filepath.Join(a.state, "session.json") }

// loadSession returns nil, nil when there is no session.
func (a *app) loadSession() (*Session, error) {
	data, err := os.ReadFile(a.sessionPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("session file is corrupt: %w", err)
	}
	return &s, nil
}

func (a *app) saveSession(s *Session) error {
	if err := a.ensureState(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(a.sessionPath(), append(data, '\n'), 0o644)
}

func (a *app) clearSession() error {
	err := os.Remove(a.sessionPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// setDeadline moves the confirmation deadline; a zero time marks the session confirmed.
func (a *app) setDeadline(t time.Time) error {
	s, err := a.loadSession()
	if err != nil {
		return err
	}
	if s == nil {
		return errors.New("the watchdog already restored the defaults")
	}
	s.Deadline = t
	return a.saveSession(s)
}
