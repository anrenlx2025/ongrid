package tunnel

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
	"testing"
	"time"
)

// writeTestCA generates a throwaway self-signed certificate and writes it
// as a PEM CA file. Only used to get buildDialer past the CA parsing step
// so the ServerName resolution logic can be exercised.
func writeTestCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "tunnel-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write ca pem: %v", err)
	}
	return path
}

// TestBuildDialerFailsClosedOnIPLiteral verifies the fail-closed behavior:
// with a CA configured, an IP-literal address and no TLSServerName
// override, buildDialer refuses to build a dialer instead of letting the
// TLS handshake fail later with a cryptic certificate error.
func TestBuildDialerFailsClosedOnIPLiteral(t *testing.T) {
	caFile := writeTestCA(t)
	c := NewClient(ClientConfig{
		ServerAddr: "192.0.2.10:40011",
		TLSCAFile:  caFile,
	})
	gc := c.(*geminioClient)

	_, err := gc.buildDialer()
	if err == nil {
		t.Fatal("buildDialer succeeded with IP-literal addr and no TLSServerName; want fail-closed error")
	}
}

// TestBuildDialerAcceptsIPLiteralWithOverride verifies the escape hatch:
// an explicit TLSServerName resolves the ambiguity and the dialer builds.
func TestBuildDialerAcceptsIPLiteralWithOverride(t *testing.T) {
	caFile := writeTestCA(t)
	c := NewClient(ClientConfig{
		ServerAddr:    "192.0.2.10:40011",
		TLSCAFile:     caFile,
		TLSServerName: "broker.example.com",
	})
	gc := c.(*geminioClient)

	dialer, err := gc.buildDialer()
	if err != nil {
		t.Fatalf("buildDialer with TLSServerName override: %v", err)
	}
	if dialer == nil {
		t.Fatal("buildDialer returned nil dialer without error")
	}
}

// TestBuildDialerFailsClosedWhenTLSRequiredNoCA verifies the TLS anchor:
// when the edge was installed with TLS (TLSRequired=1) but the CA env var
// is now missing, buildDialer must refuse to fall back to plaintext —
// the credential channel must never downgrade to cleartext unnoticed.
func TestBuildDialerFailsClosedWhenTLSRequiredNoCA(t *testing.T) {
	c := NewClient(ClientConfig{
		ServerAddr:  "broker.example.com:40011",
		TLSRequired: true,
	})
	gc := c.(*geminioClient)

	_, err := gc.buildDialer()
	if err == nil {
		t.Fatal("buildDialer succeeded with TLSRequired and no CA file; want fail-closed error")
	}
}

// TestBuildDialerAllowsPlaintextWithoutTLSRequired verifies the legacy
// behavior is preserved: without the TLS anchor an empty CA config still
// builds a plaintext dialer (install-time opt-in only).
func TestBuildDialerAllowsPlaintextWithoutTLSRequired(t *testing.T) {
	c := NewClient(ClientConfig{
		ServerAddr: "broker.example.com:40011",
	})
	gc := c.(*geminioClient)

	dialer, err := gc.buildDialer()
	if err != nil {
		t.Fatalf("buildDialer without TLS anchor: %v", err)
	}
	if dialer == nil {
		t.Fatal("buildDialer returned nil dialer without error")
	}
}
