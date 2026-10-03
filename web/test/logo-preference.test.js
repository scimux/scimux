import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import * as usage from "../js/usage.js";
import { createSettingsController } from "../js/harness.js";
import { createApp } from "../js/app.js";
import { installHost, identityAssetURL, jsonResponse, routeBody } from "./s2-helpers.js";

const web = join(dirname(fileURLToPath(import.meta.url)), "..");
const badges = {
  claude: ["claude", "Cld"], codex: ["openai", "Cdx"],
  cursor: ["cursor", "Cur"], dsh: ["deepseek", "Dsh"],
  grok: ["grok", "Grk"], vibe: ["mistral", "Mst"],
  muse: ["meta", "Mus"], opencode: ["opencode", "OpC"],
  pi: ["pi", "Pi."],
};

test("all nine badges have matching accent and monochrome SVG artwork", () => {
  for (const [agent, [file, label]] of Object.entries(badges)){
    const accent = readFileSync(join(web, "assets/agents", file + ".svg"), "utf8");
    const mono = readFileSync(join(web, "assets/agents", file + "-mono.svg"), "utf8");
    for (const art of [accent, mono]){
      assert.match(art, /viewBox="0 0 64 64"/, agent);
      assert.ok(art.includes(">" + label + "</text>"), agent);
      assert.match(art, /rotate\(-7 32 32\)/, agent);
    }
    assert.equal(
      mono.replace(/<defs>.*<\/defs>/, "<defs/>"),
      accent.replace(/<defs>.*<\/defs>/, "<defs/>"),
      agent + " variants must share geometry and lettering",
    );
    assert.match(mono, /stop-color="#f[0-9a-f]{5}"/i, agent);
    assert.doesNotMatch(mono, /#(?:fb8c63|79e9c4|5c9df8|66d9f6|fa616b|b979ed|fac42e|f9aae0)/i, agent);
  }
});

test("agent logos supply both variants through the asset URL seam", () => {
  for (const [agent, [file]] of Object.entries(badges)){
    const seen = [];
    const html = usage.agentLogo(agent, path => { seen.push(path); return "U:" + path; });
    assert.deepEqual(seen, [
      "/assets/agents/" + file + "-mono.svg",
      "/assets/agents/" + file + ".svg",
    ], agent);
    assert.match(html, /class="badge-mono"/, agent);
    assert.match(html, /class="badge-accent"/, agent);
    assert.match(html, /alt="" aria-hidden="true"/, agent);
  }
  assert.match(usage.agentLogo("unknown", p => p), /<svg/);
});

test("accent preference toggles the existing badge elements without rerendering", () => {
  const classes = new Set();
  const root = { classList: {
    toggle(name, on){ if (on) classes.add(name); else classes.delete(name); },
  } };
  assert.equal(typeof usage.setAccentHarnessLogos, "function");
  usage.setAccentHarnessLogos(root, true);
  assert.ok(classes.has("accent-harness-logos"));
  usage.setAccentHarnessLogos(root, false);
  assert.ok(!classes.has("accent-harness-logos"));
});

test("accent preference is off by default and follows confirmed settings writes", async () => {
  let saved = false;
  const controller = createSettingsController({
    read: async () => ({ use_accent_harness_logos: saved }),
    write: async body => {
      assert.deepEqual(body, { use_accent_harness_logos: true });
      saved = true;
      return { use_accent_harness_logos: true };
    },
  });
  assert.equal(controller.getState().use_accent_harness_logos, false);
  await controller.load();
  assert.equal(controller.getState().use_accent_harness_logos, false);
  assert.equal(typeof controller.setAccentHarnessLogos, "function");
  await controller.setAccentHarnessLogos(true);
  assert.equal(controller.getState().use_accent_harness_logos, true);
  await controller.load();
  assert.equal(controller.getState().use_accent_harness_logos, true);
});

test("accent preference restores confirmed off after a failed save", async () => {
  const controller = createSettingsController({
    read: async () => ({ use_accent_harness_logos: false }),
    write: async () => { throw new Error("unavailable"); },
  });
  await controller.load();
  await controller.setAccentHarnessLogos(true);
  assert.equal(controller.getState().use_accent_harness_logos, false);
});

test("attachment and logo switches are petrol; mono is the default artwork", () => {
  const html = readFileSync(join(web, "index.html"), "utf8");
  const css = readFileSync(join(web, "css/cards.css"), "utf8") +
    readFileSync(join(web, "css/sheets.css"), "utf8");
  const attachment = html.indexOf('id="m_external_attachments"');
  const logo = html.indexOf('id="m_accent_harness_logos"');
  assert.ok(attachment >= 0 && logo > attachment);
  assert.match(html, /Use accent colors for CLI harnesses logos/);
  assert.match(css, /#m_attachment_settings\s+\.hswitch\s+input[^}]*accent-color:\s*var\(--work\)/);
  assert.match(css, /\.agent-logo\s+\.badge-accent\s*\{[^}]*display:\s*none/);
  assert.match(css, /\.accent-harness-logos\s+\.agent-logo\s+\.badge-mono\s*\{[^}]*display:\s*none/);
  assert.match(css, /\.accent-harness-logos\s+\.agent-logo\s+\.badge-accent\s*\{[^}]*display:\s*block/);
});

test("app loads the saved logo choice on boot and applies checkbox changes live", async () => {
  const host = installHost({});
  let accent = true;
  const writes = [];
  const fetchImpl = async (path, opts = {}) => {
    if (String(path).split("?")[0] !== "/api/settings") return jsonResponse(routeBody(path));
    if (opts.method === "PUT"){
      const body = JSON.parse(opts.body);
      writes.push(body);
      accent = body.use_accent_harness_logos;
    }
    return jsonResponse({ use_accent_harness_logos: accent });
  };
  const app = await createApp({
    fetchImpl, assetURL: identityAssetURL,
    document: host.document, window: host.window,
  });
  const flush = async () => { for (let i = 0; i < 50; i++) await Promise.resolve(); };
  try {
    await flush();
    const root = host.document.documentElement;
    const box = host.document.getElementById("m_accent_harness_logos");
    assert.equal(box.checked, true);
    assert.ok(root.classList.contains("accent-harness-logos"));
    box.checked = false;
    box.dispatchEvent({ type: "change", target: box });
    await flush();
    assert.deepEqual(writes, [{ use_accent_harness_logos: false }]);
    assert.equal(box.checked, false);
    assert.ok(!root.classList.contains("accent-harness-logos"));
  } finally {
    app.retire();
  }
});

test("retired app cannot apply a late logo setting over its successor", async () => {
  const host = installHost({});
  let releaseFirst;
  const lateSettings = new Promise(resolve => { releaseFirst = resolve; });
  const firstFetch = async path => String(path).split("?")[0] === "/api/settings"
    ? lateSettings : jsonResponse(routeBody(path));
  const secondFetch = async path => String(path).split("?")[0] === "/api/settings"
    ? jsonResponse({ use_accent_harness_logos: false }) : jsonResponse(routeBody(path));
  const deps = fetchImpl => ({
    fetchImpl, assetURL: identityAssetURL,
    document: host.document, window: host.window,
  });
  const first = await createApp(deps(firstFetch));
  const second = await createApp(deps(secondFetch));
  const flush = async () => { for (let i = 0; i < 50; i++) await Promise.resolve(); };
  try {
    await flush();
    assert.ok(!host.document.documentElement.classList.contains("accent-harness-logos"));
    releaseFirst(jsonResponse({ use_accent_harness_logos: true }));
    await flush();
    assert.ok(!host.document.documentElement.classList.contains("accent-harness-logos"));
  } finally {
    first.retire();
    second.retire();
  }
});
