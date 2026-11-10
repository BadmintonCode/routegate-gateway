// demo-caller is a workload identity in action: it holds a client certificate
// registered with RouteGate and periodically calls two upstream services
// THROUGH the gateway, including one operation its policy forbids. Its log is
// the east-west verification surface: allow -> 200, deny -> 403.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

type call struct {
	desc   string
	method string
	path   string
}

func main() {
	gateway := envOr("GATEWAY_URL", "https://routegate:8080")
	certFile := envOr("CLIENT_CERT", "/certs/caller.crt")
	keyFile := envOr("CLIENT_KEY", "/certs/caller.key")
	caFile := envOr("GATEWAY_CA", "/certs/gateway.crt")
	interval := envOr("INTERVAL", "3s")

	client, err := newClient(certFile, keyFile, caFile)
	if err != nil {
		log.Fatalf("caller: %v", err)
	}

	calls := []call{
		{"allowed: GET /alpha/v1/ping", http.MethodGet, "/alpha/v1/ping"},
		{"allowed: GET /beta/v1/ping", http.MethodGet, "/beta/v1/ping"},
		{"denied:  GET /alpha/admin/secret", http.MethodGet, "/alpha/admin/secret"},
	}

	d, err := time.ParseDuration(interval)
	if err != nil {
		log.Fatalf("caller: bad INTERVAL %q", interval)
	}
	log.Printf("caller: calling %s every %s", gateway, d)
	for tick := 0; ; tick++ {
		c := calls[tick%len(calls)]
		do(client, gateway, c)
		time.Sleep(d)
	}
}

func do(client *http.Client, gateway string, c call) {
	req, err := http.NewRequest(c.method, gateway+c.path, nil)
	if err != nil {
		log.Printf("caller: %v", err)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("%s -> ERROR %v", c.desc, err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	log.Printf("%s -> HTTP %d %s", c.desc, resp.StatusCode, firstLine(body))
}

// newClient builds an mTLS HTTP client: own client certificate, trusting only
// the gateway's certificate as CA (hub-and-spoke: one trust relationship).
func newClient(certFile, keyFile, caFile string) (*http.Client, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read gateway ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("gateway ca: no certificates in %s", caFile)
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{cert},
				RootCAs:      pool,
				MinVersion:   tls.VersionTLS12,
			},
			MaxIdleConnsPerHost: 8,
		},
	}, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func firstLine(b []byte) string {
	for i, c := range b {
		if c == '\n' {
			return string(b[:i])
		}
	}
	return string(b)
}
