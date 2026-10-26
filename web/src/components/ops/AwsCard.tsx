import { useQuery } from "@tanstack/react-query";
import { TriangleAlert } from "lucide-react";
import { CopyButton } from "@/components/copy-button";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { useApi } from "@/lib/rpc-context";
import type { AwsAgent } from "@/lib/types";
import { ErrorState, Mono, RowsSkeleton, StatusBadge } from "./common";
import { awsAgentBadge } from "./logic";

function AgentRow({ name, agent }: { name: string; agent: AwsAgent }) {
  return (
    <>
      <dt className="label-mono self-center">{name}</dt>
      <dd className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <StatusBadge spec={awsAgentBadge(agent)} />
        {agent.installed && (
          <span className="text-xs text-muted-foreground">
            {agent.enabled ? "enabled" : "not enabled"}
            {agent.version && (
              <>
                {" · "}
                <Mono className="text-xs">{agent.version}</Mono>
              </>
            )}
            {agent.source && ` · ${agent.source}`}
          </span>
        )}
      </dd>
    </>
  );
}

/** EC2 instance facts and SSM/CloudWatch agent state; renders only on EC2. */
export function AwsCard() {
  const api = useApi();
  const q = useQuery({ queryKey: ["aws", "status"], queryFn: () => api.aws.status(), refetchInterval: 60_000 });

  if (q.isPending) {
    return (
      <Card>
        <CardContent className="p-4">
          <RowsSkeleton rows={2} className="p-0" />
        </CardContent>
      </Card>
    );
  }
  if (q.isError) {
    return (
      <Card>
        <CardContent className="p-4">
          <ErrorState title="AWS status unavailable" error={q.error} onRetry={() => void q.refetch()} className="p-0" />
        </CardContent>
      </Card>
    );
  }
  const { ec2, agents } = q.data;
  if (!ec2.detected) return <p className="text-xs text-muted-foreground">Not running on EC2.</p>;

  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between gap-2 p-4 pb-2">
        <CardTitle className="label-mono text-[11px] font-medium">AWS</CardTitle>
        {ec2.instance_type && <Mono>{ec2.instance_type}</Mono>}
      </CardHeader>
      <CardContent className="grid gap-3 px-4 pb-4">
        {ec2.imds_v1_enabled === true && (
          <Alert variant="destructive">
            <TriangleAlert className="size-4" />
            <AlertDescription>
              IMDSv1 is enabled: require IMDSv2 in the instance metadata options.
            </AlertDescription>
          </Alert>
        )}
        <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-sm">
          <dt className="label-mono self-center">Region</dt>
          <dd>
            <Mono>{ec2.region ?? "–"}</Mono>
            {ec2.availability_zone && <Mono className="ml-2 text-muted-foreground">{ec2.availability_zone}</Mono>}
          </dd>
          {ec2.instance_id && (
            <>
              <dt className="label-mono self-center">Instance</dt>
              <dd className="flex items-center gap-1">
                <Mono className="min-w-0 break-all">{ec2.instance_id}</Mono>
                <CopyButton value={ec2.instance_id} label="Instance id" />
              </dd>
            </>
          )}
          <AgentRow name="SSM" agent={agents.ssm} />
          <AgentRow name="CloudWatch" agent={agents.cloudwatch} />
        </dl>
      </CardContent>
    </Card>
  );
}
