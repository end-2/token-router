package bucket

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/hashicorp/raft"
)

const distributedPath = "/internal/rate-limit/"

// adminStream carries Raft's native protocol through authenticated HTTP CONNECT
// on the admin listener. Accepted connections belong to NetworkTransport.
type adminStream struct {
	address string
	token   string
	cluster string
	peers   map[string]bool
	accept  chan net.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	conns   map[*adminConn]struct{}
}

func newAdminStream(address, token, cluster string, peers []string) *adminStream {
	ctx, cancel := context.WithCancel(context.Background())
	s := &adminStream{address: address, token: token, cluster: cluster,
		peers: make(map[string]bool, len(peers)), accept: make(chan net.Conn),
		ctx: ctx, cancel: cancel, conns: make(map[*adminConn]struct{})}
	for _, peer := range peers {
		s.peers[peer] = true
	}
	return s
}

func (s *adminStream) authorized(r *http.Request) bool {
	want := sha256.Sum256([]byte("Bearer " + s.token))
	got := sha256.Sum256([]byte(r.Header.Get("Authorization")))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1 && r.Header.Get("X-Token-Router-Cluster") == s.cluster
}

func (s *adminStream) headers(r *http.Request) {
	r.Header.Set("Authorization", "Bearer "+s.token)
	r.Header.Set("X-Token-Router-Cluster", s.cluster)
}

func (s *adminStream) Accept() (net.Conn, error) {
	select {
	case conn := <-s.accept:
		return conn, nil
	case <-s.ctx.Done():
		return nil, net.ErrClosed
	}
}

func (s *adminStream) Addr() net.Addr {
	address, _ := net.ResolveTCPAddr("tcp", s.address) // Validated numeric IP and port.
	return address
}

func (s *adminStream) Close() error {
	s.mu.Lock()
	s.cancel()
	connections := make([]*adminConn, 0, len(s.conns))
	for conn := range s.conns {
		connections = append(connections, conn)
	}
	s.mu.Unlock()
	var err error
	for _, conn := range connections {
		err = errors.Join(err, conn.Close())
	}
	return err
}

func (s *adminStream) track(conn net.Conn, reader *bufio.Reader) (net.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return nil, errors.Join(net.ErrClosed, conn.Close())
	}
	c := &adminConn{Conn: conn, reader: reader, stream: s}
	s.conns[c] = struct{}{}
	return c, nil
}

func (s *adminStream) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	if !s.peers[string(address)] {
		return nil, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(s.ctx, timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", string(address))
	if err != nil {
		return nil, ErrUnavailable
	}
	reader := bufio.NewReader(conn)
	tracked, err := s.track(conn, reader)
	if err != nil {
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			// Handshake errors are intentionally sanitized; closing is best effort.
			_ = tracked.Close()
		}
	}()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	// Writing origin-form CONNECT preserves the internal route in Go's server.
	if _, err := fmt.Fprintf(conn, "CONNECT %sraft HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nX-Token-Router-Cluster: %s\r\n\r\n",
		distributedPath, address, s.token, s.cluster); err != nil {
		return nil, ErrUnavailable
	}
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return nil, ErrUnavailable
	}
	if err := response.Body.Close(); err != nil || response.StatusCode != http.StatusOK {
		return nil, ErrUnavailable
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	failed = false
	return tracked, nil
}

func (s *adminStream) serveConnect(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, buffer, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "connection unavailable", http.StatusServiceUnavailable)
		return
	}
	tracked, err := s.track(conn, buffer.Reader)
	if err != nil {
		return
	}
	handedOff := false
	defer func() {
		if !handedOff {
			// The HTTP connection has already been hijacked, so no error can be sent.
			_ = tracked.Close()
		}
	}()
	if _, err := io.WriteString(buffer, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"); err != nil {
		return
	}
	if err := buffer.Flush(); err != nil {
		return
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return
	}
	select {
	case s.accept <- tracked:
		handedOff = true
	case <-s.ctx.Done():
	}
}

type adminConn struct {
	net.Conn
	reader *bufio.Reader
	stream *adminStream
	once   sync.Once
	err    error
}

func (c *adminConn) Read(data []byte) (int, error) { return c.reader.Read(data) }

func (c *adminConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.stream.mu.Lock()
		delete(c.stream.conns, c)
		c.stream.mu.Unlock()
	})
	return c.err
}
