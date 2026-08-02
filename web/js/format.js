/* Pure text / time / markdown / color helpers.
 *
 * Packet 6A: DOM-free copy of the formatting subset currently inlined in
 * web/index.html. Named exports are exercised by Node tests only; production
 * still uses the inline helpers until Packet 6F wires the sole module entry.
 *
 * Seams (explicit, not silent behavior narrowing):
 * - esc: pure entity encoding that matches the browser esc() contract for
 *   untrusted text/attributes (& < > " plus nullish → empty). The inline
 *   helper uses textContent→innerHTML then force-escapes "; this module is
 *   the pure equivalent of that observed contract.
 * - ageText / time formatters: optional clock and locale arguments so tests
 *   are deterministic; omitted args keep Date.now() / runtime default locale
 *   (same as the inline call sites).
 * - contrastText / cssRGB: hex colors resolve purely via hexRGB. Non-hex CSS
 *   colors (named, rgb(), var(--…)) need an optional resolveColor(c) →
 *   [r,g,b]|null injector — the browser path uses getComputedStyle. Without
 *   an injector, non-hex inputs fall back exactly as cssRGB does when
 *   document is unavailable (null → contrastText "#fff").
 *
 * No document, window, fetch, localStorage, navigator, timers, or app state.
 */

/* HTML escaping — attribute-safe for double-quoted interpolations. */
export function esc(s) {
  return String(s ?? "")
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replaceAll('"', "&quot;");
}

/* ---------- minimal markdown (escape-first; fenced code, tables, lists) ---------- */
export function mdInline(s) {
  return esc(s).replace(/`([^`]+)`/g, "<code>$1</code>")
          .replace(/\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)/g, `<a href="$2" target="_blank" rel="noopener">$1</a>`)
          .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
          /* single-asterisk italic — run after bold so `**` isn't split. The
             non-space flanking on both ends keeps arithmetic ("2 * 3 * 4") and
             glob patterns literal, matching Markdown's emphasis rules. */
          .replace(/\*([^\s*][^*]*?[^\s*]|[^\s*])\*/g, "<em>$1</em>");
}
export function md(src) {
  /* the tmux paste path delivers a multi-line prompt's newlines as \r
     (terminal Enter), and the CLI transcript records them that way — treat
     every CR flavor as a line break or the turn renders flattened */
  const lines = (src || "").replace(/\r\n?/g, "\n").split("\n");
  let out = [], para = [], table = null, list = null, quote = [];
  const flushP = () => { if (para.length){ out.push("<p>" + para.map(mdInline).join("<br>") + "</p>"); para = []; } };
  const flushT = () => { if (table){ out.push(table + "</table>"); table = null; } };
  const flushL = () => { if (list){ out.push(list.html + `</${list.tag}>`); list = null; } };
  const flushQ = () => { if (quote.length){ out.push("<blockquote>" + quote.map(mdInline).join("<br>") + "</blockquote>"); quote = []; } };
  const flushAll = () => { flushP(); flushT(); flushL(); flushQ(); };
  let code = null;
  for (const ln of lines){
    if (code){
      if (/^\s*```/.test(ln)){
        out.push(`<pre><code>${esc(code.lines.join("\n"))}</code></pre>`);
        code = null;
      } else code.lines.push(ln);
      continue;
    }
    if (/^\s*```/.test(ln)){
      flushAll();
      code = { lines: [] };
      continue;
    }
    const h = ln.match(/^(#{1,3})\s+(.+)$/);
    if (h){
      flushAll();
      out.push(`<h${h[1].length}>${mdInline(h[2])}</h${h[1].length}>`);
      continue;
    }
    const bq = ln.match(/^\s*>\s?(.*)$/);
    if (bq){
      flushP(); flushT(); flushL();
      quote.push(bq[1]);
      continue;
    }
    flushQ();
    const li = ln.match(/^(\s*)([-*]|\d+[.])\s+(.+)$/);
    if (li){
      flushP(); flushT();
      const tag = /\d+[.]/.test(li[2]) ? "ol" : "ul";
      if (!list || list.tag !== tag){ flushL(); list = { tag, html: `<${tag}>` }; }
      /* keep the author's numbering: continuation text between items splits
         the <ol>, and a restarted <ol> would renumber everything "1." */
      list.html += `<li${tag === "ol" ? ` value="${parseInt(li[2], 10)}"` : ""}>${mdInline(li[3])}</li>`;
      continue;
    }
    flushL();
    if (/^\s*\|.*\|\s*$/.test(ln)){
      const cells = ln.trim().slice(1, -1).split("|").map(c => c.trim());
      if (cells.every(c => /^[-: ]+$/.test(c))) continue;
      flushP();
      const tag = table ? "td" : "th";
      if (!table) table = "<table>";
      table += "<tr>" + cells.map(c => `<${tag}>${mdInline(c)}</${tag}>`).join("") + "</tr>";
      continue;
    }
    flushT();
    if (!ln.trim()) flushP(); else para.push(ln);
  }
  if (code) out.push(`<pre><code>${esc(code.lines.join("\n"))}</code></pre>`);
  flushAll();
  return out.join("");
}

/* ---------- time formatting ---------- */

/* Relative age of a millisecond epoch ("how long ago"). Optional nowMs is the
   test/clock seam; production callers omit it (Date.now()). */
export function ageText(ms, nowMs = Date.now()) {
  if (!ms) return "";
  const s = Math.max(0, (nowMs - ms) / 1000);
  if (s < 10) return "now";
  if (s < 60) return Math.floor(s) + "s";
  if (s < 3600) return Math.floor(s / 60) + "m";
  if (s < 86400) return Math.floor(s / 3600) + "h";
  return Math.floor(s / 86400) + "d";
}

/* a duration span (elapsed = last_activity − created_at): coarse two-field
   readout, distinct from ageText's "how long ago" single field */
export function fmtDur(ms) {
  if (!ms || ms < 0) return "0m";
  const s = ms / 1000;
  if (s < 60) return Math.floor(s) + "s";
  if (s < 3600) return Math.floor(s / 60) + "m";
  if (s < 86400) return Math.floor(s / 3600) + "h " + Math.floor((s % 3600) / 60) + "m";
  return Math.floor(s / 86400) + "d " + Math.floor((s % 86400) / 3600) + "h";
}

/* map station timestamp — locale-ordered day/month + time (was a fixed
   MM-DD slice that read as American to European users); the browser locale
   decides field order (21.07. 14:30 vs 07/21 2:30 PM). Optional locales is
   the deterministic test seam (inline uses undefined → runtime default). */
export function fmtStamp(iso, locales = undefined) {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d)) return "";
  return d.toLocaleString(locales, { day: "2-digit", month: "2-digit", hour: "2-digit", minute: "2-digit" });
}

/* Chat bubble action-row time. Optional now (Date) and locales are seams. */
export function fmtBubbleTime(t, now = new Date(), locales = undefined) {
  const d = new Date(t);
  if (isNaN(d)) return t || "";
  const sameDay = (a, b) => a.getFullYear() === b.getFullYear() &&
    a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
  const loc = locales === undefined ? [] : locales;
  if (sameDay(d, now)) return d.toLocaleTimeString(loc, { timeStyle: "short" });
  const y = new Date(now);
  y.setDate(now.getDate() - 1);
  if (sameDay(d, y)) return "Yesterday, " + d.toLocaleTimeString(loc, { timeStyle: "short" });
  return d.toLocaleString(loc, { dateStyle: "medium", timeStyle: "short" });
}

/* Bookmark / note / chat "when" stamp. Optional locales seam. */
export function fmtWhen(t, locales = undefined) {
  const d = new Date(t);
  const loc = locales === undefined ? [] : locales;
  return isNaN(d) ? (t || "") : d.toLocaleString(loc, { dateStyle: "medium", timeStyle: "short" });
}

/* Note-card meta line (section count · edited/created stamp). */
export function fmtNoteMeta(c, locales = undefined) {
  const n = c.section_count || 0;
  return `${n} section${n === 1 ? "" : "s"} · ${fmtWhen(c.edited_at || c.created_at || "", locales)}`;
}

/* bubble hover title: the colour already tells you who spoke, so the tooltip
   earns its keep by carrying the send time — the same stamp a tap reveals. */
export function bubbleTitle(role, time, locales = undefined) {
  const who = role === "user" ? "you" : "agent";
  const w = time ? fmtWhen(time, locales) : "";
  return w ? `${who} — sent ${w}` : who;
}

/* Parse an ISO/date string to epoch ms; invalid/absent → 0 (map stop sorting). */
export function stampMS(t) {
  const v = Date.parse(t || "");
  return isNaN(v) ? 0 : v;
}

/* ---------- color helpers (pure; no lane lookup or DOM resolution) ---------- */

/* A lane color is user-authored (ui.json / the lane editor) and lands in many
   style="" interpolations. Sanitizing at the accessor — rather than trusting
   every call site to esc()-wrap it — disarms the injection trap permanently:
   a crafted color can no longer break out of an attribute even if a future
   template forgets the wrap (R18.1 altitude). Only well-formed color syntax is
   returned verbatim; anything else falls back to the deterministic hash color. */
const SAFE_COLOR = /^(#[0-9a-fA-F]{3,8}|var\(--[a-zA-Z0-9-]+\)|rgba?\([\d.,%\s]+\)|hsla?\([\d.,%\s]+\)|[a-zA-Z]+)$/;
export function safeColor(c) {
  return typeof c === "string" && SAFE_COLOR.test(c.trim()) ? c.trim() : null;
}

export function hexRGB(c) {
  if (typeof c !== "string" || !c.startsWith("#")) return null;
  let h = c.slice(1);
  if (h.length === 3) h = h.split("").map(x => x + x).join("");
  if (h.length !== 6 || !/^[0-9a-fA-F]{6}$/.test(h)) return null;
  return [0, 2, 4].map(i => parseInt(h.slice(i, i + 2), 16));
}

/* Pure color → [r,g,b]. Hex is resolved here; non-hex may be resolved via the
   optional resolveColor injector (browser: getComputedStyle). */
export function cssRGB(c, resolveColor) {
  const hex = hexRGB(c);
  if (hex) return hex;
  if (typeof resolveColor === "function") return resolveColor(c);
  return null;
}

export function relLum(rgb) {
  const ch = v => {
    v /= 255;
    return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
  };
  const [r, g, b] = rgb.map(ch);
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

export function contrastRatio(a, b) {
  const l1 = Math.max(a, b), l2 = Math.min(a, b);
  return (l1 + 0.05) / (l2 + 0.05);
}

export function contrastText(c, resolveColor) {
  const rgb = cssRGB(c, resolveColor);
  if (!rgb) return "#fff";
  const lum = relLum(rgb);
  return contrastRatio(lum, 0) >= contrastRatio(lum, 1) ? "#000" : "#fff";
}
