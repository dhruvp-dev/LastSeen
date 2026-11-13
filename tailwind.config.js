module.exports = {
  future: { hoverOnlyWhenSupported: true }, // no sticky hover on touch devices
  content: ["./internal/web/templates/**/*.html", "./internal/web/*.go"],
  safelist: ["hero-ok", "hero-warn", "hero-bad", "hero-none", "banner-ok", "banner-warn", "banner-bad", "banner-none"], // class names built at render time (hero-{{.Hero.Tone}})
  theme: {
    extend: {
      colors: {
        paper: "#ffffff", surface: "#f6f6f2", ink: "#0a0a0a", mute: "#737373", faint: "#a3a3a3", rule: "#d9d9d2", soot: "#141414", canvas: "#ffffff", side: "#dbdbd2",
        brand: { DEFAULT: "#141414", dark: "#000000", soft: "#dbdbd2", wash: "#dbdbd2" }, warm: { stone: "#dbdbd2", quartz: "#d0d0c8", pebble: "#c0c0c8", lime: "#4cc02b" },
        pub: { bg: "var(--bg-color)", text: "var(--text-color)", mute: "var(--text-muted)", subtle: "var(--text-subtle)", hi: "var(--text-highlight)", card: "var(--card-bg)", rule: "var(--border-color)", ok: "var(--pub-ok)", warn: "var(--pub-warn)", bad: "var(--pub-bad)" },
        ok: "#3d9e22", warn: "#d97706", bad: "#dc2626",
      },
      borderRadius: { lg: "8px", xl: "12px" },
      boxShadow: { card: "rgba(0, 0, 0, 0.05) 0px 4px 16px 0px", float: "rgba(17, 12, 46, 0.12) 0px 12px 45px 0px" },
      fontFamily: {
        sans: ["Figtree", "system-ui", "-apple-system", "Segoe UI", "sans-serif"],
        figtree: ["Figtree", "system-ui", "-apple-system", "Segoe UI", "sans-serif"],
        mono: ['"JetBrainsMono Nerd Font"', '"JetBrainsMono NF"', '"JetBrains Mono"', "ui-monospace", "monospace"],
      },
    },
  },
};
