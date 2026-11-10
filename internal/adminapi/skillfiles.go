package adminapi

import (
	"embed"
	"net/http"
	"strings"
)

// The routegate-admin agent skill (SKILL.md, ARCHITECTURE.md, AI-PROMPT.md)
// ships inside the binary so an AI agent can install the skill from the
// gateway itself — the only prerequisite is the admin host, no repository
// checkout. The canonical sources live at skills/routegate-admin/ in the
// repository; drift-guard tests (TestSkillFilesMatchCanonical) fail the
// build when these embedded copies fall behind them.
//
//go:embed skill/SKILL.md skill/ARCHITECTURE.md skill/AI-PROMPT.md
var skillFiles embed.FS

// skillFileNames is the whitelist of servable skill files (exact names only —
// the handler never treats client input as a path).
var skillFileNames = []string{"SKILL.md", "ARCHITECTURE.md", "AI-PROMPT.md"}

// handleSkillFile serves one whitelisted skill file as text/markdown. PUBLIC
// like the other meta documentation: the skill teaches how to configure
// credentials safely — it contains no secrets, no state, no user data.
func (s *Server) handleSkillFile(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/admin/api/v1/meta/skill/")
	allowed := false
	for _, n := range skillFileNames {
		if name == n {
			allowed = true
			break
		}
	}
	if !allowed {
		errorJSON(r, w, http.StatusNotFound, "admin.not_found", "the requested admin resource does not exist")
		return
	}
	b, err := skillFiles.ReadFile("skill/" + name)
	if err != nil {
		errorJSON(r, w, http.StatusNotFound, "admin.not_found", "the requested admin resource does not exist")
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
