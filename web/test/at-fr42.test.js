/* S2 R1 — AT-FR-42-b (dual-run harness) and AT-FR-42-c (injected assetURL).
 * Failures are missing seam behaviour, not load errors or skips.
 * Drives checked-in web/js/{app,chat,polling,api}.js; no fixture stand-in. */
import test from "node:test";
import assert from "node:assert/strict";
import { attTileHTML, assetTileHTML } from "../js/chat.js";
import {
  missingSeam,
  readWeb,
  runTwice,
  exerciseAppCode,
  driveAppJsSites,
  assertFetchImplRequired,
  canonicalizeHTML,
  urlsInHTML,
  isBlobURL,
  isRendezvousURL,
  allAssetPaths,
  tilePaths,
  APP_FETCH_SITES,
  AGENT_ASSET_PATHS,
  todayTileHTML,
  todayAgentLogoHTML,
  siteSeen,
  attachmentPath,
  sessionAssetPath,
  TILE_NODE,
  TILE_ATT_IMAGE,
  TILE_ASSET_ID,
  TILE_ASSET_IMAGE,
  runAgentLogo,
} from "./s2-helpers.js";

function fail(at, detail) {
  assert.fail(missingSeam(at, detail));
}

function canonicalBundle(result, assetURL) {
  const paths = allAssetPaths();
  return JSON.stringify({
    pollingCalls: result.pollingCalls,
    apiValue: result.apiValue,
    tiles: {
      attImage: canonicalizeHTML(result.tiles.attImage, assetURL, paths),
      attFile: canonicalizeHTML(result.tiles.attFile, assetURL, paths),
      assetImage: canonicalizeHTML(result.tiles.assetImage, assetURL, paths),
      assetFile: canonicalizeHTML(result.tiles.assetFile, assetURL, paths),
      refs: canonicalizeHTML(result.tiles.refs, assetURL, paths),
      split: canonicalizeHTML(result.tiles.split.html, assetURL, paths),
    },
    logos: Object.fromEntries(
      Object.entries(result.logos).map(([k, html]) => [k, canonicalizeHTML(html, assetURL, paths)]),
    ),
  });
}

function htmlUsesSupplier(html, assetURL, path) {
  const resolved = assetURL(path);
  return String(html).includes(resolved);
}

test("AT-FR-42-b: one suite runs twice over the same app code with localhost and channel fetchImpls", async () => {
  const at = "AT-FR-42-b";
  readWeb("js/app.js");
  readWeb("js/chat.js");
  readWeb("js/polling.js");

  const { local, channel, localResult, channelResult } = await runTwice(exerciseAppCode);

  if (!localResult.pollingCalls.some(p => String(p).startsWith("/api/state"))) {
    fail(at, "localhost polling tick did not fetch /api/state through the injected fetchImpl");
  }
  if (!channelResult.pollingCalls.some(p => String(p).startsWith("/api/state"))) {
    fail(at, "channel polling tick did not fetch /api/state through the injected fetchImpl");
  }

  const localCanon = canonicalBundle(localResult, local.assetURL);
  const channelCanon = canonicalBundle(channelResult, channel.assetURL);
  if (localCanon !== channelCanon) {
    fail(at, "localhost and channel suppliers produced different application behaviour after URL canonicalisation");
  }

  const localApp = await driveAppJsSites(local);
  const channelApp = await driveAppJsSites(channel);

  for (const site of APP_FETCH_SITES) {
    if (!siteSeen(localApp.seamCalls, site)) {
      fail(at,
        `${site.pathPrefix} did not go through the injected localhost fetchImpl` +
        (localApp.missingSeam ? ` (${localApp.reason})` : "") +
        (siteSeen(localApp.globalCalls, site) ? "; the composition root still calls global fetch" : ""));
    }
    if (!siteSeen(channelApp.seamCalls, site)) {
      fail(at,
        `${site.pathPrefix} did not go through the injected channel fetchImpl` +
        (channelApp.missingSeam ? ` (${channelApp.reason})` : "") +
        (siteSeen(channelApp.globalCalls, site) ? "; the composition root still calls global fetch" : ""));
    }
  }

  const localPaths = [...new Set(local.fetchImpl.calls.map(c => c.path.split("?")[0]))].sort();
  const channelPaths = [...new Set(channel.fetchImpl.calls.map(c => c.path.split("?")[0]))].sort();
  if (JSON.stringify(localPaths) !== JSON.stringify(channelPaths)) {
    fail(at, `localhost fetchImpl paths ${JSON.stringify(localPaths)} !== channel fetchImpl paths ${JSON.stringify(channelPaths)}`);
  }

  try {
    await assertFetchImplRequired(at);
  } catch (err) {
    fail(at, err.message.replace(/^AT-FR-42-b: missing seam behaviour: /, ""));
  }
});

test("AT-FR-42-c: every asset URL resolves through injected assetURL; localhost is identity, channel is blob:", async () => {
  const at = "AT-FR-42-c";
  const usageSrc = readWeb("js/usage.js");
  const chatSrc = readWeb("js/chat.js");

  for (const path of Object.values(AGENT_ASSET_PATHS)) {
    if (!usageSrc.includes(path)) {
      fail(at, `usage.js agentLogo no longer contains today's asset path ${path}`);
    }
  }
  if (!chatSrc.includes("/attachments/")) {
    fail(at, "chat.js no longer contains the attachment URL builder");
  }
  if (!chatSrc.includes("/assets/")) {
    fail(at, "chat.js no longer contains the session-asset URL builder");
  }

  const marked = path => `SEAM:${path}`;
  const { local, channel, localResult, channelResult } = await runTwice(exerciseAppCode);

  const todayTiles = todayTileHTML();
  const todayLogos = todayAgentLogoHTML();

  for (const key of ["attImage", "attFile", "assetImage", "assetFile"]) {
    if (localResult.tiles[key] !== todayTiles[key]) {
      fail(at, `localhost assetURL is not identity for ${key}: got ${localResult.tiles[key]} want ${todayTiles[key]}`);
    }
  }
  for (const [agent, html] of Object.entries(localResult.logos)) {
    if (html !== todayLogos[agent]) {
      fail(at, `localhost assetURL is not identity for agentLogo(${agent})`);
    }
    const path = AGENT_ASSET_PATHS[agent];
    if (!urlsInHTML(html).includes(path)) {
      fail(at, `localhost agentLogo(${agent}) did not return today's path ${path} byte-for-byte`);
    }
  }

  for (const [agent, path] of Object.entries(AGENT_ASSET_PATHS)) {
    const html = runAgentLogo(agent, marked);
    if (!htmlUsesSupplier(html, marked, path)) {
      fail(at, `usage.js agentLogo(${agent}) does not resolve ${path} through injected assetURL(path)`);
    }
  }

  const markerAtt = attTileHTML(TILE_NODE, TILE_ATT_IMAGE, true, { assetURL: marked });
  const markerAsset = assetTileHTML(TILE_NODE, TILE_ASSET_ID, "alt", TILE_ASSET_IMAGE, { assetURL: marked });
  const attPath = attachmentPath(TILE_NODE, TILE_ATT_IMAGE);
  const assetPath = sessionAssetPath(TILE_NODE, TILE_ASSET_ID);
  if (!htmlUsesSupplier(markerAtt, marked, attPath)) {
    fail(at, `chat.js attachment builder does not resolve through injected assetURL(path); still hardcodes ${attPath}`);
  }
  if (!htmlUsesSupplier(markerAsset, marked, assetPath)) {
    fail(at, `chat.js session-asset builder does not resolve through injected assetURL(path); still hardcodes ${assetPath}`);
  }

  const attSinks = urlsInHTML(channelResult.tiles.attImage);
  const attFileSinks = urlsInHTML(channelResult.tiles.attFile);
  const assetSinks = urlsInHTML(channelResult.tiles.assetImage);
  const assetFileSinks = urlsInHTML(channelResult.tiles.assetFile);
  if (attSinks.length < 2) {
    fail(at, `attachment image tile must feed href+src (2 sinks), got ${attSinks.length}`);
  }
  if (attFileSinks.length < 1) {
    fail(at, "attachment file tile must feed an href sink");
  }
  if (assetSinks.length < 2) {
    fail(at, `session-asset image tile must feed href+src (2 sinks), got ${assetSinks.length}`);
  }
  if (assetFileSinks.length < 1) {
    fail(at, "session-asset file tile must feed an href sink");
  }

  const channelHTMLs = [
    ...Object.values(channelResult.logos),
    channelResult.tiles.attImage,
    channelResult.tiles.attFile,
    channelResult.tiles.assetImage,
    channelResult.tiles.assetFile,
    channelResult.tiles.refs,
    channelResult.tiles.split.html,
  ];
  for (const html of channelHTMLs) {
    for (const url of urlsInHTML(html)) {
      if (isRendezvousURL(url)) {
        fail(at, `channel asset URL resolved to a rendezvous-origin URL: ${url}`);
      }
      if (!isBlobURL(url) && !url.startsWith("#") && !url.startsWith("data:")) {
        fail(at, `channel DOM asset URL is not a blob: URL: ${url}`);
      }
    }
  }

  for (const [agent, path] of Object.entries(AGENT_ASSET_PATHS)) {
    if (!htmlUsesSupplier(channelResult.logos[agent], channel.assetURL, path)) {
      fail(at, `usage.js agentLogo(${agent}) does not resolve ${path} through injected assetURL to a blob: URL`);
    }
  }
  for (const path of tilePaths()) {
    const blob = channel.assetURL(path);
    const blobSeen = [
      channelResult.tiles.attImage,
      channelResult.tiles.attFile,
      channelResult.tiles.assetImage,
      channelResult.tiles.assetFile,
    ].some(html => String(html).includes(blob));
    if (!blobSeen) {
      fail(at, `chat.js builder did not resolve ${path} through injected assetURL (want ${blob})`);
    }
  }
});
