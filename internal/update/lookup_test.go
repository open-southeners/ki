package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLookupRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			_, _ = w.Write([]byte(`{"tag_name":"v0.4.0","name":"0.4.0","body":"notes","html_url":"https://x","prerelease":false}`))
		case strings.HasSuffix(r.URL.Path, "/releases/tags/v0.3.1"):
			_, _ = w.Write([]byte(`{"tag_name":"v0.3.1","body":"fix"}`))
		case strings.HasSuffix(r.URL.Path, "/releases"):
			_, _ = w.Write([]byte(`[{"tag_name":"v0.5.0-rc.1","draft":true},{"tag_name":"v0.5.0-rc.0","prerelease":true}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	o := LookupOptions{BaseURL: srv.URL, Current: "v0.3.0"}

	rel, err := LookupRelease(ctx, o)
	if err != nil || rel.Tag != "v0.4.0" || rel.Notes != "notes" {
		t.Fatalf("latest = %+v, %v", rel, err)
	}
	o.Channel = ChannelPrerelease
	if rel, err = LookupRelease(ctx, o); err != nil || rel.Tag != "v0.5.0-rc.0" || !rel.Prerelease {
		t.Fatalf("prerelease = %+v, %v", rel, err)
	}
	o.Version = "v0.3.1"
	if rel, err = LookupRelease(ctx, o); err != nil || rel.Tag != "v0.3.1" {
		t.Fatalf("by tag = %+v, %v", rel, err)
	}
	o.Version = "v9.9.9"
	if _, err = LookupRelease(ctx, o); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing tag err = %v", err)
	}
	o.Version = "../x"
	if _, err = LookupRelease(ctx, o); err == nil {
		t.Error("invalid version accepted")
	}
}
