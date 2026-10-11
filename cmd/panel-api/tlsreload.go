package main

import (
	"crypto/tls"
	"os"
	"sync"
	"time"
)

// certFiles serves the panel's TLS certificate from its files and loads them
// again when they change. The agent replaces them when it installs a panel
// certificate. The API used to be restarted for that, which dropped every
// open request and event stream. Only the listener without socket activation
// uses TLS: behind nginx, the API speaks plain HTTP on its socket.
type certFiles struct {
	cert, key string

	mu   sync.Mutex
	mod  [2]time.Time
	pair *tls.Certificate
}

// get is a tls.Config GetCertificate. It keeps serving the last good pair if
// the files are briefly unreadable or do not match.
func (c *certFiles) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var mod [2]time.Time
	for i, p := range []string{c.cert, c.key} {
		if fi, err := os.Stat(p); err == nil {
			mod[i] = fi.ModTime()
		}
	}
	if c.pair != nil && mod == c.mod {
		return c.pair, nil
	}
	pair, err := tls.LoadX509KeyPair(c.cert, c.key)
	if err != nil {
		if c.pair != nil {
			return c.pair, nil
		}
		return nil, err
	}
	c.pair, c.mod = &pair, mod
	return c.pair, nil
}

// tlsConfig is the API's TLS configuration over certFiles.
func tlsConfig(cert, key string) *tls.Config {
	c := &certFiles{cert: cert, key: key}
	return &tls.Config{GetCertificate: c.get, MinVersion: tls.VersionTLS12}
}
