/* Characterization tests for web/js/api.js — CSRF wrapper, response decoding,
 * and exact /api/ui request contracts. Imports the real module (not HTML
 * substring extraction). Uses scripted fake fetch; never touches a real network. */
import test from "node:test";
import assert from "node:assert/strict";
import {
  UNSAFE,
  UI_PATH,
  MAX_FLUSH_ATTEMPTS,
  CSRF_HEADER,
  withCsrf,
  decodeResponse,
  api,
  uiInitialGetRequest,
  uiPollGetRequest,
  uiPutRequest,
  readETag,
  isConflictStatus,
  pagehideShouldSend,
  pagehidePutRequest,
} from "../js/api.js";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const apiSrc = readFileSync(join(__dirname, "../js/api.js"), "utf8");

/* Minimal Headers stand-in if needed; Node 22 provides global Headers. */
const H = Headers;

/* ---------- purity / injected boundaries ---------- */
test("api.js has no document/window/localStorage or bare fetch globals", () => {
  const code = apiSrc
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/\/\/.*$/gm, "");
  const forbidden = [
    /\bdocument\b/,
    /\bwindow\b/,
    /\blocalStorage\b/,
    /\bnavigator\b/,
    /\bsetTimeout\b/,
    /\bsetInterval\b/,
    /\brequestAnimationFrame\b/,
    /\baddEventListener\b/,
    /\binnerHTML\b/,
    /\bcreateElement\b/,
    /\brenderSyncState\b/,
    /\brenderBookmarksPane\b/,
    /\brenderCards\b/,
    /\bapplyRemoteUI\b/,
    /\buiTimer\b/,
    /\buiLoaded\b/,
    /\buiSaving\b/,
    /\buiOps\b/,
    /\buiRev\b/,
  ];
  for (const re of forbidden) {
    assert.equal(re.test(code), false, `api.js must not reference ${re}`);
  }
  // Bare `fetch(` would call the global; only fetchImpl( is allowed for network.
  assert.equal(/\bfetch\s*\(/.test(code), false, "must not call global fetch(");
  // CSRF token must not be read from the meta tag in this module.
  assert.equal(/scimux-csrf/.test(code), false, "must not read the CSRF meta tag");
  assert.equal(/querySelector/.test(code), false);
});

test("api.js exports named helpers and reuses state.js", async () => {
  const mod = await import("../js/api.js");
  for (const name of [
    "UNSAFE",
    "UI_PATH",
    "MAX_FLUSH_ATTEMPTS",
    "CSRF_HEADER",
    "withCsrf",
    "decodeResponse",
    "api",
    "uiInitialGetRequest",
    "uiPollGetRequest",
    "uiPutRequest",
    "readETag",
    "isConflictStatus",
    "pagehideShouldSend",
    "pagehidePutRequest",
    "emptyUISyncState",
    "loadUIState",
    "flushUIState",
    "pollUIState",
  ]) {
    assert.equal(typeof mod[name] !== "undefined", true, `missing export ${name}`);
  }
  assert.match(apiSrc, /from\s+["']\.\/state\.js["']/);
  assert.match(apiSrc, /\badoptRemoteAndReplay\b/);
  assert.equal(/function\s+adoptRemote\b/.test(apiSrc), false);
  assert.equal(MAX_FLUSH_ATTEMPTS, 4);
  assert.equal(UI_PATH, "/api/ui");
  assert.equal(CSRF_HEADER, "X-Scimux-CSRF");
});

/* ---------- UNSAFE / withCsrf ---------- */
test("safe-method options pass through unchanged (same reference)", () => {
  const opts = { method: "GET", headers: { Accept: "text/plain" } };
  assert.equal(withCsrf(opts, "tok"), opts);
  assert.equal(withCsrf({ method: "HEAD" }, "tok").method, "HEAD");
  assert.equal(withCsrf({ method: "OPTIONS" }, "tok").method, "OPTIONS");
  // Default method is GET when omitted
  const bare = { headers: { A: "1" } };
  assert.equal(withCsrf(bare, "tok"), bare);
});

test("case-insensitive unsafe methods receive CSRF", () => {
  for (const method of ["POST", "post", "Put", "PUT", "PATCH", "patch", "DELETE", "delete"]) {
    const out = withCsrf({ method, body: "{}" }, "secret", H);
    assert.notEqual(out, undefined);
    assert.equal(out.headers.get(CSRF_HEADER), "secret", method);
  }
});

test("existing headers retained; X-Scimux-CSRF overwritten with supplied token", () => {
  const out = withCsrf(
    {
      method: "POST",
      headers: {
        Accept: "application/json",
        "X-Scimux-CSRF": "stale",
        "X-Custom": "keep",
      },
      body: "{}",
    },
    "fresh",
    H,
  );
  assert.equal(out.headers.get("Accept"), "application/json");
  assert.equal(out.headers.get("X-Custom"), "keep");
  assert.equal(out.headers.get(CSRF_HEADER), "fresh");
});

test("Content-Type becomes application/json only for string bodies lacking it", () => {
  const auto = withCsrf({ method: "POST", body: JSON.stringify({ a: 1 }) }, "t", H);
  assert.equal(auto.headers.get("Content-Type"), "application/json");

  const existing = withCsrf(
    {
      method: "POST",
      headers: { "Content-Type": "text/plain" },
      body: "raw",
    },
    "t",
    H,
  );
  assert.equal(existing.headers.get("Content-Type"), "text/plain");

  // Case-insensitive has() — existing content-type blocks auto JSON
  const mixed = withCsrf(
    {
      method: "PUT",
      headers: { "content-type": "application/x-www-form-urlencoded" },
      body: "a=1",
    },
    "t",
    H,
  );
  assert.match(mixed.headers.get("Content-Type").toLowerCase(), /x-www-form-urlencoded/);
});

test("multipart/FormData does not receive an explicit JSON content type", () => {
  const fd = new FormData();
  fd.append("f", "v");
  const out = withCsrf({ method: "POST", body: fd }, "t", H);
  assert.equal(out.headers.has("Content-Type"), false);
  assert.equal(out.headers.get(CSRF_HEADER), "t");
  assert.equal(out.body, fd);
});

/* ---------- decodeResponse / api ---------- */
function fakeResponse({ ok = true, status = 200, body = "", contentType = "text/plain", headers = {} } = {}) {
  const h = new Headers(headers);
  if (contentType) h.set("Content-Type", contentType);
  return {
    ok,
    status,
    headers: h,
    async text() {
      return typeof body === "string" ? body : JSON.stringify(body);
    },
    async json() {
      if (typeof body === "string") return JSON.parse(body);
      return body;
    },
  };
}

test("generic non-OK responses produce Error with response text and numeric status", async () => {
  await assert.rejects(
    () => decodeResponse(fakeResponse({ ok: false, status: 409, body: "conflict body", contentType: "text/plain" })),
    (err) => {
      assert.equal(err instanceof Error, true);
      assert.equal(err.message, "conflict body");
      assert.equal(err.status, 409);
      return true;
    },
  );
  await assert.rejects(
    () => decodeResponse(fakeResponse({ ok: false, status: 500, body: "boom" })),
    (err) => err.status === 500 && err.message === "boom",
  );
});

test("successful responses decode as JSON when content-type contains json, otherwise text", async () => {
  const j = await decodeResponse(
    fakeResponse({ body: { x: 1 }, contentType: "application/json; charset=utf-8" }),
  );
  assert.deepEqual(j, { x: 1 });

  const j2 = await decodeResponse(
    fakeResponse({ body: '{"y":2}', contentType: "application/vnd.api+json" }),
  );
  assert.deepEqual(j2, { y: 2 });

  const t = await decodeResponse(
    fakeResponse({ body: "plain ok", contentType: "text/plain" }),
  );
  assert.equal(t, "plain ok");

  // missing content-type → text path
  const t2 = await decodeResponse(
    fakeResponse({ body: "no-ct", contentType: "" }),
  );
  assert.equal(t2, "no-ct");
});

test("malformed JSON rejects rather than silently falling back", async () => {
  await assert.rejects(
    () =>
      decodeResponse(
        fakeResponse({ body: "{not-json", contentType: "application/json" }),
      ),
    (err) => err instanceof SyntaxError || /JSON|Unexpected/i.test(String(err)),
  );
});

test("api() uses fetchImpl, applies CSRF on unsafe methods, and decodes", async () => {
  const calls = [];
  const fetchImpl = async (path, opts) => {
    calls.push({ path, opts });
    return fakeResponse({ body: { id: "n1" }, contentType: "application/json" });
  };
  const out = await api(
    "/api/nodes",
    { method: "POST", body: JSON.stringify({ title: "t" }) },
    { fetchImpl, csrf: "tok" },
  );
  assert.deepEqual(out, { id: "n1" });
  assert.equal(calls.length, 1);
  assert.equal(calls[0].path, "/api/nodes");
  assert.equal(calls[0].opts.headers.get(CSRF_HEADER), "tok");
  assert.equal(calls[0].opts.headers.get("Content-Type"), "application/json");
});

test("api() leaves safe GET opts untouched before fetchImpl", async () => {
  const opts = { method: "GET", headers: { Accept: "application/json" } };
  let seen;
  const fetchImpl = async (_p, o) => {
    seen = o;
    return fakeResponse({ body: "x", contentType: "text/plain" });
  };
  await api("/api/state", opts, { fetchImpl, csrf: "tok" });
  assert.equal(seen, opts);
  assert.equal(seen.headers["Accept"] || seen.headers.Accept, "application/json");
});

/* ---------- /api/ui request contracts ---------- */
test("unconditional initial GET has no conditional headers", () => {
  const { path, opts } = uiInitialGetRequest();
  assert.equal(path, "/api/ui");
  assert.deepEqual(opts, {});
  assert.equal(opts.headers, undefined);
});

test("conditional poll GET includes If-None-Match only when a revision exists", () => {
  const withRev = uiPollGetRequest('"abc"');
  assert.equal(withRev.path, "/api/ui");
  assert.deepEqual(withRev.opts.headers, { "If-None-Match": '"abc"' });

  const noRev = uiPollGetRequest("");
  assert.deepEqual(noRev.opts.headers, {});
  const missing = uiPollGetRequest(undefined);
  assert.deepEqual(missing.opts.headers, {});
});

test("conditional PUT has exact If-Match, JSON body, CSRF, and never If-Match: *", () => {
  const doc = { groups: [], archived: ["a"], bookmarks: [], lanes: [], pinned: [] };
  const { path, opts } = uiPutRequest(doc, '"rev-1"', { csrf: "c", HeadersImpl: H });
  assert.equal(path, "/api/ui");
  assert.equal(opts.method, "PUT");
  assert.equal(opts.headers.get("If-Match"), '"rev-1"');
  assert.notEqual(opts.headers.get("If-Match"), "*");
  assert.equal(opts.headers.get("Content-Type"), "application/json");
  assert.equal(opts.headers.get(CSRF_HEADER), "c");
  assert.equal(opts.body, JSON.stringify(doc));
  assert.equal(opts.keepalive, undefined);

  // Empty rev is still the exact string "" if a caller builds it — contracts
  // never invent "*". Production pagehide/flush gates prevent empty-rev PUTs.
  const empty = uiPutRequest(doc, "", { csrf: "c", HeadersImpl: H });
  assert.equal(empty.opts.headers.get("If-Match"), "");
  assert.notEqual(empty.opts.headers.get("If-Match"), "*");
});

test("optional keepalive for pagehide PUT", () => {
  const doc = { groups: [], archived: [], bookmarks: [], lanes: [], pinned: [] };
  const { opts } = uiPutRequest(doc, '"r"', { csrf: "c", keepalive: true, HeadersImpl: H });
  assert.equal(opts.keepalive, true);
  assert.equal(opts.headers.get("If-Match"), '"r"');
});

test("readETag missing → empty string; present → exact value", () => {
  assert.equal(readETag(fakeResponse({ headers: {} })), "");
  assert.equal(readETag(fakeResponse({ headers: { ETag: '"e1"' } })), '"e1"');
});

test("isConflictStatus recognizes 409 and 428 only", () => {
  assert.equal(isConflictStatus(409), true);
  assert.equal(isConflictStatus(428), true);
  assert.equal(isConflictStatus(304), false);
  assert.equal(isConflictStatus(500), false);
  assert.equal(isConflictStatus(200), false);
});

test("pagehide sends only when UI is loaded, pending work exists, and revision is known", () => {
  assert.equal(pagehideShouldSend({ loaded: true, ops: [{ k: "pin", id: "n" }], rev: '"r"' }), true);
  assert.equal(pagehideShouldSend({ loaded: false, ops: [{ k: "pin" }], rev: '"r"' }), false);
  assert.equal(pagehideShouldSend({ loaded: true, ops: [], rev: '"r"' }), false);
  assert.equal(pagehideShouldSend({ loaded: true, ops: [{ k: "pin" }], rev: "" }), false);
  assert.equal(pagehideShouldSend({ loaded: true, ops: null, rev: '"r"' }), false);

  const doc = { groups: [1], archived: [], bookmarks: [], lanes: [], pinned: [] };
  const req = pagehidePutRequest(doc, {
    loaded: true,
    ops: [{ k: "groups" }],
    rev: '"ok"',
    csrf: "tok",
  });
  assert.equal(req.path, "/api/ui");
  assert.equal(req.opts.keepalive, true);
  assert.equal(req.opts.headers.get("If-Match"), '"ok"');
  assert.equal(req.opts.headers.get(CSRF_HEADER), "tok");

  assert.equal(
    pagehidePutRequest(doc, { loaded: true, ops: [{ k: "x" }], rev: "", csrf: "tok" }),
    null,
  );
});

test("UNSAFE regex matches production methods only", () => {
  assert.equal(UNSAFE.test("POST"), true);
  assert.equal(UNSAFE.test("GET"), false);
  assert.equal(UNSAFE.test("TRACE"), false);
});
