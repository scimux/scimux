/* S2 R1 — AT-FR-15-b (localhost transport unchanged) and AT-FR-15a-a
 * (no service worker on any origin in any mode). Failures are missing
 * seam behaviour, not load errors or skips. */
import test from "node:test";
import assert from "node:assert/strict";
import {
  missingSeam,
  readWeb,
  servedJsNames,
  runTwice,
  exerciseAppCode,
  driveAppJsSites,
  assertFetchImplRequired,
  identityAssetURL,
  urlsInHTML,
  AGENT_ASSET_PATHS,
  APP_FETCH_SITES,
  todayTileHTML,
  todayAgentLogoHTML,
  siteSeen,
  installServiceWorkerTrap,
  localhostTransport,
  channelTransport,
} from "./s2-helpers.js";

function fail(at, detail) {
  assert.fail(missingSeam(at, detail));
}

function servedSources() {
  const files = ["index.html", ...servedJsNames().map(n => `js/${n}`)];
  return files.map(rel => ({ rel, src: readWeb(rel) }));
}

test("AT-FR-15-b: localhost path is byte-for-byte the current one — no WebRTC, no transport indirection, fetchImpl is plain fetch", async () => {
  const at = "AT-FR-15-b";
  readWeb("js/app.js");
  readWeb("js/polling.js");

  for (const { rel, src } of servedSources()) {
    if (/\b(?:webkit)?RTCPeerConnection\b/.test(src) || /\bRTCDataChannel\b/.test(src)) {
      fail(at, `${rel} constructs WebRTC on the localhost path`);
    }
  }

  const { local, localResult } = await runTwice(exerciseAppCode);
  if (local.assetURL !== identityAssetURL && local.assetURL("/x") !== "/x") {
    fail(at, "localhost assetURL is not the identity function");
  }
  if (identityAssetURL("/assets/agents/claude.svg") !== "/assets/agents/claude.svg") {
    fail(at, "localhost assetURL(path) must return today's string byte-for-byte");
  }

  const todayTiles = todayTileHTML();
  const todayLogos = todayAgentLogoHTML();
  for (const key of ["attImage", "attFile", "assetImage", "assetFile"]) {
    if (localResult.tiles[key] !== todayTiles[key]) {
      fail(at, `localhost tile ${key} changed relative to today's builder output`);
    }
  }
  for (const [agent, path] of Object.entries(AGENT_ASSET_PATHS)) {
    const html = localResult.logos[agent];
    if (html !== todayLogos[agent]) {
      fail(at, `localhost agentLogo(${agent}) is not today's markup`);
    }
    if (!urlsInHTML(html).includes(path)) {
      fail(at, `localhost agentLogo(${agent}) did not return today's path ${path} byte-for-byte`);
    }
  }

  const webrtcBuilt = [];
  const PrevRTC = globalThis.RTCPeerConnection;
  globalThis.RTCPeerConnection = class {
    constructor() {
      webrtcBuilt.push("RTCPeerConnection");
    }
  };
  try {
    const app = await driveAppJsSites(local);
    if (webrtcBuilt.length) {
      fail(at, "localhost boot constructed RTCPeerConnection");
    }
    for (const site of APP_FETCH_SITES) {
      if (!siteSeen(app.seamCalls, site)) {
        fail(at,
          `localhost ${site.pathPrefix} is not routed through an injected fetchImpl that is plain fetch` +
          (app.missingSeam ? ` (${app.reason})` : "") +
          (siteSeen(app.globalCalls, site) ? "; it still calls global fetch rather than the supplier" : ""));
      }
    }
    const used = app.seamCalls.filter(c => APP_FETCH_SITES.some(s => siteSeen([c], s)));
    if (used.some(c => c.kind && c.kind !== "localhost")) {
      fail(at, "localhost fetchImpl is not the localhost supplier (transport indirection)");
    }
  } finally {
    if (PrevRTC) globalThis.RTCPeerConnection = PrevRTC;
    else delete globalThis.RTCPeerConnection;
  }

  try {
    await assertFetchImplRequired(at);
  } catch (err) {
    fail(at, err.message.replace(/^AT-FR-15-b: missing seam behaviour: /, ""));
  }
});

test("AT-FR-15a-a: navigator.serviceWorker.register is never called on any origin in any mode", async () => {
  const at = "AT-FR-15a-a";
  readWeb("js/app.js");

  for (const { rel, src } of servedSources()) {
    if (/\bserviceWorker\s*\.?\s*register\b/.test(src) || /\bnavigator\.serviceWorker\b/.test(src)) {
      fail(at, `${rel} references navigator.serviceWorker`);
    }
  }

  const origins = [];
  for (const transport of [localhostTransport(), channelTransport()]) {
    const trap = installServiceWorkerTrap();
    let result;
    try {
      result = await driveAppJsSites(transport, { serviceWorker: trap.serviceWorker });
    } catch (err) {
      const msg = err && err.message ? err.message : String(err);
      if (/serviceWorker\.register was called/.test(msg) || trap.calls.length) {
        fail(at, `navigator.serviceWorker.register was called on ${transport.origin} (${transport.mode})`);
      }
      fail(at, msg);
    }
    if (trap.calls.length) {
      fail(at, `navigator.serviceWorker.register was called on ${transport.origin} (${transport.mode})`);
    }
    origins.push({
      mode: transport.mode,
      origin: result.origin,
      missingSeam: result.missingSeam,
      reason: result.reason,
    });
  }

  const modes = new Set(origins.map(o => o.mode));
  if (!modes.has("localhost") || !modes.has("channel")) {
    fail(at, "did not exercise both localhost and channel modes");
  }
  const remote = origins.find(o => o.mode === "channel");
  if (remote && remote.missingSeam) {
    fail(at,
      `cannot prove no service worker on a remote origin (${remote.origin}) because the transport-injected boot is missing (${remote.reason})`);
  }
});
