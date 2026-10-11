//go:build !linux

package blocklist

import (
	"errors"
	"net"
)

// The failure log socket needs the sender's credentials on each datagram,
// which only Linux gives; the agent runs on Linux alone.

var credSpace = 64

func passCredentials(*net.UnixConn) error {
	return errors.New("the failure log socket needs Linux")
}

func sentBy([]byte, int) bool { return false }
