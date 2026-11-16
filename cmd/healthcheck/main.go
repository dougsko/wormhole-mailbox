// Command healthcheck reports whether the mailbox server answers on its
// TCP port. It exists so the container HEALTHCHECK needs no shell
// utilities (and no cgo, no QEMU, any architecture).
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

func main() {
	addr := "127.0.0.1:4000"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		os.Exit(1)
	}
	_, _ = io.WriteString(conn, "GET / HTTP/1.0\r\n\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	conn.Close()
	if err != nil || n == 0 {
		fmt.Fprintln(os.Stderr, "healthcheck: no response")
		os.Exit(1)
	}
}
