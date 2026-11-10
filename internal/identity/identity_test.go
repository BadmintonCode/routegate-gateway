package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
)

func selfSigned(t *testing.T, cn string, notBefore, notAfter time.Time, uris []*url.URL) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		URIs:         uris,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func stateWith(cert *x509.Certificate) *tls.ConnectionState {
	return &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
}

func TestResolveByFingerprint(t *testing.T) {
	now := time.Now()
	cert := selfSigned(t, "service-a", now.Add(-time.Hour), now.Add(time.Hour), nil)
	snap := &snapshot.Snapshot{Identities: map[string]*snapshot.Identity{
		Fingerprint(cert): {Name: "service-a", Cert: cert, NotBefore: cert.NotBefore, NotAfter: cert.NotAfter},
	}}
	got, err := Resolve(stateWith(cert), snap, now)
	if err != nil || got != "service-a" {
		t.Fatalf("Resolve = %q, %v; want service-a, nil", got, err)
	}
}

func TestResolveUnknownFingerprint(t *testing.T) {
	now := time.Now()
	cert := selfSigned(t, "service-x", now.Add(-time.Hour), now.Add(time.Hour), nil)
	got, err := Resolve(stateWith(cert), &snapshot.Snapshot{Identities: map[string]*snapshot.Identity{}}, now)
	if got != "" || err == nil {
		t.Fatalf("unknown fingerprint: got %q, %v; want error", got, err)
	}
}

func TestResolveExpiredCertificate(t *testing.T) {
	now := time.Now()
	cert := selfSigned(t, "service-a", now.Add(-2*time.Hour), now.Add(-time.Hour), nil)
	snap := &snapshot.Snapshot{Identities: map[string]*snapshot.Identity{
		Fingerprint(cert): {Name: "service-a", Cert: cert, NotBefore: cert.NotBefore, NotAfter: cert.NotAfter},
	}}
	if _, err := Resolve(stateWith(cert), snap, now); err == nil {
		t.Fatal("expired certificate must be rejected")
	}
}

func TestResolveSANMismatch(t *testing.T) {
	now := time.Now()
	uri, _ := url.Parse(SANPrefix + "service-b")
	cert := selfSigned(t, "service-a", now.Add(-time.Hour), now.Add(time.Hour), []*url.URL{uri})
	snap := &snapshot.Snapshot{Identities: map[string]*snapshot.Identity{
		Fingerprint(cert): {Name: "service-a", Cert: cert},
	}}
	// A cert carrying URI SAN for service-b pinned under name service-a must
	// be rejected: a valid certificate cannot impersonate another caller.
	badCert := cert
	// simulate mismatch by registering the cert under a different name
	snap.Identities[Fingerprint(badCert)] = &snapshot.Identity{Name: "service-zzz", Cert: badCert}
	if _, err := Resolve(stateWith(badCert), snap, now); err == nil {
		t.Fatal("SAN mismatch must be rejected")
	}
}

func TestResolveSANMatch(t *testing.T) {
	now := time.Now()
	uri, _ := url.Parse(SANPrefix + "service-a")
	cert := selfSigned(t, "service-a", now.Add(-time.Hour), now.Add(time.Hour), []*url.URL{uri})
	snap := &snapshot.Snapshot{Identities: map[string]*snapshot.Identity{
		Fingerprint(cert): {Name: "service-a", Cert: cert},
	}}
	if got, err := Resolve(stateWith(cert), snap, now); err != nil || got != "service-a" {
		t.Fatalf("matching SAN rejected: %q, %v", got, err)
	}
}
