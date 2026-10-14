package unixsock

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sockPath(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "ks")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return filepath.Join(d, "k.sock")
}

func pair(t *testing.T) (client, server interface {
	Read(context.Context) ([]byte, error)
	Write(context.Context, []byte) error
	Close() error
}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l, err := Listen(sockPath(t), 0o660)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	ch := make(chan any, 1)
	go func() {
		c, err := l.Accept(ctx)
		if err != nil {
			ch <- err
			return
		}
		ch <- c
	}()
	cl, err := Dial(ctx, l.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	v := <-ch
	sv, ok := v.(interface {
		Read(context.Context) ([]byte, error)
		Write(context.Context, []byte) error
		Close() error
	})
	if !ok {
		t.Fatalf("accept: %v", v)
	}
	t.Cleanup(func() { sv.Close() })
	return cl, sv
}

func TestRoundTrip(t *testing.T) {
	cl, sv := pair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	big := append(append([]byte(`{"x":"`), bytes.Repeat([]byte("a"), 1<<20)...), `"}`...)
	for _, m := range [][]byte{[]byte(`{"a":1}`), []byte(`[{"a":1},{"b":2}]`), big} {
		werr := make(chan error, 1)
		go func() { werr <- cl.Write(ctx, m) }()
		got, err := sv.Read(ctx)
		if e := <-werr; e != nil {
			t.Fatal(e)
		}
		if err != nil || !bytes.Equal(got, m) {
			t.Fatalf("got %d bytes err %v", len(got), err)
		}
		go func() { werr <- sv.Write(ctx, m) }()
		got, err = cl.Read(ctx)
		if e := <-werr; e != nil {
			t.Fatal(e)
		}
		if err != nil || !bytes.Equal(got, m) {
			t.Fatalf("reply got %d bytes err %v", len(got), err)
		}
	}
}

func TestWriteRejectsNewline(t *testing.T) {
	cl, _ := pair(t)
	if err := cl.Write(context.Background(), []byte("a\nb")); err == nil {
		t.Fatal("expected error")
	}
}

func TestReadCancel(t *testing.T) {
	cl, sv := pair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := cl.Read(ctx); err == nil {
		t.Fatal("expected ctx error")
	}
	// Connection remains usable after a cancelled read.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := sv.Write(ctx2, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if got, err := cl.Read(ctx2); err != nil || string(got) != "{}" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestLineTooLong(t *testing.T) {
	cl, sv := pair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		raw := bytes.Repeat([]byte("a"), MaxLine+10)
		raw = append(raw, '\n')
		cl.(*conn).c.Write(raw)
	}()
	if _, err := sv.Read(ctx); err != ErrLineTooLong {
		t.Fatalf("err = %v", err)
	}
}

func TestPeerClose(t *testing.T) {
	cl, sv := pair(t)
	cl.Close()
	if _, err := sv.Read(context.Background()); err == nil {
		t.Fatal("expected EOF")
	}
}

func TestStaleSocketReplacedAndMode(t *testing.T) {
	p := sockPath(t)
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: p, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	stale.Close() // leaves the socket file behind
	if _, err := os.Lstat(p); err != nil {
		t.Fatal("stale socket missing")
	}
	l, err := Listen(p, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
	l.Close()
	if _, err := os.Lstat(p); err == nil {
		t.Fatal("socket not removed on Close")
	}
}

func TestRefuseNonSocket(t *testing.T) {
	p := sockPath(t)
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(p, 0o600); err == nil {
		t.Fatal("expected refusal")
	}
	if b, _ := os.ReadFile(p); string(b) != "x" {
		t.Fatal("file was modified")
	}
}

func TestAcceptCancel(t *testing.T) {
	l, err := Listen(sockPath(t), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := l.Accept(ctx); err == nil {
		t.Fatal("expected ctx error")
	}
}
