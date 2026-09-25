package store

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func writeTestCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// With a CA configured, a server that cannot do verified TLS must be refused, never
// reached over the plaintext fallback that sslmode=prefer (the default) would add.
func TestOpenWithCANeverFallsBackToPlaintext(t *testing.T) {
	defer func(n int) { connectAttemptsMax = n }(connectAttemptsMax)
	connectAttemptsMax = 1
	u := strings.Replace(newDatabase(t), "sslmode=disable", "sslmode=prefer", 1) // no TLS on the server
	s, err := Open(t.Context(), Options{URL: u, CAFile: writeTestCA(t),
		Pub: &recordingPublisher{}, Log: zerolog.Nop()})
	if err == nil {
		s.Close()
		t.Fatal("connected without verified TLS")
	}
}
