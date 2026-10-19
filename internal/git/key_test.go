package git

import (
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestGenerateDeployKey(t *testing.T) {
	pemKey, authorized, err := GenerateDeployKey("ki-agent:app")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(pemKey)
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	if signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
		t.Errorf("type = %s", signer.PublicKey().Type())
	}
	if strings.HasSuffix(authorized, "\n") || !strings.HasPrefix(authorized, "ssh-ed25519 ") || !strings.HasSuffix(authorized, " ki-agent:app") {
		t.Errorf("authorized key = %q", authorized)
	}
	pub, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(authorized))
	if err != nil {
		t.Fatalf("ParseAuthorizedKey: %v", err)
	}
	if comment != "ki-agent:app" || string(pub.Marshal()) != string(signer.PublicKey().Marshal()) {
		t.Errorf("public key does not match private key (comment %q)", comment)
	}
}

func TestGenerateDeployKeyBadComment(t *testing.T) {
	if _, _, err := GenerateDeployKey("a\nb"); err == nil {
		t.Error("expected error for multi-line comment")
	}
}
