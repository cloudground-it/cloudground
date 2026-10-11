package install

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"time"
)

const tokenAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// newToken returns n characters drawn uniformly from [A-Za-z0-9].
func newToken(n int) (string, error) {
	out := make([]byte, 0, n)
	var buf [64]byte
	for len(out) < n {
		if _, err := rand.Read(buf[:]); err != nil {
			return "", err
		}
		for _, c := range buf {
			// 248 = 4*62: reject the tail so every letter is equally likely.
			if c < 248 && len(out) < n {
				out = append(out, tokenAlphabet[int(c)%len(tokenAlphabet)])
			}
		}
	}
	return string(out), nil
}

// certDays is the bootstrap certificates' lifetime.
const certDays = 3650

// selfSigned returns a self-signed certificate and its key as PEM: RSA 2048,
// SHA-256, valid for certDays from now, subject CN=cn, key in PKCS#8 — what
// `openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -subj /CN=<cn>` made.
func selfSigned(cn string, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(certDays * 24 * time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}), nil
}
