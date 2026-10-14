// Package api embeds the OpenRPC document that describes the ki-agent
// JSON-RPC protocol.
package api

import _ "embed"

// ProtocolVersion is the semantic version of the JSON-RPC protocol. It equals
// info.version in the embedded OpenRPC document; its major version is what
// agent.hello negotiates.
const ProtocolVersion = "1.0.0"

// OpenRPC is the OpenRPC 1.3 document served by rpc.discover.
//
//go:embed openrpc.json
var OpenRPC []byte
