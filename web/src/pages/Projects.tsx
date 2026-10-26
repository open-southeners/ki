import { PageHeader } from "@/components/page";
import { ProjectsCard } from "@/components/ops/ProjectsTable";

/** Every project with status, trigger, commit, last deploy and service health. */
export default function Projects() {
  return (
    <>
      <PageHeader eyebrow="Deployments" title="Projects" description="Every project this agent deploys." />
      <ProjectsCard />
    </>
  );
}
