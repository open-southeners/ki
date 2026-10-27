package aws

import (
	"context"
	"encoding/json"

	"github.com/open-southeners/ki/internal/rpc"
)

// MethodStatus is the RPC method returning [Result].
const MethodStatus = "aws.status"

// Result is the aws.status payload.
type Result struct {
	EC2    EC2    `json:"ec2"`
	Agents Agents `json:"agents"`
}

// Service answers aws.status.
type Service struct {
	imds *IMDS
	det  *detector
}

// New returns a Service using the real metadata endpoint and systemd.
func New() *Service {
	return &Service{imds: NewIMDS("", nil), det: newDetector()}
}

// NewWith returns a Service with an injected metadata client, command runner
// and file reader (nil keeps the real one).
func NewWith(imds *IMDS, run Runner, read FileReader) *Service {
	s := New()
	if imds != nil {
		s.imds = imds
	}
	if run != nil {
		s.det.run = run
	}
	if read != nil {
		s.det.read = read
	}
	return s
}

// Status reports the EC2 identity and the local AWS agents.
func (s *Service) Status(ctx context.Context) Result {
	return Result{EC2: s.imds.Identity(ctx), Agents: s.det.agents(ctx)}
}

// Register installs aws.status on r.
func (s *Service) Register(r *rpc.Router) {
	r.Handle(MethodStatus, func(ctx context.Context, _ *rpc.Peer, _ json.RawMessage) (any, error) {
		return s.Status(ctx), nil
	})
}
