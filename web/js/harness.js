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

/**
 * The whole section body, one .item per harness in the order the server sent
 * them. `latest` is the /api/harnesses/latest map, or null before the check.
 * Every string that came off the network is escaped here.
 *
 * `deps.agentLogo` is injected rather than imported: the logo needs the FR-42
 * `assetURL` supplier, which only the shell has, and a module that reached for
 * it directly would break under the bootstrap loader.
 */
export function harnessRowsHTML(rows, latest, deps = {}){
  const list = Array.isArray(rows) ? rows : [];
  if (!list.length){
    return `<div class="hnote">no agent CLIs found on this computer</div>`;
  }
  const answers = latest || {};
  const agentLogo = typeof deps.agentLogo === "function" ? deps.agentLogo : () => "";
  return list.map(row => {
    const s = harnessState(row, answers[row && row.agent]);
    const name = usageAgentDisplayName(s.agent);
    const tag = s.version ? esc(s.version) : "—";
    return `<div class="item" data-agent="${esc(s.agent || "")}" data-state="${esc(s.state)}">` +
      `<span><span class="agent-logo" aria-hidden="true">${agentLogo(s.agent)}</span> ${esc(name)}</span>` +
      `<span class="tag hver">${tag}</span>` +
      (s.note ? `<span class="hnote">${esc(s.note)}</span>` : "") +
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
