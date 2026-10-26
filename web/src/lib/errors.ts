import { toast } from "sonner";
import { AuthError, describeAuthError } from "./auth";
import { RpcError } from "./rpc";
import { ErrorCode } from "./types";

/** A human-readable message for any error thrown by the RPC or auth clients. */
export function errorMessage(e: unknown): string {
  if (e instanceof AuthError) return describeAuthError(e);
  if (e instanceof RpcError) {
    const d = e.data as { detail?: unknown } | undefined;
    const detail = d && typeof d === "object" && typeof d.detail === "string" && d.detail ? d.detail : "";
    if (e.code === ErrorCode.ConnectionLost) return "Connection to the agent was lost.";
    if (e.code === ErrorCode.Timeout) return "The agent did not answer in time.";
    return detail && !e.message.includes(detail) ? `${e.message}: ${detail}` : e.message;
  }
  return e instanceof Error ? e.message : String(e);
}

export function toastError(title: string, e: unknown): void {
  toast.error(title, { description: errorMessage(e) });
}
