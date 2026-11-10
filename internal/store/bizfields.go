package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"strings"
	"sync/atomic"
)

// BizFields is the global business-envelope fallback (ADR-0001 §6.7): dotted
// response-body paths used for a route when the route itself does not define
// its own biz_code_path / biz_message_path. Stored in settings.biz_fields.
type BizFields struct {
	CodePath    string `json:"code_path"`
	MessagePath string `json:"message_path"`
}

const bizFieldsName = "biz_fields"

// BizFieldsHolder carries the runtime-global fields between the sync worker
// (writer) and the data plane (reader), mirroring the debug-settings flow.
type BizFieldsHolder struct {
	p atomic.Pointer[BizFields]
}

// NewBizFieldsHolder starts with the empty fallback.
func NewBizFieldsHolder() *BizFieldsHolder { return &BizFieldsHolder{} }

func (h *BizFieldsHolder) Get() BizFields {
	if f := h.p.Load(); f != nil {
		return *f
	}
	return BizFields{}
}

func (h *BizFieldsHolder) Set(f BizFields) { h.p.Store(&f) }

// LoadBizFieldsSync reads settings.biz_fields for the sync tick.
//
// Fail-static contract: a missing row yields the empty fallback (nil error);
// an unparsable row yields the empty fallback with a log line (nil error); a
// transient read error yields the empty fallback AND the error, so the caller
// can keep its previous runtime state instead of silently wiping the global
// envelope fallback.
func LoadBizFieldsSync(ctx context.Context, q querier) (BizFields, error) {
	var raw sql.NullString
	err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE name = ?`, bizFieldsName).Scan(&raw)
	if err == sql.ErrNoRows {
		return BizFields{}, nil
	}
	if err != nil {
		return BizFields{}, err
	}
	var f BizFields
	if json.Unmarshal([]byte(raw.String), &f) != nil {
		log.Printf("sync: settings.biz_fields unparsable; global envelope extraction disabled")
		return BizFields{}, nil
	}
	return sanitizeBizFields(f), nil
}

// SaveBizFields upserts the row inside the caller's transaction.
func SaveBizFields(ctx context.Context, tx *sql.Tx, f BizFields) error {
	b, err := json.Marshal(sanitizeBizFields(f))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO settings (name, value) VALUES (?, CAST(? AS JSON))
		 ON DUPLICATE KEY UPDATE value = CAST(? AS JSON)`,
		bizFieldsName, string(b), string(b))
	return err
}

// sanitizeBizFields drops paths that do not satisfy the dotted-path grammar
// so a hand-edited row can never poison the extraction.
func sanitizeBizFields(f BizFields) BizFields {
	f.CodePath = normalizeBizPath(f.CodePath)
	f.MessagePath = normalizeBizPath(f.MessagePath)
	return f
}

func normalizeBizPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || validateBodyPath(p) != nil {
		if p != "" {
			log.Printf("sync: settings.biz_fields path %q invalid; dropped", p)
		}
		return ""
	}
	return p
}
