package httpserver

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

type observingListener struct {
	net.Listener
	accepted chan struct{}
}

func (l *observingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepted <- struct{}{}
	}
	return c, err
}

func awaitAccepted(t *testing.T, l *observingListener) {
	t.Helper()
	select {
	case <-l.accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("connection was not accepted")
	}
}

func TestServerConnectionCapAndRelease(t *testing.T) {
	cfg := serverConfig(t)
	cfg.MaxConnections = 1
	s, err := NewServer(cfg, http.HandlerFunc(testHandler))
	if err != nil {
		t.Fatal(err)
	}
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &observingListener{Listener: inner, accepted: make(chan struct{}, 10)}
	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()
	t.Cleanup(func() {
		_ = s.Close()
		select {
		case err := <-done:
			if !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("shutdown did not wake connection admission")
		}
	})
	first := dialServer(t, l.Addr().String())
	awaitAccepted(t, l)
	writeRequest(t, first)
	response := readResponse(t, bufio.NewReader(first))
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	second := dialServer(t, l.Addr().String())
	writeRequest(t, second)
	// The first connection is now an idle keep-alive. A fully written request
	// on the next TCP socket must remain in the OS backlog until it closes.
	select {
	case <-l.accepted:
		t.Fatal("accepted a connection beyond the configured capacity")
	case <-time.After(150 * time.Millisecond):
	}
	_ = first.Close()
	awaitAccepted(t, l)
	response = readResponse(t, bufio.NewReader(second))
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status after capacity release=%d", response.StatusCode)
	}
	// Leave the accepted keep-alive open. Close must wake a Serve loop
	// blocked waiting for capacity rather than waiting for this client.
}

func TestConnectionCloseReleasesExactlyOnce(t *testing.T) {
	left, right := net.Pipe()
	defer right.Close()
	releases := 0
	c := &connection{Conn: left, release: func() { releases++ }}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if releases != 1 {
		t.Fatalf("capacity released %d times", releases)
	}
}

func TestConnectionListenerAcceptFailureReleasesCapacity(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = inner.Close()
	l := newConnectionListener(inner, 1)
	defer l.Close()
	if _, err := l.Accept(); err == nil {
		t.Fatal("closed listener accepted connection")
	}
	l.mu.Lock()
	used := l.used
	l.mu.Unlock()
	if used != 0 {
		t.Fatalf("failed accept retained %d capacity", used)
	}
}
