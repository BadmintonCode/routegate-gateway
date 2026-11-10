//go:build !panel

// Package web mounts the admin panel SPA. The real panel is built with npm
// (web/admin) and embedded under the `panel` build tag; without the tag this
// placeholder keeps the binary buildable without a Node toolchain.
package web

import "net/http"

// Panel serves the admin SPA at /admin/.
func Panel() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<!doctype html><title>RouteGate</title>
<p>The admin panel bundle is not built into this binary. Build it with
<code>scripts/build-panel.sh</code> and rebuild with <code>-tags panel</code>,
or use the Admin API directly under <code>/admin/api/v1/</code>.</p>`))
	})
}
