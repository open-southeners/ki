package client

import (
	"io"
	"log/slog"
)

// discardLogger silences the rpc peer's own logging; the CLI reports errors
// itself.
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
