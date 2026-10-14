package rpctest_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/rpc/rpctest"
)

func TestPipe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := rpctest.Pipe()
	if err := a.Write(ctx, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if got, err := b.Read(ctx); err != nil || string(got) != "hi" {
		t.Fatalf("got %q, %v", got, err)
	}

	// Buffered data drains before EOF is reported.
	if err := a.Write(ctx, []byte("last")); err != nil {
		t.Fatal(err)
	}
	a.Close()
	if got, err := b.Read(ctx); err != nil || string(got) != "last" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := b.Read(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v", err)
	}
	if err := b.Write(ctx, []byte("x")); err == nil {
		t.Fatal("write to closed peer should fail")
	}
	if _, err := a.Read(ctx); err == nil {
		t.Fatal("read on closed end should fail")
	}

	c, _ := rpctest.Pipe()
	short, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if _, err := c.Read(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}
