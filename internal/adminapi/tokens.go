package adminapi

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BadmintonCode/routegate-gateway/internal/store"
)

// Self-service admin API tokens (migrations/0009): user-scoped temporary
// Bearer credentials for AI and machine clients. A token carries no
// independent permission scope — it authenticates AS its issuing user with
// the role resolved live — so these endpoints are available to every
// authenticated user, not just superadmins. The raw secret crosses the wire
// exactly once in the create response: it is never stored (only its SHA-256
// hash), never logged, and never written to an audit row.

// tokenNameMax is the display-name limit enforced at mint time.
const tokenNameMax = 64

// rawTokenShape documents (and tests pin) the wire form of a minted secret:
// rgt_ + 64 lowercase hex chars.
var rawTokenShape = regexp.MustCompile(`^rgt_[0-9a-f]{64}$`)

// handleListTokens returns the caller's own tokens, including revoked and
// expired rows so the panel can badge them. The stored hash and the raw
// secret are never part of the response.
func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	uid, _ := r.Context().Value(ctxUserID).(int64)
	tokens, err := store.ListTokensForUser(r.Context(), s.db, uid)
	if err != nil {
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	items := make([]map[string]any, 0, len(tokens))
	for _, t := range tokens {
		item := map[string]any{
			"id": t.ID, "name": t.Name, "prefix": t.Prefix,
			"created_at": t.CreatedAt.UTC().Format(time.RFC3339),
			"expires_at": t.ExpiresAt.UTC().Format(time.RFC3339),
		}
		if t.LastUsedAt != nil {
			item["last_used_at"] = t.LastUsedAt.UTC().Format(time.RFC3339)
		}
		if t.RevokedAt != nil {
			item["revoked_at"] = t.RevokedAt.UTC().Format(time.RFC3339)
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleCreateAPIToken mints one token for the calling user. The mandatory
// expiry keeps every token temporary; the response carries the raw secret
// exactly once while the audit after_state records display metadata only.
func (s *Server) handleCreateAPIToken(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	var req struct {
		Name          string `json:"name"`
		ExpiresInDays int    `json:"expires_in_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorJSON(r, w, http.StatusBadRequest, "admin.bad_request", "request body must be valid JSON")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_token", "a token name is required")
		return
	}
	if len(req.Name) > tokenNameMax {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_token", "a token name must be at most 64 characters")
		return
	}
	if req.ExpiresInDays < 1 || req.ExpiresInDays > 365 {
		errorJSON(r, w, http.StatusBadRequest, "admin.invalid_token", "expires_in_days must be between 1 and 365")
		return
	}
	raw, err := store.GenerateAPITokenSecret()
	if err != nil {
		// An RNG failure must fail closed: never mint a predictable token.
		log.Printf("admin api error: %v", err)
		errorJSON(r, w, http.StatusInternalServerError, "admin.error", "internal error")
		return
	}
	prefix := store.APITokenDisplayPrefix(raw)
	hash := store.HashAPIToken(raw)
	expiresAt := time.Now().AddDate(0, 0, req.ExpiresInDays)
	// Audit after_state: metadata only — the raw secret and its hash must
	// never reach the audit row.
	after := map[string]any{
		"name": req.Name, "prefix": prefix,
		"expires_in_days": req.ExpiresInDays,
		"expires_at":      expiresAt.UTC().Format(time.RFC3339),
	}
	var newID int64
	err = s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "token.create", "api_token", req.Name, 0, nil, after,
		func(tx *sql.Tx) error {
			id, err := store.CreateToken(r.Context(), tx, uid, req.Name, expiresAt, hash, prefix)
			newID = id
			return err
		})
	if err != nil {
		writeTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": newID, "token": raw, "name": req.Name, "prefix": prefix,
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
	})
}

// handleRevokeToken revokes one of the caller's own tokens. The owner check
// lives in the SQL (id AND user_id must both match), so a foreign or missing
// id is the uniform 404; already-revoked tokens keep their original
// revocation timestamp.
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(string)
	uid, _ := r.Context().Value(ctxUserID).(int64)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	before := s.ownTokenMeta(r, uid, id)
	err := s.writeTx(r.Context(), clientIP(r), requestID(r), uid, actor, "token.revoke", "api_token", strconv.FormatInt(id, 10), 0, before, nil,
		func(tx *sql.Tx) error {
			return store.RevokeToken(r.Context(), tx, id, uid)
		})
	if err != nil {
		writeTxError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "revoked"})
}

// ownTokenMeta loads the audit-safe metadata (no hash, no secret) of one of
// the caller's tokens for the revoke audit before-state; nil when the caller
// owns no such token (the revoke inside the transaction then fails with the
// same 404 mapping).
func (s *Server) ownTokenMeta(r *http.Request, uid, id int64) map[string]any {
	tokens, err := store.ListTokensForUser(r.Context(), s.db, uid)
	if err != nil {
		return nil
	}
	for _, t := range tokens {
		if t.ID != id {
			continue
		}
		meta := map[string]any{
			"id": t.ID, "name": t.Name, "prefix": t.Prefix,
			"expires_at": t.ExpiresAt.UTC().Format(time.RFC3339),
		}
		if t.RevokedAt != nil {
			meta["revoked_at"] = t.RevokedAt.UTC().Format(time.RFC3339)
		}
		return meta
	}
	return nil
}
