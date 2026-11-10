// Package identity resolves the verified caller behind a data-plane request.
//
// M1 implements the pinned-certificate flow (ADR-0001 §6.3.2 source A): the
// TLS handshake must present a client certificate; the SHA-256 fingerprint of
// the leaf must match an active mtls_cert identity in the current snapshot;
// the validity window is checked per request (a certificate can expire while a
// snapshot is being served); and URI SANs of the form
// https://routegate.local/identities/<name> must agree with the identity name
// so a valid certificate cannot impersonate another caller.
package identity

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/BadmintonCode/routegate-gateway/internal/snapshot"
)

// ErrUnauthenticated is returned when no verified caller can be resolved.
var ErrUnauthenticated = errors.New("identity: no verified caller")

// SANPrefix is the URI SAN scheme bound to identity names.
const SANPrefix = "https://routegate.local/identities/"

// Fingerprint returns the hex SHA-256 of a certificate's DER encoding.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// Resolve maps a TLS connection state to a caller name using the snapshot.
func Resolve(tlsState *tls.ConnectionState, snap *snapshot.Snapshot, now time.Time) (string, error) {
	if tlsState == nil || len(tlsState.PeerCertificates) == 0 {
		return "", ErrUnauthenticated
	}
	leaf := tlsState.PeerCertificates[0]
	if snap == nil {
		return "", ErrUnauthenticated
	}
	id, ok := snap.Identities[Fingerprint(leaf)]
	if !ok {
		return "", ErrUnauthenticated
	}
	if !id.NotBefore.IsZero() && now.Before(id.NotBefore) {
		return "", fmt.Errorf("%w: certificate not yet valid", ErrUnauthenticated)
	}
	if !id.NotAfter.IsZero() && now.After(id.NotAfter) {
		return "", fmt.Errorf("%w: certificate expired", ErrUnauthenticated)
	}
	for _, san := range leaf.URIs {
		if san.Scheme == "https" && san.Host == "routegate.local" && len(san.Path) > 0 &&
			len(san.Path) > len("/identities/") && san.Path[:len("/identities/")] == "/identities/" {
			if san.Path[len("/identities/"):] != id.Name {
				return "", fmt.Errorf("%w: SAN identity %q does not match registered name %q",
					ErrUnauthenticated, san.Path[len("/identities/"):], id.Name)
			}
		}
	}
	return id.Name, nil
}
