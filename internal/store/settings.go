package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
)

// DebugSettings is the panel-managed debug-logging configuration, persisted in
// settings.debug_log (JSON) and applied by every instance on its next sync
// tick (ADR-0001 §6.6). There are no debug environment variables: the settings
// store is the single source of truth.
type DebugSettings struct {
	Enabled              bool   `json:"enabled"`
	File                 string `json:"file"`
	MaxMB                int64  `json:"max_mb"`
	RetainDays           int    `json:"retain_days"`
	RequestBodyMaxBytes  int    `json:"request_body_max_bytes"`
	ResponseBodyMaxBytes int    `json:"response_body_max_bytes"`
	HeadersMaxBytes      int    `json:"headers_max_bytes"`
	QueueCapacity        int    `json:"queue_capacity"`
}

const debugSettingsName = "debug_log"

// DefaultDebugSettings mirrors the documented defaults: disabled, and the same
// rotation/capture caps the env configuration used previously.
func DefaultDebugSettings() DebugSettings {
	return DebugSettings{
		Enabled:              false,
		File:                 "",
		MaxMB:                2,
		RetainDays:           7,
		RequestBodyMaxBytes:  64 * 1024,
		ResponseBodyMaxBytes: 64 * 1024,
		HeadersMaxBytes:      32 * 1024,
		QueueCapacity:        1024,
	}
}

// Validate checks the values the admin plane accepts. Enabled debug logging
// requires a dedicated file path; caps must be positive.
func (s DebugSettings) Validate() error {
	if !s.Enabled {
		return nil
	}
	if s.File == "" {
		return errDebugFileRequired
	}
	if s.MaxMB < 1 || s.RetainDays < 1 || s.RequestBodyMaxBytes < 1 ||
		s.ResponseBodyMaxBytes < 1 || s.HeadersMaxBytes < 1 || s.QueueCapacity < 1 {
		return errDebugCapInvalid
	}
	return nil
}

// LoadDebugSettingsSync reads settings.debug_log for the sync tick.
//
// Fail-static contract: a missing row yields the defaults (nil error); an
// unparsable row yields sanitized defaults (disabled) with a log line (nil
// error — a broken settings row must never crash the sync loop); a transient
// read error yields the defaults AND the error, so the caller can keep its
// previous runtime state instead of silently disabling capture.
func LoadDebugSettingsSync(ctx context.Context, q querier) (DebugSettings, error) {
	var raw sql.NullString
	err := q.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE name = ?`, debugSettingsName).Scan(&raw)
	if err == sql.ErrNoRows {
		return DefaultDebugSettings(), nil
	}
	if err != nil {
		return DefaultDebugSettings(), err
	}
	var s DebugSettings
	if json.Unmarshal([]byte(raw.String), &s) != nil {
		log.Printf("sync: settings.debug_log unparsable; falling back to defaults (debug disabled)")
		return DefaultDebugSettings(), nil
	}
	return sanitizeDebugSettings(s), nil
}

// SaveDebugSettings upserts the row inside the caller's transaction (the
// admin write transaction also bumps config_revision and writes the audit).
func SaveDebugSettings(ctx context.Context, tx *sql.Tx, s DebugSettings) error {
	b, err := json.Marshal(sanitizeDebugSettings(s))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO settings (name, value) VALUES (?, CAST(? AS JSON))
		 ON DUPLICATE KEY UPDATE value = CAST(? AS JSON)`,
		debugSettingsName, string(b), string(b))
	return err
}

// sanitizeDebugSettings clamps nonsensical values to the documented defaults
// so a hand-edited row can never produce a zero-cap or runaway sink.
func sanitizeDebugSettings(s DebugSettings) DebugSettings {
	d := DefaultDebugSettings()
	if s.MaxMB < 1 {
		s.MaxMB = d.MaxMB
	}
	if s.RetainDays < 1 {
		s.RetainDays = d.RetainDays
	}
	if s.RequestBodyMaxBytes < 1 {
		s.RequestBodyMaxBytes = d.RequestBodyMaxBytes
	}
	if s.ResponseBodyMaxBytes < 1 {
		s.ResponseBodyMaxBytes = d.ResponseBodyMaxBytes
	}
	if s.HeadersMaxBytes < 1 {
		s.HeadersMaxBytes = d.HeadersMaxBytes
	}
	if s.QueueCapacity < 1 {
		s.QueueCapacity = d.QueueCapacity
	}
	if !s.Enabled {
		return s
	}
	if s.File == "" {
		// Enabled without a path cannot capture; disable instead of guessing.
		log.Printf("sync: settings.debug_log enabled without a file path; treating as disabled")
		s.Enabled = false
	}
	return s
}

type debugSettingsError string

func (e debugSettingsError) Error() string { return string(e) }

const (
	errDebugFileRequired = debugSettingsError("enabled debug logging requires a file path")
	errDebugCapInvalid   = debugSettingsError("all debug caps must be positive integers")
)
