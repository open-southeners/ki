// Package aws detects whether the host is an EC2 instance and which AWS
// agents (SSM, CloudWatch) it runs. It only reads instance metadata and local
// systemd state; it never requests credentials.
package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	defaultIMDSBase = "http://169.254.169.254"
	imdsTimeout     = time.Second
	identityTTL     = 10 * time.Minute
	tokenTTLSeconds = "60"
	maxBody         = 64 << 10

	pathToken = "/latest/api/token"
)

// Metadata paths the client is allowed to request. Anything else, notably
// iam/security-credentials, is refused before a request is made.
const (
	pathInstanceID = "instance-id"
	pathType       = "instance-type"
	pathRegion     = "placement/region"
	pathAZ         = "placement/availability-zone"
	pathAMI        = "ami-id"
	pathIAMInfo    = "iam/info"
)

var allowedPaths = map[string]bool{
	pathInstanceID: true,
	pathType:       true,
	pathRegion:     true,
	pathAZ:         true,
	pathAMI:        true,
	pathIAMInfo:    true,
}

// ErrPathNotAllowed is returned for a metadata path outside the allowlist.
var ErrPathNotAllowed = errors.New("aws: metadata path not allowed")

var errNotFound = errors.New("aws: metadata not found")

// EC2 is the instance identity reported by aws.status.
type EC2 struct {
	Detected           bool   `json:"detected"`
	InstanceID         string `json:"instance_id,omitempty"`
	InstanceType       string `json:"instance_type,omitempty"`
	Region             string `json:"region,omitempty"`
	AvailabilityZone   string `json:"availability_zone,omitempty"`
	AMIID              string `json:"ami_id,omitempty"`
	InstanceProfileARN string `json:"instance_profile_arn,omitempty"`
	IMDSv1Enabled      *bool  `json:"imds_v1_enabled,omitempty"`
}

// IMDS is a minimal IMDSv2 client with a 10 minute identity cache that also
// remembers a negative answer (not on EC2).
type IMDS struct {
	base string
	hc   *http.Client
	now  func() time.Time

	mu      sync.Mutex
	cached  bool
	expires time.Time
	value   EC2
}

// NewIMDS returns a client for the metadata service at base (empty selects
// the link-local address). A nil hc gets a client with 1 s timeouts.
func NewIMDS(base string, hc *http.Client) *IMDS {
	if base == "" {
		base = defaultIMDSBase
	}
	if hc == nil {
		hc = &http.Client{Timeout: imdsTimeout}
	}
	return &IMDS{base: strings.TrimRight(base, "/"), hc: hc, now: time.Now}
}

// Identity returns the (cached) instance identity.
func (c *IMDS) Identity(ctx context.Context) EC2 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached && c.now().Before(c.expires) {
		return c.value
	}
	c.value = c.fetch(ctx)
	c.cached = true
	c.expires = c.now().Add(identityTTL)
	return c.value
}

func (c *IMDS) fetch(ctx context.Context) EC2 {
	token, err := c.token(ctx)
	if err != nil {
		return EC2{}
	}
	id, err := c.get(ctx, token, pathInstanceID)
	if err != nil || id == "" {
		return EC2{}
	}
	out := EC2{Detected: true, InstanceID: id}
	out.InstanceType, _ = c.get(ctx, token, pathType)
	out.Region, _ = c.get(ctx, token, pathRegion)
	out.AvailabilityZone, _ = c.get(ctx, token, pathAZ)
	out.AMIID, _ = c.get(ctx, token, pathAMI)
	if info, err := c.get(ctx, token, pathIAMInfo); err == nil {
		var v struct {
			InstanceProfileArn string
		}
		if json.Unmarshal([]byte(info), &v) == nil {
			out.InstanceProfileARN = v.InstanceProfileArn
		}
	}
	_, err = c.get(ctx, "", pathInstanceID)
	v1 := err == nil
	out.IMDSv1Enabled = &v1
	return out
}

func (c *IMDS) token(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+pathToken, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", tokenTTLSeconds)
	body, status, err := c.do(req)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK || body == "" {
		return "", fmt.Errorf("aws: token request: status %d", status)
	}
	return body, nil
}

// get fetches an allowlisted metadata path. An empty token sends the request
// without one (the IMDSv1 probe).
func (c *IMDS) get(ctx context.Context, token, path string) (string, error) {
	if !allowedPaths[path] {
		return "", fmt.Errorf("%w: %q", ErrPathNotAllowed, path)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/latest/meta-data/"+path, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("X-aws-ec2-metadata-token", token)
	}
	body, status, err := c.do(req)
	if err != nil {
		return "", err
	}
	switch status {
	case http.StatusOK:
		return body, nil
	case http.StatusNotFound:
		return "", errNotFound
	default:
		return "", fmt.Errorf("aws: GET %s: status %d", path, status)
	}
}

func (c *IMDS) do(req *http.Request) (string, int, error) {
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", 0, err
	}
	return strings.TrimSpace(string(b)), resp.StatusCode, nil
}
