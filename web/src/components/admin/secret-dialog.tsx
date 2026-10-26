import { ShieldAlert } from "lucide-react";
import * as React from "react";
import { CopyButton } from "@/components/copy-button";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

/** A value shown once with a copy button (setup link, API token). */
export function SecretDialog({
  open,
  onOpenChange,
  title,
  description,
  warning,
  valueLabel,
  value,
  extra,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description: React.ReactNode;
  warning: React.ReactNode;
  valueLabel: string;
  value: string;
  /** Additional labelled values (e.g. the certificate fingerprint). */
  extra?: Array<{ label: string; value: string }>;
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>{description}</DialogDescription>
        </DialogHeader>
        <Alert variant="warning">
          <ShieldAlert />
          <AlertDescription>{warning}</AlertDescription>
        </Alert>
        <div className="grid gap-1.5">
          <p className="label-mono">{valueLabel}</p>
          <div className="flex items-start gap-2">
            <code className="min-w-0 flex-1 break-all rounded-md border bg-muted px-2.5 py-2 font-mono text-[13px]">
              {value}
            </code>
            <CopyButton value={value} label={valueLabel} />
          </div>
        </div>
        {extra?.map((x) => (
          <div key={x.label} className="grid gap-1.5">
            <p className="label-mono">{x.label}</p>
            <div className="flex items-start gap-2">
              <code className="min-w-0 flex-1 break-all rounded-md border bg-muted px-2.5 py-2 font-mono text-xs">
                {x.value}
              </code>
              <CopyButton value={x.value} label={x.label} />
            </div>
          </div>
        ))}
        <DialogFooter>
          <DialogClose asChild>
            <Button>Done</Button>
          </DialogClose>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
