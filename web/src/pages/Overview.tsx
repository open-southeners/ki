import { Link } from "react-router-dom";
import { PageHeader } from "@/components/page";
import { AwsCard } from "@/components/ops/AwsCard";
import { EdgeCard } from "@/components/ops/EdgeCard";
import { HostPanel } from "@/components/ops/HostPanel";
import { ProjectsCard } from "@/components/ops/ProjectsTable";
import { TunnelCard } from "@/components/ops/TunnelCard";

/** Host load (live + 1 h history), projects summary and edge status. */
export default function Overview() {
  return (
    <>
      <PageHeader eyebrow="Host" title="Overview" description="Live host load, projects and edge status." />
      <div className="grid gap-6">
        <HostPanel />
        <section aria-labelledby="ov-projects" className="grid gap-3">
          <div className="flex items-baseline justify-between">
            <h2 id="ov-projects" className="label-mono">
              Projects
            </h2>
            <Link to="/projects" className="text-sm text-primary underline-offset-4 hover:underline">
              All projects
            </Link>
          </div>
          <ProjectsCard compact />
        </section>
        <section aria-labelledby="ov-edge" className="grid gap-3 md:max-w-md">
          <h2 id="ov-edge" className="sr-only">
            Edge
          </h2>
          <EdgeCard />
        </section>
        <section aria-labelledby="ov-tunnel" className="grid gap-3 md:max-w-md">
          <h2 id="ov-tunnel" className="sr-only">
            Tunnel
          </h2>
          <TunnelCard />
        </section>
        <section aria-labelledby="ov-aws" className="grid gap-3 md:max-w-md">
          <h2 id="ov-aws" className="sr-only">
            AWS
          </h2>
          <AwsCard />
        </section>
      </div>
    </>
  );
}
