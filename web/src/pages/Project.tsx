import { useQuery } from "@tanstack/react-query";
import { Link, useSearchParams, useParams } from "react-router-dom";
import { PageHeader } from "@/components/page";
import { CopyButton } from "@/components/copy-button";
import { ErrorState, Mono, StatusBadge } from "@/components/ops/common";
import { DeploysTab } from "@/components/ops/DeploysTab";
import { projectStatusBadge } from "@/components/ops/logic";
import { ServicesTab } from "@/components/ops/ServicesTab";
import { Card } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { composeLabel } from "@/lib/compose";
import { shortSha } from "@/lib/format";
import { useApi } from "@/lib/rpc-context";
import { RpcError } from "@/lib/rpc";
import { ErrorCode, type Project as ProjectInfo } from "@/lib/types";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { useCan } from "@/lib/auth-context";
import { ProjectCompose } from "@/pages/admin/ProjectCompose";
import { ProjectEnv } from "@/pages/admin/ProjectEnv";
import { ProjectExposure } from "@/pages/admin/ProjectExposure";

/**
 * Project detail at /projects/:slug. The active tab lives in `?tab=` so tabs
 * are linkable. Services and Deploys are the operations tabs; Env and Exposure
 * (admins only) come from the admin pages.
 */
function ProjectSummary({ slug }: { slug: string }) {
  const api = useApi();
  const q = useQuery({ queryKey: ["project", slug], queryFn: () => api.projects.get({ slug }), refetchInterval: 15_000 });
  if (q.isPending) return <Skeleton className="mb-6 h-24 w-full" />;
  if (q.isError) {
    const missing = q.error instanceof RpcError && q.error.code === ErrorCode.NotFound;
    return (
      <Card className="mb-6">
        {missing ? (
          <div className="px-5 py-6 text-sm">
            <p className="font-medium">Project not found</p>
            <Link to="/projects" className="text-primary underline-offset-4 hover:underline">
              Back to projects
            </Link>
          </div>
        ) : (
          <ErrorState title="Could not load the project" error={q.error} onRetry={() => void q.refetch()} />
        )}
      </Card>
    );
  }
  return <SummaryCard p={q.data} />;
}

function SummaryCard({ p }: { p: ProjectInfo }) {
  const trigger = p.trigger.tag_pattern ? `tags ${p.trigger.tag_pattern}` : p.trigger.branch || p.default_branch || "–";
  const items: Array<[string, React.ReactNode]> =
    p.source === "compose"
      ? [["Source", <Mono key="s">{composeLabel(p)}</Mono>]]
      : [
          ["Repository", <Mono key="r" className="break-all">{p.repo_url}</Mono>],
          ["Trigger", <Mono key="t">{trigger}</Mono>],
          ["Commit", <Mono key="c">{shortSha(p.commit)}</Mono>],
          [
            "Webhook",
            <span key="w" className="inline-flex items-center gap-1">
              <Mono className="break-all">{p.webhook.path}</Mono>
              <CopyButton value={p.webhook.path} label="Webhook path" />
            </span>,
          ],
        ];
  return (
    <Card className="mb-6 p-4">
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <StatusBadge spec={projectStatusBadge(p.status)} />
        {p.status_detail && <span className="text-sm text-muted-foreground">{p.status_detail}</span>}
      </div>
      <dl className="grid gap-x-6 gap-y-3 sm:grid-cols-2 lg:grid-cols-4">
        {items.map(([k, v]) => (
          <div key={k} className="min-w-0">
            <dt className="label-mono">{k}</dt>
            <dd className="mt-0.5 text-sm">{v}</dd>
          </div>
        ))}
      </dl>
    </Card>
  );
}

export default function Project() {
  const { slug = "" } = useParams();
  const isAdmin = useCan("admin");
  const [params, setParams] = useSearchParams();
  const wanted = params.get("tab") ?? "services";
  const api = useApi();
  const info = useQuery({ queryKey: ["project", slug], queryFn: () => api.projects.get({ slug }) });
  const isCompose = info.data?.source === "compose";
  const showCompose = isAdmin && isCompose;
  const tab =
    ((wanted === "env" || wanted === "exposure") && !isAdmin) || (wanted === "compose" && !showCompose)
      ? "services"
      : wanted;

  return (
    <>
      <PageHeader eyebrow="Project" title={<span className="font-mono">{slug}</span>} />
      <ProjectSummary slug={slug} />
      <Tabs
        value={tab}
        onValueChange={(v) =>
          setParams(
            (p) => {
              const n = new URLSearchParams(p);
              if (v === "services") n.delete("tab");
              else n.set("tab", v);
              return n;
            },
            { replace: true },
          )
        }
      >
        <TabsList aria-label="Project sections">
          <TabsTrigger value="services">Services</TabsTrigger>
          <TabsTrigger value="deploys">Deploys</TabsTrigger>
          {showCompose && <TabsTrigger value="compose">Compose</TabsTrigger>}
          {isAdmin && <TabsTrigger value="env">Environment</TabsTrigger>}
          {isAdmin && <TabsTrigger value="exposure">Exposure</TabsTrigger>}
        </TabsList>
        <TabsContent value="services">
          <ServicesTab slug={slug} />
        </TabsContent>
        <TabsContent value="deploys">
          <DeploysTab slug={slug} ready={info.data?.status === "ready"} compose={isCompose} />
        </TabsContent>
        {showCompose && (
          <TabsContent value="compose">
            <ProjectCompose slug={slug} ready={info.data?.status === "ready"} />
          </TabsContent>
        )}
        {isAdmin && (
          <TabsContent value="env">
            <ProjectEnv slug={slug} />
          </TabsContent>
        )}
        {isAdmin && (
          <TabsContent value="exposure">
            <ProjectExposure slug={slug} />
          </TabsContent>
        )}
      </Tabs>
    </>
  );
}
