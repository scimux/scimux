/* Pure harness-inventory presentation: installed versions, and what upstream
   publishes once the human asks. The check is manual, so every row has to
   read honestly before any upstream answer exists. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { harnessState, harnessRowsHTML, harnessCheckNote } from "../js/harness.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const appSrc = readFileSync(join(__dirname, "../js/app.js"), "utf8");
const indexSrc = readFileSync(join(__dirname, "../index.html"), "utf8");
const harnessSrc = readFileSync(join(__dirname, "../js/harness.js"), "utf8");

test("harness.js is pure; app wires it; index has the section", () => {
  assert.doesNotMatch(harnessSrc, /document\.|innerHTML\s*=/);
  assert.match(appSrc, /from "\.\/harness\.js"/);
  assert.match(indexSrc, /id="m_harnesses"/);
  assert.match(indexSrc, /id="m_hcheck"/);
});

test("state before any upstream check is unchecked, not up-to-date", () => {
  /* The check is a tap. Until it happens we know the installed version and
     nothing else, and "up to date" would be a claim nobody made. */
  const s = harnessState({ agent: "claude", present: true, launchable: true, installed: "2.1.236" }, null);
  assert.equal(s.state, "unchecked");
  assert.equal(s.version, "2.1.236");
  assert.doesNotMatch(s.note, /up to date|available/);
});

test("behind and current are distinguished by the upstream answer", () => {
  const behind = harnessState(
    { agent: "codex", present: true, launchable: true, installed: "0.147.0" },
    { version: "0.153.4", source: "npm @openai/codex" });
  assert.equal(behind.state, "behind");
  assert.match(behind.note, /0\.153\.4/);

  const current = harnessState(
    { agent: "claude", present: true, launchable: true, installed: "2.1.236" },
    { version: "2.1.236", source: "native installer (stable)" });
  assert.equal(current.state, "current");
  assert.match(current.note, /up to date/);
});

test("a native install behind npm is not reported as an update", () => {
  /* The server picks the channel; the browser must not second-guess it with
     a lexical compare of its own. Same version on the channel = current. */
  const s = harnessState(
    { agent: "claude", present: true, launchable: true, installed: "2.1.266" },
    { version: "2.1.236", source: "native installer (stable)" });
  assert.equal(s.state, "current");
});

test("segment-wise compare: 1.0.3 is behind 1.0.24", () => {
  const s = harnessState(
    { agent: "grok", present: true, launchable: true, installed: "1.0.3" },
    { version: "1.0.24", source: "xAI stable channel" });
  assert.equal(s.state, "behind");
});

test("absent, unreadable and present-but-not-launchable each read differently", () => {
  const absent = harnessState({ agent: "opencode", present: false, launchable: false }, null);
  assert.equal(absent.state, "absent");
  assert.match(absent.note, /not installed/i);

  const unreadable = harnessState({ agent: "grok", present: true, launchable: true, installed: "" }, null);
  assert.equal(unreadable.state, "unknown");
  assert.match(unreadable.note, /version/i);

  /* pi installs as `pi` but launches through `pi-acp`: installed, and still
     not offerable. Saying only "installed" would explain nothing about why
     it is missing from the new-activity dialog. */
  const notLaunchable = harnessState(
    { agent: "pi", present: true, launchable: false, installed: "0.84.3" }, null);
  assert.equal(notLaunchable.state, "unlaunchable");
  assert.match(notLaunchable.note, /pi-acp/);
});

test("rows render one item per harness, in server order", () => {
  const html = harnessRowsHTML([
    { agent: "claude", present: true, launchable: true, installed: "2.1.236" },
    { agent: "codex", present: true, launchable: true, installed: "0.147.0" },
    { agent: "opencode", present: false, launchable: false },
  ], { codex: { version: "0.153.4", source: "npm @openai/codex" } });

  assert.equal((html.match(/class="item"/g) || []).length, 3);
  assert.ok(html.indexOf("Claude") < html.indexOf("Codex"), "server order preserved");
  assert.match(html, /2\.1\.236/);
  assert.match(html, /0\.153\.4/);
  assert.match(html, /data-state="behind"/);
  assert.match(html, /data-state="absent"/);
});

test("upstream strings are escaped", () => {
  /* version and source come off the network; they are the one part of a row
     this computer did not author. */
  const html = harnessRowsHTML(
    [{ agent: "codex", present: true, launchable: true, installed: "0.147.0" }],
    { codex: { version: '1.0<img src=x onerror=alert(1)>', source: "<b>npm</b>" } });
  assert.doesNotMatch(html, /<img/);
  assert.doesNotMatch(html, /<b>/);
  assert.match(html, /&lt;img|&lt;/);
});

test("empty inventory renders a note, not an empty box", () => {
  const html = harnessRowsHTML([], null);
  assert.match(html, /no agent/i);
  assert.doesNotMatch(html, /class="item"/);
});

test("the row logo comes from the injected supplier, never from the module", () => {
  /* agentLogo needs the FR-42 assetURL supplier, which only the shell holds;
     a module that imported it would render broken masks under the bootstrap
     loader. No supplier = no logo, not a crash. */
  const html = harnessRowsHTML(
    [{ agent: "claude", present: true, launchable: true, installed: "2.1.236" }],
    null, { agentLogo: a => `<i data-logo="${a}"></i>` });
  assert.match(html, /data-logo="claude"/);

  const bare = harnessRowsHTML(
    [{ agent: "claude", present: true, launchable: true, installed: "2.1.236" }], null);
  assert.match(bare, /class="item"/);
  assert.doesNotMatch(bare, /data-logo/);
});

test("the check note names the sources that did not answer", () => {
  /* A check that half-answered must not read as a clean bill of health: the
     rows for the silent sources still say "unchecked", and that is only
     honest if something says why. */
  const rows = [
    { agent: "claude", present: true, launchable: true, installed: "2.1.236" },
    { agent: "grok", present: true, launchable: true, installed: "1.0.3" },
    { agent: "opencode", present: false, launchable: false },
  ];
  assert.equal(harnessCheckNote(rows, null), "", "no check yet says nothing");
  assert.equal(harnessCheckNote(rows, {
    claude: { version: "2.1.236", source: "native installer (stable)" },
    grok: { version: "1.0.24", source: "xAI stable channel" },
  }), "", "everything answered says nothing");

  const partial = harnessCheckNote(rows, { claude: { version: "2.1.236" } });
  assert.match(partial, /grok/i);
  assert.doesNotMatch(partial, /opencode/i, "an absent harness has no upstream to miss");
});

test("harness rows are listed alphabetically, whatever order the server sent", () => {
  /* The server returns its registry order, which is a code-organisation fact
     the reader has no way to guess. A list a human scans wants one order they
     can predict, and it is the one their eye already uses. */
  const rows = [
    { agent: "claude", present: true, launchable: true, installed: "2.1.267" },
    { agent: "codex", present: true, launchable: true, installed: "0.9.0" },
    { agent: "pi", present: false, launchable: false },
    { agent: "opencode", present: true, launchable: true, installed: "1.2.3" },
    { agent: "grok", present: true, launchable: true, installed: "1.0.24" },
  ];
  const html = harnessRowsHTML(rows, null);
  const order = [...html.matchAll(/data-agent="([^"]+)"/g)].map(m => m[1]);
  assert.deepEqual(order, ["claude", "codex", "grok", "opencode", "pi"]);
});

test("only Claude carries a usage-check switch, and it states the price", () => {
  /* Claude is the one harness whose gauge costs the user's own quota to read,
     so it is the one harness that gets a choice. The others are read for free
     and would only be confused by a switch that gates nothing. */
  const rows = [
    { agent: "claude", present: true, launchable: true, installed: "2.1.267" },
    { agent: "codex", present: true, launchable: true, installed: "0.9.0" },
  ];
  const off = harnessRowsHTML(rows, null, { usageChecks: false });
  assert.match(off, /data-usage-check="claude"/);
  assert.doesNotMatch(off, /data-usage-check="codex"/);
  /* Unchecked, and saying what enabling would cost — a switch a user cannot
     price is a switch they will not touch. */
  assert.doesNotMatch(off, /data-usage-check="claude"[^>]*checked/);
  assert.match(off, /~900 tokens/);
  assert.match(off, /15 minutes/);
  assert.match(off, /nothing is spent/i);

  const on = harnessRowsHTML(rows, null, { usageChecks: true });
  assert.match(on, /data-usage-check="claude"[^>]*checked/);
  assert.match(on, /5-hour limit/);
});

test("an absent claude gets no usage switch", () => {
  /* Consent to spend a quota that cannot be reached is not a choice, it is a
     dead control — the same reason #m_pair stays hidden until pairing could
     complete. */
  const html = harnessRowsHTML([{ agent: "claude", present: false, launchable: false }], null, { usageChecks: false });
  assert.doesNotMatch(html, /data-usage-check/);
});

/* ---------- vendor terms ---------- */

test("every single-vendor harness row links its vendor's terms", () => {
  /* scimux is not a party to any of these agreements and holds no
     credential, so the row is a pointer, not a claim: the user's agreement
     is with the vendor and the row should make that reachable in one tap. */
  const html = harnessRowsHTML([
    { agent: "claude", present: true, launchable: true, installed: "2.1.236" },
    { agent: "codex", present: true, launchable: true, installed: "0.153.4" },
    { agent: "grok", present: true, launchable: true, installed: "1.0.24" },
  ], null);

  assert.match(html, /href="https:\/\/www\.anthropic\.com\/legal\/consumer-terms"/);
  assert.match(html, /href="https:\/\/openai\.com\/policies\/terms-of-use\/"/);
  assert.match(html, /href="https:\/\/x\.ai\/legal\/terms-of-service"/);
  /* Leaving scimux must not navigate away from a live supervision page, and
     an opener handle to a third-party tab is a needless one. */
  assert.equal((html.match(/target="_blank"/g) || []).length, 3);
  assert.equal((html.match(/rel="noopener"/g) || []).length, 3);
});

test("a BYO-provider harness says whose terms apply instead of guessing", () => {
  /* pi and opencode hold no model: the binding terms are whichever provider
     key the user configured, which scimux cannot read. A link here would name
     the wrong vendor, which is worse than naming none. */
  const html = harnessRowsHTML([
    { agent: "pi", present: true, launchable: true, installed: "0.85.1" },
    { agent: "opencode", present: true, launchable: true, installed: "1.2.3" },
  ], null);

  assert.doesNotMatch(html, /href="http/);
  assert.match(html, /your model provider/i);
});

test("an absent harness still shows whose terms it would be under", () => {
  /* The row is a reference, not an action: a user deciding whether to install
     Codex is exactly the reader who wants the terms first. */
  const html = harnessRowsHTML([{ agent: "codex", present: false, launchable: false }], null);
  assert.match(html, /openai\.com\/policies\/terms-of-use/);
});
