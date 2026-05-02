package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	RunTraceModeAuto      = "auto"
	RunTraceModeExpanded  = "expanded"
	RunTraceModeCollapsed = "collapsed"
	RunTraceModeHidden    = "hidden"
)

type WebClientDisplaySettings struct {
	RunTraceMode string `json:"run_trace_mode"`
}

type WebClientSettings struct {
	WebClientDisplay WebClientDisplaySettings `json:"webclient_display"`
	UpdatedAt        time.Time                `json:"updated_at"`
}

func normalizeRunTraceMode(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case RunTraceModeAuto:
		return RunTraceModeAuto, true
	case RunTraceModeExpanded:
		return RunTraceModeExpanded, true
	case RunTraceModeCollapsed:
		return RunTraceModeCollapsed, true
	case RunTraceModeHidden:
		return RunTraceModeHidden, true
	default:
		return "", false
	}
}

func defaultWebClientSettings() WebClientSettings {
	return WebClientSettings{
		WebClientDisplay: WebClientDisplaySettings{RunTraceMode: RunTraceModeAuto},
		UpdatedAt:        time.Now().UTC(),
	}
}

func (s *Store) GetWebClientSettings() (WebClientSettings, error) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	b, err := os.ReadFile(s.settingsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return defaultWebClientSettings(), nil
		}
		return WebClientSettings{}, fmt.Errorf("read settings: %w", err)
	}
	var out WebClientSettings
	if err := json.Unmarshal(b, &out); err != nil {
		// Tolerate corrupt settings by falling back to defaults.
		return defaultWebClientSettings(), nil
	}
	if mode, ok := normalizeRunTraceMode(out.WebClientDisplay.RunTraceMode); ok {
		out.WebClientDisplay.RunTraceMode = mode
	} else {
		out.WebClientDisplay.RunTraceMode = RunTraceModeAuto
	}
	if out.UpdatedAt.IsZero() {
		out.UpdatedAt = time.Now().UTC()
	}
	return out, nil
}

func (s *Store) SetRunTraceMode(mode string) (WebClientSettings, error) {
	m, ok := normalizeRunTraceMode(mode)
	if !ok {
		return WebClientSettings{}, fmt.Errorf("invalid run_trace_mode %q", strings.TrimSpace(mode))
	}

	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	var cur WebClientSettings
	b, err := os.ReadFile(s.settingsPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return WebClientSettings{}, fmt.Errorf("read settings: %w", err)
		}
		cur = defaultWebClientSettings()
	} else if err := json.Unmarshal(b, &cur); err != nil {
		cur = defaultWebClientSettings()
	}

	cur.WebClientDisplay.RunTraceMode = m
	cur.UpdatedAt = time.Now().UTC()
	if err := atomicWriteJSON(s.settingsPath, cur); err != nil {
		return WebClientSettings{}, fmt.Errorf("write settings: %w", err)
	}
	return cur, nil
}
