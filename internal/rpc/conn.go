// Package rpc implements a symmetric JSON-RPC 2.0 peer: either side of a
// connection may send requests and notifications, independent of transport.
package rpc

import "context"

// Conn is one bidirectional message stream. Each Read returns exactly one
// complete JSON-RPC payload (an object or a batch array). Implementations must
// allow one concurrent reader and one concurrent writer.
type Conn interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, msg []byte) error
	Close() error
}
