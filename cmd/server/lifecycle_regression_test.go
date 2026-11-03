package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/config"
	"github.com/shijianus/EpoCHA/internal/store"
)

func TestServeListenerFailureClosesOtherListenerAndReleasesStore(t *testing.T) {
	cfg, err := config.Load(nil, getenvOf(testEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	public, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, public, admin, io.Discard) }()
	if err := waitForHealthz("http://" + public.Addr().String()); err != nil {
		t.Fatal(err)
	}
	_ = public.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("listener failure must be returned")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("listener failure did not stop service")
	}
	client := &http.Client{Timeout: time.Second}
	response, requestErr := client.Get("http://" + admin.Addr().String() + "/api/admin/whoami")
	if requestErr == nil {
		_ = response.Body.Close()
		t.Fatal("admin listener remained available after public listener failed")
	}
	lock, err := store.AcquireDataLock(cfg.DBPath)
	if err != nil {
		t.Fatalf("data lock remained held: %v", err)
	}
	_ = lock.Close()
}

type gatedReadConn struct {
	net.Conn
	entered    chan struct{}
	stopped    chan struct{}
	release    chan struct{}
	closeOnce  sync.Once
	readHeader bool
}

func (c *gatedReadConn) Read(p []byte) (int, error) {
	if !c.readHeader {
		n, err := c.Conn.Read(p)
		if n > 0 {
			c.readHeader = true
		}
		return n, err
	}
	close(c.entered)
	<-c.stopped
	<-c.release
	return 0, net.ErrClosed
}

func (c *gatedReadConn) Close() error {
	c.closeOnce.Do(func() { close(c.stopped) })
	return c.Conn.Close()
}

type gatedListener struct {
	net.Listener
	entered chan struct{}
	stopped chan struct{}
	release chan struct{}
}

func (l *gatedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &gatedReadConn{Conn: conn, entered: l.entered, stopped: l.stopped, release: l.release}, nil
}

func TestServeKeepsDataLockUntilCancelledHandlerReturns(t *testing.T) {
	cfg, err := config.Load(nil, getenvOf(testEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	public, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	adminBase, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &gatedListener{Listener: adminBase, entered: make(chan struct{}), stopped: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, public, listener, io.Discard) }()
	if err := waitForHealthz("http://" + public.Addr().String()); err != nil {
		t.Fatal(err)
	}
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, _ = io.WriteString(client, "POST /api/admin/sites HTTP/1.1\r\nHost: localhost\r\nContent-Length: 1\r\nAuthorization: Bearer "+sentinelAdminKey+"\r\nContent-Type: application/json\r\n\r\n")
	select {
	case <-listener.entered:
	case <-time.After(time.Second):
		t.Fatal("connection did not reach the reader")
	}
	cancel()
	select {
	case <-listener.stopped:
	case <-time.After(shutdownTimeout + 2*time.Second):
		t.Fatal("shutdown did not close the blocked connection")
	}
	if lock, err := store.AcquireDataLock(cfg.DBPath); !errors.Is(err, store.ErrLockHeld) {
		if lock != nil {
			_ = lock.Close()
		}
		t.Fatalf("data lock released before active connection returned: %v", err)
	}
	close(listener.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not finish after connection was released")
	}
}

func TestHTTPShutdownTimeoutCancelsActiveRequest(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	stopped := make(chan struct{})
	srv := newHTTPServer(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(stopped)
	}))
	t.Cleanup(func() { _ = srv.Close() })
	go func() { _ = srv.Serve(ln) }()
	go func() {
		res, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + ln.Addr().String())
		if err == nil {
			_ = res.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not reach handler")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := shutdownHTTP(ctx, srv); err == nil {
		t.Fatal("expected graceful shutdown timeout")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("active request was not cancelled after shutdown deadline")
	}
}
