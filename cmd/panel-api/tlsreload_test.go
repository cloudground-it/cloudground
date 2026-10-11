package main

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

func writePair(t *testing.T, dir, cn string, mod time.Time) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	cert, k := filepath.Join(dir, "panel.pem"), filepath.Join(dir, "panel.key")
	os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o640)
	os.WriteFile(k, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o640)
	os.Chtimes(cert, mod, mod)
	os.Chtimes(k, mod, mod)
}

func servedCN(t *testing.T, c *certFiles) string {
	t.Helper()
	pair, err := c.get(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.Subject.CommonName
}

// A certificate installed while the API runs is served without a restart;
// a half-written pair does not take the old one away.
func TestCertFilesReloadOnChange(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)
	writePair(t, dir, "self-signed", base)
	c := &certFiles{cert: filepath.Join(dir, "panel.pem"), key: filepath.Join(dir, "panel.key")}
	if cn := servedCN(t, c); cn != "self-signed" {
		t.Fatalf("served %q", cn)
	}
	writePair(t, dir, "panel.example.com", base.Add(time.Minute))
	if cn := servedCN(t, c); cn != "panel.example.com" {
		t.Fatalf("after replacement served %q", cn)
	}
	os.WriteFile(c.key, []byte("garbage"), 0o640)
	os.Chtimes(c.key, base.Add(2*time.Minute), base.Add(2*time.Minute))
	if cn := servedCN(t, c); cn != "panel.example.com" {
		t.Fatalf("a broken key replaced the good pair: %q", cn)
	}
}
