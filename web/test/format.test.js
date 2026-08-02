/* Characterization tests for web/js/format.js — pure formatting subset.
 * Imports the real module (not HTML substring extraction). */
import test from "node:test";
import assert from "node:assert/strict";
import {
  esc,
  mdInline,
  md,
  ageText,
  fmtDur,
  fmtStamp,
  fmtBubbleTime,
  fmtWhen,
  fmtNoteMeta,
  bubbleTitle,
  stampMS,
  safeColor,
  hexRGB,
  cssRGB,
  relLum,
  contrastRatio,
  contrastText,
} from "../js/format.js";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const formatSrc = readFileSync(join(__dirname, "../js/format.js"), "utf8");

/* ---------- purity / no browser globals ---------- */
test("format.js has no browser globals or implicit app state", () => {
  // Strip comments so the compatibility narrative in the header does not
  // false-positive on words like document / getComputedStyle.
  const code = formatSrc
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/\/\/.*$/gm, "");
  const forbidden = [
    /\bdocument\b/,
    /\bwindow\b/,
    /\bfetch\b/,
    /\blocalStorage\b/,
    /\bnavigator\b/,
    /\bsetTimeout\b/,
    /\bsetInterval\b/,
    /\brequestAnimationFrame\b/,
    /\blaneById\b/,
    /\blaneColor\b/,
    /\blaneList\b/,
    /\bhashStr\b/,
    /\bLANE_COLORS\b/,
    /\bgetComputedStyle\b/,
    /\bcreateElement\b/,
  ];
  for (const re of forbidden) {
    assert.equal(re.test(code), false, `format.js must not reference ${re}`);
  }
});

test("format.js exports named pure helpers", async () => {
  const mod = await import("../js/format.js");
  for (const name of [
    "esc", "mdInline", "md", "ageText", "fmtDur", "fmtStamp",
    "fmtBubbleTime", "fmtWhen", "fmtNoteMeta", "bubbleTitle", "stampMS",
    "safeColor", "hexRGB", "cssRGB", "relLum", "contrastRatio", "contrastText",
  ]) {
    assert.equal(typeof mod[name], "function", name);
  }
});

/* ---------- HTML escaping ---------- */
test("esc: null and undefined become empty string", () => {
  assert.equal(esc(null), "");
  assert.equal(esc(undefined), "");
});

test("esc: empty and plain text pass through", () => {
  assert.equal(esc(""), "");
  assert.equal(esc("hello"), "hello");
  assert.equal(esc(0), "0");
  assert.equal(esc(42), "42");
});

test("esc: ampersand, angle brackets, double quotes", () => {
  assert.equal(esc("a&b"), "a&amp;b");
  assert.equal(esc("<script>"), "&lt;script&gt;");
  assert.equal(esc("a>b"), "a&gt;b");
  assert.equal(esc('say "hi"'), "say &quot;hi&quot;");
  assert.equal(esc(`a&b<>"'`), "a&amp;b&lt;&gt;&quot;'");
});

test("esc: single quotes are not entity-escaped (browser textContent parity)", () => {
  assert.equal(esc("it's"), "it's");
});

test("esc: ampersand is encoded before angles (no double-encode of entities)", () => {
  assert.equal(esc("&lt;"), "&amp;lt;");
  assert.equal(esc("&amp;"), "&amp;amp;");
});

test("esc: newlines and non-ASCII preserved", () => {
  assert.equal(esc("line\nbreak"), "line\nbreak");
  assert.equal(esc("café"), "café");
});

/* ---------- markdown inline ---------- */
test("mdInline: inline code, links, bold, italic", () => {
  assert.equal(mdInline("`code`"), "<code>code</code>");
  assert.equal(
    mdInline("[x](https://example.com/a)"),
    `<a href="https://example.com/a" target="_blank" rel="noopener">x</a>`,
  );
  assert.equal(mdInline("**test**"), "<strong>test</strong>");
  assert.equal(mdInline("*test*"), "<em>test</em>");
  assert.equal(mdInline("a *b* and **c**"), "a <em>b</em> and <strong>c</strong>");
});

test("mdInline: arithmetic and globs stay literal (flanking guard)", () => {
  assert.equal(mdInline("2 * 3 * 4"), "2 * 3 * 4");
});

test("mdInline: escape-first — raw HTML is neutralized before tags are added", () => {
  assert.equal(mdInline("a <b> tag"), "a &lt;b&gt; tag");
  assert.equal(mdInline("**x <y>**"), "<strong>x &lt;y&gt;</strong>");
  assert.equal(mdInline("`a&b`"), "<code>a&amp;b</code>");
});

test("mdInline: only http(s) links; other schemes stay text", () => {
  assert.equal(mdInline("[x](javascript:alert(1))"), "[x](javascript:alert(1))");
  assert.equal(mdInline("[x](ftp://example.com)"), "[x](ftp://example.com)");
});

test("mdInline: unclosed emphasis and empty input", () => {
  assert.equal(mdInline("*open"), "*open");
  assert.equal(mdInline("**open"), "**open");
  assert.equal(mdInline(""), "");
  assert.equal(mdInline(null), "");
  assert.equal(mdInline(undefined), "");
});

/* ---------- markdown block (md) ---------- */
test("md: paragraphs join lines with <br>; blank line starts new <p>", () => {
  assert.equal(md("line one\nline two"), "<p>line one<br>line two</p>");
  assert.equal(md("solo"), "<p>solo</p>");
  assert.equal(md("p1 l1\np1 l2\n\np2"), "<p>p1 l1<br>p1 l2</p><p>p2</p>");
});

test("md: CR and CRLF normalize to line breaks", () => {
  assert.equal(md("a\rb"), "<p>a<br>b</p>");
  assert.equal(md("a\r\nb"), "<p>a<br>b</p>");
  assert.equal(md("a\r\nb\rc"), "<p>a<br>b<br>c</p>");
});

test("md: headings h1–h3; deeper # stay paragraphs", () => {
  assert.equal(md("# Title"), "<h1>Title</h1>");
  assert.equal(md("## Sub"), "<h2>Sub</h2>");
  assert.equal(md("### Deep"), "<h3>Deep</h3>");
  assert.equal(md("#### Four"), "<p>#### Four</p>");
});

test("md: fenced code blocks escape contents; unclosed fence still emits", () => {
  assert.equal(
    md("```\nconst x = 1 < 2\n```"),
    "<pre><code>const x = 1 &lt; 2</code></pre>",
  );
  assert.equal(
    md("before\n```\ncode & stuff\n```\nafter"),
    "<p>before</p><pre><code>code &amp; stuff</code></pre><p>after</p>",
  );
  assert.equal(
    md("```\nstill open"),
    "<pre><code>still open</code></pre>",
  );
});

test("md: unordered and ordered lists; author numbering preserved", () => {
  assert.equal(md("- a\n- b"), "<ul><li>a</li><li>b</li></ul>");
  assert.equal(md("* a\n* b"), "<ul><li>a</li><li>b</li></ul>");
  assert.equal(
    md("1. one\n2. two"),
    `<ol><li value="1">one</li><li value="2">two</li></ol>`,
  );
  assert.equal(
    md("3. third\n9. ninth"),
    `<ol><li value="3">third</li><li value="9">ninth</li></ol>`,
  );
});

test("md: tables skip separator rows; first row is th, later td", () => {
  assert.equal(
    md("| A | B |\n| --- | --- |\n| 1 | 2 |"),
    "<table><tr><th>A</th><th>B</th></tr><tr><td>1</td><td>2</td></tr></table>",
  );
});

test("md: blockquotes", () => {
  assert.equal(md("> quoted"), "<blockquote>quoted</blockquote>");
  assert.equal(md("> a\n> b"), "<blockquote>a<br>b</blockquote>");
});

test("md: malformed and empty input", () => {
  assert.equal(md(""), "");
  assert.equal(md(null), "");
  assert.equal(md(undefined), "");
  assert.equal(md("a <b> tag\nnext"), "<p>a &lt;b&gt; tag<br>next</p>");
});

test("md: mixed blocks flush correctly", () => {
  const src = "# H\n\npara\n\n- item\n\n```\nx\n```\n\n> q";
  assert.equal(
    md(src),
    "<h1>H</h1><p>para</p><ul><li>item</li></ul><pre><code>x</code></pre><blockquote>q</blockquote>",
  );
});

/* ---------- age / duration ---------- */
test("ageText: boundaries with injected clock", () => {
  const now = 1_000_000_000_000;
  assert.equal(ageText(0, now), "");
  assert.equal(ageText(null, now), "");
  assert.equal(ageText(undefined, now), "");
  assert.equal(ageText(now, now), "now");                 // 0s
  assert.equal(ageText(now - 9_000, now), "now");         // < 10s
  assert.equal(ageText(now - 10_000, now), "10s");
  assert.equal(ageText(now - 59_000, now), "59s");
  assert.equal(ageText(now - 60_000, now), "1m");
  assert.equal(ageText(now - 3_599_000, now), "59m");
  assert.equal(ageText(now - 3_600_000, now), "1h");
  assert.equal(ageText(now - 86_399_000, now), "23h");
  assert.equal(ageText(now - 86_400_000, now), "1d");
  assert.equal(ageText(now - 3 * 86_400_000, now), "3d");
  // future timestamps clamp to "now" (non-negative age)
  assert.equal(ageText(now + 60_000, now), "now");
});

test("fmtDur: boundaries and invalid", () => {
  assert.equal(fmtDur(0), "0m");
  assert.equal(fmtDur(null), "0m");
  assert.equal(fmtDur(undefined), "0m");
  assert.equal(fmtDur(-100), "0m");
  assert.equal(fmtDur(999), "0s");
  assert.equal(fmtDur(1_000), "1s");
  assert.equal(fmtDur(59_000), "59s");
  assert.equal(fmtDur(60_000), "1m");
  assert.equal(fmtDur(3_599_000), "59m");
  assert.equal(fmtDur(3_600_000), "1h 0m");
  assert.equal(fmtDur(3_660_000), "1h 1m");
  assert.equal(fmtDur(86_400_000), "1d 0h");
  assert.equal(fmtDur(90_000_000), "1d 1h");
});

/* ---------- stamps / chat / note times (locale-injected) ---------- */
const LOC = "en-US";
// Fixed UTC instants so locale formatting is deterministic under a fixed locale.
const ISO_DAY = "2024-06-15T14:30:00.000Z";
const ISO_OLD = "2024-01-05T09:15:00.000Z";

test("fmtStamp: absent and invalid", () => {
  assert.equal(fmtStamp(""), "");
  assert.equal(fmtStamp(null), "");
  assert.equal(fmtStamp(undefined), "");
  assert.equal(fmtStamp("not-a-date"), "");
});

test("fmtStamp: locale-injected output is non-empty and stable", () => {
  const a = fmtStamp(ISO_DAY, LOC);
  const b = fmtStamp(ISO_DAY, LOC);
  assert.ok(a.length > 0);
  assert.equal(a, b);
  // en-US day/month digits appear somewhere
  assert.match(a, /\d/);
});

test("fmtWhen: invalid returns raw or empty; valid uses locale", () => {
  // Match inline: new Date("") / new Date(undefined) → Invalid → t || "".
  assert.equal(fmtWhen(""), "");
  assert.equal(fmtWhen(undefined), "");
  assert.equal(fmtWhen("bogus"), "bogus");
  // new Date(null) is epoch 0 (JS Date quirk) — preserve, do not special-case.
  const epoch = fmtWhen(null, LOC);
  assert.ok(epoch.length > 0);
  assert.equal(epoch, new Date(null).toLocaleString(LOC, { dateStyle: "medium", timeStyle: "short" }));
  const w = fmtWhen(ISO_DAY, LOC);
  assert.ok(w.length > 0);
  assert.equal(w, fmtWhen(ISO_DAY, LOC));
});

test("fmtBubbleTime: same day, yesterday, older, invalid", () => {
  // Construct "now" in local timezone so sameDay logic is stable.
  const now = new Date(2024, 5, 15, 18, 0, 0); // local Jun 15 2024 18:00
  const same = new Date(2024, 5, 15, 9, 5, 0).toISOString();
  const yday = new Date(2024, 5, 14, 9, 5, 0).toISOString();
  const older = new Date(2024, 0, 5, 9, 15, 0).toISOString();

  const sameOut = fmtBubbleTime(same, now, LOC);
  assert.ok(!sameOut.includes("Yesterday"));
  assert.match(sameOut, /\d/);

  const yOut = fmtBubbleTime(yday, now, LOC);
  assert.ok(yOut.startsWith("Yesterday, "));

  const oldOut = fmtBubbleTime(older, now, LOC);
  assert.ok(!oldOut.startsWith("Yesterday"));
  assert.match(oldOut, /\d/);

  assert.equal(fmtBubbleTime("nope", now, LOC), "nope");
  assert.equal(fmtBubbleTime("", now, LOC), "");
  // new Date(null) is epoch — older-than-yesterday path under a 2024 "now".
  const nullOut = fmtBubbleTime(null, now, LOC);
  assert.ok(nullOut.length > 0);
  assert.ok(!nullOut.startsWith("Yesterday"));
});

test("fmtNoteMeta: section pluralization and stamp source", () => {
  assert.equal(
    fmtNoteMeta({ section_count: 1, created_at: ISO_DAY }, LOC),
    `1 section · ${fmtWhen(ISO_DAY, LOC)}`,
  );
  assert.equal(
    fmtNoteMeta({ section_count: 0, created_at: ISO_DAY }, LOC),
    `0 sections · ${fmtWhen(ISO_DAY, LOC)}`,
  );
  assert.equal(
    fmtNoteMeta({ section_count: 3, edited_at: ISO_OLD, created_at: ISO_DAY }, LOC),
    `3 sections · ${fmtWhen(ISO_OLD, LOC)}`,
  );
  assert.equal(fmtNoteMeta({}, LOC), "0 sections · ");
});

test("bubbleTitle: role and optional time", () => {
  assert.equal(bubbleTitle("user"), "you");
  assert.equal(bubbleTitle("assistant"), "agent");
  assert.equal(bubbleTitle("other"), "agent");
  const t = bubbleTitle("user", ISO_DAY, LOC);
  assert.equal(t, `you — sent ${fmtWhen(ISO_DAY, LOC)}`);
});

test("stampMS: parse boundaries", () => {
  assert.equal(stampMS(""), 0);
  assert.equal(stampMS(null), 0);
  assert.equal(stampMS(undefined), 0);
  assert.equal(stampMS("not-a-date"), 0);
  assert.equal(stampMS(ISO_DAY), Date.parse(ISO_DAY));
  assert.ok(stampMS(ISO_DAY) > 0);
});

/* ---------- color helpers ---------- */
test("safeColor: accepts well-formed syntax; rejects injection", () => {
  assert.equal(safeColor("#abc"), "#abc");
  assert.equal(safeColor("#aabbcc"), "#aabbcc");
  assert.equal(safeColor("#aabbccdd"), "#aabbccdd");
  assert.equal(safeColor("  #fff  "), "#fff");
  assert.equal(safeColor("var(--lane)"), "var(--lane)");
  assert.equal(safeColor("rgb(1, 2, 3)"), "rgb(1, 2, 3)");
  assert.equal(safeColor("rgba(1,2,3,0.5)"), "rgba(1,2,3,0.5)");
  assert.equal(safeColor("hsl(120, 50%, 50%)"), "hsl(120, 50%, 50%)");
  assert.equal(safeColor("red"), "red");
  assert.equal(safeColor(null), null);
  assert.equal(safeColor(undefined), null);
  assert.equal(safeColor(12), null);
  assert.equal(safeColor(""), null);
  assert.equal(safeColor("#gg"), null);
  assert.equal(safeColor("red; background:url(x)"), null);
  assert.equal(safeColor('red" onload="x'), null);
  assert.equal(safeColor("url(javascript:alert(1))"), null);
  assert.equal(safeColor("var(--x);x"), null);
});

test("hexRGB: 3- and 6-digit hex; rejects others", () => {
  assert.deepEqual(hexRGB("#000"), [0, 0, 0]);
  assert.deepEqual(hexRGB("#fff"), [255, 255, 255]);
  assert.deepEqual(hexRGB("#ABC"), [170, 187, 204]);
  assert.deepEqual(hexRGB("#112233"), [17, 34, 51]);
  assert.equal(hexRGB("#11223344"), null); // 8-digit not expanded here
  assert.equal(hexRGB("#12"), null);
  assert.equal(hexRGB("112233"), null);
  assert.equal(hexRGB("red"), null);
  assert.equal(hexRGB(null), null);
  assert.equal(hexRGB(""), null);
});

test("cssRGB: pure hex path; optional resolveColor inject", () => {
  assert.deepEqual(cssRGB("#000000"), [0, 0, 0]);
  assert.equal(cssRGB("red"), null); // no DOM → no named-color resolution
  assert.deepEqual(cssRGB("red", () => [255, 0, 0]), [255, 0, 0]);
  assert.equal(cssRGB("red", () => null), null);
  // hex wins over injector
  assert.deepEqual(cssRGB("#00ff00", () => [1, 2, 3]), [0, 255, 0]);
});

test("relLum and contrastRatio: WCAG math boundaries", () => {
  assert.equal(relLum([0, 0, 0]), 0);
  assert.ok(Math.abs(relLum([255, 255, 255]) - 1) < 1e-10);
  assert.equal(contrastRatio(1, 0), 21);
  assert.equal(contrastRatio(0, 0), 1);
  assert.equal(contrastRatio(0.5, 0.5), 1);
});

test("contrastText: dark lane gets light label; light gets dark", () => {
  assert.equal(contrastText("#000000"), "#fff");
  assert.equal(contrastText("#ffffff"), "#000");
  assert.equal(contrastText("#000"), "#fff");
  assert.equal(contrastText("#fff"), "#000");
  // unknown / non-hex without injector → light text fallback
  assert.equal(contrastText("not-a-color"), "#fff");
  assert.equal(contrastText("red"), "#fff");
  // injector restores named-color path parity with browser cssRGB
  // pure red is mid-luminance: black text contrasts better than white
  assert.equal(contrastText("red", () => [255, 0, 0]), "#000");
  assert.equal(contrastText("white", () => [255, 255, 255]), "#000");
  assert.equal(contrastText("black", () => [0, 0, 0]), "#fff");
});

test("contrastText: production system palette keeps WCAG label choices", () => {
  assert.equal(contrastText("#007AFF"), "#000", "system blue");
  assert.equal(contrastText("#5856D6"), "#fff", "system indigo");
  assert.equal(contrastText("#FF9500"), "#000", "system orange");
  for (const color of ["#007AFF", "#5856D6", "#FF9500"]) {
    const text = contrastText(color);
    assert.ok(
      contrastRatio(relLum(hexRGB(color)), relLum(hexRGB(text))) >= 4.5,
      `${color} with ${text} must meet WCAG AA`,
    );
  }
});
