//go:build !linux && !darwin

package agentrpc

import (
	"errors"
	"net"
)

// kernelPeerUID has no implementation here: every connection is refused.
func kernelPeerUID(net.Conn) (uint32, error) {
	return 0, errors.New("peer credentials are not supported on this platform")
}
