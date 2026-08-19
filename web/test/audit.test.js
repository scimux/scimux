/* AT-FR-42-a — the transport-seam audit, as a ratchet.
 *
 * FR-42 states its two halves as *behavioural classes, not syntaxes*, and says
 * so because this test has been got wrong twice by grepping `src=`, `href=`
 * and `download=`. The standing lesson from the plan's decision log: a
 * requirement that says "not a syntax" does not make the query behind it stop
 * being one. So the scanner below is organised by class:
 *
 *   asset URL reaching the DOM  — `src`/`href` and every other URL-bearing
 *                                 attribute, CSS `url()` inside an inline
 *                                 `style` attribute, DOM property assignment,
 *                                 and URLs built by template literals (tracked
 *                                 through the identifier they are bound to, so
 *                                 `href="${url}"` is seen as a URL sink).
 *   bypassing navigation        — `target=_blank` on a laptop-served URL, and
 *                                 the `download` attribute in its BARE,
 *                                 value-less form, which is the form this
 *                                 repository actually writes.
 *
 * It runs as a ratchet: ALLOWLIST names exactly the sites that exist today and
 * the found set must equal it. That fails on a new bypass AND on a stale entry
 * — a plain "no new matches" check only does the first, and rots into silence
 * once the code it described is gone.
 *
 * Two exclusions are by construction rather than by allowlist, because an
 * allowlist entry for them would be a standing invitation to mis-add:
 *   - `url(#fragment)` and `url(data:...)` are not network URLs at all
 *     (map.js:648 is `filter="url(#attnglow)"`, an in-document SVG reference).
 *   - `target="_blank"` on an absolute `https?:` URL is an external link, which
 *     FR-42 places outside the seam by definition (format.js:51).
 *
 * Run with AUDIT_DUMP=1 to print every finding — the intended way to refresh
 * the allowlist after a deliberate change.
 */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const WEB = join(__dirname, "..");

/* ---------- the allowlist: exactly the sites that exist at 74af7a5 ---------- */

/* Each entry is {file, kind, detail, count}. Line numbers are deliberately NOT
 * part of the key — they churn on every edit above the site and would make the
 * ratchet noisy without making it stricter. Duplicates are carried by `count`,
 * so removing one of two identical sinks still fails. */
const ALLOWLIST = [
  /* --- direct transport: 3 sites, all in app.js (FR-42's stated count) ---
   * These are what S2 moves behind the seam. api.js already takes fetchImpl as
   * an injected dependency; these three call the global directly. */
  { file: "js/app.js", kind: "transport", detail: "fetch", count: 3 },

  /* --- asset URLs reaching the DOM: 7 builders feeding 11 sinks ---
   * app.js :293/:296/:303 write the agent logo as CSS url() inside an inline
   * style attribute — the class a `src=`/`href=` grep misses entirely. */
  { file: "js/app.js", kind: "dom-url", detail: "style-url", count: 3 },
  /* app.js :298/:300 write the same logos as <img src>. */
  { file: "js/app.js", kind: "dom-url", detail: "attr:src", count: 2 },
  /* app.js sets the release-page href from update-check data. The value is
   * dynamic, and it is EXTERNAL by intent (a codeberg release URL, with a
   * codeberg fallback) — recorded here with that reason rather than omitted,
   * because a dynamic href is exactly what a syntax audit cannot classify. */
  { file: "js/app.js", kind: "dom-url", detail: "prop:href:dynamic", count: 1 },

  /* chat.js:384 (attachments) and chat.js:411 (assets) each build one URL by
   * template literal and feed it to three sinks: a thumbnail href, an <img
   * src>, and a file href. 2 builders x 3 sinks = 6. */
  { file: "js/chat.js", kind: "dom-url", detail: "attr:href", count: 4 },
  { file: "js/chat.js", kind: "dom-url", detail: "attr:src", count: 2 },

  /* --- bypassing navigation: 4 sites, all chat.js ---
   * :388/:390/:415/:417 carry target="_blank" on a laptop-served URL. On the
   * remote path that is a top-level navigation against the rendezvous origin,
   * which FR-14/FR-33 guarantee serves no application content. FR-42's closing
   * clause makes replacing these a V1 obligation, not an audit note. */
  { file: "js/chat.js", kind: "nav-bypass", detail: "blank:local", count: 4 },
  /* :390/:417 additionally carry a BARE `download` — value-less, the form a
   * `download=` query does not match. */
  { file: "js/chat.js", kind: "nav-bypass", detail: "download:bare", count: 2 },

  /* index.html:400 — <a id="m_relurl" target="_blank"> with NO href in markup;
   * app.js assigns it (see prop:href:dynamic above). Allowlisted with that
   * cross-reference because the element alone carries no URL to classify. */
  { file: "index.html", kind: "nav-bypass", detail: "blank:unknown", count: 1 },

  /* --- the boot document's own static references ---
   * These are not FR-42 findings against the application: they are the boot
   * itself, and FR-40 owns replacing them. They are carried here so the audit
   * describes the COMPLETE set of URLs the page reaches the network with —
   * FR-42's audit counted JS builders only, which is why the plan's figures do
   * not name them. The counts are load-bearing for FR-40: 10 stylesheets (the
   * ones import maps and blob: specifier rewriting do not cover) and the single
   * entry module. If either number moves, the bootstrap manifest is stale. */
  { file: "index.html", kind: "dom-url", detail: "attr:href", count: 10 },
  { file: "index.html", kind: "dom-url", detail: "attr:src", count: 1 },
];

/* ---------- scanner ---------- */

/* URL-bearing HTML attributes. Listed as a class rather than as the three
 * names the previous audits used. */
const URL_ATTRS = [
  "src", "href", "srcset", "poster", "formaction", "action",
  "cite", "ping", "background", "data-src", "xlink:href",
];

const NON_NETWORK = /^(#|data:|blob:|about:|javascript:|mailto:|tel:)/i;
const ABSOLUTE = /^(https?:)?\/\//i;

function isLocalURL(value, urlIdents) {
  const v = value.trim();
  if (!v) return false;
  if (NON_NETWORK.test(v)) return false;
  if (ABSOLUTE.test(v)) return false;
  if (v.startsWith("/")) return true;
  /* A URL built by a template literal and bound to an identifier: the class
   * FR-42 names explicitly. `${url}` alone is opaque to a syntax audit. */
  for (const id of urlIdents) {
    if (v.includes("${" + id + "}") || v.includes("${ " + id + " }")) return true;
  }
  return false;
}

/* Identifiers bound to a same-origin URL, so `${url}` in an attribute is
 * recognised as a URL sink rather than as opaque interpolation. */
function localURLIdentifiers(src) {
  const out = new Set();
  const re = /(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*([`"'])([^`"']*?)\2/g;
  let m;
  while ((m = re.exec(src))) {
    const val = m[3];
    if (val.startsWith("/") && !ABSOLUTE.test(val)) out.add(m[1]);
  }
  return out;
}

function lineOf(src, index) {
  return src.slice(0, index).split("\n").length;
}

/* Blank out comments, preserving every byte offset so reported line numbers
 * stay true. Prose is not code: without this the audit reports composer.js's
 * header comment ("withCsrf + fetch (multipart upload)") and state.js's
 * ("after a remote fetch") as direct transport sites. A ratchet that has to
 * allowlist two prose mentions is a ratchet whose allowlist has stopped
 * meaning "sites outside the seam".
 *
 * Stripping is state-aware rather than a regex: `web/js/format.js` contains
 * `https?:\/\/` inside a regex literal, and a naive `//`-to-end-of-line strip
 * would eat the rest of that line — silently blinding the audit to whatever
 * followed. Strings, template literals and regex literals are all tracked.
 */
function stripComments(file, src) {
  const out = src.split("");
  const blank = (from, to) => {
    for (let i = from; i < to && i < out.length; i++) {
      if (out[i] !== "\n") out[i] = " ";
    }
  };

  if (file.endsWith(".html")) {
    const re = /<!--[\s\S]*?-->/g;
    let m;
    while ((m = re.exec(src))) blank(m.index, m.index + m[0].length);
    return out.join("");
  }

  /* Previous significant character decides whether `/` opens a regex literal
   * or is division — the standard heuristic, and sufficient for this corpus. */
  const regexAllowedAfter = /[(,=:[!&|?{};+\-*%~^]|^$/;
  let prevSig = "";
  for (let i = 0; i < src.length; i++) {
    const c = src[i];
    const d = src[i + 1];
    if (c === "/" && d === "/") {
      let j = i;
      while (j < src.length && src[j] !== "\n") j++;
      blank(i, j);
      i = j - 1;
      continue;
    }
    if (c === "/" && d === "*") {
      const end = src.indexOf("*/", i + 2);
      const j = end < 0 ? src.length : end + 2;
      blank(i, j);
      i = j - 1;
      continue;
    }
    if (c === '"' || c === "'" || c === "`") {
      let j = i + 1;
      while (j < src.length) {
        if (src[j] === "\\") { j += 2; continue; }
        if (src[j] === c) break;
        j++;
      }
      i = j;
      prevSig = c;
      continue;
    }
    if (c === "/" && regexAllowedAfter.test(prevSig)) {
      /* Regex literal: skip to the unescaped closing slash, respecting classes. */
      let j = i + 1;
      let inClass = false;
      let closed = false;
      while (j < src.length && src[j] !== "\n") {
        if (src[j] === "\\") { j += 2; continue; }
        if (src[j] === "[") inClass = true;
        else if (src[j] === "]") inClass = false;
        else if (src[j] === "/" && !inClass) { closed = true; break; }
        j++;
      }
      if (closed) {
        i = j;
        prevSig = "/";
        continue;
      }
    }
    if (!/\s/.test(c)) prevSig = c;
  }
  return out.join("");
}

/* Every `url(...)` whose target is an actual network fetch. Fragment and data:
 * targets are excluded HERE, by construction — they are not network URLs, and
 * an allowlist entry for them would be a place to hide a real one. */
function cssURLTargets(text) {
  const out = [];
  const re = /url\(\s*(['"]?)([^)'"]*)\1\s*\)/g;
  let m;
  while ((m = re.exec(text))) {
    const t = m[2].trim();
    if (!t || NON_NETWORK.test(t)) continue;
    out.push({ target: t, index: m.index });
  }
  return out;
}

function scanSource(file, rawSrc) {
  const found = [];
  const src = stripComments(file, rawSrc);
  const urlIdents = localURLIdentifiers(src);
  const add = (kind, detail, index) =>
    found.push({ file, kind, detail, line: lineOf(src, index) });

  /* --- class: direct transport --- */
  const transports = [
    [/\bfetch\s*\(/g, "fetch"],
    [/\bnew\s+XMLHttpRequest\b/g, "xhr"],
    [/\bXMLHttpRequest\b/g, "xhr"],
    [/\bnew\s+WebSocket\b/g, "websocket"],
    [/\bnew\s+EventSource\b/g, "eventsource"],
    [/\bnavigator\s*\.\s*sendBeacon\s*\(/g, "sendbeacon"],
    [/\bimport\s*\(/g, "dynamic-import"],
  ];
  const seen = new Set();
  for (const [re, name] of transports) {
    let m;
    while ((m = re.exec(src))) {
      /* `new XMLHttpRequest` matches both XHR patterns; count the site once. */
      const key = name + ":" + m.index;
      const alt = "xhr:" + (m.index + 4);
      if (seen.has(key) || seen.has(alt)) continue;
      seen.add(key);
      add("transport", name, m.index);
    }
  }

  /* --- class: navigation that bypasses the seam --- */
  /* window.open / location assignment / form submission. */
  for (const [re, name] of [
    [/\bwindow\s*\.\s*open\s*\(/g, "window.open"],
    [/\blocation\s*\.\s*(?:href|assign|replace)\s*[=(]/g, "location"],
    [/<form\b/gi, "form"],
    [/\.\s*submit\s*\(\s*\)/g, "form.submit"],
  ]) {
    let m;
    while ((m = re.exec(src))) add("nav-bypass", name, m.index);
  }

  /* target=_blank, classified by the URL on the SAME element. External
   * https?: links are outside the seam by definition (format.js:51). */
  {
    const re = /<a\b[^>]*>/gi;
    let m;
    while ((m = re.exec(src))) {
      const tag = m[0];
      if (!/\btarget\s*=\s*(['"`]?)_blank\1/i.test(tag)) continue;
      const href = /\bhref\s*=\s*(['"`])([\s\S]*?)\1/i.exec(tag);
      let detail;
      if (!href) detail = "blank:unknown";
      else if (ABSOLUTE.test(href[2].trim()) || href[2].trim().startsWith("$2"))
        detail = "blank:external";
      else if (isLocalURL(href[2], urlIdents)) detail = "blank:local";
      else detail = "blank:unknown";
      if (detail !== "blank:external") add("nav-bypass", detail, m.index);

      /* The BARE download attribute: `download` present as a standalone token,
       * not followed by `=`. This is the form the repository writes and the
       * form a `download=` query does not match. */
      if (/\bdownload\s*=/.test(tag)) add("nav-bypass", "download:valued", m.index);
      else if (/\bdownload\b/.test(tag)) add("nav-bypass", "download:bare", m.index);
    }
  }

  /* --- class: asset URL reaching the DOM --- */
  /* Inline style attributes carrying a network url(). */
  {
    const re = /\bstyle\s*=\s*(['"`])([\s\S]*?)\1/g;
    let m;
    while ((m = re.exec(src))) {
      for (const _ of cssURLTargets(m[2])) add("dom-url", "style-url", m.index);
    }
  }

  /* URL-bearing attributes in markup. */
  {
    const re = new RegExp(
      "\\b(" + URL_ATTRS.join("|").replace(/\./g, "\\.") + ")\\s*=\\s*(['\"`])([\\s\\S]*?)\\2",
      "gi",
    );
    let m;
    while ((m = re.exec(src))) {
      if (isLocalURL(m[3], urlIdents)) add("dom-url", "attr:" + m[1].toLowerCase(), m.index);
    }
  }

  /* DOM property assignment: el.src = ..., el.href = ... */
  {
    const re = /\.\s*(src|href|srcset|action|poster)\s*=\s*([^;\n]+)/g;
    let m;
    while ((m = re.exec(src))) {
      const rhs = m[2].trim();
      /* Skip comparisons and the attribute-in-a-string cases already counted. */
      if (rhs.startsWith("=")) continue;
      const lit = /^(['"`])([\s\S]*?)\1/.exec(rhs);
      if (lit) {
        if (isLocalURL(lit[2], urlIdents)) add("dom-url", "prop:" + m[1] + ":literal", m.index);
        continue;
      }
      /* Non-literal right-hand side: opaque to any syntax audit, so it must be
       * allowlisted with a reason rather than silently passed. */
      add("dom-url", "prop:" + m[1] + ":dynamic", m.index);
    }
  }

  return found;
}

/* ---------- corpus ---------- */

function auditedFiles() {
  const files = [{ rel: "index.html", abs: join(WEB, "index.html") }];
  for (const name of readdirSync(join(WEB, "js")).sort()) {
    if (name.endsWith(".js")) files.push({ rel: "js/" + name, abs: join(WEB, "js", name) });
  }
  return files;
}

function runAudit() {
  const found = [];
  for (const f of auditedFiles()) {
    found.push(...scanSource(f.rel, readFileSync(f.abs, "utf8")));
  }
  return found;
}

function tally(found) {
  const m = new Map();
  for (const f of found) {
    const key = f.file + "|" + f.kind + "|" + f.detail;
    if (!m.has(key)) m.set(key, { ...f, count: 0, lines: [] });
    const e = m.get(key);
    e.count++;
    e.lines.push(f.line);
  }
  return m;
}

/* ---------- tests ---------- */

test("audit corpus covers the whole checked-in web source", () => {
  const files = auditedFiles();
  /* Anti-vacuity: a path bug that scanned nothing would make the ratchet pass
   * forever while reporting a clean seam. */
  assert.ok(
    files.length >= 20,
    `audit scanned only ${files.length} files; it is not reaching web/`,
  );
  assert.ok(files.some(f => f.rel === "js/app.js"), "app.js missing from audit corpus");
  assert.ok(files.some(f => f.rel === "js/chat.js"), "chat.js missing from audit corpus");
  assert.ok(files.some(f => f.rel === "index.html"), "index.html missing from audit corpus");
});

test("scanner detects each FR-42 behavioural class it claims to", () => {
  /* The scanner is the load-bearing part of this ratchet, so it is tested
   * against synthetic sources rather than trusted. Each case is a form the
   * previous two audits missed. */
  const cases = [
    [`h = \`<span style="--logo:url('/assets/x.svg')"></span>\`;`, "dom-url", "style-url"],
    [`const url = \`/api/a/b\`;\nh = \`<img src="\${url}">\`;`, "dom-url", "attr:src"],
    [`const url = \`/api/a/b\`;\nh = \`<a href="\${url}">x</a>\`;`, "dom-url", "attr:href"],
    [`h = \`<a href="/api/x" target="_blank" download>f</a>\`;`, "nav-bypass", "download:bare"],
    [`h = \`<a href="/api/x" target="_blank">f</a>\`;`, "nav-bypass", "blank:local"],
    [`el.href = someValue;`, "dom-url", "prop:href:dynamic"],
    [`const r = await fetch("/api/x");`, "transport", "fetch"],
    [`const x = new XMLHttpRequest();`, "transport", "xhr"],
    [`window.open("/api/x");`, "nav-bypass", "window.open"],
  ];
  for (const [src, kind, detail] of cases) {
    const got = scanSource("probe.js", src);
    assert.ok(
      got.some(f => f.kind === kind && f.detail === detail),
      `scanner missed ${kind}/${detail} in: ${src}\n  saw: ${JSON.stringify(got)}`,
    );
  }
});

test("scanner excludes non-network url() and external _blank by construction", () => {
  /* These must NOT be allowlist entries — an allowlist row is a place a real
   * bypass can be hidden. They are excluded in the scanner itself. */
  const frag = scanSource(
    "probe.js",
    `h = \`<circle filter="url(#attnglow)" style="--x:url(#g)"/>\`;`,
  );
  assert.deepEqual(
    frag.filter(f => f.detail === "style-url"),
    [],
    "url(#fragment) is an in-document reference, not a network URL",
  );

  const ext = scanSource(
    "probe.js",
    `h = \`<a href="https://example.org/x" target="_blank" rel="noopener">x</a>\`;`,
  );
  assert.deepEqual(
    ext.filter(f => f.kind === "nav-bypass" && f.detail.startsWith("blank")),
    [],
    "target=_blank on an absolute https: link is outside the seam by definition",
  );
});

test("AT-FR-42-a: the seam audit matches the allowlist exactly", () => {
  const found = runAudit();
  const got = tally(found);

  if (process.env.AUDIT_DUMP) {
    for (const [key, e] of [...got].sort()) {
      console.log(`  ${key}  x${e.count}  lines ${e.lines.join(",")}`);
    }
  }

  const want = new Map(
    ALLOWLIST.map(a => [a.file + "|" + a.kind + "|" + a.detail, a.count]),
  );

  const problems = [];
  for (const [key, e] of got) {
    if (!want.has(key)) {
      problems.push(
        `NEW seam bypass: ${key} at line(s) ${e.lines.join(",")}. ` +
        `Route it through the transport seam, or add an allowlist entry stating why it is outside it.`,
      );
    } else if (want.get(key) !== e.count) {
      problems.push(
        `COUNT CHANGED: ${key} allowlisted x${want.get(key)}, found x${e.count} ` +
        `at line(s) ${e.lines.join(",")}.`,
      );
    }
  }
  for (const [key, n] of want) {
    if (!got.has(key)) {
      problems.push(
        `STALE allowlist entry: ${key} (x${n}) no longer exists. ` +
        `Remove it — a ratchet that keeps describing gone code stops describing the seam.`,
      );
    }
  }

  assert.deepEqual(problems, [], "\n" + problems.join("\n") + "\n");
});

test("no allowlist entry hides a direct transport outside app.js", () => {
  /* FR-42's stated position: the three direct fetch sites are all in app.js and
   * are what S2 moves behind the seam. api.js is the seam itself and takes
   * fetchImpl injected, so it must not call the global. */
  for (const a of ALLOWLIST) {
    if (a.kind !== "transport") continue;
    assert.equal(
      a.file,
      "js/app.js",
      `transport allowlisted in ${a.file}; FR-42 places every direct transport site in app.js`,
    );
  }
  const apiSrc = readFileSync(join(WEB, "js", "api.js"), "utf8");
  assert.ok(
    !/\bfetch\s*\(/.test(apiSrc.replace(/fetchImpl\s*\(/g, "")),
    "api.js is the seam and must not call the global fetch",
  );
});
