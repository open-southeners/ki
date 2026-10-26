import { cn } from "@/lib/utils";

/** The "ki" mark: a stem with a branch and a status dot, in the accent colour. */
export function LogoMark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 32 32" className={cn("size-8", className)} aria-hidden>
      <rect width="32" height="32" rx="4" className="fill-primary" />
      <path
        d="M9 7v18M9 17l9-10M12.5 14l7.5 11"
        className="stroke-primary-foreground"
        strokeWidth="2.6"
        fill="none"
        strokeLinecap="square"
      />
      <circle cx="24" cy="8" r="2.2" className="fill-primary-foreground" />
    </svg>
  );
}

export function Wordmark({ className }: { className?: string }) {
  return (
    <span className={cn("flex items-center gap-2.5", className)}>
      <LogoMark className="size-7" />
      <span className="font-mono text-[15px] font-medium tracking-tight">
        ki<span className="text-muted-foreground">-agent</span>
      </span>
    </span>
  );
}
