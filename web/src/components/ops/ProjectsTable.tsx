import { useQueries, useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { Card } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { composeLabel } from "@/lib/compose";
import { formatRelative, shortSha } from "@/lib/format";
import { useApi } from "@/lib/rpc-context";
import type { Deploy, Project, ProjectService } from "@/lib/types";
import { EmptyState, ErrorState, Mono, RowsSkeleton, StatusBadge } from "./common";
import { deployStatusBadge, projectStatusBadge, serviceCounts } from "./logic";

const REFRESH_MS = 15_000;

export interface ProjectRow {
  project: Project;
  lastDeploy: Deploy | null | undefined;
  services: ProjectService[] | undefined;
}

/** projects.list plus, per project, the newest deploy and the services (cached, refreshed every 15 s). */
export function useProjectRows() {
  const api = useApi();
  const list = useQuery({
    queryKey: ["projects"],
    queryFn: () => api.projects.list(),
    refetchInterval: REFRESH_MS,
  });
  const projects = list.data ?? [];
  const deploys = useQueries({
    queries: projects.map((p) => ({
      queryKey: ["deploys", p.slug, "latest"],
      queryFn: async () => (await api.deploys.list({ project: p.slug, limit: 1 }))[0] ?? null,
      refetchInterval: REFRESH_MS,
    })),
  });
  const services = useQueries({
    queries: projects.map((p) => ({
      queryKey: ["services", p.slug],
      queryFn: async () => (await api.projects.services({ slug: p.slug })).services,
      refetchInterval: REFRESH_MS,
      enabled: p.status === "ready",
    })),
  });
  const rows: ProjectRow[] = projects.map((project, i) => ({
    project,
    lastDeploy: deploys[i]?.data,
    services: services[i]?.data,
  }));
  return { list, rows };
}

function triggerText(p: Project): string {
  if (p.source === "compose") return composeLabel(p);
  if (p.trigger.tag_pattern) return `tags ${p.trigger.tag_pattern}`;
  return p.trigger.branch || p.default_branch || "–";
}

function HealthCell({ services }: { services: ProjectService[] | undefined }) {
  if (!services) return <span className="text-muted-foreground">–</span>;
  if (services.length === 0) return <span className="text-muted-foreground">no services</span>;
  const c = serviceCounts(services);
  return (
    <Mono className={c.problem > 0 ? "text-destructive" : undefined}>
      {c.running}/{c.total} up{c.problem > 0 ? `, ${c.problem} degraded` : ""}
    </Mono>
  );
}

/** The projects table used by the overview (compact) and the projects page. */
export function ProjectsTable({ compact = false }: { compact?: boolean }) {
  const { list, rows } = useProjectRows();

  if (list.isPending) return <RowsSkeleton />;
  if (list.isError) return <ErrorState title="Could not load projects" error={list.error} onRetry={() => void list.refetch()} />;
  if (rows.length === 0)
    return (
      <EmptyState title="No projects yet">
        Create one with <Mono>ki-agent projects create &lt;repo-url&gt;</Mono> and it shows up here.
      </EmptyState>
    );

  return (
    <Table>
      <TableHeader>
        <TableRow className="hover:bg-transparent">
          <TableHead>Project</TableHead>
          <TableHead>Status</TableHead>
          {!compact && <TableHead>Trigger</TableHead>}
          <TableHead>Commit</TableHead>
          <TableHead>Last deploy</TableHead>
          <TableHead>Services</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map(({ project: p, lastDeploy, services }) => (
          <TableRow key={p.slug}>
            <TableCell>
              <Link
                to={`/projects/${encodeURIComponent(p.slug)}`}
                className="font-mono font-medium text-primary underline-offset-4 hover:underline"
              >
                {p.slug}
              </Link>
            </TableCell>
            <TableCell>
              <StatusBadge spec={projectStatusBadge(p.status)} />
            </TableCell>
            {!compact && (
              <TableCell>
                <Mono>{triggerText(p)}</Mono>
              </TableCell>
            )}
            <TableCell>
              <Mono>{p.source === "compose" ? "–" : shortSha(p.commit)}</Mono>
            </TableCell>
            <TableCell>
              {lastDeploy === undefined ? (
                <span className="text-muted-foreground">…</span>
              ) : lastDeploy === null ? (
                <span className="text-muted-foreground">never</span>
              ) : (
                <span className="inline-flex flex-wrap items-center gap-2">
                  <StatusBadge spec={deployStatusBadge(lastDeploy.status)} />
                  <span className="text-xs text-muted-foreground">
                    {formatRelative(lastDeploy.finished_at ?? lastDeploy.created_at)}
                  </span>
                </span>
              )}
            </TableCell>
            <TableCell>
              <HealthCell services={services} />
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

export function ProjectsCard({ compact }: { compact?: boolean }) {
  return (
    <Card className="overflow-hidden">
      <ProjectsTable compact={compact} />
    </Card>
  );
}
