"use strict";
/* Pure harness-inventory presentation for the burger menu: what agent CLIs
 * this computer has, at which version, and — once the human taps the check —
 * what upstream publishes. Shell (app.js) owns the fetches and the innerHTML
 * write; nothing here touches the DOM.
 *
 * The version comparison is the server's job (harness_version.go picks the
 * right channel per install and compares numerically). This module compares
 * only to decide the row's presentation, and it must use the same
 * segment-wise rule: grok went 1.0.3 → 1.0.24, where a string compare says
 * the older one is newer. */
import { esc } from "./format.js";
import { usageAgentDisplayName } from "./usage.js";

/* Numeric-prefix compare. Returns true only on evidence: anything
 * unparseable, equal or lower is not an update. */
function isNewer(installed, latest){
  const seg = (v) => {
    const m = String(v || "").match(/(\d+(?:\.\d+)+)/);
    return m ? m[1].split(".").map(Number) : [];
  };
  const a = seg(installed), b = seg(latest);
  if (!a.length || !b.length) return false;
  for (let i = 0; i < Math.max(a.length, b.length); i++){
    const x = a[i] || 0, y = b[i] || 0;
    if (x !== y) return y > x;
  }
  return false;
}

/* Why a harness needs a different binary to launch than to interrogate: pi
 * speaks ACP through `pi-acp`, so `pi` alone is installed-but-not-offerable.
 * Naming the missing binary is the whole value of the row. */
const LAUNCH_BIN = Object.freeze({ pi: "pi-acp" });

/* Whose agreement the user is actually under, per harness.
 *
 * scimux is not a party to any of these, holds no credential for any of them,
 * and runs no login flow — so the honest thing a row can do is point at the
 * agreement the user already signed, not summarise it. One tap, one vendor.
 *
 * Consumer terms are linked because a coding-harness subscription is the
 * common case; an API-key user is under the same vendor's commercial terms,
 * which the README lists. The menu is a pointer, the README is the reference.
 *
 * pi and opencode are deliberately absent: they are BYO-provider routers
 * holding no model of their own, so the binding terms are whichever provider
 * key the user configured — something scimux cannot read and must not guess.
 * Naming the wrong vendor would be worse than naming none, so those rows get
 * a sentence instead of a link.
 *
 * Each entry is the whole anchor, with its URL written out literally, and
 * that is deliberate rather than lazy: the FR-42 audit classifies a
 * target=_blank by the href on the *same* element, and only a literal
 * absolute URL is provably external. Building the href from a variable would
 * be indistinguishable from a local navigation to the scanner and would need
 * an allowlist entry — for three constants that never vary. Keep them
 * literal and the ratchet stays honest.
 *
 * Names match each vendor's own document title; OpenAI's is "Terms of Use".
 * target=_blank so leaving does not navigate away from a live supervision
 * page, rel=noopener because a third-party tab has no business holding a
 * handle to this one. */
const HARNESS_TERMS = Object.freeze({
  claude: `<a href="https://www.anthropic.com/legal/consumer-terms" target="_blank" rel="noopener">Terms of Service</a>`,
  codex: `<a href="https://openai.com/policies/terms-of-use/" target="_blank" rel="noopener">Terms of Use</a>`,
  grok: `<a href="https://x.ai/legal/terms-of-service" target="_blank" rel="noopener">Terms of Service</a>`,
});

const BYO_PROVIDER_NOTE =
  "Terms are your model provider's — scimux cannot see which one you configured.";

/* The terms line for one row. Absent harnesses keep it: someone deciding
 * whether to install a harness is exactly the reader who wants the terms
 * first, and the row is a reference rather than an action. */
export function harnessTermsHTML(agent){
  const link = HARNESS_TERMS[agent];
  if (link) return `<span class="hnote hterms">${link}</span>`;
  if (agent === "pi" || agent === "opencode"){
    return `<span class="hnote hterms">${esc(BYO_PROVIDER_NOTE)}</span>`;
  }
  return "";
}

/**
 * One row's state and the sentence under it.
 *
 * States: "absent" (not on PATH), "unknown" (present, `--version` unreadable),
 * "unlaunchable" (present, but the binary scimux launches is missing),
 * "unchecked" (installed, no upstream answer yet), "behind", "current".
 *
 * "unchecked" exists because the upstream check is a deliberate tap: before
 * it, "up to date" would be a claim nobody has made.
 */
export function harnessState(row, latest){
  const r = row || {};
  const version = r.installed || "";
  if (!r.present){
    return { agent: r.agent, state: "absent", version: "", note: "not installed" };
  }
  if (!version){
    return { agent: r.agent, state: "unknown", version: "",
      note: "installed, but it did not report a version" };
  }
  if (!r.launchable){
    if (r.agent === "muse"){
      return { agent: r.agent, state: "unlaunchable", version,
        note: "installed, but Muse model policy is unavailable — scimux cannot launch it" };
    }
    const bin = LAUNCH_BIN[r.agent] || r.agent;
    return { agent: r.agent, state: "unlaunchable", version,
      note: `installed, but ${bin} is missing — scimux cannot launch it` };
  }
  const up = latest && latest.version ? latest : null;
  if (!up){
    return { agent: r.agent, state: "unchecked", version, note: "" };
  }
  if (isNewer(version, up.version)){
    return { agent: r.agent, state: "behind", version,
      note: `${up.version} available${up.source ? " · " + up.source : ""}` };
  }
  return { agent: r.agent, state: "current", version,
    note: `up to date${up.source ? " · " + up.source : ""}` };
}

/* The usage-check switch's own copy. Claude is the only harness whose gauge
 * costs the user's own subscription quota to read, so it is the only one that
 * gets a choice — and a choice nobody can price is a choice nobody makes, so
 * both states name what is spent. The off text says what turning it on would
 * cost; the on text says what is being spent now.
 */
/* Computer-owned settings flags are JSON booleans. Anything else — missing,
   stringly, numeric — is off, matching the server's own degrade direction. */
export function computerSettingOn(v){
  return v === true;
}

const CLAUDE_USAGE_KEY = "claude_usage_checks";
const MUSE_CONSENT_KEY = "muse_approval_judge_consent";

function settingsSnapshot(s){
  return {
    claude_usage_checks: !!s.claude_usage_checks,
    muse_approval_judge_consent: !!s.muse_approval_judge_consent,
  };
}

function flagFrom(raw, key){
  if (!raw || typeof raw !== "object" || !(key in raw)) return false;
  return computerSettingOn(raw[key]);
}

/**
 * Computer-owned settings read/write. Injected read/write/render; no DOM.
 * Operations run FIFO. Confirmed server state is the only thing rendered.
 */
export function createSettingsController(deps = {}){
  const read = typeof deps.read === "function" ? deps.read : async () => ({});
  const write = typeof deps.write === "function" ? deps.write : async () => ({});
  const render = typeof deps.render === "function" ? deps.render : () => {};
  let confirmed = { claude_usage_checks: false, muse_approval_judge_consent: false };
  let tail = Promise.resolve();

  function snapshot(){
    return settingsSnapshot(confirmed);
  }

  function paint(){
    render(snapshot());
  }

  function applyRead(raw){
    confirmed = {
      claude_usage_checks: computerSettingOn(raw && raw[CLAUDE_USAGE_KEY]),
      muse_approval_judge_consent: computerSettingOn(raw && raw[MUSE_CONSENT_KEY]),
    };
  }

  function applyWrite(changedKey, raw){
    confirmed[changedKey] = flagFrom(raw, changedKey);
    const other = changedKey === MUSE_CONSENT_KEY ? CLAUDE_USAGE_KEY : MUSE_CONSENT_KEY;
    if (raw && typeof raw === "object" && other in raw)
      confirmed[other] = computerSettingOn(raw[other]);
  }

  function enqueue(work){
    const run = tail.then(() => work(), () => work());
    tail = run.then(() => {}, () => {});
    return run.then(s => s, () => snapshot());
  }

  function load(){
    return enqueue(async () => {
      try {
        const s = await read();
        applyRead(s);
      } catch {
        confirmed = { claude_usage_checks: false, muse_approval_judge_consent: false };
      }
      paint();
      return snapshot();
    });
  }

  function put(changedKey, want){
    paint();
    const body = { [changedKey]: !!want };
    return enqueue(async () => {
      try {
        const s = await write({ [changedKey]: body[changedKey] });
        applyWrite(changedKey, s);
      } catch { /* keep last confirmed */ }
      paint();
      return snapshot();
    });
  }

  return {
    load,
    setMuseConsent(want){ return put(MUSE_CONSENT_KEY, want); },
    setClaudeUsage(want){ return put(CLAUDE_USAGE_KEY, want); },
    getState(){ return snapshot(); },
  };
}

export function museConsentNote(on){
  return on
    ? "On — You can start Muse sessions. Muse may use part of your plan's " +
      "usage limit to check tool actions. Actions that still need approval " +
      "stay paused until they are allowed or rejected."
    : "Off — You cannot start a Muse session. Turn this on to use Muse. Muse " +
      "may use part of your plan's usage limit when it checks whether tool " +
      "actions are safe.";
}

export function usageCheckNote(on){
  return on
    ? "On — one small check (~900 tokens) every 15 minutes, and only while " +
      "you're working in a Claude session. It counts against the same 5-hour " +
      "limit it reports; a full working day of checks adds up to about one " +
      "ordinary message."
    : "Off — nothing is spent. Turn it on for one small check (~900 tokens) " +
      "every 15 minutes, and only while you're working in a Claude session; a " +
      "full working day of checks adds up to about one ordinary message. The " +
      "other harnesses report usage for free.";
}

/* Alphabetical by the name on screen, not by the agent key and not in the
 * order the server sent. The server's order is its registry order — a
 * code-organisation fact no reader can guess — while a scanned list wants the
 * one order a human already has in their eye. */
function harnessDisplayOrder(rows){
  return rows.slice().sort((a, b) =>
    usageAgentDisplayName(a && a.agent).localeCompare(usageAgentDisplayName(b && b.agent)));
}

/**
 * The whole section body, one .item per harness, alphabetically.
 * `latest` is the /api/harnesses/latest map, or null before the check.
 * Every string that came off the network is escaped here.
 *
 * `deps.agentLogo` is injected rather than imported: the logo needs the FR-42
 * `assetURL` supplier, which only the shell has, and a module that reached for
 * it directly would break under the bootstrap loader. `deps.usageChecks` is
 * the stored consent (`claude_usage_checks`), rendered as a switch on the one
 * row it governs. `deps.museConsent` is `muse_approval_judge_consent`,
 * rendered only on a present Muse row.
 */
export function harnessRowsHTML(rows, latest, deps = {}){
  const list = Array.isArray(rows) ? rows : [];
  if (!list.length){
    return `<div class="hnote">no agent CLIs found on this computer</div>`;
  }
  const answers = latest || {};
  const agentLogo = typeof deps.agentLogo === "function" ? deps.agentLogo : () => "";
  const usageFlag = !!deps.usageChecks;
  const museFlag = computerSettingOn(deps.museConsent);
  return harnessDisplayOrder(list).map(row => {
    const s = harnessState(row, answers[row && row.agent]);
    const name = usageAgentDisplayName(s.agent);
    const tag = s.version ? esc(s.version) : "—";
    /* An absent claude gets no switch: consent to spend a quota that cannot be
       reached is a dead control, the same reason #m_pair stays hidden until
       pairing could actually complete. */
    const usage = (s.agent === "claude" && row && row.present)
      ? `<label class="hswitch"><input type="checkbox" data-usage-check="claude"` +
        `${usageFlag ? " checked" : ""}> Check usage</label>` +
        `<span class="hnote">${esc(usageCheckNote(usageFlag))}</span>`
      : "";
    let museConsent = "";
    if (s.agent === "muse"){
      if (row && row.present){
        museConsent = `<label class="hswitch"><input type="checkbox" data-muse-consent="muse"` +
          `${museFlag ? " checked" : ""}> Enable Muse</label>` +
          `<span class="hnote">${esc(museConsentNote(museFlag))}</span>`;
      } else {
        museConsent = `<span class="hnote">${esc(museConsentNote(false))}</span>`;
      }
    }
    return `<div class="item" data-agent="${esc(s.agent || "")}" data-state="${esc(s.state)}">` +
      `<span><span class="agent-logo" aria-hidden="true">${agentLogo(s.agent)}</span> ${esc(name)}</span>` +
      `<span class="tag hver">${tag}</span>` +
      (s.note ? `<span class="hnote">${esc(s.note)}</span>` : "") +
      harnessTermsHTML(s.agent) +
      usage +
      museConsent +
      `</div>`;
  }).join("");
}

/**
 * The sentence under the check button after a check, or "" before one.
 *
 * A check that only half-answered leaves those rows saying "unchecked", which
 * is honest but silent; naming the sources that did not answer is what keeps
 * the panel from reading as a clean bill of health. Harnesses this computer
 * does not have are skipped — an absent harness has no upstream to miss.
 */
export function harnessCheckNote(rows, latest){
  if (!latest) return "";
  const missing = (Array.isArray(rows) ? rows : [])
    .filter(r => r && r.present && r.installed && !(latest[r.agent] && latest[r.agent].version))
    .map(r => usageAgentDisplayName(r.agent));
  if (!missing.length) return "";
  return `no upstream answer for ${missing.join(", ")}`;
}
