package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// awsAgent mirrors aws.Agent.
type awsAgent struct {
	Installed bool   `json:"installed"`
	Unit      string `json:"unit"`
	Active    bool   `json:"active"`
	Enabled   bool   `json:"enabled"`
	Version   string `json:"version"`
	Source    string `json:"source"`
}

// awsStatus mirrors aws.Result.
type awsStatus struct {
	EC2 struct {
		Detected           bool   `json:"detected"`
		InstanceID         string `json:"instance_id"`
		InstanceType       string `json:"instance_type"`
		Region             string `json:"region"`
		AvailabilityZone   string `json:"availability_zone"`
		AMIID              string `json:"ami_id"`
		InstanceProfileARN string `json:"instance_profile_arn"`
		IMDSv1Enabled      *bool  `json:"imds_v1_enabled"`
	} `json:"ec2"`
	Agents struct {
		CloudWatch awsAgent `json:"cloudwatch"`
		SSM        awsAgent `json:"ssm"`
	} `json:"agents"`
}

func newAWSCmd(f *flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "aws",
		Short: "Inspect the AWS environment of this server",
	}
	cmd.AddCommand(newAWSStatusCmd(f))
	return cmd
}

func newAWSStatusCmd(f *flags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the EC2 instance identity and the SSM and CloudWatch agents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var raw json.RawMessage
			if err := callOnce(cmd, f, "aws.status", nil, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var st awsStatus
			if err := json.Unmarshal(raw, &st); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			if !st.EC2.Detected {
				fmt.Fprintln(tw, "EC2\tnot detected")
			} else {
				e := st.EC2
				fmt.Fprintf(tw, "Instance\t%s (%s)\n", e.InstanceID, e.InstanceType)
				fmt.Fprintf(tw, "Region\t%s (%s)\n", e.Region, e.AvailabilityZone)
				fmt.Fprintf(tw, "AMI\t%s\n", e.AMIID)
				profile := "-"
				if e.InstanceProfileARN != "" {
					profile = e.InstanceProfileARN
				}
				fmt.Fprintf(tw, "Instance profile\t%s\n", profile)
				v1 := "unknown"
				if e.IMDSv1Enabled != nil {
					v1 = "disabled"
					if *e.IMDSv1Enabled {
						v1 = "enabled (consider requiring IMDSv2)"
					}
				}
				fmt.Fprintf(tw, "IMDSv1\t%s\n", v1)
			}
			fmt.Fprintf(tw, "SSM agent\t%s\n", describeAWSAgent(st.Agents.SSM))
			fmt.Fprintf(tw, "CloudWatch agent\t%s\n", describeAWSAgent(st.Agents.CloudWatch))
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the raw JSON result")
	return cmd
}

func describeAWSAgent(a awsAgent) string {
	if !a.Installed {
		return "not installed"
	}
	state := "inactive"
	if a.Active {
		state = "active"
	}
	enabled := "disabled"
	if a.Enabled {
		enabled = "enabled"
	}
	s := fmt.Sprintf("%s, %s", state, enabled)
	if a.Version != "" {
		s += ", " + a.Version
	}
	if a.Source != "" {
		s += " (" + a.Source + ")"
	}
	return s
}
