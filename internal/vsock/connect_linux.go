//go:build linux

package vsock

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// ConnectDirect connects via AF_VSOCK (Linux only).
func ConnectDirect(cid uint32, port uint32) (*Client, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}

	addr := &unix.SockaddrVM{CID: cid, Port: port}
	if err := unix.Connect(fd, addr); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock connect cid=%d port=%d: %w", cid, port, err)
	}

	file := os.NewFile(uintptr(fd), "vsock")
	conn, err := net.FileConn(file)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock file conn: %w", err)
	}

	return newClient(conn), nil
}
