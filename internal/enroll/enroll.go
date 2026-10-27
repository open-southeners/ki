// Package enroll implements the agent side of device-flow enrollment: it
// discovers a control plane's enrollment endpoints, starts an OAuth 2.0 Device
// Authorization Grant (RFC 8628), polls until the operator approves it and
// stores the resulting control-plane credentials.
//
// The device code and the access token are secrets: they are never logged and
// never included in error strings.
package enroll

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/open-southeners/ki/internal/config"
)

// Failure reasons carried in Error.Reason.
const (
	ReasonUnreachable     = "unreachable"
	ReasonInvalidResponse = "invalid_response"
	ReasonDenied          = "denied"
	ReasonExpired         = "expired"
	ReasonFailed          = "failed"
)

const (
	discoveryPath   = "/.well-known/ki-agent-enrollment"
	clientID        = "ki-agent"
	grantType       = "urn:ietf:params:oauth:grant-type:device_code"
	requestTimeout  = 30 * time.Second
	maxBodyBytes    = 64 << 10
	maxLifetime     = 30 * time.Minute
	defaultInterval = 5 * time.Second
	slowDownStep    = 5 * time.Second
	maxTransient    = 5
	maxFieldLen     = 2048
)

// Request describes the device that is enrolling.
type Request struct {
	// BaseURL is the enrollment base URL (https; http only for loopback hosts).
	BaseURL      string
	Hostname     string
	OS           string
	Arch         string
	AgentVersion string
	Protocol     string
}

// Pending is an authorization started by Client.Start that the operator has
// yet to approve.
type Pending struct {
	// UserCode is the short code the operator enters at VerificationURI.
	UserCode        string
	VerificationURI string
	// VerificationURIComplete embeds the code; empty when the server gave none.
	VerificationURIComplete string
	ExpiresAt               time.Time

	baseURL       string
	deviceCode    string
	tokenEndpoint string
	interval      time.Duration
}

// Credentials are the control-plane connection details produced by enrollment.
type Credentials struct {
	URL           string    `json:"url"`
	Token         string    `json:"token"`
	EnrolledAt    time.Time `json:"enrolled_at"`
	EnrollmentURL string    `json:"enrollment_url"`
}

// Error is an enrollment failure. Detail is safe to show to users and never
// contains secrets.
type Error struct {
	Reason string
	Detail string
}

// Error implements error.
func (e *Error) Error() string {
	if e.Detail == "" {
		return "enrollment " + e.Reason
	}
	return "enrollment " + e.Reason + ": " + e.Detail
}

func errf(reason, format string, args ...any) *Error {
	return &Error{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// Options configures a Client.
type Options struct {
	// TLSConfig is used for https requests; nil means the Go defaults.
	TLSConfig *tls.Config
	// UserAgent is sent on every request.
	UserAgent string
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
	// Sleep waits for d or until ctx ends, returning ctx's error in that case;
	// nil uses a real timer. Tests replace it to poll quickly.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Client performs the enrollment protocol.
type Client struct {
	http      *http.Client
	userAgent string
	now       func() time.Time
	sleep     func(ctx context.Context, d time.Duration) error
}

// NewClient returns a Client using opts.
func NewClient(opts Options) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if opts.TLSConfig != nil {
		tr.TLSClientConfig = opts.TLSConfig.Clone()
	}
	c := &Client{
		userAgent: opts.UserAgent,
		now:       opts.Now,
		sleep:     opts.Sleep,
	}
	c.http = &http.Client{
		Transport: tr,
		Timeout:   requestTimeout,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			return checkEndpointURL(req.URL.String())
		},
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.sleep == nil {
		c.sleep = sleepContext
	}
	return c
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Start discovers the endpoints under r.BaseURL and requests a device code.
func (c *Client) Start(ctx context.Context, r Request) (*Pending, error) {
	if err := checkEndpointURL(r.BaseURL); err != nil {
		return nil, errf(ReasonFailed, "enrollment url: %v", err)
	}
	disc := strings.TrimRight(r.BaseURL, "/") + discoveryPath

	var d struct {
		DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
		TokenEndpoint               string `json:"token_endpoint"`
	}
	status, err := c.do(ctx, http.MethodGet, disc, nil, &d)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, errf(ReasonInvalidResponse, "discovery returned HTTP %d", status)
	}
	if err := checkEndpointURL(d.DeviceAuthorizationEndpoint); err != nil {
		return nil, errf(ReasonInvalidResponse, "device_authorization_endpoint: %v", err)
	}
	if err := checkEndpointURL(d.TokenEndpoint); err != nil {
		return nil, errf(ReasonInvalidResponse, "token_endpoint: %v", err)
	}

	form := url.Values{
		"client_id":     {clientID},
		"hostname":      {r.Hostname},
		"os":            {r.OS},
		"arch":          {r.Arch},
		"agent_version": {r.AgentVersion},
		"protocol":      {r.Protocol},
	}
	var a struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int64  `json:"expires_in"`
		Interval                int64  `json:"interval"`
	}
	status, err = c.do(ctx, http.MethodPost, d.DeviceAuthorizationEndpoint, form, &a)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, errf(ReasonFailed, "device authorization returned HTTP %d", status)
	}
	if a.DeviceCode == "" || a.UserCode == "" || a.VerificationURI == "" {
		return nil, errf(ReasonInvalidResponse, "device authorization response is missing required fields")
	}
	if a.ExpiresIn <= 0 {
		return nil, errf(ReasonInvalidResponse, "device authorization response has no positive expires_in")
	}
	if err := checkDisplayText("user_code", a.UserCode); err != nil {
		return nil, err
	}
	if err := checkDisplayURL("verification_uri", a.VerificationURI); err != nil {
		return nil, err
	}
	if a.VerificationURIComplete != "" {
		if err := checkDisplayURL("verification_uri_complete", a.VerificationURIComplete); err != nil {
			return nil, err
		}
	}

	life := maxLifetime
	if a.ExpiresIn < int64(maxLifetime/time.Second) {
		life = time.Duration(a.ExpiresIn) * time.Second
	}
	interval := defaultInterval
	if a.Interval > 0 {
		interval = time.Duration(min(a.Interval, 3600)) * time.Second
	}
	return &Pending{
		UserCode:                a.UserCode,
		VerificationURI:         a.VerificationURI,
		VerificationURIComplete: a.VerificationURIComplete,
		ExpiresAt:               c.now().Add(life),
		baseURL:                 r.BaseURL,
		deviceCode:              a.DeviceCode,
		tokenEndpoint:           d.TokenEndpoint,
		interval:                interval,
	}, nil
}

// Wait polls the token endpoint until the authorization is approved, denied,
// expired or ctx ends (in which case ctx's error is returned).
func (c *Client) Wait(ctx context.Context, p *Pending) (Credentials, error) {
	if p == nil || p.deviceCode == "" {
		return Credentials{}, errf(ReasonFailed, "no pending authorization")
	}
	remaining := p.ExpiresAt.Sub(c.now())
	if remaining <= 0 {
		return Credentials{}, errf(ReasonExpired, "the authorization expired before it was approved")
	}
	dctx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	// ended maps the end of dctx to the right error.
	ended := func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errf(ReasonExpired, "the authorization expired before it was approved")
	}

	interval := p.interval
	transient := 0
	for {
		if !c.now().Before(p.ExpiresAt) {
			return Credentials{}, errf(ReasonExpired, "the authorization expired before it was approved")
		}
		if err := c.sleep(dctx, interval); err != nil {
			if dctx.Err() != nil {
				return Credentials{}, ended()
			}
			return Credentials{}, err
		}
		if !c.now().Before(p.ExpiresAt) {
			return Credentials{}, errf(ReasonExpired, "the authorization expired before it was approved")
		}

		form := url.Values{
			"grant_type":  {grantType},
			"device_code": {p.deviceCode},
			"client_id":   {clientID},
		}
		var t struct {
			AccessToken     string `json:"access_token"`
			TokenType       string `json:"token_type"`
			ControlPlaneURL string `json:"control_plane_url"`
			Error           string `json:"error"`
		}
		status, err := c.do(dctx, http.MethodPost, p.tokenEndpoint, form, &t)
		if err != nil {
			if dctx.Err() != nil {
				return Credentials{}, ended()
			}
			var ee *Error
			if errors.As(err, &ee) && ee.Reason == ReasonUnreachable {
				if transient++; transient < maxTransient {
					continue
				}
			}
			return Credentials{}, err
		}
		if status >= 500 {
			if transient++; transient < maxTransient {
				continue
			}
			return Credentials{}, errf(ReasonUnreachable, "token endpoint returned HTTP %d", status)
		}
		transient = 0

		switch status {
		case http.StatusOK:
			return c.credentials(p, t.AccessToken, t.TokenType, t.ControlPlaneURL)
		case http.StatusBadRequest:
			switch t.Error {
			case "authorization_pending":
			case "slow_down":
				interval += slowDownStep
			case "access_denied":
				return Credentials{}, errf(ReasonDenied, "the operator denied the enrollment")
			case "expired_token":
				return Credentials{}, errf(ReasonExpired, "the authorization expired before it was approved")
			case "":
				return Credentials{}, errf(ReasonInvalidResponse, "token endpoint returned HTTP 400 without an error code")
			default:
				return Credentials{}, errf(ReasonFailed, "token endpoint returned error %q", truncate(t.Error, 64))
			}
		default:
			return Credentials{}, errf(ReasonFailed, "token endpoint returned HTTP %d", status)
		}
	}
}

func (c *Client) credentials(p *Pending, token, tokenType, cpURL string) (Credentials, error) {
	if token == "" {
		return Credentials{}, errf(ReasonInvalidResponse, "token response has no access_token")
	}
	if !strings.EqualFold(tokenType, "bearer") {
		return Credentials{}, errf(ReasonInvalidResponse, "token response has unsupported token_type %q", truncate(tokenType, 32))
	}
	if err := config.CheckControlPlaneURL(cpURL); err != nil {
		return Credentials{}, errf(ReasonInvalidResponse, "control_plane_url: %v", err)
	}
	return Credentials{
		URL:           cpURL,
		Token:         token,
		EnrolledAt:    c.now().UTC(),
		EnrollmentURL: p.baseURL,
	}, nil
}

// do sends one request and decodes a JSON body of at most maxBodyBytes into
// out, returning the HTTP status. Non-JSON bodies are an invalid_response
// unless the status is a 5xx, whose body is ignored.
func (c *Client) do(ctx context.Context, method, target string, form url.Values, out any) (int, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return 0, errf(ReasonFailed, "build request: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, errf(ReasonUnreachable, "%v", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return 0, errf(ReasonUnreachable, "read response: %v", err)
	}
	if len(b) > maxBodyBytes {
		return 0, errf(ReasonInvalidResponse, "response body exceeds %d bytes", maxBodyBytes)
	}
	if resp.StatusCode >= 500 {
		return resp.StatusCode, nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		if resp.StatusCode != http.StatusOK && len(b) == 0 {
			return resp.StatusCode, nil
		}
		return 0, errf(ReasonInvalidResponse, "response is not valid JSON")
	}
	return resp.StatusCode, nil
}

// checkEndpointURL requires an absolute https URL with a host, or http for
// loopback hosts only.
func checkEndpointURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || raw == "" {
		return errors.New("not a valid URL")
	}
	if u.User != nil {
		return errors.New("must not contain credentials")
	}
	if u.Hostname() == "" {
		return errors.New("must include a host")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !config.IsLoopbackHost(u.Hostname()) {
			return fmt.Errorf("must use https:// unless the host is loopback (got http:// to %q)", u.Hostname())
		}
	default:
		return fmt.Errorf("scheme %q is not supported: want https:// (or http:// for loopback)", u.Scheme)
	}
	return nil
}

// checkDisplayText rejects server-supplied text that is unsuitable to print.
func checkDisplayText(field, s string) *Error {
	if len(s) > maxFieldLen {
		return errf(ReasonInvalidResponse, "%s is too long", field)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return errf(ReasonInvalidResponse, "%s contains control characters", field)
		}
	}
	return nil
}

func checkDisplayURL(field, s string) *Error {
	if err := checkDisplayText(field, s); err != nil {
		return err
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return errf(ReasonInvalidResponse, "%s is not an http(s) URL", field)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
