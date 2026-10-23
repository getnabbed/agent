// Package tlsconf builds the TLS configuration shared by the agent's HTTPS
// and gRPC clients.
package tlsconf

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
)

// Options mirror the tls block of the local policy.
type Options struct {
	// CAFile adds PEM roots for self-hosted control planes. System roots
	// remain trusted.
	CAFile string
	// SPKIPins, if set, require some certificate in the verified chain to
	// have a SubjectPublicKeyInfo whose SHA-256, base64-encoded, is listed.
	SPKIPins []string
}

// New returns a client TLS config. Verification is never disabled.
func New(o Options) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.CAFile != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read ca_file: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %s contains no PEM certificates", o.CAFile)
		}
		cfg.RootCAs = pool
	}
	if len(o.SPKIPins) > 0 {
		pins := make(map[string]bool, len(o.SPKIPins))
		for _, p := range o.SPKIPins {
			pins[p] = true
		}
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			for _, chain := range cs.VerifiedChains {
				for _, cert := range chain {
					if pins[SPKIHash(cert)] {
						return nil
					}
				}
			}
			return errors.New("tls: no certificate in the chain matches a configured SPKI pin")
		}
	}
	return cfg, nil
}

// SPKIHash returns base64(sha256(cert.RawSubjectPublicKeyInfo)).
func SPKIHash(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:])
}
