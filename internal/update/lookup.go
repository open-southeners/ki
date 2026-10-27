package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LookupOptions configures [LookupRelease]. Only Current is required.
type LookupOptions struct {
	// BaseURL overrides the GitHub API root (tests).
	BaseURL string
	// Client overrides the HTTP client (tests).
	Client *http.Client
	// Current is the running version, sent in the User-Agent.
	Current string
	// Channel is ChannelStable (default) or ChannelPrerelease; it is ignored
	// when Version is set.
	Channel string
	// Version selects one release by tag instead of the latest.
	Version string
}

// LookupRelease fetches release information directly from GitHub, without a
// cache: the latest release of the channel, or the one tagged Version. It is
// what `sudo ki-agent update` uses, so no running agent is needed.
func LookupRelease(ctx context.Context, o LookupOptions) (*Release, error) {
	if o.BaseURL == "" {
		o.BaseURL = DefaultAPIBaseURL
	}
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 20 * time.Second}
	}
	url := o.BaseURL + repoPath + "/releases/latest"
	list := false
	switch {
	case o.Version != "":
		if !ValidTag(o.Version) {
			return nil, fmt.Errorf("invalid version %q", o.Version)
		}
		url = o.BaseURL + repoPath + "/releases/tags/" + o.Version
	case o.Channel == ChannelPrerelease:
		url = o.BaseURL + repoPath + "/releases?per_page=10"
		list = true
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "ki-agent/"+o.Current)
	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contact GitHub: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		if o.Version != "" {
			return nil, fmt.Errorf("release %s does not exist", o.Version)
		}
		return nil, errors.New("no release has been published yet")
	default:
		return nil, fmt.Errorf("GitHub returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("read GitHub response: %w", err)
	}
	var releases []ghRelease
	if list {
		err = json.Unmarshal(body, &releases)
	} else {
		var one ghRelease
		if err = json.Unmarshal(body, &one); err == nil {
			releases = []ghRelease{one}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("decode GitHub response: %w", err)
	}
	for _, r := range releases {
		if r.Draft || !ValidTag(r.TagName) {
			continue
		}
		return &Release{
			Tag: r.TagName, Name: r.Name, PublishedAt: r.PublishedAt, URL: r.HTMLURL,
			Notes: capNotes(r.Body), Prerelease: r.Prerelease,
		}, nil
	}
	return nil, errors.New("no usable release found")
}
