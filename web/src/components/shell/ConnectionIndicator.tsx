import { useConnectionState } from "@/lib/rpc-context";
import type { ConnState } from "@/lib/rpc";
import { cn } from "@/lib/utils";

const VIEW: Record<ConnState, { label: string; dot: string; pulse?: boolean }> = {
  idle: { label: "Connecting", dot: "bg-warning", pulse: true },
  connecting: { label: "Connecting", dot: "bg-warning", pulse: true },
  open: { label: "Live", dot: "bg-success" },
  reconnecting: { label: "Reconnecting", dot: "bg-warning", pulse: true },
  unauthorized: { label: "Signed out", dot: "bg-destructive" },
  closed: { label: "Disconnected", dot: "bg-destructive" },
};

/** Shows the state of the agent connection; announced politely to screen readers. */
export function ConnectionIndicator({ className }: { className?: string }) {
  const state = useConnectionState();
  const v = VIEW[state];
  return (
    <div className={cn("flex items-center gap-2", className)} role="status" aria-live="polite">
      <span className={cn("size-2 rounded-full", v.dot, v.pulse && "animate-ki-pulse")} aria-hidden />
      <span className="label-mono">
        <span className="sr-only">Agent connection: </span>
        {v.label}
      </span>
    </div>
  );
}
