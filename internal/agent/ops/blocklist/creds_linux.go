package blocklist

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// credSpace is the room a datagram's credentials take.
var credSpace = unix.CmsgSpace(unix.SizeofUcred)

// passCredentials asks the kernel to attach each datagram's sender.
func passCredentials(c *net.UnixConn) error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_PASSCRED, 1)
	}); err != nil {
		return err
	}
	return serr
}

// sentBy reports whether a datagram's credentials name root or uid.
func sentBy(oob []byte, uid int) bool {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return false
	}
	for i := range msgs {
		cred, err := unix.ParseUnixCredentials(&msgs[i])
		if err != nil {
			continue
		}
		return cred.Uid == 0 || int(cred.Uid) == uid
	}
	return false
}
