//go:build panel

package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Panel serves the embedded admin SPA at /admin/. Non-asset paths fall back
// to index.html (client-side routing). Asset requests are stripped of the
// /admin prefix before the file server: the SPA mounts at /admin/ but the
// embedded dist root holds assets/ directly.
func Panel() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("web: embedded dist missing: " + err.Error())
	}
	fileServer := http.StripPrefix("/admin", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/admin/")
		if p == "" || p == "/" || !strings.Contains(p, ".") {
			index, rerr := fs.ReadFile(sub, "index.html")
			if rerr != nil {
				http.Error(w, "panel bundle incomplete", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(index)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}
