/* Vibe catalog inspection is a one-shot choice beside the harness update
   action. The server is the authority: the checkbox only decides whether
   this click sends the explicit signal. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  applyAgentsProbe, modelOptionsHTML, effortLevelsFor, effortOptionsHTML,
} from "../js/sheets.js";
import { acceptHarnessMenuResult } from "../js/harness.js";

const dir = dirname(fileURLToPath(import.meta.url));
const indexSrc = readFileSync(join(dir, "../index.html"), "utf8");
const label = "Inspect Mistral Vibe model and thinking choices on the next update check (opens a temporary local session).";

test("the harness update action starts with an unchecked one-shot Vibe inspection choice", () => {
  const at = indexSrc.indexOf('id="m_hcheck"');
  assert.ok(at > 0, "Check for harness updates is missing");
  const around = indexSrc.slice(Math.max(0, at - 700), at + 80);
  assert.match(around, /id="m_vibe_inspect"/);
  assert.match(around, /id="m_vibe_inspect"[^>]*autocomplete="off"/);
  assert.match(around, /Inspect Mistral Vibe model and thinking choices on the next update check \(opens a temporary local session\)\./);
  assert.match(around, /id="m_vibe_inspect_row" hidden/);
  assert.doesNotMatch(indexSrc, /id="m_vibe_inspect"[^>]*\bchecked\b/);
});

test("a harness refresh publishes models only when the payload carries them", () => {
  const sinks = { latest: [], rows: [], agents: [], rendered: 0 };
  const record = {
    latest: (v) => sinks.latest.push(v),
    rows: (v) => sinks.rows.push(v),
    agents: (v) => sinks.agents.push(v),
    render: () => { sinks.rendered += 1; },
  };
  acceptHarnessMenuResult(null, record);
  acceptHarnessMenuResult({ latest: { grok: { version: "1" } } }, record);
  acceptHarnessMenuResult({ harnesses: [{ agent: "vibe" }], agents: { vibe: { models: ["alpha"] } } }, record);
  acceptHarnessMenuResult({ harnesses: "nope", agents: "nope" }, { render: () => { sinks.rendered += 1; } });
  acceptHarnessMenuResult(
    { harnesses: [], agents: { vibe: { models: [] } } },
    { latest: () => sinks.latest.push("bare"), render: () => { sinks.rendered += 1; } },
  );
  assert.deepEqual(sinks.latest, [{}, { grok: { version: "1" } }, {}, "bare"]);
  assert.deepEqual(sinks.rows, [[{ agent: "vibe" }]]);
  assert.deepEqual(sinks.agents, [{ vibe: { models: ["alpha"] } }]);
  assert.equal(sinks.rendered, 5);
  acceptHarnessMenuResult({ latest: {} }, {});
});

test("an empty Vibe catalog offers default model and default thinking, and a returned catalog shows both", () => {
  const emptyModels = {};
  const emptyEfforts = {};
  applyAgentsProbe(emptyModels, emptyEfforts, { vibe: { models: [] } });
  assert.equal(modelOptionsHTML(emptyModels, "vibe", null), `<option value="">(default)</option>`);
  const bare = effortLevelsFor("vibe", "", emptyEfforts);
  assert.deepEqual(bare.list, []);
  assert.equal(effortOptionsHTML(bare.list, bare.default), `<option value="">(default)</option>`);

  const models = {};
  const efforts = {};
  applyAgentsProbe(models, efforts, {
    vibe: {
      models: ["alpha", "beta"],
      efforts: { alpha: { levels: ["lvl-high"] } },
    },
  });
  const modelMenu = modelOptionsHTML(models, "vibe", null);
  assert.match(modelMenu, /value="alpha"/);
  assert.match(modelMenu, /value="beta"/);
  assert.match(modelMenu, /\(default\)/);
  const thinking = effortLevelsFor("vibe", "alpha", efforts);
  assert.deepEqual(thinking.list, ["lvl-high"]);
  assert.match(effortOptionsHTML(thinking.list, thinking.default, undefined, thinking.required), /lvl-high/);
  assert.deepEqual(effortLevelsFor("vibe", "beta", efforts).list, []);
});

async function loadHarnessUpdate() {
  const mod = await import("../js/harness.js");
  assert.equal(mod.VIBE_INSPECT_LABEL, label);
  assert.equal(typeof mod.harnessUpdateCall, "function");
  assert.equal(typeof mod.runHarnessMenuCheck, "function");
  assert.equal(typeof mod.vibeInspectAvailable, "function");
  return mod;
}

test("Vibe inspection is offered only for a launchable installed Vibe", async () => {
  const { vibeInspectAvailable, updateVibeInspectControl, clearVibeInspectChoice } = await loadHarnessUpdate();
  assert.equal(vibeInspectAvailable(null), false);
  assert.equal(vibeInspectAvailable([{ agent: "grok", present: true, launchable: true }]), false);
  assert.equal(vibeInspectAvailable([{ agent: "vibe", present: false, launchable: false }]), false);
  assert.equal(vibeInspectAvailable([{ agent: "vibe", present: true, launchable: true }]), true);
  const row = { hidden: true }, box = { checked: true };
  assert.equal(updateVibeInspectControl([{ agent: "vibe", present: true, launchable: true }], row, box), true);
  assert.equal(row.hidden, false);
  assert.equal(box.checked, true);
  assert.equal(updateVibeInspectControl([{ agent: "vibe", present: false }], row, box), false);
  assert.equal(row.hidden, true);
  assert.equal(box.checked, false);
  box.checked = true;
  clearVibeInspectChoice(box);
  assert.equal(box.checked, false);
  clearVibeInspectChoice(null);
  updateVibeInspectControl(null, null, null);
});

test("the check consumes and disables the Vibe choice before its request", async () => {
  const { runHarnessMenuCheck } = await loadHarnessUpdate();
  const box = { checked: true, disabled: false };
  let release;
  const gate = new Promise(resolve => { release = resolve; });
  const calls = [];
  const running = runHarnessMenuCheck({
    box,
    api: async (path, opts) => { calls.push({ path, opts }); await gate; return {}; },
    scimux: async () => ({}),
  });
  assert.equal(box.checked, false);
  assert.equal(box.disabled, true);
  box.checked = false;
  await Promise.resolve();
  assert.equal(calls[0].opts.method, "POST");
  release();
  await running;
  assert.equal(box.disabled, false);
});

test("the checkbox sends the explicit signal only while it is checked, then resets", async () => {
  const { harnessUpdateCall, runHarnessMenuCheck } = await loadHarnessUpdate();
  assert.deepEqual(harnessUpdateCall(false), { path: "/api/harnesses/latest" });
  assert.deepEqual(harnessUpdateCall(undefined), { path: "/api/harnesses/latest" });
  assert.deepEqual(harnessUpdateCall(1), { path: "/api/harnesses/latest" });
  assert.deepEqual(harnessUpdateCall(true), {
    path: "/api/harnesses/latest",
    opts: { method: "POST", body: JSON.stringify({ inspect_vibe: true }) },
  });

  const calls = [];
  const box = { checked: false };
  const button = { disabled: false, textContent: "Check for harness updates" };
  const scimuxButton = { disabled: false };
  const applied = [];
  const api = async (path, opts) => {
    calls.push({ path, method: opts && opts.method, body: opts && opts.body });
    if (opts && opts.method === "POST") {
      return {
        latest: {},
        agents: { vibe: { models: ["alpha"], efforts: { alpha: { levels: ["lvl-high"] } } } },
      };
    }
    return { latest: {}, agents: { vibe: { models: [] } } };
  };
  await runHarnessMenuCheck({
    button, scimuxButton, box, api,
    scimux: async () => ({ available: false }),
    apply: (d) => applied.push(d),
  });
  assert.equal(calls.length, 1);
  assert.equal(calls[0].method, undefined);
  assert.equal(box.checked, false);
  assert.equal(button.disabled, false);
  assert.equal(button.textContent, "Check for harness updates");
  assert.deepEqual(applied[0].agents.vibe.models, []);

  box.checked = true;
  await runHarnessMenuCheck({
    button, scimuxButton, box, api,
    scimux: async () => { throw new Error("release feed down"); },
    apply: (d) => applied.push(d),
  });
  assert.equal(calls[1].method, "POST");
  assert.equal(calls[1].body, JSON.stringify({ inspect_vibe: true }));
  assert.equal(box.checked, false, "the choice resets after the action");
  assert.equal(button.textContent, "Check for harness updates");
  const shown = {};
  const shownEfforts = {};
  applyAgentsProbe(shown, shownEfforts, applied[1].agents);
  assert.match(modelOptionsHTML(shown, "vibe", null), /alpha/);
  assert.deepEqual(effortLevelsFor("vibe", "alpha", shownEfforts).list, ["lvl-high"]);

  await runHarnessMenuCheck({
    button, scimuxButton, box, api,
    scimux: async () => ({}),
    apply: (d) => applied.push(d),
  });
  assert.equal(calls[2].method, undefined, "a reset checkbox is not stored consent");
  assert.equal(box.checked, false);
});

test("a failed update resets the checkbox and does not present a catalog", async () => {
  const { runHarnessMenuCheck } = await loadHarnessUpdate();
  const box = { checked: true };
  const button = { disabled: false, textContent: "Check for harness updates" };
  let applied = 0;
  const checked = await runHarnessMenuCheck({
    button,
    scimuxButton: { disabled: false },
    box,
    api: async () => { throw new Error("offline"); },
    scimux: async () => ({}),
    apply: () => { applied++; },
  });
  assert.equal(checked.harness.status, "rejected");
  assert.equal(box.checked, false);
  assert.equal(button.disabled, false);
  assert.equal(button.textContent, "check failed");
  assert.equal(applied, 0);
});

test("a missing checkbox and a missing catalog payload still finish the ordinary check", async () => {
  const { runHarnessMenuCheck } = await loadHarnessUpdate();
  const calls = [];
  const button = { disabled: false, textContent: "" };
  let received;
  await runHarnessMenuCheck({
    button,
    api: async (path, opts) => {
      calls.push(opts && opts.method);
      return { latest: {} };
    },
    scimux: async () => ({}),
    apply: (d) => { received = d; },
  });
  assert.deepEqual(calls, [undefined]);
  assert.equal(button.textContent, "Check for harness updates");
  assert.deepEqual(received, { latest: {} });

  let bare;
  await runHarnessMenuCheck({
    api: async () => { bare = true; return null; },
    scimux: async () => ({}),
  });
  assert.equal(bare, true);

  const rejected = await runHarnessMenuCheck({
    api: async () => { throw new Error("offline"); },
    scimux: async () => ({}),
  });
  assert.equal(rejected.harness.status, "rejected");
});
