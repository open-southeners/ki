import * as React from "react";

/**
 * Counts down a Retry-After (seconds) so a rate-limited form can disable
 * itself. `start(seconds)` begins; `remaining` is 0 when submitting is allowed.
 */
export function useRetryCountdown(): { remaining: number; start: (seconds: number | undefined) => void } {
  const [until, setUntil] = React.useState(0);
  const [now, setNow] = React.useState(() => Date.now());

  React.useEffect(() => {
    if (until <= Date.now()) return;
    const t = setInterval(() => {
      const n = Date.now();
      setNow(n);
      if (n >= until) clearInterval(t);
    }, 500);
    return () => clearInterval(t);
  }, [until]);

  const start = React.useCallback((seconds: number | undefined) => {
    const n = Date.now();
    setNow(n);
    setUntil(seconds && seconds > 0 ? n + seconds * 1000 : 0);
  }, []);

  return { remaining: Math.max(0, Math.ceil((until - now) / 1000)), start };
}
