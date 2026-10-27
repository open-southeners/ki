package cli

import (
	"strings"
	"testing"

	"github.com/open-southeners/ki/internal/rpc"
)

const awsStatusJSON = `{"ec2":{"detected":true,"instance_id":"i-0123456789abcdef0","instance_type":"t3.small",
"region":"eu-west-1","availability_zone":"eu-west-1a","ami_id":"ami-0123456789abcdef0","imds_v1_enabled":true},
"agents":{"cloudwatch":{"installed":false,"active":false,"enabled":false,"source":""},
"ssm":{"installed":true,"unit":"snap.amazon-ssm-agent.amazon-ssm-agent.service","active":true,"enabled":true,"version":"3.3.0.0","source":"snap"}}}`

func TestAWSStatus(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, func(r *rpc.Router) { r.Handle(stub(rec, "aws.status", awsStatusJSON)) })

	out, _, err := runCLI(t, sock, "", "aws", "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"i-0123456789abcdef0", "eu-west-1a", "enabled (consider requiring IMDSv2)",
		"active, enabled, 3.3.0.0 (snap)", "not installed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	out, _, err = runCLI(t, sock, "", "aws", "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"instance_id"`) {
		t.Errorf("json output = %s", out)
	}
}
