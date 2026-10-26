import { useInfiniteQuery } from "@tanstack/react-query";
import { Search } from "lucide-react";
import * as React from "react";
import { EmptyState, QueryBoundary } from "@/components/admin/state";
import { DataTable, Mono, Td, Th, Tr } from "@/components/admin/table";
import { PageHeader } from "@/components/page";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { auditFailed, filterAudit } from "@/lib/admin";
import { formatDateTime } from "@/lib/format";
import { useApi } from "@/lib/rpc-context";
import { cn } from "@/lib/utils";

const PAGE = 100;

/** Admin: audit log, newest first, loaded page by page (before_id) and filtered in the browser. */
export default function Audit() {
  const api = useApi();
  const q = useInfiniteQuery({
    queryKey: ["admin", "audit"],
    initialPageParam: undefined as number | undefined,
    queryFn: ({ pageParam }) => api.audit.list({ limit: PAGE, before_id: pageParam }),
    getNextPageParam: (last) => (last.length < PAGE ? undefined : last[last.length - 1]?.id),
  });
  const [filter, setFilter] = React.useState("");

  const all = React.useMemo(() => q.data?.pages.flat() ?? [], [q.data]);
  const rows = React.useMemo(() => filterAudit(all, filter), [all, filter]);

  return (
    <>
      <PageHeader
        eyebrow="Admin"
        title="Audit log"
        description="Who did what, from where, and how it ended. Failed actions show their error code."
      />
      <div className="relative mb-4 max-w-sm">
        <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
        <Input
          type="search"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          placeholder="Filter loaded entries"
          aria-label="Filter loaded audit entries"
          className="pl-8"
        />
      </div>
      <QueryBoundary query={q} what="the audit log">
        {() =>
          all.length === 0 ? (
            <EmptyState>No audit entries</EmptyState>
          ) : (
            <>
              <DataTable>
                <thead>
                  <tr>
                    <Th>Time</Th>
                    <Th>Principal</Th>
                    <Th>IP</Th>
                    <Th>Action</Th>
                    <Th>Target</Th>
                    <Th>Result</Th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((e) => (
                    <Tr key={e.id}>
                      <Td className="whitespace-nowrap text-muted-foreground">{formatDateTime(e.time)}</Td>
                      <Td className="whitespace-nowrap">
                        <Mono>{e.principal_name || "–"}</Mono>{" "}
                        <Badge variant="outline">{e.principal_kind}</Badge>
                      </Td>
                      <Td>
                        <Mono>{e.ip || "–"}</Mono>
                      </Td>
                      <Td>
                        <Mono>{e.action}</Mono>
                      </Td>
                      <Td>
                        <Mono className="break-all">{e.target || "–"}</Mono>
                      </Td>
                      <Td>
                        <Mono className={cn(auditFailed(e.result) ? "text-destructive" : "text-success")}>
                          {e.result || "–"}
                        </Mono>
                      </Td>
                    </Tr>
                  ))}
                  {rows.length === 0 && (
                    <tr>
                      <Td colSpan={6} className="py-6 text-center text-muted-foreground">
                        No loaded entry matches “{filter}”.
                      </Td>
                    </tr>
                  )}
                </tbody>
              </DataTable>
              <div className="mt-3 flex flex-wrap items-center justify-between gap-2">
                <p className="label-mono">
                  {filter ? `${rows.length} of ${all.length} loaded` : `${all.length} loaded`}
                </p>
                {q.hasNextPage && (
                  <Button variant="outline" size="sm" disabled={q.isFetchingNextPage} onClick={() => void q.fetchNextPage()}>
                    {q.isFetchingNextPage ? "Loading…" : "Load older"}
                  </Button>
                )}
              </div>
            </>
          )
        }
      </QueryBoundary>
    </>
  );
}
