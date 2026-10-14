// Package rpctest provides in-memory helpers for testing code built on
// package rpc.
package rpctest

import (
	"context"
	"io"
	"sync"

	"github.com/open-southeners/ki-agent/internal/rpc"
)

const pipeBuffer = 64

// Pipe returns two connected in-memory Conns: a message written to one is read
// from the other. Closing either end makes reads on the other return io.EOF
// (after buffered messages drain) and its writes fail with io.ErrClosedPipe.
func Pipe() (rpc.Conn, rpc.Conn) {
	ab := make(chan []byte, pipeBuffer)
	ba := make(chan []byte, pipeBuffer)
	adone, bdone := make(chan struct{}), make(chan struct{})
	a := &end{in: ba, out: ab, done: adone, peerDone: bdone}
	b := &end{in: ab, out: ba, done: bdone, peerDone: adone}
	return a, b
}

type end struct {
	in       chan []byte
	out      chan []byte
	done     chan struct{}
	peerDone <-chan struct{}
	once     sync.Once
}

func (e *end) Read(ctx context.Context) ([]byte, error) {
	select {
	case <-e.done:
		return nil, io.ErrClosedPipe
	default:
	}
	select {
	case m := <-e.in:
		return m, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.done:
		return nil, io.ErrClosedPipe
	case <-e.peerDone:
		select {
		case m := <-e.in:
			return m, nil
		default:
			return nil, io.EOF
		}
	}
}

func (e *end) Write(ctx context.Context, msg []byte) error {
	select {
	case <-e.done:
		return io.ErrClosedPipe
	case <-e.peerDone:
		return io.ErrClosedPipe
	default:
	}
	cp := append([]byte(nil), msg...)
	select {
	case e.out <- cp:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-e.done:
		return io.ErrClosedPipe
	case <-e.peerDone:
		return io.ErrClosedPipe
	}
}

func (e *end) Close() error {
	e.once.Do(func() { close(e.done) })
	return nil
}
