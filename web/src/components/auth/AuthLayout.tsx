import { AlertCircle } from "lucide-react";
import * as React from "react";
import { LogoMark } from "@/components/shell/Logo";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { cn } from "@/lib/utils";

/** Full-page frame for sign-in and setup: dot-grid backdrop and a centred console card. */
export function AuthLayout({
  title,
  subtitle,
  children,
  wide,
}: {
  title: string;
  subtitle?: React.ReactNode;
  children: React.ReactNode;
  wide?: boolean;
}) {
  return (
    <div className="bg-grid grid min-h-screen place-items-center px-4 py-10">
      <main className={cn("w-full animate-ki-in", wide ? "max-w-xl" : "max-w-sm")}>
        <div className="mb-6 flex items-center gap-3">
          <LogoMark className="size-9" />
          <div>
            <p className="label-mono">ki-agent console</p>
            <h1 className="text-xl font-semibold leading-tight tracking-tight">{title}</h1>
          </div>
        </div>
        <div className="rounded-lg border bg-card p-6">
          {subtitle && <p className="mb-5 text-sm text-muted-foreground">{subtitle}</p>}
          {children}
        </div>
        <p className="mt-4 text-center font-mono text-[11px] text-muted-foreground">
          Password and authenticator code required.
        </p>
      </main>
    </div>
  );
}

/** Inline form error; announced to screen readers. */
export function FormError({ children, id }: { children: React.ReactNode; id?: string }) {
  return (
    <Alert variant="destructive" id={id}>
      <AlertCircle aria-hidden />
      <AlertDescription className="font-medium">{children}</AlertDescription>
    </Alert>
  );
}
