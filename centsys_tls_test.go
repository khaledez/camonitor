package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

// makeCA returns a self-signed CA cert + its key.
func makeCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test-CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CA: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	return cert, key
}

// legacyLeaf signs a leaf that, like the real broker's, carries only a
// Common Name and no SAN — the shape Go's default verifier rejects.
func legacyLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey) []byte {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: centsysTLSServerName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		// Deliberately no DNSNames / IPAddresses (no SAN).
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("leaf: %v", err)
	}
	return der
}

func TestVerifyBrokerCert(t *testing.T) {
	ca, caKey := makeCA(t)
	leaf := legacyLeaf(t, ca, caKey)

	// Pinned to the right CA: a CN-only, SAN-less leaf is accepted.
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	c := &CentsysController{caPool: pool}
	if err := c.verifyBrokerCert([][]byte{leaf}, nil); err != nil {
		t.Errorf("legacy-CN leaf from pinned CA should verify, got: %v", err)
	}

	// Pinned to a different CA: the same leaf is rejected.
	otherCA, _ := makeCA(t)
	otherPool := x509.NewCertPool()
	otherPool.AddCert(otherCA)
	c2 := &CentsysController{caPool: otherPool}
	if err := c2.verifyBrokerCert([][]byte{leaf}, nil); err == nil {
		t.Error("leaf from an untrusted CA must be rejected")
	}

	// No certificate at all is an error.
	if err := c.verifyBrokerCert(nil, nil); err == nil {
		t.Error("empty chain must be rejected")
	}
}
