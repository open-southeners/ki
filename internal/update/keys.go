package update

import "crypto/ed25519"

// releaseKeys are the trusted release signing keys, indexed by [KeyID].
//
// It is empty until the real key exists, which makes [Verify] fail closed: a
// build without a key cannot install updates. To add the key, generate it with
// `go run ./tools/release-sign keygen --out <file>` and paste the line it
// prints (a `"<key id>": ed25519.PublicKey{...},` entry) into this map.
var releaseKeys = map[string]ed25519.PublicKey{
	"0fa347d3e755a552": ed25519.PublicKey{0x40, 0x73, 0x53, 0xa7, 0x4c, 0xb4, 0xaa, 0x85, 0xf7, 0xab, 0xa8, 0xfa, 0xe8, 0x94, 0x7e, 0x1a, 0x5e, 0x56, 0xed, 0x01, 0x2d, 0x7a, 0x5c, 0xee, 0x06, 0x71, 0xd4, 0x86, 0x59, 0x43, 0xaa, 0xe0},
}

// ReleaseKeys returns a copy of the compiled-in trusted release keys.
func ReleaseKeys() map[string]ed25519.PublicKey {
	out := make(map[string]ed25519.PublicKey, len(releaseKeys))
	for id, k := range releaseKeys {
		out[id] = append(ed25519.PublicKey(nil), k...)
	}
	return out
}
