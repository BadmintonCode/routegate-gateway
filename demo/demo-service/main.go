// demo-service is a minimal upstream service for the RouteGate quickstart.
// It echoes who the gateway said the caller is, so the demo makes the
// identity propagation visible.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	name := flag.String("name", "demo", "service name")
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"service":   *name,
			"path":      r.URL.Path,
			"method":    r.Method,
			"caller":    r.Header.Get("X-Gateway-Caller"),
			"requestId": r.Header.Get("X-Gateway-Request-Id"),
			"user":      r.Header.Get("X-Gateway-User-Id"),
			"time":      time.Now().UTC().Format(time.RFC3339),
		})
	})

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("demo service %q listening on %s", *name, *addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
	_ = os.Stdout.Sync()
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
