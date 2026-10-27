import { cn } from "@/lib/utils";

/** Rounded teal tile with the "i" cut out (even-odd), so whatever is behind shows through. */
const LOGO_I_TILE =
  "M143 48h22a13 13 0 0 1 13 13v22a13 13 0 0 1-13 13h-22a13 13 0 0 1-13-13v-22a13 13 0 0 1 13-13Z" +
  "M149 61.5a5 5 0 1 0 10 0a5 5 0 1 0-10 0Z" +
  "M149.75 73a4.25 4.25 0 0 1 8.5 0V86a4.25 4.25 0 0 1-8.5 0Z";

/** The Kⁱ mark: a monoline K in the text colour with a solid "i" tile raised like an exponent. */
export function LogoMark({ className }: { className?: string }) {
  return (
    <svg viewBox="43 48 136 136" className={cn("size-8", className)} aria-hidden>
      <path
        d="M50 78V176M50 142L118 82M69 125.2L122 176"
        className="stroke-foreground"
        strokeWidth="13"
        fill="none"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
      <path d={LOGO_I_TILE} fillRule="evenodd" className="fill-[#00A896] dark:fill-[#2DD4BF]" />
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
