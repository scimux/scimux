/* S2 R1 — dual-run harness for the transport seam.
 *
 * AT-FR-42-b: one suite, run twice over the SAME checked-in web/js/ modules,
 * once with a localhost fetchImpl and once with a fake-channel fetchImpl.
 * This file is not served (not under web/js/). R2 implements the seam; these
 * helpers only drive the real modules and record whether injection was honoured.
 */
import { readFileSync, readdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { createPollingFeature } from "../js/polling.js";
import {
  attTileHTML,
  assetTileHTML,
  refTilesHTML,
  splitAssetRefs,
} from "../js/chat.js";
import { api } from "../js/api.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
export const WEB = join(__dirname, "..");
export const APP_JS = join(WEB, "js/app.js");
export const CHAT_JS = join(WEB, "js/chat.js");
export const POLLING_JS = join(WEB, "js/polling.js");

export const APP_FETCH_SITES = Object.freeze([
  { id: "usage", pathPrefix: "/api/usage", cache: "no-store" },
  { id: "preview", pathPrefix: "/api/preview" },
  { id: "state", pathPrefix: "/api/state", cache: "no-store" },
]);

export const AGENT_ASSET_PATHS = Object.freeze({
  claude: "/assets/agents/claude.svg",
  openai: "/assets/agents/openai.svg",
  pi: "/assets/agents/pi.svg",
  opencode: "/assets/agents/opencode.svg",
  grok: "/assets/agents/grok.svg",
});

export const TILE_NODE = "n1";
export const TILE_ATT_IMAGE = "deadbeef-shot.png";
export const TILE_ATT_FILE = "deadbeef-notes.md";
export const TILE_ASSET_ID = "asset-1";
export const TILE_ASSET_IMAGE = { name: "a.png" };
export const TILE_ASSET_FILE = { name: "a.pdf" };

export function missingSeam(at, detail) {
  return `${at}: missing seam behaviour: ${detail}`;
}

export function readWeb(rel) {
  try {
    return readFileSync(join(WEB, rel), "utf8");
  } catch (err) {
    throw new Error(`${rel} is missing: ${err.message}`);
  }
}

export function servedJsNames() {
  return readdirSync(join(WEB, "js")).filter(f => f.endsWith(".js")).sort();
}

export function identityAssetURL(path) {
  return path;
}

export function attachmentPath(nodeId, leaf) {
  return `/api/nodes/${encodeURIComponent(nodeId)}/attachments/${encodeURIComponent(leaf)}`;
}

export function sessionAssetPath(nodeId, id) {
  return `/api/nodes/${encodeURIComponent(nodeId)}/assets/${encodeURIComponent(id)}`;
}

export const TODAY_TILE_PATHS = Object.freeze([
  attachmentPath(TILE_NODE, TILE_ATT_IMAGE),
  attachmentPath(TILE_NODE, TILE_ATT_FILE),
  sessionAssetPath(TILE_NODE, TILE_ASSET_ID),
]);

export function jsonResponse(body, { status = 200, etag = '"e1"' } = {}) {
  const headers = new Headers({ "Content-Type": "application/json" });
  if (etag) headers.set("ETag", etag);
  return {
    ok: status >= 200 && status < 300,
    status,
    headers,
    async json() {
      return body;
    },
    async text() {
      return typeof body === "string" ? body : JSON.stringify(body);
    },
  };
}

export const CANNED = Object.freeze({
  state: {
    nodes: [],
    unadopted: [],
    hostname: "laptop",
    version: "2.0",
    sys: {},
  },
  ui: { lanes: [], groups: [], bookmarks: [], archived: [], pinned: [] },
  usage: {
    agents: {
      claude: { available: false },
      codex: { available: false },
      grok: { available: false },
    },
  },
  preview: { node: TILE_NODE, title: "t", agent: "claude", model: "opus", turns: [] },
  updateCheck: {
    current: "1.0",
    latest: "2.0",
    available: true,
    url: "https://codeberg.org/chrberger/scimux/releases",
    notes: "",
  },
  update: {},
  agents: {},
});

export function routeBody(path) {
  const p = String(path || "").split("?")[0];
  if (p === "/api/state") return CANNED.state;
  if (p === "/api/ui") return CANNED.ui;
  if (p === "/api/usage") return CANNED.usage;
  if (p === "/api/preview") return CANNED.preview;
  if (p === "/api/update/check") return CANNED.updateCheck;
  if (p === "/api/update") return CANNED.update;
  if (p === "/api/agents") return CANNED.agents;
  return {};
}

export function makeFetchImpl(kind) {
  const calls = [];
  const fetchImpl = async (path, opts = {}) => {
    calls.push({
      kind,
      path: String(path),
      method: opts.method || "GET",
      cache: opts.cache,
      opts,
    });
    return jsonResponse(routeBody(path));
  };
  fetchImpl.kind = kind;
  fetchImpl.calls = calls;
  return fetchImpl;
}

export function localhostTransport() {
  return {
    mode: "localhost",
    origin: "http://127.0.0.1:8787",
    fetchImpl: makeFetchImpl("localhost"),
    assetURL: identityAssetURL,
  };
}

export function channelTransport() {
  const blobs = new Map();
  let n = 0;
  function assetURL(path) {
    const key = String(path);
    if (blobs.has(key)) return blobs.get(key);
    n += 1;
    const url = `blob:channel/${n}/${encodeURIComponent(key)}`;
    blobs.set(key, url);
    return url;
  }
  return {
    mode: "channel",
    origin: "https://my.scimux.eu",
    fetchImpl: makeFetchImpl("channel"),
    assetURL,
    blobs,
  };
}

export async function runTwice(exercise) {
  const local = localhostTransport();
  const channel = channelTransport();
  const localResult = await exercise(local);
  const channelResult = await exercise(channel);
  return { local, channel, localResult, channelResult };
}

export function memStorage(seed = {}) {
  const map = new Map(Object.entries(seed));
  return {
    getItem(k) {
      return map.has(k) ? map.get(k) : null;
    },
    setItem(k, v) {
      map.set(k, String(v));
    },
    removeItem(k) {
      map.delete(k);
    },
  };
}

export function callMatches(call, site) {
  const path = String(call.path || "");
  if (!path.startsWith(site.pathPrefix)) return false;
  if (site.cache && call.cache !== site.cache) return false;
  return true;
}

export function siteSeen(calls, site) {
  return (calls || []).some(c => callMatches(c, site));
}

export function canonicalizeHTML(html, assetURL, paths) {
  let s = String(html);
  for (const path of paths) {
    const resolved = assetURL(path);
    if (resolved && resolved !== path) s = s.split(resolved).join(path);
  }
  return s;
}

export function urlsInHTML(html) {
  const s = String(html);
  const out = [];
  const re = /(?:href|src)=["']([^"']+)["']|url\(['"]?([^'")]+)['"]?\)/g;
  let m;
  while ((m = re.exec(s))) out.push(m[1] || m[2]);
  return out;
}

export function isBlobURL(url) {
  return String(url || "").startsWith("blob:");
}

export function isRendezvousURL(url) {
  return /^https?:\/\//i.test(String(url || "")) && !String(url).startsWith("blob:");
}

function extractNamedFunction(src, name) {
  const needle = `function ${name}(`;
  const start = src.indexOf(needle);
  if (start < 0) return null;
  const brace = src.indexOf("{", start);
  if (brace < 0) return null;
  let depth = 0;
  for (let i = brace; i < src.length; i++) {
    const ch = src[i];
    if (ch === "{") depth++;
    else if (ch === "}") {
      depth--;
      if (depth === 0) return src.slice(start, i + 1);
    }
  }
  return null;
}

export function runAgentLogo(agent, assetURL) {
  const src = readWeb("js/app.js");
  const fnSrc = extractNamedFunction(src, "agentLogo");
  if (!fnSrc) {
    throw new Error("app.js has no agentLogo; the 5 asset-URL builders are missing");
  }
  const make = new Function("assetURL", `"use strict";\n${fnSrc}\nreturn agentLogo;`);
  return make(assetURL)(agent);
}

export function todayAgentLogoHTML() {
  return {
    claude: runAgentLogo("claude", identityAssetURL),
    openai: runAgentLogo("openai", identityAssetURL),
    pi: runAgentLogo("pi", identityAssetURL),
    opencode: runAgentLogo("opencode", identityAssetURL),
    grok: runAgentLogo("grok", identityAssetURL),
  };
}

export function todayTileHTML() {
  return {
    attImage: attTileHTML(TILE_NODE, TILE_ATT_IMAGE, true),
    attFile: attTileHTML(TILE_NODE, TILE_ATT_FILE, false),
    assetImage: assetTileHTML(TILE_NODE, TILE_ASSET_ID, "alt", TILE_ASSET_IMAGE),
    assetFile: assetTileHTML(TILE_NODE, TILE_ASSET_ID, "doc", TILE_ASSET_FILE),
  };
}

export function detectAppBoot(src) {
  const text = src || readWeb("js/app.js");
  const names = ["createApp", "bootApp", "boot"];
  for (const name of names) {
    const fn = new RegExp(String.raw`export\s+(async\s+)?function\s+${name}\b`);
    const named = new RegExp(String.raw`export\s*\{[^}]*\b${name}\b`);
    if (fn.test(text) || named.test(text)) return name;
  }
  return null;
}

function fakeTarget() {
  return {
    hidden: false,
    addEventListener() {},
    removeEventListener() {},
  };
}

export async function exercisePolling(fetchImpl) {
  const f = createPollingFeature({
    document: fakeTarget(),
    window: fakeTarget(),
    storage: memStorage(),
    fetchImpl,
    setTimeout() {
      return 1;
    },
    clearTimeout() {},
  });
  await f.tick();
  return f;
}

export async function exerciseApi(fetchImpl) {
  return api("/api/usage", { cache: "no-store" }, { fetchImpl });
}

export function exerciseTiles(assetURL) {
  const deps = { assetURL };
  const attImage = attTileHTML(TILE_NODE, TILE_ATT_IMAGE, true, deps);
  const attFile = attTileHTML(TILE_NODE, TILE_ATT_FILE, false, deps);
  const assetImage = assetTileHTML(TILE_NODE, TILE_ASSET_ID, "alt", TILE_ASSET_IMAGE, deps);
  const assetFile = assetTileHTML(TILE_NODE, TILE_ASSET_ID, "doc", TILE_ASSET_FILE, deps);
  const refs = refTilesHTML(
    [{ path: TILE_ATT_IMAGE, mime: "image/png" }],
    TILE_NODE,
    deps,
  );
  const split = splitAssetRefs(
    `see ![pic](scimux-asset:${TILE_ASSET_ID})`,
    TILE_NODE,
    { [TILE_ASSET_ID]: TILE_ASSET_IMAGE },
    deps,
  );
  return { attImage, attFile, assetImage, assetFile, refs, split };
}

export function exerciseLogos(assetURL) {
  return {
    claude: runAgentLogo("claude", assetURL),
    openai: runAgentLogo("openai", assetURL),
    pi: runAgentLogo("pi", assetURL),
    opencode: runAgentLogo("opencode", assetURL),
    grok: runAgentLogo("grok", assetURL),
  };
}

export function tilePaths() {
  return [
    attachmentPath(TILE_NODE, TILE_ATT_IMAGE),
    attachmentPath(TILE_NODE, TILE_ATT_FILE),
    sessionAssetPath(TILE_NODE, TILE_ASSET_ID),
  ];
}

export function logoPaths() {
  return Object.values(AGENT_ASSET_PATHS);
}

export function allAssetPaths() {
  return [...logoPaths(), ...tilePaths()];
}

export async function exerciseAppCode(transport) {
  const pollingCallsBefore = transport.fetchImpl.calls.length;
  await exercisePolling(transport.fetchImpl);
  const pollingCalls = transport.fetchImpl.calls
    .slice(pollingCallsBefore)
    .map(c => c.path);
  const apiValue = await exerciseApi(transport.fetchImpl);
  const tiles = exerciseTiles(transport.assetURL);
  const logos = exerciseLogos(transport.assetURL);
  return {
    mode: transport.mode,
    pollingCalls,
    apiValue,
    tiles,
    logos,
    seamCalls: transport.fetchImpl.calls.slice(),
  };
}

async function flush(n = 12) {
  for (let i = 0; i < n; i++) await Promise.resolve();
}

function classList() {
  const set = new Set();
  return {
    add(...xs) {
      xs.forEach(x => set.add(x));
    },
    remove(...xs) {
      xs.forEach(x => set.delete(x));
    },
    toggle(x, force) {
      if (force === true) {
        set.add(x);
        return true;
      }
      if (force === false) {
        set.delete(x);
        return false;
      }
      if (set.has(x)) {
        set.delete(x);
        return false;
      }
      set.add(x);
      return true;
    },
    contains(x) {
      return set.has(x);
    },
  };
}

function makeEl(tag, id) {
  const attrs = {};
  const listeners = new Map();
  const children = [];
  const el = {
    id: id || "",
    tagName: String(tag || "div").toUpperCase(),
    nodeName: String(tag || "div").toUpperCase(),
    hidden: false,
    disabled: false,
    textContent: "",
    innerHTML: "",
    innerText: "",
    value: "",
    checked: false,
    className: "",
    classList: classList(),
    style: {
      setProperty() {},
      cssText: "",
      color: "",
      display: "",
      top: "",
      left: "",
      width: "",
      height: "",
    },
    dataset: {},
    children,
    parentNode: null,
    parentElement: null,
    scrollTop: 0,
    scrollHeight: 0,
    clientHeight: 100,
    offsetHeight: 100,
    offsetWidth: 100,
    href: "",
    content: "",
    isContentEditable: false,
    addEventListener(type, fn) {
      if (!listeners.has(type)) listeners.set(type, new Set());
      listeners.get(type).add(fn);
    },
    removeEventListener(type, fn) {
      listeners.get(type)?.delete(fn);
    },
    dispatchEvent(ev) {
      for (const fn of listeners.get(ev && ev.type) || []) fn(ev);
      return true;
    },
    setAttribute(k, v) {
      attrs[k] = String(v);
      if (k === "id") el.id = String(v);
    },
    getAttribute(k) {
      return Object.prototype.hasOwnProperty.call(attrs, k) ? attrs[k] : null;
    },
    removeAttribute(k) {
      delete attrs[k];
    },
    hasAttribute(k) {
      return Object.prototype.hasOwnProperty.call(attrs, k);
    },
    querySelector() {
      return null;
    },
    querySelectorAll() {
      return [];
    },
    appendChild(c) {
      children.push(c);
      if (c) {
        c.parentNode = el;
        c.parentElement = el;
      }
      return c;
    },
    removeChild(c) {
      const i = children.indexOf(c);
      if (i >= 0) children.splice(i, 1);
      return c;
    },
    remove() {
      if (el.parentNode && el.parentNode.removeChild) el.parentNode.removeChild(el);
    },
    click() {
      el.dispatchEvent({
        type: "click",
        target: el,
        currentTarget: el,
        preventDefault() {},
        stopPropagation() {},
      });
    },
    focus() {},
    blur() {},
    select() {},
    setSelectionRange() {},
    getBoundingClientRect() {
      return { top: 0, left: 0, bottom: 0, right: 0, width: 0, height: 0 };
    },
    closest(sel) {
      let n = el;
      while (n) {
        const cls = String(n.className || "").split(/\s+/);
        if (typeof sel === "string" && sel.startsWith(".") && cls.includes(sel.slice(1))) return n;
        if (typeof sel === "string" && sel.startsWith("#") && n.id === sel.slice(1)) return n;
        n = n.parentElement;
      }
      return null;
    },
    contains() {
      return false;
    },
    matches() {
      return false;
    },
    insertBefore(c) {
      return el.appendChild(c);
    },
  };
  return el;
}

export function installServiceWorkerTrap() {
  const calls = [];
  const serviceWorker = {
    register(scriptURL, options) {
      calls.push({ scriptURL, options });
      throw new Error("AT-FR-15a-a: navigator.serviceWorker.register was called");
    },
    getRegistration: async () => undefined,
    getRegistrations: async () => [],
    addEventListener() {},
    removeEventListener() {},
  };
  return { calls, serviceWorker };
}

function def(name, value) {
  Object.defineProperty(globalThis, name, {
    configurable: true,
    writable: true,
    value,
  });
}

export function installHost({ origin, fetchImpl, serviceWorker } = {}) {
  const html = readWeb("index.html");
  const ids = [...html.matchAll(/\sid="([^"]+)"/g)].map(m => m[1]);
  const byId = new Map();
  for (const id of ids) byId.set(id, makeEl("div", id));
  const body = makeEl("body", "");
  const documentElement = makeEl("html", "");
  documentElement.clientHeight = 800;
  const csrf = makeEl("meta", "");
  csrf.content = "test-csrf";
  const globalCalls = [];
  const trapFetch = async (path, opts = {}) => {
    globalCalls.push({
      kind: "global",
      path: String(path),
      method: (opts && opts.method) || "GET",
      cache: opts && opts.cache,
      opts,
    });
    return jsonResponse(routeBody(path));
  };

  const document = {
    hidden: false,
    body,
    documentElement,
    activeElement: body,
    title: "",
    querySelector(sel) {
      if (sel === 'meta[name="scimux-csrf"]') return csrf;
      if (typeof sel === "string" && sel.startsWith("#") && !/[ .\[>:]/.test(sel.slice(1))) {
        return byId.get(sel.slice(1)) || null;
      }
      if (sel === "#menu .lic") return null;
      return null;
    },
    querySelectorAll(sel) {
      if (sel === "#menu .lic") return [];
      const one = document.querySelector(sel);
      return one ? [one] : [];
    },
    getElementById(id) {
      return byId.get(id) || null;
    },
    createElement(tag) {
      return makeEl(tag);
    },
    addEventListener() {},
    removeEventListener() {},
    contains() {
      return true;
    },
  };

  function mql(matches) {
    return {
      matches,
      addEventListener() {},
      removeEventListener() {},
      addListener() {},
      removeListener() {},
    };
  }

  const storage = memStorage();
  const loc = {
    href: origin || "http://127.0.0.1:8787/",
    origin: origin || "http://127.0.0.1:8787",
    reload() {},
  };
  const sw = serviceWorker || installServiceWorkerTrap().serviceWorker;
  const nav = {
    clipboard: { writeText: async () => {} },
    serviceWorker: sw,
  };
  const windowObj = {
    document,
    localStorage: storage,
    matchMedia: q => mql(/min-width:\s*900/.test(q)),
    addEventListener() {},
    removeEventListener() {},
    innerHeight: 800,
    innerWidth: 1200,
    isSecureContext: true,
    visualViewport: {
      addEventListener() {},
      removeEventListener() {},
      height: 800,
      offsetTop: 0,
    },
    location: loc,
    getComputedStyle() {
      return { color: "rgb(0,0,0)", getPropertyValue: () => "" };
    },
    requestAnimationFrame: fn => setTimeout(fn, 0),
    CSS: {
      escape: s => String(s).replace(/[^a-zA-Z0-9_-]/g, c => "\\" + c),
    },
    navigator: nav,
    confirm: () => true,
    alert() {},
    prompt: () => "",
    fetch: fetchImpl || trapFetch,
  };

  def("document", document);
  def("window", windowObj);
  def("localStorage", storage);
  def("matchMedia", windowObj.matchMedia);
  def("CSS", windowObj.CSS);
  def("getComputedStyle", windowObj.getComputedStyle);
  def("requestAnimationFrame", windowObj.requestAnimationFrame);
  def("navigator", nav);
  def("confirm", windowObj.confirm);
  def("alert", windowObj.alert);
  def("prompt", windowObj.prompt);
  def("innerWidth", 1200);
  def("innerHeight", 800);
  def("location", loc);
  globalThis.fetch = trapFetch;

  return { document, window: windowObj, byId, globalCalls, trapFetch, origin: loc.origin };
}

function bareAppFetchSites(src) {
  const found = [];
  for (const site of APP_FETCH_SITES) {
    const re = new RegExp(String.raw`\bfetch\s*\(\s*["'\`]${site.pathPrefix}`);
    if (re.test(src)) found.push(site.id);
  }
  return found;
}

async function triggerAppSites(host) {
  await flush(20);
  await new Promise(resolve => setTimeout(resolve, 0));
  const check = host.byId.get("m_check");
  if (check && typeof check.click === "function") check.click();
  await flush(20);
  await new Promise(resolve => setTimeout(resolve, 0));
  const feed = host.byId.get("searchfeed");
  if (feed && typeof feed.dispatchEvent === "function") {
    const hit = makeEl("div");
    hit.className = "searchhit";
    hit.dataset.uid = "uid-1";
    hit.dataset.segment = "0";
    hit.dataset.record = "0";
    hit.parentElement = feed;
    feed.dispatchEvent({
      type: "click",
      target: hit,
      currentTarget: feed,
      preventDefault() {},
      stopPropagation() {},
    });
  }
  await flush(20);
}

export async function driveAppJsSites(transport, { serviceWorker } = {}) {
  const src = readWeb("js/app.js");
  const bootName = detectAppBoot(src);
  const host = installHost({
    origin: transport.origin,
    fetchImpl: transport.fetchImpl,
    serviceWorker,
  });
  const callsBefore = transport.fetchImpl.calls.length;

  if (bootName) {
    const mod = await import("../js/app.js");
    const boot = mod[bootName];
    if (typeof boot !== "function") {
      return {
        missingSeam: true,
        reason: `app.js names ${bootName} but does not export a function`,
        seamCalls: transport.fetchImpl.calls.slice(callsBefore),
        globalCalls: host.globalCalls.slice(),
        origin: host.origin,
      };
    }
    await boot({
      fetchImpl: transport.fetchImpl,
      assetURL: transport.assetURL,
      document: host.document,
      window: host.window,
    });
    try { await triggerAppSites(host); } catch { /* keep observations */ }
    return {
      missingSeam: false,
      seamCalls: transport.fetchImpl.calls.slice(callsBefore),
      globalCalls: host.globalCalls.slice(),
      origin: host.origin,
    };
  }

  const bare = bareAppFetchSites(src);
  const bareNote = bare.length
    ? `; still has bare fetch() for ${bare.join(", ")}`
    : "";
  return {
    missingSeam: true,
    reason: "app.js does not export a transport-injected boot; the 3 fetch sites close over global fetch" + bareNote,
    seamCalls: transport.fetchImpl.calls.slice(callsBefore),
    globalCalls: host.globalCalls.slice(),
    origin: host.origin,
  };
}

export async function assertFetchImplRequired(at) {
  const prev = globalThis.fetch;
  let usedGlobal = false;
  globalThis.fetch = async (path, opts) => {
    usedGlobal = true;
    return jsonResponse(routeBody(path));
  };
  try {
    const f = createPollingFeature({
      document: fakeTarget(),
      window: fakeTarget(),
      storage: memStorage(),
      setTimeout() {
        return 1;
      },
      clearTimeout() {},
    });
    if (typeof f.tick === "function") await f.tick();
  } catch (err) {
    const msg = err && err.message ? err.message : String(err);
    if (/fetchImpl/i.test(msg)) return;
    if (usedGlobal) {
      throw new Error(missingSeam(at,
        "polling.js still defaults fetchImpl to globalThis.fetch; the supplier must be required so a future caller cannot silently acquire the global"));
    }
    throw err;
  } finally {
    globalThis.fetch = prev;
  }
  if (usedGlobal) {
    throw new Error(missingSeam(at,
      "polling.js still defaults fetchImpl to globalThis.fetch; the supplier must be required so a future caller cannot silently acquire the global"));
  }
  throw new Error(missingSeam(at,
    "polling.js accepted a missing fetchImpl without throwing"));
}
