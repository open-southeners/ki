import * as React from "react";

export type ThemePref = "system" | "light" | "dark";

interface ThemeCtx {
  pref: ThemePref;
  resolved: "light" | "dark";
  setPref: (p: ThemePref) => void;
}

const Ctx = React.createContext<ThemeCtx | null>(null);
const KEY = "ki-theme";
const mq = () => window.matchMedia("(prefers-color-scheme: dark)");

function readPref(): ThemePref {
  try {
    const v = localStorage.getItem(KEY);
    if (v === "light" || v === "dark" || v === "system") return v;
  } catch {
    /* storage unavailable */
  }
  return "system";
}

/** Follows the system theme unless the user picked one (see public/theme-init.js for first paint). */
export function ThemeProvider({ children }: { children: React.ReactNode }) {
  const [pref, setPrefState] = React.useState<ThemePref>(readPref);
  const [systemDark, setSystemDark] = React.useState(() => mq().matches);

  React.useEffect(() => {
    const m = mq();
    const on = () => setSystemDark(m.matches);
    m.addEventListener("change", on);
    return () => m.removeEventListener("change", on);
  }, []);

  const resolved = pref === "system" ? (systemDark ? "dark" : "light") : pref;
  React.useEffect(() => {
    document.documentElement.classList.toggle("dark", resolved === "dark");
  }, [resolved]);

  const setPref = React.useCallback((p: ThemePref) => {
    setPrefState(p);
    try {
      localStorage.setItem(KEY, p);
    } catch {
      /* ignore */
    }
  }, []);

  const value = React.useMemo(() => ({ pref, resolved, setPref }), [pref, resolved, setPref]);
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useTheme(): ThemeCtx {
  const c = React.useContext(Ctx);
  if (!c) throw new Error("useTheme outside ThemeProvider");
  return c;
}
