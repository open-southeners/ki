package update

import "crypto/ed25519"

// releaseKeys are the trusted release signing keys, indexed by [KeyID].
//
// It is empty until the real key exists, which makes [Verify] fail closed: a
// build without a key cannot install updates. To add the key, generate it with
// `go run ./tools/release-sign keygen --out <file>` and paste the line it
// prints (a `"<key id>": ed25519.PublicKey{...},` entry) into this map.
var releaseKeys = map[string]ed25519.PublicKey{}

// ReleaseKeys returns a copy of the compiled-in trusted release keys.
func ReleaseKeys() map[string]ed25519.PublicKey {
	out := make(map[string]ed25519.PublicKey, len(releaseKeys))
	for id, k := range releaseKeys {
		out[id] = append(ed25519.PublicKey(nil), k...)
	}
	return out
}
