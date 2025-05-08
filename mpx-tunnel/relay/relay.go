package relay

import (
	"io"
	"net"
)

// Relay proxies data bidirectionally between left and right, closing the opposite connection to unblock Copy.
func Relay(left, right net.Conn) (int64, int64, error) {
	type res struct {
		N   int64
		Err error
	}
	ch := make(chan res, 1)

	go func() {
		n, err := io.Copy(right, left)
		right.Close()
		ch <- res{N: n, Err: err}
	}()

	n, err := io.Copy(left, right)
	left.Close()
	rs := <-ch

	if err == nil {
		err = rs.Err
	}
	return n, rs.N, err
} 