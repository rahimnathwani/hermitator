package egress

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"syscall"
	"unsafe"
)

const soOriginalDst = 80 // SO_ORIGINAL_DST, from linux/netfilter_ipv4.h

// originalDst is where a connection redirected by nftables was going.
func originalDst(c net.Conn) (string, error) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return "", errors.New("not TCP")
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return "", err
	}
	var addr syscall.RawSockaddrInet4
	var sockErr error
	err = raw.Control(func(fd uintptr) {
		size := uint32(unsafe.Sizeof(addr))
		_, _, errno := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, syscall.SOL_IP, soOriginalDst,
			uintptr(unsafe.Pointer(&addr)), uintptr(unsafe.Pointer(&size)), 0)
		if errno != 0 {
			sockErr = errno
		}
	})
	if err != nil {
		return "", err
	}
	if sockErr != nil {
		return "", sockErr
	}
	port := binary.BigEndian.Uint16((*[2]byte)(unsafe.Pointer(&addr.Port))[:])
	return netip.AddrPortFrom(netip.AddrFrom4(addr.Addr), port).String(), nil
}

// ServeTransparent serves connections nftables redirected to l, sending
// each on to where it was going.
func (p *Proxy) ServeTransparent(ctx context.Context, l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go func() {
			dst, err := originalDst(conn)
			if err != nil {
				conn.Close()
				return
			}
			p.Handle(ctx, conn, dst)
		}()
	}
}
