package adminapi

import (
	"net/http"
)

// handleAIPrompt serves the paste-ready AI bootstrap prompt as text/markdown.
// The content is the embedded skill/AI-PROMPT.md (single source shared with
// the meta/skill/* self-hosted bundle; skills/routegate-admin/AI-PROMPT.md is
// the canonical file, enforced by the drift-guard tests). PUBLIC like the
// rest of the meta documentation: static content, no state, no secrets — a
// human (or their agent) copies it into an AI session to bootstrap the skill.
func (s *Server) handleAIPrompt(w http.ResponseWriter, r *http.Request) {
	b, err := skillFiles.ReadFile("skill/AI-PROMPT.md")
	if err != nil {
		errorJSON(r, w, http.StatusNotFound, "admin.not_found", "the requested admin resource does not exist")
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
