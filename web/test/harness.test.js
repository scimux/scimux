/* Pure harness-inventory presentation: installed versions, and what upstream
   publishes once the human asks. The check is manual, so every row has to
   read honestly before any upstream answer exists. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  harnessState, harnessRowsHTML, harnessCheckNote,
  museConsentNote, computerSettingOn, createSettingsController,
  openAttachmentSettings, applyExternalAttachmentChange,
  createScimuxUpdateCheck, runUpdateChecks,
} from "../js/harness.js";

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

test("combined update check starts both lanes and preserves partial results", async () => {
  let startedHarness = false, startedScimux = false;
  let releaseHarness;
  const harness = new Promise(resolve => { releaseHarness = resolve; });
  const pending = runUpdateChecks({
    harness: async () => {
      startedHarness = true;
      return harness;
    },
    scimux: async () => {
      startedScimux = true;
      throw new Error("release feed down");
    },
  });
  await Promise.resolve();
  assert.equal(startedHarness, true);
  assert.equal(startedScimux, true);

  releaseHarness({
    latest: { grok: { version: "1.0.25" } },
    agents: { grok: { models: ["grok-4.7"] } },
    harnesses: [{ agent: "grok", installed: "1.0.24" }],
  });
  const got = await pending;
  assert.equal(got.harness.status, "fulfilled");
  assert.equal(got.harness.value.agents.grok.models[0], "grok-4.7");
  assert.equal(got.scimux.status, "rejected");
});

test("scimux update controller owns check state, errors, and button lifetime", async () => {
  const events = [];
  const button = { disabled: false };
  const success = createScimuxUpdateCheck({
    read: async () => ({ current: "1.0", latest: "2.0", available: true }),
    checking: () => events.push("checking"),
    result: info => events.push(`result:${info.latest}`),
    failed: () => events.push("failed"),
  });
  const info = await success.runButton(button);
  assert.equal(info.latest, "2.0");
  assert.equal(success.current(), info);
  assert.equal(button.disabled, false);
  assert.deepEqual(events, ["checking", "result:2.0"]);

  const failure = createScimuxUpdateCheck({
    read: async () => { throw new Error("feed down"); },
    checking: () => events.push("checking-failure"),
    result: () => events.push("unexpected-result"),
    failed: () => events.push("failed"),
  });
  await assert.rejects(failure.check(), /feed down/);
  assert.equal(failure.current(), null);
  assert.deepEqual(events.slice(-2), ["checking-failure", "failed"]);

  const quiet = createScimuxUpdateCheck({ read: async () => ({ latest: "2.1" }) });
  assert.equal((await quiet.runButton(button)).latest, "2.1");
  const quietFailure = createScimuxUpdateCheck({
    read: async () => { throw new Error("still down"); },
  });
  assert.equal(await quietFailure.runButton(button), null);
  assert.equal(button.disabled, false);
});

test("overlapping scimux checks share one request and one result", async () => {
  let reads = 0, release;
  const response = new Promise(resolve => { release = resolve; });
  const events = [];
  const check = createScimuxUpdateCheck({
    read: () => { reads++; return response; },
    checking: () => events.push("checking"),
    result: info => events.push(`result:${info.latest}`),
    failed: () => events.push("failed"),
  });

  const older = check.check();
  const newer = check.check();
  assert.equal(reads, 1);
  assert.deepEqual(events, ["checking"]);

  release({ current: "1.0", latest: "2.0", available: true });
  const [a, b] = await Promise.all([older, newer]);
  assert.equal(a, b);
  assert.equal(check.current(), a);
  assert.deepEqual(events, ["checking", "result:2.0"]);

  await check.check();
  assert.equal(reads, 2, "a later explicit check must start a fresh request");
});

test("state before any upstream check is unchecked, not up-to-date", () => {
  /* The check is a tap. Until it happens we know the installed version and
     nothing else, and "up to date" would be a claim nobody made. */
  const s = harnessState({ agent: "claude", present: true, launchable: true, installed: "2.1.236" }, null);
  assert.equal(s.state, "unchecked");
  assert.equal(s.version, "2.1.236");
  assert.doesNotMatch(s.note, /up to date|available/);
});

test("a row without a public version source has a distinct honest state", () => {
  const noSource = harnessState({
    agent: "future-agent", present: true, launchable: true,
    installed: "1.2.3", has_source: false,
  }, null);
  assert.equal(noSource.state, "no-source");
  assert.equal(noSource.version, "1.2.3");
  assert.match(noSource.note, /no public version channel/i);
  assert.doesNotMatch(noSource.note, /missing|unchecked|no upstream answer/i);
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

  const withoutSource = harnessCheckNote([
    ...rows,
    { agent: "cursor", present: true, launchable: true,
      installed: "2026.09.15-d2fe57e", has_source: false },
  ], { claude: { version: "2.1.236" } });
  assert.match(withoutSource, /grok/i);
  assert.doesNotMatch(withoutSource, /cursor/i,
    "a harness with no source is not a failed upstream answer");
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
    { agent: "cursor", present: true, launchable: true, installed: "2026.09.15-d2fe57e" },
  ], null);

  assert.match(html, /href="https:\/\/www\.anthropic\.com\/legal\/consumer-terms"/);
  assert.match(html, /href="https:\/\/openai\.com\/policies\/terms-of-use\/"/);
  assert.match(html, /href="https:\/\/x\.ai\/legal\/terms-of-service"/);
  assert.match(html, /href="https:\/\/cursor\.com\/terms-of-service"/);
  /* Leaving scimux must not navigate away from a live supervision page, and
     an opener handle to a third-party tab is a needless one. */
  assert.equal((html.match(/target="_blank"/g) || []).length, 8);
  assert.equal((html.match(/rel="noopener"/g) || []).length, 4);
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
  assert.match(html, /configured model provider/i);
});

test("an absent harness still shows whose terms it would be under", () => {
  /* The row is a reference, not an action: a user deciding whether to install
     Codex is exactly the reader who wants the terms first. */
  const html = harnessRowsHTML([{ agent: "codex", present: false, launchable: false }], null);
  assert.match(html, /openai\.com\/policies\/terms-of-use/);
});

/* ---------- Phase 6: Muse approval-judge consent ---------- */

test("computerSettingOn is true only for the boolean true", () => {
  assert.equal(computerSettingOn(true), true);
  assert.equal(computerSettingOn(false), false);
  assert.equal(computerSettingOn(undefined), false);
  assert.equal(computerSettingOn(null), false);
  assert.equal(computerSettingOn(""), false);
  assert.equal(computerSettingOn("true"), false);
  assert.equal(computerSettingOn(1), false);
  assert.equal(computerSettingOn({}), false);
});

test("museConsentNote separates token consent from blocking approvals", () => {
  const off = museConsentNote(false);
  assert.equal(off,
    "Off — You cannot start a Muse session. Turn this on to use Muse. Muse " +
    "may use part of your plan's usage limit when it checks whether tool " +
    "actions are safe.");

  const on = museConsentNote(true);
  assert.equal(on,
    "On — You can start Muse sessions. Muse may use part of your plan's " +
    "usage limit to check tool actions. Actions that still need approval " +
    "stay paused until they are allowed or rejected.");
  assert.doesNotMatch(off + on, /approval judge|subscription tokens/i);
});

test("only a present Muse row gets an active consent switch", () => {
  const present = harnessRowsHTML([
    { agent: "muse", present: true, launchable: true, installed: "0.1.0" },
    { agent: "claude", present: true, launchable: true, installed: "2.1.267" },
  ], null, { museConsent: false, usageChecks: false });
  assert.match(present, /data-muse-consent="muse"/);
  assert.doesNotMatch(present, /data-muse-consent="claude"/);
  assert.doesNotMatch(present, /data-muse-consent="muse"[^>]*checked/);
  assert.match(present, /You cannot start a Muse session/);
  assert.match(present, /> Enable Muse<\/label>/);
  assert.doesNotMatch(present, /Approval-judge consent/);

  const on = harnessRowsHTML([
    { agent: "muse", present: true, launchable: true, installed: "0.1.0" },
  ], null, { museConsent: true });
  assert.match(on, /data-muse-consent="muse"[^>]*checked/);
	assert.match(on, /Actions that still need approval stay paused/);

  const absent = harnessRowsHTML([
    { agent: "muse", present: false, launchable: false },
  ], null, { museConsent: true });
  assert.doesNotMatch(absent, /data-muse-consent/);
  assert.doesNotMatch(absent, /<input type="checkbox"/);
  assert.match(absent, /You cannot start a Muse session|not installed/);
});

test("Muse consent is not coupled to auto-approve or Claude usage checks", () => {
  const html = harnessRowsHTML([
    { agent: "muse", present: true, launchable: true, installed: "0.1.0" },
    { agent: "claude", present: true, launchable: true, installed: "2.1.267" },
  ], null, { museConsent: true, usageChecks: false });
  assert.match(html, /data-muse-consent="muse"[^>]*checked/);
  assert.doesNotMatch(html, /data-usage-check="claude"[^>]*checked/);
  assert.doesNotMatch(html, /auto-approve/i);
  assert.doesNotMatch(harnessSrc, /auto-approve/);
});

test("six harness rows render alphabetically and installed Muse is launchable", () => {
  const rows = [
    { agent: "claude", present: true, launchable: true, installed: "2.1.267" },
    { agent: "codex", present: true, launchable: true, installed: "0.9.0" },
    { agent: "pi", present: false, launchable: false },
    { agent: "opencode", present: true, launchable: true, installed: "1.2.3" },
    { agent: "grok", present: true, launchable: true, installed: "1.0.24" },
    { agent: "muse", present: true, launchable: true, installed: "0.1.0" },
  ];
  const html = harnessRowsHTML(rows, null);
  const order = [...html.matchAll(/data-agent="([^"]+)"/g)].map(m => m[1]);
  assert.deepEqual(order, ["claude", "codex", "grok", "muse", "opencode", "pi"]);
  assert.equal((html.match(/class="item"/g) || []).length, 6);
  const museState = harnessState(rows[5], null);
  assert.equal(museState.state, "unchecked");
  assert.equal(museState.note, "");
  assert.doesNotMatch(html, /Muse model policy is unavailable/);
  assert.doesNotMatch(html, /muse is missing/);
  assert.match(html, /You cannot start a Muse session/);
});

test("absent Muse is not installed; omitted latest stays unchecked; Meta terms remain accessible", () => {
  const absent = harnessState({ agent: "muse", present: false, launchable: false }, null);
  assert.equal(absent.state, "absent");
  assert.match(absent.note, /not installed/i);

  const present = harnessState(
    { agent: "muse", present: true, launchable: true, installed: "0.1.0" }, null);
  assert.equal(present.state, "unchecked");
  assert.doesNotMatch(present.note, /up to date/);

  const html = harnessRowsHTML([
    { agent: "muse", present: true, launchable: true, installed: "0.1.0" },
  ], {});
  assert.doesNotMatch(html, /up to date/);
  assert.match(html, /href="https:\/\/dev\.meta\.ai\/legal\/terms-of-service"/);
  assert.match(html, /href="https:\/\/www\.facebook\.com\/privacy\/policy\/"/);
  assert.doesNotMatch(indexSrc, /five registries/);
  assert.doesNotMatch(appSrc, /five registries/);
});

test("app settings wiring reads and writes computer-owned flags without clobbering", () => {
  assert.match(appSrc, /from "\.\/harness\.js"/);
  assert.match(appSrc, /createSettingsController/);
  assert.match(appSrc, /data-muse-consent/);
  assert.match(harnessSrc, /muse_approval_judge_consent/);
  assert.match(harnessSrc, /claude_usage_checks/);
  assert.match(harnessSrc, /allow_external_attachments/);
  assert.doesNotMatch(harnessSrc, /document\.|innerHTML\s*=/);
});

function makeSettingsHarness(opts = {}){
  const renders = [];
  const writes = [];
  const reads = [];
  let readImpl = opts.read || (async () => ({}));
  let writeImpl = opts.write || (async body => ({ ...body }));
  const controller = createSettingsController({
    read: async () => {
      reads.push("read");
      return readImpl();
    },
    write: async body => {
      writes.push({ ...body });
      return writeImpl(body);
    },
    render: state => { renders.push({
      claude_usage_checks: !!state.claude_usage_checks,
      muse_approval_judge_consent: !!state.muse_approval_judge_consent,
      allow_external_attachments: !!state.allow_external_attachments,
    }); },
  });
  return {
    controller, renders, writes, reads,
    setRead(fn){ readImpl = fn; },
    setWrite(fn){ writeImpl = fn; },
    last(){ return renders[renders.length - 1]; },
  };
}

async function micro(){
  for (let i = 0; i < 12; i++) await Promise.resolve();
}

test("settings read: booleans, missing, and malformed values fail closed", async () => {
  const h = makeSettingsHarness({
    read: async () => ({ claude_usage_checks: true, muse_approval_judge_consent: true }),
  });
  await h.controller.load();
  assert.deepEqual(h.last(), { claude_usage_checks: true, muse_approval_judge_consent: true, allow_external_attachments: false });

  const cases = [
    { claude_usage_checks: false, muse_approval_judge_consent: false },
    {},
    { claude_usage_checks: "true", muse_approval_judge_consent: 1 },
    { claude_usage_checks: null, muse_approval_judge_consent: [] },
    { claude_usage_checks: {}, muse_approval_judge_consent: { on: true } },
  ];
  for (const raw of cases){
    const c = makeSettingsHarness({ read: async () => raw });
    await c.controller.load();
    assert.deepEqual(c.last(), { claude_usage_checks: false, muse_approval_judge_consent: false, allow_external_attachments: false }, JSON.stringify(raw));
  }
});

test("settings read: failed read is both off, including after a previous true", async () => {
  let impl = async () => ({ claude_usage_checks: true, muse_approval_judge_consent: true });
  const h = makeSettingsHarness({ read: () => impl() });
  await h.controller.load();
  assert.equal(h.last().muse_approval_judge_consent, true);
  impl = async () => { throw new Error("down"); };
  await h.controller.load();
  assert.deepEqual(h.last(), { claude_usage_checks: false, muse_approval_judge_consent: false, allow_external_attachments: false });
});

test("stale settings read cannot overwrite a newer result", async () => {
  let release1, release2;
  const pRead1 = new Promise(r => { release1 = r; });
  const pRead2 = new Promise(r => { release2 = r; });
  let n = 0;
  const h = makeSettingsHarness({
    read: () => { n++; return n === 1 ? pRead1 : pRead2; },
  });
  const p1 = h.controller.load();
  const p2 = h.controller.load();
  await micro();
  assert.equal(n, 1, "the second read waits behind the first");
  release1({ claude_usage_checks: true, muse_approval_judge_consent: true });
  await p1;
  await micro();
  assert.equal(n, 2);
  release2({ claude_usage_checks: false, muse_approval_judge_consent: false });
  await p2;
  assert.deepEqual(h.last(), { claude_usage_checks: false, muse_approval_judge_consent: false, allow_external_attachments: false });
});

test("Muse consent write sends only that field and follows the server response", async () => {
  const h = makeSettingsHarness({
    read: async () => ({ claude_usage_checks: true, muse_approval_judge_consent: false }),
    write: async body => ({ claude_usage_checks: true, ...body }),
  });
  await h.controller.load();
  await h.controller.setMuseConsent(true);
  assert.deepEqual(h.writes, [{ muse_approval_judge_consent: true }]);
  assert.equal("claude_usage_checks" in h.writes[0], false);
  assert.equal(h.last().muse_approval_judge_consent, true);
  assert.equal(h.last().claude_usage_checks, true);
});

test("Muse consent write: missing/malformed field fails safe; failed write restores", async () => {
  const h = makeSettingsHarness({
    read: async () => ({ muse_approval_judge_consent: false, claude_usage_checks: true }),
    write: async () => ({ ok: true }),
  });
  await h.controller.load();
  await h.controller.setMuseConsent(true);
  assert.equal(h.last().muse_approval_judge_consent, false);
  assert.equal(h.last().claude_usage_checks, true);

  h.setWrite(async () => ({ muse_approval_judge_consent: "yes", claude_usage_checks: true }));
  await h.controller.setMuseConsent(true);
  assert.equal(h.last().muse_approval_judge_consent, false);

  const confirmed = h.last();
  h.setWrite(async () => { throw new Error("nope"); });
  await h.controller.setMuseConsent(true);
  assert.deepEqual(h.last(), confirmed);
});

test("Claude usage write sends only that field and does not clobber Muse", async () => {
  const h = makeSettingsHarness({
    read: async () => ({ claude_usage_checks: false, muse_approval_judge_consent: true }),
    write: async body => ({ muse_approval_judge_consent: true, ...body }),
  });
  await h.controller.load();
  await h.controller.setClaudeUsage(true);
  assert.deepEqual(h.writes, [{ claude_usage_checks: true }]);
  assert.equal("muse_approval_judge_consent" in h.writes[0], false);
  assert.equal(h.last().claude_usage_checks, true);
  assert.equal(h.last().muse_approval_judge_consent, true);
});

test("overlapping settings writes are generation-guarded; stale true cannot land", async () => {
  let releaseTrue, releaseFalse;
  const trueP = new Promise(r => { releaseTrue = r; });
  const falseP = new Promise(r => { releaseFalse = r; });
  let n = 0;
  const h = makeSettingsHarness({
    read: async () => ({ claude_usage_checks: false, muse_approval_judge_consent: false }),
    write: async body => {
      n++;
      return n === 1 ? trueP : falseP;
    },
  });
  await h.controller.load();
  const pTrue = h.controller.setMuseConsent(true);
  const pFalse = h.controller.setMuseConsent(false);
  await micro();
  assert.equal(n, 1, "second write waits behind the first");
  releaseTrue({ muse_approval_judge_consent: true, claude_usage_checks: false });
  await pTrue;
  await micro();
  assert.equal(n, 2);
  releaseFalse({ muse_approval_judge_consent: false, claude_usage_checks: false });
  await pFalse;
  assert.equal(h.last().muse_approval_judge_consent, false);
  assert.equal(h.last().claude_usage_checks, false);
});

test("failed settings write does not become an unhandled rejection", async () => {
  const h = makeSettingsHarness({
    write: async () => { throw new Error("boom"); },
  });
  await h.controller.setClaudeUsage(true);
  assert.deepEqual(h.last(), { claude_usage_checks: false, muse_approval_judge_consent: false, allow_external_attachments: false });
});

test("settings writes serialize: true then false ends false and invokes writes in order", async () => {
  let release1, release2;
  const w1 = new Promise(r => { release1 = r; });
  const w2 = new Promise(r => { release2 = r; });
  let n = 0;
  const h = makeSettingsHarness({
    read: async () => ({ claude_usage_checks: false, muse_approval_judge_consent: false }),
    write: async body => { n++; return n === 1 ? w1 : w2; },
  });
  await h.controller.load();
  const pTrue = h.controller.setMuseConsent(true);
  const pFalse = h.controller.setMuseConsent(false);
  await micro();
  assert.equal(n, 1);
  assert.deepEqual(h.writes, [{ muse_approval_judge_consent: true }]);
  assert.equal(h.controller.getState().muse_approval_judge_consent, false);
  release1({ muse_approval_judge_consent: true });
  await pTrue;
  await micro();
  assert.equal(n, 2);
  assert.deepEqual(h.writes, [
    { muse_approval_judge_consent: true },
    { muse_approval_judge_consent: false },
  ]);
  release2({ muse_approval_judge_consent: false });
  const snap = await pFalse;
  assert.equal(snap.muse_approval_judge_consent, false);
  assert.equal(h.controller.getState().muse_approval_judge_consent, false);
  assert.equal(h.last().muse_approval_judge_consent, false);
  snap.muse_approval_judge_consent = true;
  assert.equal(h.controller.getState().muse_approval_judge_consent, false);
});

test("settings writes serialize: false then true ends true", async () => {
  let release1, release2;
  const w1 = new Promise(r => { release1 = r; });
  const w2 = new Promise(r => { release2 = r; });
  let n = 0;
  const h = makeSettingsHarness({
    read: async () => ({ claude_usage_checks: false, muse_approval_judge_consent: true }),
    write: async () => { n++; return n === 1 ? w1 : w2; },
  });
  await h.controller.load();
  const pFalse = h.controller.setMuseConsent(false);
  const pTrue = h.controller.setMuseConsent(true);
  await micro();
  assert.equal(n, 1);
  release1({ muse_approval_judge_consent: false });
  await pFalse;
  await micro();
  assert.equal(n, 2);
  release2({ muse_approval_judge_consent: true });
  await pTrue;
  assert.equal(h.controller.getState().muse_approval_judge_consent, true);
  assert.equal(h.last().muse_approval_judge_consent, true);
});

test("cross-setting writes serialize Muse then Claude", async () => {
  let releaseMuse, releaseClaude;
  const museP = new Promise(r => { releaseMuse = r; });
  const claudeP = new Promise(r => { releaseClaude = r; });
  const started = [];
  const h = makeSettingsHarness({
    read: async () => ({ claude_usage_checks: false, muse_approval_judge_consent: true }),
    write: async body => {
      started.push({ ...body });
      return ("muse_approval_judge_consent" in body) ? museP : claudeP;
    },
  });
  await h.controller.load();
  const pMuse = h.controller.setMuseConsent(false);
  const pClaude = h.controller.setClaudeUsage(true);
  await micro();
  assert.deepEqual(started, [{ muse_approval_judge_consent: false }]);
  releaseMuse({ muse_approval_judge_consent: false, claude_usage_checks: false });
  await pMuse;
  await micro();
  assert.deepEqual(started, [
    { muse_approval_judge_consent: false },
    { claude_usage_checks: true },
  ]);
  releaseClaude({ claude_usage_checks: true, muse_approval_judge_consent: false });
  await pClaude;
  assert.deepEqual(h.controller.getState(), {
    claude_usage_checks: true,
    muse_approval_judge_consent: false,
    allow_external_attachments: false,
  });
});

test("cross-setting writes serialize Claude then Muse", async () => {
  let releaseClaude, releaseMuse;
  const claudeP = new Promise(r => { releaseClaude = r; });
  const museP = new Promise(r => { releaseMuse = r; });
  const started = [];
  const h = makeSettingsHarness({
    read: async () => ({ claude_usage_checks: true, muse_approval_judge_consent: false }),
    write: async body => {
      started.push({ ...body });
      return ("claude_usage_checks" in body) ? claudeP : museP;
    },
  });
  await h.controller.load();
  const pClaude = h.controller.setClaudeUsage(false);
  const pMuse = h.controller.setMuseConsent(true);
  await micro();
  assert.deepEqual(started, [{ claude_usage_checks: false }]);
  releaseClaude({ claude_usage_checks: false, muse_approval_judge_consent: false });
  await pClaude;
  await micro();
  assert.deepEqual(started, [
    { claude_usage_checks: false },
    { muse_approval_judge_consent: true },
  ]);
  releaseMuse({ claude_usage_checks: false, muse_approval_judge_consent: true });
  await pMuse;
  assert.deepEqual(h.controller.getState(), {
    claude_usage_checks: false,
    muse_approval_judge_consent: true,
    allow_external_attachments: false,
  });
});

test("a rejected queued write does not poison later writes", async () => {
  let n = 0;
  let release2;
  const w2 = new Promise(r => { release2 = r; });
  const h = makeSettingsHarness({
    read: async () => ({ claude_usage_checks: false, muse_approval_judge_consent: false }),
    write: async body => {
      n++;
      if (n === 1) throw new Error("first failed");
      return w2;
    },
  });
  await h.controller.load();
  const p1 = h.controller.setMuseConsent(true);
  const p2 = h.controller.setMuseConsent(false);
  const s1 = await p1;
  assert.equal(s1.muse_approval_judge_consent, false);
  await micro();
  assert.equal(n, 2);
  release2({ muse_approval_judge_consent: false });
  const s2 = await p2;
  assert.equal(s2.muse_approval_judge_consent, false);
  assert.equal(h.controller.getState().muse_approval_judge_consent, false);
});

test("a failed external-attachment save re-renders the last confirmed off state", async () => {
  const h = makeSettingsHarness({
    read: async () => ({ allow_external_attachments: false }),
    write: async () => { throw new Error("disk full"); },
  });
  await h.controller.load();
  const state = await h.controller.setExternalAttachments(true);
  assert.equal(state.allow_external_attachments, false);
  assert.equal(h.last().allow_external_attachments, false);
  assert.deepEqual(h.writes, [{ allow_external_attachments: true }]);
});

test("attachment settings helpers navigate and forward the checkbox value", async () => {
  let clicks = 0, scroll;
  openAttachmentSettings({ click(){ clicks++; } }, { scrollIntoView(opts){ scroll = opts; } });
  assert.equal(clicks, 1);
  assert.deepEqual(scroll, { block: "center" });
  openAttachmentSettings(null, null);
  let value;
  const result = await applyExternalAttachmentChange({
    setExternalAttachments(v){ value = v; return "saved"; },
  }, { target: { checked: true } });
  assert.equal(value, true);
  assert.equal(result, "saved");
});

test("writes wait for a pending read; a later read waits for a pending write", async () => {
  let releaseRead1, releaseRead2, releaseWrite;
  const read1 = new Promise(r => { releaseRead1 = r; });
  const read2 = new Promise(r => { releaseRead2 = r; });
  const writeP = new Promise(r => { releaseWrite = r; });
  let reads = 0, writes = 0;
  const h = makeSettingsHarness({
    read: async () => { reads++; return reads === 1 ? read1 : read2; },
    write: async () => { writes++; return writeP; },
  });
  const pLoad = h.controller.load();
  const pWrite = h.controller.setMuseConsent(true);
  await micro();
  assert.equal(reads, 1);
  assert.equal(writes, 0);
  releaseRead1({ claude_usage_checks: false, muse_approval_judge_consent: false });
  await pLoad;
  await micro();
  assert.equal(writes, 1);
  const pLoad2 = h.controller.load();
  await micro();
  assert.equal(reads, 1, "second read waits behind the in-flight write");
  releaseWrite({ muse_approval_judge_consent: true });
  await pWrite;
  await micro();
  assert.equal(reads, 2);
  releaseRead2({ claude_usage_checks: false, muse_approval_judge_consent: true });
  await pLoad2;
  assert.equal(h.controller.getState().muse_approval_judge_consent, true);
});

test("failed read at queue head turns both off; a later write still runs", async () => {
  let rejectRead, releaseWrite;
  const readP = new Promise((_, rej) => { rejectRead = rej; });
  const writeP = new Promise(r => { releaseWrite = r; });
  let writes = 0;
  const h = makeSettingsHarness({
    read: async () => readP,
    write: async body => { writes++; return writeP; },
  });
  const pLoad = h.controller.load();
  const pWrite = h.controller.setMuseConsent(true);
  await micro();
  assert.equal(writes, 0);
  rejectRead(new Error("down"));
  await pLoad;
  assert.deepEqual(h.controller.getState(), {
    claude_usage_checks: false, muse_approval_judge_consent: false,
    allow_external_attachments: false,
  });
  await micro();
  assert.equal(writes, 1);
  releaseWrite({ muse_approval_judge_consent: true });
  await pWrite;
  assert.equal(h.controller.getState().muse_approval_judge_consent, true);
});

test("admitting a Muse enable immediately repaints confirmed false", async () => {
  let release;
  const pending = new Promise(r => { release = r; });
  const h = makeSettingsHarness({
    read: async () => ({ claude_usage_checks: false, muse_approval_judge_consent: false }),
    write: async () => pending,
  });
  await h.controller.load();
  const before = h.renders.length;
  const p = h.controller.setMuseConsent(true);
  assert.equal(h.controller.getState().muse_approval_judge_consent, false);
  assert.ok(h.renders.length > before);
  assert.equal(h.last().muse_approval_judge_consent, false);
  release({ muse_approval_judge_consent: true });
  await p;
  assert.equal(h.last().muse_approval_judge_consent, true);
});

test("admitting a Muse disable immediately repaints confirmed true", async () => {
  let release;
  const pending = new Promise(r => { release = r; });
  const h = makeSettingsHarness({
    read: async () => ({ claude_usage_checks: false, muse_approval_judge_consent: true }),
    write: async () => pending,
  });
  await h.controller.load();
  const before = h.renders.length;
  const p = h.controller.setMuseConsent(false);
  assert.equal(h.controller.getState().muse_approval_judge_consent, true);
  assert.ok(h.renders.length > before);
  assert.equal(h.last().muse_approval_judge_consent, true);
  release({ muse_approval_judge_consent: false });
  await p;
  assert.equal(h.last().muse_approval_judge_consent, false);
});

test("queued second write never renders its requested value before its response", async () => {
  let release1, release2;
  const w1 = new Promise(r => { release1 = r; });
  const w2 = new Promise(r => { release2 = r; });
  let n = 0;
  const h = makeSettingsHarness({
    read: async () => ({ claude_usage_checks: false, muse_approval_judge_consent: false }),
    write: async () => { n++; return n === 1 ? w1 : w2; },
  });
  await h.controller.load();
  const p1 = h.controller.setMuseConsent(true);
  const p2 = h.controller.setMuseConsent(true);
  assert.equal(h.last().muse_approval_judge_consent, false);
  await micro();
  assert.equal(h.last().muse_approval_judge_consent, false);
  release1({ muse_approval_judge_consent: true });
  await p1;
  assert.equal(h.last().muse_approval_judge_consent, true);
  const p2admit = h.last();
  await micro();
  /* Second write is now in flight; still only server-confirmed true. */
  assert.equal(h.controller.getState().muse_approval_judge_consent, true);
  release2({ muse_approval_judge_consent: true });
  await p2;
  assert.equal(h.last().muse_approval_judge_consent, true);
  assert.equal(p2admit.muse_approval_judge_consent, true);
});

test("rejected enable never renders enabled; rejected disable never renders disabled", async () => {
  const h = makeSettingsHarness({
    read: async () => ({ claude_usage_checks: false, muse_approval_judge_consent: false }),
    write: async () => { throw new Error("no"); },
  });
  await h.controller.load();
  await h.controller.setMuseConsent(true);
  assert.equal(h.last().muse_approval_judge_consent, false);
  assert.ok(h.renders.every(r => r.muse_approval_judge_consent === false));

  const h2 = makeSettingsHarness({
    read: async () => ({ claude_usage_checks: false, muse_approval_judge_consent: true }),
    write: async () => { throw new Error("no"); },
  });
  await h2.controller.load();
  await h2.controller.setMuseConsent(false);
  assert.equal(h2.last().muse_approval_judge_consent, true);
  assert.ok(h2.renders.every(r => r.muse_approval_judge_consent === true));
});

test("eight harness rows render alphabetically with dsh between cursor and grok", () => {
  /* Adding a harness must not disturb the one order a reader can predict.
     dsh sorts on its own lowercase name, as pi and opencode do. */
  const rows = [
    { agent: "claude", present: true, launchable: true, installed: "2.1.267" },
    { agent: "codex", present: true, launchable: true, installed: "0.9.0" },
    { agent: "pi", present: false, launchable: false },
    { agent: "opencode", present: true, launchable: true, installed: "1.2.3" },
    { agent: "grok", present: true, launchable: true, installed: "1.0.24" },
    { agent: "cursor", present: true, launchable: true, installed: "2026.09.15-d2fe57e" },
    { agent: "muse", present: true, launchable: true, installed: "0.1.0" },
    { agent: "dsh", present: true, launchable: true, installed: "0.1.5-rc.1" },
  ];
  const html = harnessRowsHTML(rows, null);
  const order = [...html.matchAll(/data-agent="([^"]+)"/g)].map(m => m[1]);
  assert.deepEqual(order, ["claude", "codex", "cursor", "dsh", "grok", "muse", "opencode", "pi"]);
  assert.equal((html.match(/class="item"/g) || []).length, 8);
});

test("Mistral Vibe sorts between Grok and Muse and carries no terms link", () => {
  const rows = [
    { agent: "claude", present: true, launchable: true, installed: "2.1.267" },
    { agent: "codex", present: true, launchable: true, installed: "0.9.0" },
    { agent: "pi", present: false, launchable: false },
    { agent: "opencode", present: true, launchable: true, installed: "1.2.3" },
    { agent: "grok", present: true, launchable: true, installed: "1.0.24" },
    { agent: "cursor", present: true, launchable: true, installed: "2026.09.15-d2fe57e", has_source: false },
    { agent: "muse", present: true, launchable: true, installed: "0.1.0" },
    { agent: "dsh", present: true, launchable: true, installed: "0.1.5-rc.1" },
    { agent: "vibe", present: true, launchable: true, installed: "1.2.3", has_source: true },
  ];
  const html = harnessRowsHTML(rows, null);
  const order = [...html.matchAll(/data-agent="([^"]+)"/g)].map(m => m[1]);
  assert.deepEqual(order, ["claude", "codex", "cursor", "dsh", "grok", "vibe", "muse", "opencode", "pi"]);
  const vibeAt = html.indexOf('data-agent="vibe"');
  assert.ok(vibeAt >= 0);
  const vibe = html.slice(vibeAt, html.indexOf("</div>", vibeAt) + "</div>".length);
  assert.match(vibe, / Mistral Vibe</);
  assert.doesNotMatch(vibe, /no public version channel/);
  assert.doesNotMatch(vibe, /href=/);
  assert.doesNotMatch(vibe, /Terms of Service|Terms of Use|Terms and privacy/i);
});

test("Vibe's unknown version says local metadata was unavailable", () => {
  const s = harnessState({ agent: "vibe", present: true, launchable: true, installed: "" }, null);
  assert.equal(s.state, "unknown");
  assert.equal(s.note, "installed; version metadata unavailable");
});

test("Vibe update text shows only the available version", () => {
  const row = { agent: "vibe", present: true, launchable: true, installed: "2.25.0", has_source: true };
  const newer = harnessState(row, { version: "2.25.8", source: "PyPI mistral-vibe" });
  assert.equal(newer.state, "behind");
  assert.equal(newer.note, "2.25.8 available");
  const current = harnessState(row, { version: "2.25.0", source: "PyPI mistral-vibe" });
  assert.equal(current.state, "current");
  assert.equal(current.note, "up to date");
  const other = harnessState({ ...row, agent: "codex" }, { version: "2.25.8", source: "npm codex" });
  assert.match(other.note, /npm codex/);
  const html = harnessRowsHTML([row], { vibe: { version: "2.25.8", source: "PyPI mistral-vibe" } });
  assert.match(html, /2\.25\.8 available/);
  assert.doesNotMatch(html, /PyPI/);
});

test("an installed dsh reads as unchecked with its prerelease version intact", () => {
  /* dsh ships prerelease semver; truncating it to 0.1.5 would compare a
     different version against upstream than the one on disk. */
  const s = harnessState({ agent: "dsh", present: true, launchable: true, installed: "0.1.5-rc.1" }, null);
  assert.equal(s.state, "unchecked");
  assert.equal(s.version, "0.1.5-rc.1");
  const absent = harnessState({ agent: "dsh", present: false, launchable: false }, null);
  assert.equal(absent.state, "absent");
  assert.equal(absent.note, "not installed");
});

test("dsh carries both its vendor's terms and the BYO caveat", () => {
  /* dsh is the only harness that is both. DeepSeek ships it with a DeepSeek
     default, so a DeepSeek session is real and its terms apply; it also
     routes to whatever provider the profile names, and scimux cannot read
     which. The link alone would report the default as binding; the BYO note
     alone would deny that a DeepSeek session exists. It gets both. */
  const html = harnessRowsHTML(
    [{ agent: "dsh", present: true, launchable: true, installed: "0.1.5-rc.1" }], null);
  assert.match(html, /href="https:\/\/cdn\.deepseek\.com\/policies\/en-US\/deepseek-terms-of-use\.html"/);
  assert.match(html, /target="_blank"/);
  assert.match(html, /rel="noopener"/);
  assert.match(html, /other configured providers/i);
});

test("the BYO-only harnesses did not inherit dsh's link", () => {
  /* pi and opencode hold no model of their own: adding a both-branch for dsh
     must not turn their honest silence into a vendor claim. */
  const html = harnessRowsHTML([
    { agent: "pi", present: true, launchable: true, installed: "0.85.1" },
    { agent: "opencode", present: true, launchable: true, installed: "1.2.3" },
  ], null);
  assert.doesNotMatch(html, /href="http/);
  assert.doesNotMatch(html, /deepseek/i);
});


test("provider privacy links accompany terms in the burger menu", () => {
  for (const agent of ["claude", "codex", "grok", "cursor", "dsh", "muse"]){
    const html = harnessRowsHTML([{ agent, present: true, launchable: true }], null);
    assert.match(html, /href="https:[^"]+" target="_blank" rel="noopener noreferrer">Privacy<\/a>/);
  }
  const muse = harnessRowsHTML([{ agent: "muse", present: true }], null);
  assert.match(muse, /https:\/\/dev\.meta\.ai\/legal\/terms-of-service/);
  for (const agent of ["pi", "opencode"]){
    const html = harnessRowsHTML([{ agent, present: true }], null);
    assert.match(html, /Terms and privacy depend on your configured model provider/);
    assert.doesNotMatch(html, /href=/);
  }
});
