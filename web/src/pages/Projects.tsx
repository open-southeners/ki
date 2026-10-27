import { NewProjectButton } from "@/components/admin/NewProject";
import { PageHeader } from "@/components/page";
import { ProjectsCard } from "@/components/ops/ProjectsTable";
import { useCan } from "@/lib/auth-context";

/** Every project with status, trigger, commit, last deploy and service health. */
export default function Projects() {
  const isAdmin = useCan("admin");
  return (
    <>
      <PageHeader
        eyebrow="Deployments"
        title="Projects"
        description="Every project this agent deploys."
        actions={isAdmin ? <NewProjectButton /> : undefined}
      />
      <ProjectsCard />
    </>
  );
}
