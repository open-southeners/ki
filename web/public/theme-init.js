// Runs before first paint (external file: the CSP forbids inline scripts).
// Applies the stored theme choice, defaulting to the system preference.
(function () {
  try {
    var pref = localStorage.getItem("ki-theme") || "system";
    var dark = pref === "dark" || (pref === "system" && matchMedia("(prefers-color-scheme: dark)").matches);
    document.documentElement.classList.toggle("dark", dark);
  } catch (e) {
    /* storage unavailable: stay on the light theme */
  }
})();
