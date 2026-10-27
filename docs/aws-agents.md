# AWS agents on EC2 servers

What `ki agent aws status` / `aws.status` reports about AWS-related software on
an instance, what it deliberately does not, and the findings behind those
choices. These are generic observations from Ubuntu servers on EC2; check your
own image.

## What the agent reports

- **EC2 identity** (instance id, type, region, availability zone, AMI, the
  instance profile ARN) from the instance metadata service (IMDS), using
  IMDSv2: a `PUT` for a session token, then `GET`s with the token. Every
  request has a short timeout, so a server that is not on EC2 answers
  "not detected" in about a second. The answer, including the negative one,
  is cached for 10 minutes.
- **`imds_v1_enabled`**: whether an unauthenticated `GET` works. IMDSv1 lets a
  server-side request forgery bug in any app on the host read instance data, so
  requiring IMDSv2 (`aws ec2 modify-instance-metadata-options
  --http-tokens required`) is recommended; the panel shows a warning while it
  is on.
- **CloudWatch agent** and **SSM agent**: whether installed, active and
  enabled, the version and how it was installed (`deb` or `snap`), read from
  systemd without root.

The agent never requests `iam/security-credentials`; the instance role's
credentials are for the software that needs them (the ECR credential helper,
the SSM agent), not for ki-agent.

## Findings

- **The CloudWatch agent is commonly absent.** Many instances never had it
  installed, so detection treats "not installed" as a normal state and the panel
  shows it neutrally rather than as a fault.
- **The SSM agent runs from the snap or from the deb.** Recent Ubuntu AMIs ship
  it as a snap, whose unit is `snap.amazon-ssm-agent.amazon-ssm-agent.service`;
  other images install the deb with `amazon-ssm-agent.service`. Checking only
  one of them misreports the other as missing, so both are looked at. Version
  comes from the deb binary or from the snap's `meta/snap.yaml`.
- **Registration cannot be seen locally.** Whether an SSM agent is registered
  (a managed instance) is stored in root-only files and its log; reading it
  needs root, which ki-agent does not have. The control plane can ask AWS
  instead, with `ssm:DescribeInstanceInformation` for the instance id this
  endpoint reports.
- **Hop limit.** IMDSv2 responses are limited by the instance's metadata
  `HttpPutResponseHopLimit`. With the default of 1, a process inside a
  container cannot get a token (the response does not cross the Docker bridge),
  but the agent runs on the host and is unaffected. Containers that need the
  instance role (for example an application calling AWS APIs) need a hop limit
  of 2.
- **Unit state through systemd.** `systemctl show -p LoadState,ActiveState,UnitFileState`
  is read-only and works inside the agent's systemd sandbox
  (`ProtectSystem=strict`, no address-family restrictions: it only needs the
  system bus socket).

## Permissions to plan for

| For | Needs |
|---|---|
| Pulling private images from ECR | instance role with `ecr:GetAuthorizationToken`, `ecr:BatchGetImage`, `ecr:GetDownloadUrlForLayer`, `ecr:BatchCheckLayerAvailability`, plus `docker-credential-ecr-login` (see the README) |
| Seeing whether SSM is registered (control plane) | `ssm:DescribeInstanceInformation` |
| Reading this endpoint | none; local metadata only |
