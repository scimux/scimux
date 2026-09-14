/* S1 (remote refactor) — AT-NFR-14-a WebCrypto half.
 *
 * Independently recomputes internal/remote/testdata/vectors.sha256 against
 * the vendored vector directory using Web Crypto, then executes every
 * construction vector from its documented inputs. HTTP/rejection rows are
 * structurally validated; they are not replayed through an rv handler.
 * Unknown kinds fail. Hermetic: no network, no child_process, no rv
 * checkout, no Go test.
 */
import test from "node:test";
import assert from "node:assert/strict";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(__dirname, "..", "..");
const specRel = join("docs", "protocol", "rendezvous-v1.md");
const vectorRel = join("internal", "remote", "testdata", "vectors");
const digestRel = join("internal", "remote", "testdata", "vectors.sha256");
const CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";
const ED25519_PKCS8_PREFIX = hexToBytes("302e020100300506032b657004220420");
const P256_PKCS8_PREFIX = hexToBytes("3041020100301306072a8648ce3d020106082a8648ce3d030107042730250201010420");

function vendoredPath(...parts) {
  return join(repoRoot, ...parts);
}

function readVendored(rel) {
  const path = vendoredPath(rel);
  try {
    return readFileSync(path);
  } catch (err) {
    assert.fail(`AT-NFR-14-a: ${rel} is absent: ${err.message}`);
  }
}

function listVectorFiles() {
  const dir = vendoredPath(vectorRel);
  let st;
  try {
    st = statSync(dir);
  } catch (err) {
    assert.fail(`AT-NFR-14-a: vector directory ${vectorRel} is absent: ${err.message}`);
  }
  assert.ok(st.isDirectory(), `AT-NFR-14-a: ${vectorRel} is not a directory`);
  const names = [];
  for (const ent of readdirSync(dir, { withFileTypes: true })) {
    if (ent.isDirectory()) {
      assert.fail(`AT-NFR-14-a: extra subdirectory ${vectorRel}/${ent.name}`);
    }
    if (!ent.name.endsWith(".json")) {
      assert.fail(`AT-NFR-14-a: extra unlisted file ${vectorRel}/${ent.name}`);
    }
    names.push(ent.name);
  }
  names.sort();
  assert.ok(names.length > 0, `AT-NFR-14-a: ${vectorRel} has no *.json files`);
  return names;
}

async function sha256Hex(bytes) {
  const digest = await crypto.subtle.digest("SHA-256", bytes);
  return bytesToHex(new Uint8Array(digest));
}

function bytesToHex(bytes) {
  let out = "";
  for (const b of bytes) out += b.toString(16).padStart(2, "0");
  return out;
}

function hexToBytes(h) {
  if (!h || h.length % 2) {
    throw new Error(`bad hex ${h}`);
  }
  if (![...h].every((c) => "0123456789abcdefABCDEF".includes(c))) {
    throw new Error(`bad hex ${h}`);
  }
  const out = new Uint8Array(h.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = parseInt(h.slice(i * 2, i * 2 + 2), 16);
  return out;
}

function concatBytes(...parts) {
  const total = parts.reduce((n, p) => n + p.length, 0);
  const out = new Uint8Array(total);
  let off = 0;
  for (const p of parts) {
    out.set(p, off);
    off += p.length;
  }
  return out;
}

function utf8(s) {
  return new TextEncoder().encode(s);
}

function base64urlToBytes(s) {
  const pad = s.length % 4 === 0 ? "" : "=".repeat(4 - (s.length % 4));
  const b64 = s.replace(/-/g, "+").replace(/_/g, "/") + pad;
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function equalBytes(a, b) {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false;
  return true;
}

function parseDigest(raw) {
  const text = new TextDecoder("utf-8").decode(raw);
  assert.ok(text.endsWith("\n"), "AT-NFR-14-a: vectors.sha256 must end with a newline");
  const lines = text.slice(0, -1).split("\n");
  assert.ok(lines.length > 0, "AT-NFR-14-a: vectors.sha256 has no entries");
  const entries = [];
  const seen = new Map();
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    assert.notEqual(line, "", `AT-NFR-14-a: digest has a blank line at ${i + 1}`);
    assert.ok(!line.includes("\r"), `AT-NFR-14-a: digest line ${i + 1} contains CR`);
    const sep = line.indexOf("  ");
    assert.ok(sep === 64, `AT-NFR-14-a: digest line ${i + 1} is malformed (want \`hex  filename\`)`);
    const hexPart = line.slice(0, sep);
    const name = line.slice(sep + 2);
    assert.ok(/^[0-9a-f]{64}$/.test(hexPart), `AT-NFR-14-a: digest line ${i + 1} has a malformed SHA-256`);
    assert.ok(name.endsWith(".json"), `AT-NFR-14-a: digest line ${i + 1} does not name a .json file`);
    assert.ok(!/[\\/ \t]/.test(name), `AT-NFR-14-a: digest line ${i + 1} filename must be a basename`);
    assert.equal(seen.get(name), undefined, `AT-NFR-14-a: duplicate digest entry for ${name}`);
    seen.set(name, hexPart);
    entries.push({ hex: hexPart, name });
  }
  return { entries, byName: seen };
}

function crockford(src) {
  let acc = 0n;
  let bits = 0;
  let out = "";
  for (const b of src) {
    acc = (acc << 8n) | BigInt(b);
    bits += 8;
    while (bits >= 5) {
      bits -= 5;
      out += CROCKFORD[Number((acc >> BigInt(bits)) & 31n)];
    }
  }
  if (bits > 0) out += CROCKFORD[Number((acc << BigInt(5 - bits)) & 31n)];
  return out;
}

function httpBody(side) {
  if (!side) return new Uint8Array();
  if (side.body_hex && side.body_utf8) {
    throw new Error("both body_hex and body_utf8");
  }
  if (side.body_hex) return hexToBytes(side.body_hex);
  return utf8(side.body_utf8 || "");
}

function headerLookup(headers, key) {
  if (!headers) return undefined;
  if (Object.prototype.hasOwnProperty.call(headers, key)) return headers[key];
  const want = key.toLowerCase();
  for (const [k, v] of Object.entries(headers)) {
    if (k.toLowerCase() === want) return v;
  }
  return undefined;
}

function jsonNumberV(body) {
  if (body.length === 0) return { present: false };
  let text;
  try {
    text = new TextDecoder("utf-8").decode(body).trim();
  } catch {
    return { present: false };
  }
  if (!text.startsWith("{")) return { present: false };
  let obj;
  try {
    obj = JSON.parse(text);
  } catch {
    return { present: false };
  }
  if (!obj || typeof obj !== "object" || !("v" in obj)) return { present: false };
  return { present: true, numeric: typeof obj.v === "number", value: obj.v };
}

function isRateLimit(message) {
  return message === "envelope-rate-limit" || message === "pair-rate-limit";
}

function isRejection(v) {
  return v.kind === "rejection" || v.message === "constant-rejection" || v.message === "p-rejection" || String(v.message).startsWith("rejection-");
}

function validateHTTP(v) {
  assert.ok(v.request, `${v.id}: http/rejection vector needs a request`);
  assert.ok(v.response, `${v.id}: http/rejection vector needs a response`);
  assert.ok(v.request.method, `${v.id}: request needs a method`);
  assert.ok(v.request.path, `${v.id}: request needs a path`);
  assert.ok(v.response.status, `${v.id}: response needs a status`);
  for (const side of ["request", "response"]) {
    const h = v[side];
    const body = httpBody(h);
    const clHeader = headerLookup(h.headers, "Content-Length");
    if (clHeader !== undefined) {
      const n = Number(clHeader);
      assert.equal(n, body.length, `${v.id}: ${side} Content-Length ${n}, body is ${body.length} bytes`);
    }
    if (typeof h.content_length === "number" && h.content_length >= 0) {
      assert.equal(h.content_length, body.length, `${v.id}: ${side} content_length ${h.content_length}, body is ${body.length} bytes`);
    }
    if (side === "response") {
      const ct = headerLookup(h.headers, "Content-Type") || "";
      if (ct.includes("application/json") && body.length > 0) {
        JSON.parse(new TextDecoder("utf-8").decode(body));
      }
    }
  }
  if (isRateLimit(v.message)) {
    assert.equal(v.response.status, 429, `${v.id}: rate-limit status`);
    assert.ok(headerLookup(v.response.headers, "Retry-After"), `${v.id}: rate-limit needs Retry-After`);
    assert.equal(httpBody(v.response).length, 0, `${v.id}: rate-limit body must be empty`);
    return;
  }
  if (isRejection(v)) {
    assert.equal(v.response.status, 404, `${v.id}: rejection status ${v.response.status}, want 404`);
    assert.equal(new TextDecoder("utf-8").decode(httpBody(v.response)), "not found\n", `${v.id}: rejection body`);
    assert.equal(headerLookup(v.response.headers, "Content-Type"), "text/plain; charset=utf-8", `${v.id}: rejection Content-Type`);
    assert.equal(String(headerLookup(v.response.headers, "X-Content-Type-Options")).toLowerCase(), "nosniff", `${v.id}: rejection nosniff`);
  }
}

function concatAuth(origin, version, route, handle, challenge) {
  return concatBytes(
    utf8("scimux-rv/auth/v1"),
    new Uint8Array([0]),
    utf8(origin),
    new Uint8Array([0]),
    utf8(String(version)),
    new Uint8Array([0]),
    utf8(route),
    new Uint8Array([0]),
    utf8(handle),
    new Uint8Array([0]),
    challenge,
  );
}

async function importEd25519(seed) {
  const pkcs8 = concatBytes(ED25519_PKCS8_PREFIX, seed);
  return crypto.subtle.importKey("pkcs8", pkcs8, { name: "Ed25519" }, true, ["sign"]);
}

async function importP256Priv(d) {
  const pkcs8 = concatBytes(P256_PKCS8_PREFIX, d);
  return crypto.subtle.importKey("pkcs8", pkcs8, { name: "ECDH", namedCurve: "P-256" }, false, ["deriveBits"]);
}

async function importP256Pub(raw) {
  return crypto.subtle.importKey("raw", raw, { name: "ECDH", namedCurve: "P-256" }, false, []);
}

async function ecdhP256(privRaw, pubRaw) {
  const priv = await importP256Priv(privRaw);
  const pub = await importP256Pub(pubRaw);
  return new Uint8Array(await crypto.subtle.deriveBits({ name: "ECDH", public: pub }, priv, 256));
}

async function hkdfSha256(ikm, salt, info, length) {
  const key = await crypto.subtle.importKey("raw", ikm, "HKDF", false, ["deriveBits"]);
  return new Uint8Array(await crypto.subtle.deriveBits(
    { name: "HKDF", hash: "SHA-256", salt, info },
    key,
    length * 8,
  ));
}

async function checkAuthMessage(v) {
  const got = concatAuth(v.origin, v.version, v.route, v.handle, hexToBytes(v.challenge_hex));
  assert.ok(equalBytes(got, hexToBytes(v.message_hex)), `${v.id}: auth-message mismatch`);
}

async function checkEd25519(v) {
  const seed = hexToBytes(v.seed_hex);
  const msg = hexToBytes(v.message_hex);
  const wantSig = hexToBytes(v.signature_hex);
  const wantPub = hexToBytes(v.public_key_hex);
  const key = await importEd25519(seed);
  const sig = new Uint8Array(await crypto.subtle.sign("Ed25519", key, msg));
  const jwk = await crypto.subtle.exportKey("jwk", key);
  const pub = base64urlToBytes(jwk.x);
  assert.ok(equalBytes(pub, wantPub), `${v.id}: public key mismatch`);
  assert.ok(equalBytes(sig, wantSig), `${v.id}: signature mismatch`);
  const pubKey = await crypto.subtle.importKey("raw", pub, { name: "Ed25519" }, false, ["verify"]);
  const ok = await crypto.subtle.verify("Ed25519", pubKey, sig, msg);
  assert.ok(ok, `${v.id}: verify rejected`);
}

function checkHandle(v) {
  const got = "ih_" + crockford(hexToBytes(v.entropy_hex));
  assert.equal(got, v.handle, `${v.id}: handle`);
}

function checkRID(v) {
  const ent = hexToBytes(v.entropy_hex);
  assert.equal(ent.length, 32, `${v.id}: rid entropy ${ent.length}`);
  assert.equal(bytesToHex(ent), v.rid, `${v.id}: rid`);
}

async function checkRIDCollision(v) {
  const seed = hexToBytes(v.seed_hex);
  const taken = new Set(v.taken || []);
  let got = "";
  let retries = 0;
  for (let i = 0; i < 8; i++) {
    const labeled = concatBytes(
      utf8("TEST-ONLY/not-a-minting-rule/rid"),
      new Uint8Array([0]),
      seed,
      new Uint8Array([0, i]),
    );
    const id = bytesToHex(new Uint8Array(await crypto.subtle.digest("SHA-256", labeled)));
    if (taken.has(id)) {
      retries++;
      continue;
    }
    got = id;
    break;
  }
  assert.equal(got, v.rid, `${v.id}: rid after retry`);
  if (v.retries !== undefined) assert.equal(retries, v.retries, `${v.id}: retries`);
}

function checkPairingCode(v) {
  const ent = hexToBytes(v.entropy_hex);
  assert.equal(ent.length, 5, `${v.id}: pairing-code entropy`);
  assert.equal(crockford(ent), v.code, `${v.id}: pairing-code`);
}

async function checkPairingSAS(v) {
  const shared = await ecdhP256(hexToBytes(v.sender_priv_hex), hexToBytes(v.recipient_pub_hex));
  const tr = hexToBytes(v.transcript_hex);
  const out = await hkdfSha256(shared, utf8("scimux-rv/sas/v1"), tr, 4);
  const n = new DataView(out.buffer, out.byteOffset, 4).getUint32(0);
  const sas = String(n % 1000000).padStart(6, "0");
  if (v.kind === "pairing-sas") assert.equal(sas, v.sas, `${v.id}: sas`);
}

async function checkEnvelopeSeal(v) {
  const ePriv = hexToBytes(v.ephemeral_priv_hex);
  const recipPub = hexToBytes(v.recipient_pub_hex);
  const nonce = hexToBytes(v.nonce_hex);
  const plain = hexToBytes(v.plaintext_hex);
  const ad = hexToBytes(v.ad_hex);
  const shared = await ecdhP256(ePriv, recipPub);
  const extractable = await crypto.subtle.importKey(
    "pkcs8",
    concatBytes(P256_PKCS8_PREFIX, ePriv),
    { name: "ECDH", namedCurve: "P-256" },
    true,
    ["deriveBits"],
  );
  const jwk = await crypto.subtle.exportKey("jwk", extractable);
  const ePub = concatBytes(new Uint8Array([0x04]), base64urlToBytes(jwk.x), base64urlToBytes(jwk.y));
  if (v.ephemeral_pub_hex) {
    assert.equal(bytesToHex(ePub), v.ephemeral_pub_hex, `${v.id}: ephemeral_pub`);
  }
  const info = concatBytes(utf8("seal"), new Uint8Array([0]), ePub, new Uint8Array([0]), recipPub);
  const keyBytes = await hkdfSha256(shared, utf8("scimux-rv/envelope/v1"), info, 32);
  const cryptoKey = await crypto.subtle.importKey("raw", keyBytes, { name: "AES-GCM" }, false, ["encrypt"]);
  const ct = new Uint8Array(await crypto.subtle.encrypt(
    { name: "AES-GCM", iv: nonce, additionalData: ad, tagLength: 128 },
    cryptoKey,
    plain,
  ));
  const sealed = concatBytes(ePub, nonce, ct);
  assert.ok(equalBytes(sealed, hexToBytes(v.sealed_hex)), `${v.id}: sealed mismatch`);
}

// p256Public derives the uncompressed point for a raw scalar. WebCrypto has
// no direct route, so the scalar is imported as PKCS#8 and exported as JWK —
// the same detour checkEnvelopeSeal takes for the ephemeral.
async function p256Public(priv) {
  const k = await crypto.subtle.importKey(
    "pkcs8",
    concatBytes(P256_PKCS8_PREFIX, priv),
    { name: "ECDH", namedCurve: "P-256" },
    true,
    ["deriveBits"],
  );
  const jwk = await crypto.subtle.exportKey("jwk", k);
  return concatBytes(new Uint8Array([0x04]), base64urlToBytes(jwk.x), base64urlToBytes(jwk.y));
}

async function checkEnvelopeSealV2(v) {
  const ePriv = hexToBytes(v.ephemeral_priv_hex);
  const senderPriv = hexToBytes(v.sender_priv_hex);
  const recipPub = hexToBytes(v.recipient_pub_hex);
  const nonce = hexToBytes(v.nonce_hex);
  const plain = hexToBytes(v.plaintext_hex);
  const ad = hexToBytes(v.ad_hex);

  const ePub = await p256Public(ePriv);
  const senderPub = await p256Public(senderPriv);
  if (v.ephemeral_pub_hex) {
    assert.equal(bytesToHex(ePub), v.ephemeral_pub_hex, `${v.id}: ephemeral_pub`);
  }
  // The sender's point is asserted rather than taken from the vector: S goes
  // into the info, so a vector whose sender_pub_hex did not belong to
  // sender_priv_hex would describe a construction nobody can reproduce.
  assert.equal(bytesToHex(senderPub), v.sender_pub_hex, `${v.id}: sender_pub`);

  const sharedE = await ecdhP256(ePriv, recipPub);
  const sharedS = await ecdhP256(senderPriv, recipPub);
  const info = concatBytes(
    utf8("seal-v2"), new Uint8Array([0]),
    ePub, new Uint8Array([0]),
    recipPub, new Uint8Array([0]),
    senderPub,
  );
  const keyBytes = await hkdfSha256(concatBytes(sharedE, sharedS), utf8("scimux-rv/envelope/v2"), info, 32);
  const cryptoKey = await crypto.subtle.importKey("raw", keyBytes, { name: "AES-GCM" }, false, ["encrypt"]);
  const ct = new Uint8Array(await crypto.subtle.encrypt(
    { name: "AES-GCM", iv: nonce, additionalData: ad, tagLength: 128 },
    cryptoKey,
    plain,
  ));
  const sealed = concatBytes(ePub, nonce, ct);
  assert.ok(equalBytes(sealed, hexToBytes(v.sealed_hex)), `${v.id}: sealed mismatch`);
}

function checkEnvelopeInner(v) {
  const body = hexToBytes(v.plaintext_hex);
  const obj = JSON.parse(new TextDecoder("utf-8").decode(body));
  for (const k of ["v", "type", "sdp", "fingerprint"]) {
    assert.ok(k in obj, `${v.id}: missing ${k}`);
  }
}

function xorMapped(tid, ip, port) {
  const cookie = hexToBytes("2112a442");
  const xport = port ^ 0x2112;
  let family;
  let xaddr;
  if (ip.includes(":")) {
    family = 2;
    const v6 = ipToV6(ip);
    const mask = concatBytes(cookie, tid);
    xaddr = new Uint8Array(16);
    for (let i = 0; i < 16; i++) xaddr[i] = v6[i] ^ mask[i];
  } else {
    family = 1;
    const v4 = Uint8Array.from(ip.split(".").map((n) => Number(n)));
    xaddr = new Uint8Array(4);
    for (let i = 0; i < 4; i++) xaddr[i] = v4[i] ^ cookie[i];
  }
  const attrLen = 4 + xaddr.length;
  const msgLen = 4 + attrLen;
  const out = new Uint8Array(20 + msgLen);
  const view = new DataView(out.buffer);
  view.setUint16(0, 0x0101);
  view.setUint16(2, msgLen);
  view.setUint32(4, 0x2112a442);
  out.set(tid, 8);
  view.setUint16(20, 0x0020);
  view.setUint16(22, attrLen);
  out[24] = 0;
  out[25] = family;
  view.setUint16(26, xport);
  out.set(xaddr, 28);
  return out;
}

function ipToV6(ip) {
  const buf = new Uint8Array(16);
  const expanded = ip.includes("::") ? expandV6(ip) : ip.split(":");
  expanded.forEach((g, i) => {
    const n = parseInt(g || "0", 16);
    buf[i * 2] = (n >> 8) & 0xff;
    buf[i * 2 + 1] = n & 0xff;
  });
  return buf;
}

function expandV6(ip) {
  const [left, right] = ip.split("::");
  const L = left ? left.split(":") : [];
  const R = right ? right.split(":") : [];
  const mid = 8 - L.length - R.length;
  return [...L, ...Array(mid).fill("0"), ...R];
}

function checkSTUN(v) {
  if (v.message === "stun-binding-request") {
    const raw = hexToBytes(v.stun_hex);
    assert.equal(raw.length, 20, `${v.id}: request length`);
    const view = new DataView(raw.buffer, raw.byteOffset, raw.byteLength);
    assert.equal(view.getUint16(0), 0x0001, `${v.id}: type`);
    assert.equal(view.getUint16(2), 0, `${v.id}: length`);
    assert.equal(view.getUint32(4), 0x2112a442, `${v.id}: cookie`);
    assert.equal(bytesToHex(raw.subarray(8, 20)), v.transaction_id_hex, `${v.id}: tid`);
    return;
  }
  if (v.message === "stun-binding-success") {
    const tid = hexToBytes(v.transaction_id_hex);
    const got = xorMapped(tid, v.mapped_ip, v.mapped_port);
    assert.ok(equalBytes(got, hexToBytes(v.response_hex)), `${v.id}: XOR-MAPPED-ADDRESS`);
    return;
  }
  assert.fail(`${v.id}: unhandled stun message ${v.message}`);
}

function checkSTUNDrop(v) {
  assert.ok(v.stun_hex, `${v.id}: stun-drop needs stun_hex`);
  hexToBytes(v.stun_hex);
  assert.equal(v.response_hex, undefined, `${v.id}: stun-drop must not carry a response`);
}

async function executeVector(v) {
  switch (v.kind) {
    case "auth-message":
      await checkAuthMessage(v);
      return "construction";
    case "ed25519":
      await checkEd25519(v);
      return "construction";
    case "handle":
      checkHandle(v);
      return "construction";
    case "rid":
      checkRID(v);
      return "construction";
    case "rid-collision-retry":
      await checkRIDCollision(v);
      return "construction";
    case "pairing-code":
      checkPairingCode(v);
      return "construction";
    case "pairing-sas":
    case "pairing-transcript":
      await checkPairingSAS(v);
      return "construction";
    case "envelope-seal":
      await checkEnvelopeSeal(v);
      return "construction";
    case "envelope-seal-v2":
      await checkEnvelopeSealV2(v);
      return "construction";
    case "envelope-inner":
      checkEnvelopeInner(v);
      return "construction";
    case "stun":
      checkSTUN(v);
      return "construction";
    case "stun-drop":
      checkSTUNDrop(v);
      return "construction";
    case "http":
    case "rejection":
      validateHTTP(v);
      return "http";
    default:
      assert.fail(`${v.id}: unknown kind ${v.kind}`);
  }
}

test("AT-NFR-14-a: vendored specification, digest, and vector directory exist", () => {
  readVendored(specRel);
  readVendored(digestRel);
  listVectorFiles();
});

test("AT-NFR-14-a: recomputed WebCrypto SHA-256 digest matches vectors.sha256", async () => {
  const want = readVendored(digestRel);
  const names = listVectorFiles();
  const parsed = parseDigest(want);
  const digestNames = new Set(parsed.byName.keys());
  for (const name of names) {
    assert.ok(digestNames.has(name), `AT-NFR-14-a: vector file ${name} is unlisted in vectors.sha256`);
  }
  for (const name of digestNames) {
    assert.ok(names.includes(name), `AT-NFR-14-a: digest lists ${name}, which is not a vector JSON file`);
  }
  const lines = [];
  for (const name of names) {
    const raw = readFileSync(vendoredPath(vectorRel, name));
    const sum = await sha256Hex(raw);
    lines.push(`${sum}  ${name}`);
  }
  const got = lines.join("\n") + "\n";
  const wantText = new TextDecoder("utf-8").decode(want);
  assert.equal(got, wantText, `AT-NFR-14-a: vectors.sha256 mismatch\n got:\n${got}\nwant:\n${wantText}`);
});

test("AT-NFR-14-a: WebCrypto executes every vector; unknown kinds fail", async () => {
  readVendored(digestRel);
  const names = listVectorFiles();
  const want = readVendored(digestRel);
  const lines = [];
  for (const name of names) {
    const raw = readFileSync(vendoredPath(vectorRel, name));
    lines.push(`${await sha256Hex(raw)}  ${name}`);
  }
  assert.equal(lines.join("\n") + "\n", new TextDecoder("utf-8").decode(want), "AT-NFR-14-a: digest mismatch before vector execution");

  const vectors = [];
  const seen = new Map();
  for (const name of names) {
    let arr;
    try {
      arr = JSON.parse(readFileSync(vendoredPath(vectorRel, name), "utf8"));
    } catch (err) {
      assert.fail(`AT-NFR-14-a: malformed vector file ${name}: ${err.message}`);
    }
    assert.ok(Array.isArray(arr) && arr.length > 0, `AT-NFR-14-a: vector file ${name} contains no objects`);
    for (const v of arr) {
      assert.ok(v.id, `AT-NFR-14-a: ${name} vector missing id`);
      assert.ok(v.message, `AT-NFR-14-a: vector ${v.id} missing message`);
      assert.ok(v.kind, `AT-NFR-14-a: vector ${v.id} missing kind`);
      assert.equal(seen.get(v.id), undefined, `AT-NFR-14-a: duplicate vector id ${v.id}`);
      seen.set(v.id, name);
      vectors.push(v);
    }
  }
  assert.ok(vectors.length > 0, "AT-NFR-14-a: vector set is empty; nothing to execute");

  let constructions = 0;
  let httpRows = 0;
  for (const v of vectors) {
    const kind = await executeVector(v);
    if (kind === "construction") constructions++;
    else httpRows++;
  }
  assert.equal(constructions + httpRows, vectors.length, "AT-NFR-14-a: some vectors were not accounted for");
  assert.ok(constructions > 0, "AT-NFR-14-a: no construction vectors executed");
});
