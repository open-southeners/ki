package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-southeners/ki-agent/internal/rpc"
)

func TestClientIP(t *testing.T) {
	tests := []struct {
		name   string
		remote string
		xff    []string
		trust  bool
		want   string
	}{
		{"direct ignores xff", "127.0.0.1:5000", []string{"9.9.9.9"}, false, "127.0.0.1"},
		{"trusted last entry", "127.0.0.1:5000", []string{"1.1.1.1, 2.2.2.2"}, true, "2.2.2.2"},
		{"trusted multiple headers", "127.0.0.1:5000", []string{"1.1.1.1", "3.3.3.3"}, true, "3.3.3.3"},
		{"trusted ipv6", "127.0.0.1:5000", []string{"::1"}, true, "::1"},
		{"trusted absent", "127.0.0.1:5000", nil, true, "127.0.0.1"},
		{"trusted garbage", "127.0.0.1:5000", []string{"nonsense"}, true, "127.0.0.1"},
		{"no port", "10.0.0.1", nil, false, "10.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tt.remote
			for _, v := range tt.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := ClientIP(r, tt.trust); got != tt.want {
				t.Fatalf("ClientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPeerClientIP(t *testing.T) {
	p := rpc.NewPeer(nil, rpc.NewRouter())
	if got := ClientIPOf(p); got != "" {
		t.Fatalf("unset = %q", got)
	}
	SetClientIP(p, "1.2.3.4")
	if got := ClientIPOf(p); got != "1.2.3.4" {
		t.Fatalf("got %q", got)
	}
}
