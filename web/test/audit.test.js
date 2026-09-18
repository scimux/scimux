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
 *   bypassing navigation        — `target=_blank` on a computer-served URL, and
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
  /* S2 routed the three direct fetch sites and the seven computer-served asset
   * URL builders through the seam. They are still *found* — classified
   * `seam-routed` — and are excluded from this bypass list because of that
   * classification, not because the detector stopped seeing them. A URL inside
   * any wrapper other than assetURL(...) remains a finding. */

  /* app.js sets the release-page href from update-check data. The value is
   * dynamic, and it is EXTERNAL by intent (a github release URL, with a
   * github fallback) — recorded here with that reason rather than omitted,
   * because a dynamic href is exactly what a syntax audit cannot classify. */
  { file: "js/app.js", kind: "dom-url", detail: "prop:href:dynamic", count: 1 },

  /* Claude's URL-mode MCP elicitation renders a validated absolute http(s)
   * URL. The scanner cannot prove that runtime validation from the template,
   * so the dynamic target is recorded explicitly as external by contract. */
  { file: "js/chat.js", kind: "nav-bypass", detail: "blank:unknown", count: 1 },

  /* --- bypassing navigation: owed to S8 ---
   * target=_blank / bare-download cannot be fixed by injecting a supplier —
   * there is nothing to inject into. The ratchet is meant to fail if this
   * annotation outlives S8. */
  { file: "js/chat.js", kind: "nav-bypass", detail: "blank:local", count: 4 }, /* owed to S8 */
  { file: "js/chat.js", kind: "nav-bypass", detail: "download:bare", count: 2 }, /* owed to S8 */

  /* index.html:400 — <a id="m_relurl" target="_blank"> with NO href in markup;
   * app.js assigns it (see prop:href:dynamic above). Allowlisted with that
   * cross-reference because the element alone carries no URL to classify.
   * Owed to S8. */
  { file: "index.html", kind: "nav-bypass", detail: "blank:unknown", count: 1 }, /* owed to S8 */

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
/* The one wrapper that is the FR-42 seam. Any other function name around a
 * local literal is a bypass that happens to look like injection. */
const SEAM_WRAPPER = "assetURL";
/* fn('/local') / fn("/local") / fn(`/local…`) — used to see through a wrapper
 * rather than treating the call as opaque (or, worse, as invisible). */
const WRAPPED_LOCAL = /([A-Za-z_$][\w$]*)\s*\(\s*(['"`])(\/[\s\S]*?)\2\s*\)/;

function isLocalURL(value, urlIdents) {
  return classifyLocalValue(value, urlIdents) !== null;
}

function interpolatesIdent(value, id) {
  return value.includes("${" + id + "}") || value.includes("${ " + id + " }");
}

/* Classify a URL-bearing value: local-and-bare, local-and-seam-routed, or not
 * a local URL. Seeing through a wrapper is the point — a vanished site cannot
 * be classified, and a rogue wrapper (notTheSeam, somethingElse) must stay a
 * finding. */
function classifyLocalValue(value, urlIdents) {
  const v = String(value || "").trim();
  if (!v) return null;
  if (NON_NETWORK.test(v) || ABSOLUTE.test(v)) return null;

  const wrap = WRAPPED_LOCAL.exec(v);
  if (wrap) {
    const path = wrap[3];
    if (path.startsWith("/") && !ABSOLUTE.test(path)) {
      return { seamRouted: wrap[1] === SEAM_WRAPPER };
    }
  }

  if (v.startsWith("/")) return { seamRouted: false };

  for (const [id, info] of urlIdents) {
    if (interpolatesIdent(v, id)) {
      return { seamRouted: !!(info && info.seamRouted) };
    }
  }
  return null;
}

function isSeamRoutedValue(value, urlIdents) {
  const cls = classifyLocalValue(value, urlIdents);
  return !!(cls && cls.seamRouted);
}

/* Identifiers bound to a same-origin URL, so `${url}` in an attribute is
 * recognised as a URL sink rather than as opaque interpolation.
 * Also binds `const X = assetURL(<local literal>)` (and any other wrapper
 * around a local literal) so a wrapped site is still found; the wrapper name
 * decides seam-routed vs still-a-bypass. */
function localURLIdentifiers(src) {
  const out = new Map();
  const bare = /(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(['"`])([^`"']*?)\2/g;
  let m;
  while ((m = bare.exec(src))) {
    const val = m[3];
    if (val.startsWith("/") && !ABSOLUTE.test(val)) {
      out.set(m[1], { seamRouted: false });
    }
  }
  const wrapped =
    /(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*([A-Za-z_$][\w$]*)\s*\(\s*(['"`])(\/[\s\S]*?)\3\s*\)/g;
  while ((m = wrapped.exec(src))) {
    const path = m[4];
    if (path.startsWith("/") && !ABSOLUTE.test(path)) {
      out.set(m[1], { seamRouted: m[2] === SEAM_WRAPPER });
    }
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
 * an allowlist entry for them would be a place to hide a real one.
 *
 * The argument is taken with parenthesis depth so a wrapper call inside
 * (assetURL('/x'), somethingElse('/x')) is visible. A char-class that stops at
 * ) / ' / " would make those wrappers vanish, which is how a rogue wrapper
 * would also disappear. */
function cssURLTargets(text) {
  const out = [];
  let i = 0;
  while (i < text.length) {
    const start = text.indexOf("url(", i);
    if (start < 0) break;
    if (start > 0 && /[A-Za-z0-9_$]/.test(text[start - 1])) {
      i = start + 1;
      continue;
    }
    let depth = 1;
    let j = start + 4;
    while (j < text.length && depth > 0) {
      if (text[j] === "(") depth++;
      else if (text[j] === ")") depth--;
      j++;
    }
    if (depth !== 0) break;
    let inner = text.slice(start + 4, j - 1).trim();
    const q = inner[0];
    if ((q === "'" || q === '"' || q === "`") && inner.length >= 2 && inner.endsWith(q)) {
      inner = inner.slice(1, -1);
    }
    const t = inner.trim();
    if (t && !NON_NETWORK.test(t)) out.push({ target: t, index: start });
    i = j;
  }
  return out;
}

function addDomURL(add, detail, index, value, urlIdents) {
  if (isSeamRoutedValue(value, urlIdents)) add("dom-url", "seam-routed", index);
  else add("dom-url", detail, index);
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
      for (const t of cssURLTargets(m[2])) {
        addDomURL(add, "style-url", m.index, t.target, urlIdents);
      }
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
      if (isLocalURL(m[3], urlIdents)) {
        addDomURL(add, "attr:" + m[1].toLowerCase(), m.index, m[3], urlIdents);
      }
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
    /* Rogue wrappers must stay findings — they are not the seam. If these
     * vanish, recognition of assetURL(...) has gone vacuous. */
    [`const url = notTheSeam(\`/api/x\`);\nh = \`<img src="\${url}">\`;`, "dom-url", "attr:src"],
    [`h = \`<span style="--logo:url('\${somethingElse('/assets/x.svg')}')"></span>\`;`, "dom-url", "style-url"],
    /* The seam itself is still found, then classified. */
    [`h = \`<span style="--logo:url('\${assetURL('/assets/x.svg')}')"></span>\`;`, "dom-url", "seam-routed"],
    [`h = \`<img src="\${assetURL('/assets/x.svg')}">\`;`, "dom-url", "seam-routed"],
    [`const url = assetURL(\`/api/a/b\`);\nh = \`<img src="\${url}">\`;`, "dom-url", "seam-routed"],
  ];
  for (const [src, kind, detail] of cases) {
    const got = scanSource("probe.js", src);
    assert.ok(
      got.some(f => f.kind === kind && f.detail === detail),
      `scanner missed ${kind}/${detail} in: ${src}\n  saw: ${JSON.stringify(got)}`,
    );
  }
});

test("scanner classifies assetURL(...) as seam-routed; other wrappers stay findings", () => {
  /* Positive recognition: the site is found. A detector that looks at less
   * would also miss notTheSeam / somethingElse — those stay attr:src / style-url. */
  const routedStyle = scanSource(
    "probe.js",
    `h = \`<span style="--logo:url('\${assetURL('/assets/x.svg')}')"></span>\`;`,
  );
  assert.equal(routedStyle.filter(f => f.detail === "style-url").length, 0);
  assert.ok(routedStyle.some(f => f.detail === "seam-routed"));

  const routedImg = scanSource(
    "probe.js",
    `const url = assetURL(\`/api/a/b\`);\nh = \`<img src="\${url}">\`;`,
  );
  assert.equal(routedImg.filter(f => f.detail === "attr:src").length, 0);
  assert.ok(routedImg.some(f => f.detail === "seam-routed"));

  const rogueIdent = scanSource(
    "probe.js",
    `const url = notTheSeam(\`/api/x\`);\nh = \`<img src="\${url}">\`;`,
  );
  assert.ok(rogueIdent.some(f => f.kind === "dom-url" && f.detail === "attr:src"));
  assert.equal(rogueIdent.filter(f => f.detail === "seam-routed").length, 0);

  const rogueStyle = scanSource(
    "probe.js",
    `h = \`<span style="--logo:url('\${somethingElse('/assets/x.svg')}')"></span>\`;`,
  );
  assert.ok(rogueStyle.some(f => f.kind === "dom-url" && f.detail === "style-url"));
  assert.equal(rogueStyle.filter(f => f.detail === "seam-routed").length, 0);
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
  const routed = found.filter(f => f.detail === "seam-routed");
  /* 5 agentLogo sinks + 6 chat tile sinks. If this drops, the classifier
   * stopped seeing production builders — the allowlist going quiet would
   * then be a vanish, not a classification. */
  assert.ok(
    routed.length >= 11,
    `production assetURL builders must be found and classified seam-routed; got ${routed.length}`,
  );
  assert.ok(routed.some(f => f.file === "js/usage.js"), "usage.js agentLogo sinks not classified seam-routed");
  assert.ok(routed.some(f => f.file === "js/chat.js"), "chat.js tile sinks not classified seam-routed");

  const bypasses = found.filter(f => f.detail !== "seam-routed");
  const got = tally(bypasses);

  if (process.env.AUDIT_DUMP) {
    for (const [key, e] of [...tally(found)].sort()) {
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
  /* S2 moved the three app.js fetch sites behind api(); none remain to
   * allowlist. api.js is the seam itself and takes fetchImpl injected, so it
   * must not call the global. */
  for (const a of ALLOWLIST) {
    assert.notEqual(
      a.kind,
      "transport",
      `transport allowlisted in ${a.file}; direct fetch is no longer outside the seam`,
    );
  }
  const apiSrc = readFileSync(join(WEB, "js", "api.js"), "utf8");
  assert.ok(
    !/\bfetch\s*\(/.test(apiSrc.replace(/fetchImpl\s*\(/g, "")),
    "api.js is the seam and must not call the global fetch",
  );
});
