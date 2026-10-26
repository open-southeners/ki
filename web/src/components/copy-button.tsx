import { Check, Copy } from "lucide-react";
import * as React from "react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";

/** Copies `value` to the clipboard and confirms with a toast. `label` names what was copied. */
export function CopyButton({
  value,
  label,
  className,
}: {
  value: string;
  label: string;
  className?: string;
}) {
  const [done, setDone] = React.useState(false);
  React.useEffect(() => {
    if (!done) return;
    const t = setTimeout(() => setDone(false), 1800);
    return () => clearTimeout(t);
  }, [done]);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value);
      setDone(true);
      toast.success(`${label} copied`);
    } catch {
      toast.error("Could not copy", { description: "Select the text and copy it manually." });
    }
  };

  return (
    <Button type="button" variant="outline" size="sm" className={className} onClick={() => void copy()}>
      {done ? <Check /> : <Copy />}
      {done ? "Copied" : "Copy"}
      <span className="sr-only"> {label}</span>
    </Button>
  );
}
