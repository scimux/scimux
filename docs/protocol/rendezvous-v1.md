# scimux-rv rendezvous protocol

**Protocol-Version:** 1
**Spec-Status:** 1
**Minimum accepted request `v`:** 1
**Vector digest:** [`vectors.sha256`](vectors.sha256)

This is the versioned wire specification required by NFR-14. A
browser and a computer that have not read the scimux-rv repository
can be implemented from this document and the JSON files in
`internal/rv/testdata/vectors/`. Those files are the source copy;
the scimux side MUST check `vectors.sha256` against its own copy
before treating the vectors as current.

The invite-code format (NFR-11) is **referenced, not defined**.
Issue and verify both happen inside rv; scimux passes the string
through opaquely. P1 owns that format (`internal/invite`).

rv holds no durable secret that grants or mints access (FR-31).
It never relays peer traffic.

---

## Contents

1. [Scope and roles](#1-scope-and-roles)
2. [Message catalog](#2-message-catalog)
3. [Version negotiation](#3-version-negotiation)
4. [Admission](#4-admission)
5. [Signed message](#5-signed-message)
6. [Installation handle](#6-installation-handle)
7. [Constant rejection](#7-constant-rejection)
8. [Rendezvous identifiers](#8-rendezvous-identifiers)
9. [Wait and envelopes](#9-wait-and-envelopes)
10. [Pairing (rv half)](#10-pairing-rv-half)
11. [Pairing transcript and SAS](#11-pairing-transcript-and-sas)
12. [Envelope inner format and seal](#12-envelope-inner-format-and-seal)
13. [`/p` bootstrap inventory](#13-p-bootstrap-inventory)
14. [Invite codes](#14-invite-codes)
15. [Test vectors](#15-test-vectors)
16. [Superseded text](#16-superseded-text)
17. [STUN Binding](#17-stun-binding)
18. [Aggregate health](#18-aggregate-health)

---

## 1. Scope and roles

| Role | Who | Authenticates as |
|---|---|---|
| Installation | scimux on the computer | bound Ed25519 key, via handle + challenge + signature |
| Device | paired phone or tablet | not at all — holds a rendezvous ID, or a pairing code |
| Operator | `scimux-rv` CLI | not on the wire; same uid as the daemon, local log only |

Origin is the `-origin` flag (default `https://my.scimux.eu`). It is
never taken from `Host`.

All versioned POSTs use UTF-8 JSON unless a section says otherwise.
Hex fields are lowercase. `Content-Length` is mandatory on every
POST listed here; a missing, chunked, or over-cap length is the
constant rejection. GET on a POST route is the constant rejection
— a 405 would be an existence oracle.

---

## 2. Message catalog

Every row is a wire message. AT-NFR-14-b requires at least one
vector whose `"message"` field equals the `id`. Rejection rows are
first-class.

HTTP vectors whose `route` is registered on the server are
**replayed** through the handler and compared byte-for-byte,
headers included. A row that names a registered route cannot stay
declarative: once P3 registers `POST /v1/wait`, its vectors must
replay.

| id | class | route | direction |
|---|---|---|---|
| `hello-request` | admission | `GET /v1/hello` | installation → rv |
| `hello-response` | admission | `GET /v1/hello` | rv → installation |
| `enroll-request` | admission | `POST /v1/enroll` | installation → rv |
| `enroll-response` | admission | `POST /v1/enroll` | rv → installation |
| `challenge-request` | admission | `POST /v1/challenge` | installation → rv |
| `challenge-response` | admission | `POST /v1/challenge` | rv → installation |
| `verify-request` | admission | `POST /v1/verify` | installation → rv |
| `verify-response` | admission | `POST /v1/verify` | rv → installation |
| `unenroll-request` | admission | `POST /v1/unenroll` | installation → rv |
| `unenroll-response` | admission | `POST /v1/unenroll` | rv → installation |
| `auth-message` | construction | — | signed bytes, not an HTTP body |
| `handle` | construction | — | `ih_` + 16 Crockford characters |
| `wait-request` | rendezvous | `POST /v1/wait` | installation → rv |
| `wait-response-envelope` | rendezvous | `POST /v1/wait` | rv → installation |
| `wait-response-empty` | rendezvous | `POST /v1/wait` | rv → installation |
| `envelope-request` | rendezvous | `POST /v1/envelope/{id}` | device → rv |
| `envelope-reply` | rendezvous | `POST /v1/wait` | installation → rv (`reply` on the next wait) |
| `envelope-response-reply` | rendezvous | `POST /v1/envelope/{id}` | rv → device |
| `envelope-rate-limit` | rendezvous | `POST /v1/envelope/{id}` | rv → device |
| `rid` | construction | — | 256-bit ID, 64-char lowercase hex |
| `pair-wait-request` | pairing | `POST /v1/pair/wait` | installation → rv |
| `pair-wait-response` | pairing | `POST /v1/pair/wait` | rv → installation |
| `pair-offer-request` | pairing | `POST /v1/pair/offer` | device → rv |
| `pair-offer-response` | pairing | `POST /v1/pair/offer` | rv → device |
| `pair-cancel-request` | pairing | `POST /v1/pair/cancel` | installation → rv |
| `pair-cancel-response` | pairing | `POST /v1/pair/cancel` | rv → installation |
| `pair-rate-limit` | pairing | `POST /v1/pair/offer` | rv → device |
| `pairing-code` | construction | — | 8 Crockford characters |
| `pairing-transcript` | construction | — | SAS input |
| `pairing-sas` | construction | — | 6-digit numeric comparison |
| `envelope-inner` | construction | — | plaintext inside the seal |
| `envelope-seal` | construction | — | ECDH P-256 + HKDF-SHA-256 + AES-256-GCM |
| `p-page` | bootstrap | `GET /p` | device → rv |
| `p-boot-js` | bootstrap | `GET /p/boot.js` | device → rv |
| `p-boot-css` | bootstrap | `GET /p/boot.css` | device → rv |
| `p-codec-js` | bootstrap | `GET /p/codec.js` | device → rv |
| `p-connection-js` | bootstrap | `GET /p/connection.js` | device → rv |
| `p-start-js` | bootstrap | `GET /p/start.js` | device → rv |
| `p-page-js` | bootstrap | `GET /p/page.js` | device → rv |
| `p-pairing-js` | bootstrap | `GET /p/pairing.js` | device → rv |
| `p-rendezvous-js` | bootstrap | `GET /p/rendezvous.js` | device → rv |
| `p-session-js` | bootstrap | `GET /p/session.js` | device → rv |
| `p-store-js` | bootstrap | `GET /p/store.js` | device → rv |
| `p-manifest` | bootstrap | `GET /p/manifest.webmanifest` | device → rv |
| `p-icon-180` | bootstrap | `GET /p/icon-180.png` | device → rv |
| `p-icon-512` | bootstrap | `GET /p/icon-512.png` | device → rv |
| `p-rejection` | bootstrap | `GET /p/not-in-inventory` | rv → device |
| `stun-binding-request` | stun | — | client → rv (UDP Binding) |
| `stun-binding-success` | stun | — | rv → client (XOR-MAPPED-ADDRESS) |
| `stun-drop` | stun | — | malformed or oversized; no datagram |
| `health-response` | health | `GET /health` | operator → rv (loopback only) |
| `constant-rejection` | rejection | `GET /v1/enroll` | rv → caller |
| `rejection-unknown` | rejection | `POST /v1/enroll` | unknown code, handle, or ID |
| `rejection-malformed` | rejection | `POST /v1/enroll` | unparseable or wrong-typed fields |
| `rejection-unauthorised` | rejection | `POST /v1/verify` | missing, wrong, or cross-route signature |
| `rejection-replay` | rejection | `POST /v1/verify` | challenge presented a second time |
| `rejection-stale` | rejection | `POST /v1/verify` | challenge past its 30 s TTL |
| `rejection-revoked` | rejection | `POST /v1/challenge` | handle bound but revoked |
| `rejection-idle` | rejection | `POST /v1/envelope/{id}` | ID with no live waiter |
| `rejection-timeout` | rejection | `POST /v1/envelope/{id}` | waiter did not reply inside ReplyHold |
| `rejection-method` | rejection | `GET /v1/enroll` | GET on a POST route |
| `rejection-content-length` | rejection | `POST /v1/enroll` | missing, chunked, or over-cap `Content-Length` |
| `rejection-version` | rejection | `POST /v1/enroll` | `v` missing or below the minimum |
| `rejection-collision` | rejection | `POST /v1/wait` | ID already live under another installation |
| `rejection-unclean-path` | rejection | `POST /v1/envelope/{id}` | path is not already cleaned; a 301 would leak the ID |

Every `rejection-*` row except the two `*-rate-limit` messages is
byte-identical to `constant-rejection` (status, headers, body).
`envelope-rate-limit` and `pair-rate-limit` are the only non-404
failures on the public surface.

---

## 3. Version negotiation

FR-37.

- Request field `v` is a JSON number.
- `v` missing, non-numeric, or `< 1` is `rejection-version`.
- `v >= 1` is accepted even if greater than `Protocol-Version`.
  Unknown fields are ignored.
- The response `v`, when present, is always the server constant
  `1`, never the request's `v`.
- The version segment of the signed message (§5) is that same
  server constant. Signing the request's `v` when it differs
  fails.

Downgrade is refused, not performed. A `v: 0` enroll does not
bind.

---

## 4. Admission

As built in P2. Not redesigned.

Admission body cap: **2048 bytes**. Read deadline: **2 s**.

### 4.0 `GET /v1/hello`

The pre-flight probe. Unauthenticated, stateless, and answerable
before the caller holds an invite, a handle, or a key — which is
the whole reason it exists.

An installation's invite is single-use, so a client that cannot
distinguish "this rendezvous is unreachable" from "this rendezvous
rejected me" must spend the credential to find out. `/v1/hello`
lets it prove reachability and version compatibility first, and
leave the invite untouched when either fails.

Request: no body, no credential.

Response: `200`, `application/json`.

| field | type | meaning |
|---|---|---|
| `v` | number | the server constant of §3 |
| `min` | number | the lowest `v` this server accepts |

Both bounds are present. A client reading only `v` can tell the
server is newer than it expected but not whether the server would
still accept it; `min` is what lets the client's error name the
side that is behind.

The response carries nothing else. It is reachable by anyone who
can resolve the host, so build strings, counts and operator detail
must not accumulate here. Any method other than `GET` (and the
`HEAD` the method-pattern router answers from it) is the constant
rejection of §7.

### 4.1 `POST /v1/enroll`

One-shot redemption. Atomically verifies the invite, binds the
presented Ed25519 public key, marks the invite redeemed, and
returns a handle.

Request (JSON object):

| field | type | encoding |
|---|---|---|
| `v` | number | §3 |
| `code` | string | invite plaintext; rv parses it (P1) |
| `pubkey` | string | 32-byte Ed25519 public key, 64 hex chars |

Response `200`:

```
Content-Type: application/json
X-Content-Type-Options: nosniff

{"handle":"ih_041061050R3GG28A","v":1}
```

(`json.Encoder` emits a trailing newline; keys are sorted.) Vector
`enroll-response-ok`.

A redeemed code presented again is `rejection-unknown`. Two
concurrent enrolls on one code: exactly one binds; the loser is
`rejection-unknown`.

### 4.2 `POST /v1/challenge`

Allocated only after the handle resolves to a bound, unrevoked
installation.

Request: `{ "v": 1, "handle": "ih_…" }`.

Response `200`:

```
Content-Type: application/json
X-Content-Type-Options: nosniff

{"challenge":"<64 hex>","v":1}
```

The challenge is 32 bytes, hex-encoded. In-memory only. TTL
**30 s**. Single-use **even when the signature that later presents
it fails**. Pinned to the handle that received it. Bounded: 256
global, 4 per handle. Reclamation is a lazy sweep on insert.

### 4.3 `POST /v1/verify`

Proves the caller holds the bound private key. Shared `admit()`
used later by `/v1/wait` and `/v1/pair/*`.

Request:

| field | type | encoding |
|---|---|---|
| `v` | number | §3 |
| `handle` | string | §6 |
| `challenge` | string | 32-byte nonce, 64 hex chars |
| `sig` | string | 64-byte Ed25519 signature, 128 hex chars |

Response: `204` with an empty body and no extra headers.

The handle alone is not a credential. Presenting it without a
valid signature is `rejection-unauthorised`.

### 4.4 `POST /v1/unenroll`

Releases an installation at its own request, so a computer that
unlinks locally does not leave a bound row only an operator can
clear.

Request: identical in shape to §4.3, and authenticated the same
way. The route segment of the signed message (§5) is
`/v1/unenroll`, so a signature made to reconnect can never be
replayed to delete the installation instead.

| field | type | encoding |
|---|---|---|
| `v` | number | §3 |
| `handle` | string | §6 |
| `challenge` | string | 32-byte nonce, 64 hex chars |
| `sig` | string | 64-byte Ed25519 signature, 128 hex chars |

Response: `204` with an empty body and no extra headers.

On success the server records the same revocation an operator
`revoke` writes. Afterwards the handle no longer resolves to a
usable installation: it cannot obtain a challenge, and a second
release is `rejection-unauthorised`. A released installation and
one that never existed are indistinguishable to any caller.

**It releases the installation, never the code.** The redeemed
invite stays redeemed. Unlinking is not a refund, the code log is
append-only, and there is no operation anywhere in this protocol
that returns a spent invite to the pool.

Clients treat this as best-effort. An unreachable rendezvous must
not prevent a local unlink — if it did, the failure mode this
route exists to avoid would simply move.

---

## 5. Signed message

Ed25519 over the **concatenation**, not a digest. Ed25519 hashes
internally; do not pre-hash.

```
"scimux-rv/auth/v1" || 0x00
|| origin || 0x00
|| itoa(ProtocolVersion) || 0x00
|| route || 0x00
|| handle || 0x00
|| raw challenge
```

- `origin` is the `-origin` flag, UTF-8, no trailing slash
  normalisation beyond what the operator configured.
- `ProtocolVersion` is the server constant `1`, decimal ASCII,
  no leading zeros.
- `route` is the path being authorised: `/v1/verify`,
  `/v1/unenroll`, `/v1/wait`, `/v1/pair/wait`, `/v1/pair/cancel`.
  A signature for one route does not authenticate another.
- `raw challenge` is the 32 nonce bytes, not their hex.

Vector `auth-message-verify` / `ed25519-verify-sig`.

---

## 6. Installation handle

`ih_` + 16 Crockford Base32 characters (alphabet
`0123456789ABCDEFGHJKMNPQRSTVWXYZ`). 10 CSPRNG bytes, 80 bits.
Server-issued at enroll. Appears in `list` / `revoke`. Disclosure
grants nothing (FR-06b). Vector `handle-ih`.

---

## 7. Constant rejection

One response for every public failure that is not a rate limit:

```
404
Content-Type: text/plain; charset=utf-8
X-Content-Type-Options: nosniff

not found
```

(the body is the ten bytes `6e 6f 74 20 66 6f 75 6e 64 0a`). No
path echo, no method echo, no `Allow`, no server banner. A 405 on
`GET /v1/enroll` would tell a scanner the route exists; therefore
GET is 404.

Unknown, malformed, unauthorised, replayed, stale, revoked, idle,
timeout, wrong method, missing `Content-Length`, below-minimum
`v`, and an unclean path (`.` / `..` / `//` that ServeMux would
301) are this message. Vectors `constant-rejection-get-enroll` and
the `rejection-*` rows that name a built route are replayed
against the handler and compared to this dump, headers included.

---

## 8. Rendezvous identifiers

**Decision 2 — who mints.** Locked. The enrolled installation
mints the ID. rv never allocates one.

This is what FR-04's "scimux MUST **register** one" already says.
`remote-scimux.md` called IDs "a random key held by both" and
"free to mint". The first stands; the second described an
unauthenticated property that V1 admission removes. AT-NFR-17-a's
server-side reading (a `newRendezvousID` / `generateRID` in rv)
is superseded: that grep becomes a **negative** assertion — no
minting function may exist in non-test rv source. Collision retry
is a vector (`rid-collision-retry-test-only`), not a server test.

**Canonical encoding.** 32 bytes (256 bits; NFR-17 floor 128,
prefer 256) from `crypto/rand`. On the wire: 64 lowercase hex
characters, no `0x`, no separators. Uppercase, mixed case, or any
other encoding is `rejection-malformed`. Vector `rid-256`.

**CSPRNG is a MUST.** A client that derives IDs from a password,
a counter, or a KDF has implemented this spec incorrectly. The
vector kind `rid-collision-retry` uses a seeded SHA-256 labeled
`TEST-ONLY/not-a-minting-rule/rid` solely so the retry count is
reproducible. It is not a protocol construction. A reader who
copies it mints predictable IDs, and the ID *is* the capability
to post an envelope.

**Collision.** If an ID is already live under another
installation, rv returns `rejection-collision` (constant 404, no
echo). The minter retries. At 256 bits this branch will not fire
in practice; it is defensive. Do not delete the check, and do not
read the catalog row as evidence that collisions are expected.

**Disclosure.** IDs never appear in error bodies, `Location`,
`Referer`, query strings, or logs. They appear as the path
element of `POST /v1/envelope/{id}` (the allowed routing
position) and in JSON fields of authenticated wait/pair requests.
A request whose path is not already cleaned (`.` / `..` / `//`)
is `rejection-unclean-path` **before** ServeMux sees it. Without
that wrap the mux 301s to the cleaned path and the ID lands in
`Location` and an HTML body — the surfaces this paragraph
forbids. Vector `rejection-unclean-path-dotdot`. The wrap is
global; `/v1//wait` and `/v1/x/../enroll` 301 the same way.

**P4 consequence.** The pairing QR and fragment-link carry a
client-minted ID. The typed short code only *indexes* a temporary
waiter the computer already registered. rv does not mint pairing
IDs either.

---

## 9. Wait and envelopes

**Decision 1 — FR-07's reply.** Locked as documentation of
`remote-by-invite-only.md` §4.2 ("the envelope endpoint returns a
reply meaningful only to a holder of the installation's key") and
FR-07 / AT-FR-07-a, not as a new design. The sealed reply is the
response body of `POST /v1/envelope/{id}`. The waiter hands it
back as the `reply` field of its next `POST /v1/wait`.

`/v1/envelope/{id}` stays unauthenticated: the paired phone holds
a rendezvous ID and no installation key. There is no second
long-held connection.

`/v1/wait` is the only **deadline-exempt** route. The envelope
POST is a **bounded hold**, not an exemption: its write deadline
is **8 s**, which is above `PendingTTL + ReplyHold` = 6 s. A
stalled writer is torn down (AT-FR-34-c). `POST /v1/pair/offer`
uses the same 8 s write deadline.

### 9.1 Two clocks

The design document's "~2 s" is **one** of these. They are
sequential. Name them separately.

| name | symbol | value | what it bounds |
|---|---|---|---|
| Pending-envelope TTL | `PendingTTL` | **2 s** | how long an undelivered envelope may sit before a waiter takes it. If no waiter attaches, drop it. This is the design-doc window that keeps rv a rendezvous and not a queue. |
| Reply hold | `ReplyHold` | **4 s** | after delivery to a live waiter, how long the envelope POST waits for the waiter's next wait-with-`reply`. This is the SDP-answer budget. |

The poster's total bound is `PendingTTL + ReplyHold` = **6 s**
in the worst case (envelope arrives just as the waiter is down,
waiter reconnects at T+2 s, then 4 s to reply). Typical case
with a live waiter: `ReplyHold` only, ≤ 4 s.

`/v1/wait` is not covered by either clock. It is the long poll.

### 9.2 `POST /v1/wait`

Authenticated via `admit()` with route `/v1/wait`. JSON body:

| field | type | required |
|---|---|---|
| `v` | number | yes |
| `handle` | string | yes |
| `challenge` | string | yes, 64 hex |
| `sig` | string | yes, 128 hex |
| `id` | string | yes, 64 hex lowercase (client-minted) |
| `reply` | string | no; hex of the sealed reply to the previous envelope on this ID |
| `max_ms` | number | no; default 60000, max 120000 |

The body is capped at 8 KiB (auth JSON plus one 4 KiB hex reply).
`Content-Length` mandatory.

On success the server registers the waiter under `id` (or
delivers `reply` to the held poster, then re-registers). One
waiter per ID. A second wait on a live ID from the **same**
installation replaces the first (reconnect). A second wait from a
**different** installation is `rejection-collision`.

Response when an envelope is delivered (`wait-response-envelope`):

```
200
Content-Type: application/octet-stream
X-Content-Type-Options: nosniff
X-Rv-Challenge: <64 lowercase hex>

<raw sealed bytes, unmodified>
```

Response when `max_ms` elapses with no envelope
(`wait-response-empty`):

```
204
X-Content-Type-Options: nosniff
X-Rv-Challenge: <64 lowercase hex>
```

`X-Rv-Challenge` is the next challenge, minted **when the wait
response is written**, so its 30 s TTL starts when it becomes
useful. Same store, same single-use / handle-pin / caps as
`POST /v1/challenge`. The signed route for the next wait is still
`/v1/wait`. The `reply` field is not part of the signed message;
the computer may pre-sign the next wait as soon as this header
arrives, then fill `reply` after it has sealed the SDP answer.

A challenge obtained from `POST /v1/challenge` *before* a long
poll is dead on arrival: `ChallengeTTL` is 30 s and a wait is
longer. Do not refresh during the wait (that burns
`MaxChallengesPerHandle` and defeats the poll). Use the header.

The 200 body is raw bytes. The server does not wrap, decode, or
re-encode them (AT-FR-07-b).

### 9.3 `POST /v1/envelope/{id}`

Unauthenticated. `{id}` is the 64-char lowercase hex RID.

1. Resolve `{id}` **before** reading the body (FR-18).
2. Unknown, idle (no live waiter and no pending slot), malformed
   ID, missing / chunked / over-cap `Content-Length`: constant
   rejection, **no body read**. Cap is **4096** bytes.
   An ID that has never had a waiter is unknown. An envelope
   POST MUST NOT allocate a map entry or a pending slot — that
   is what keeps FR-18 and AT-FR-19-b. The pending slot is
   created when a waiter **registers**, and survives a
   disconnect for `PendingTTL`. After a waiter detaches, an
   envelope POST may still land for that reconnect grace
   (`PendingTTL`, 2 s); that POST then holds for
   `PendingTTL + ReplyHold` (6 s). From detach, a poster can
   therefore remain open for about 8 s.
3. Live waiter, or a pending slot still inside `PendingTTL`
   (the slot already exists; this POST does not create it):
   read the body, deliver to the waiter, hold this POST for at
   most `ReplyHold`.
4. When the waiter submits `reply` on the next wait, this POST
   returns `200` with `Content-Type: application/octet-stream`
   and the reply bytes unmodified (`envelope-response-reply`).
5. If `ReplyHold` elapses, the waiter disconnects, or the
   installation is revoked: this POST completes with
   `constant-rejection`. The pending envelope and any unsent
   reply are erased (FR-29).

Correlation is the live ID: one waiter, one pending poster, one
reply slot. A second envelope while a poster is held is
`envelope-rate-limit` or, if the bucket still has tokens, queued
behind `PendingTTL` only if no poster is held — v1 allows **one**
pending envelope per ID.

Token bucket (lives on the waiter's existing map entry, never
allocated by an unknown POST): 4 tokens, refill 1 per 120 s.
Exhaustion:

```
429
Retry-After: 120
Content-Type: text/plain; charset=utf-8
X-Content-Type-Options: nosniff
```

empty body. This is distinguishable from 404 by status, and only
reachable with a live ID, so it is not an enumeration oracle.

Caps (FR-26), enforced before registration:

| cap | value |
|---|---|
| rendezvous IDs per installation | 16 |
| concurrent waits per installation | 8 |
| total waits | 256 |
| temporary pairing waits per installation | 32 (map residency: live, abandoned, and consumed-until-TTL) |
| pairing residents (process) | 1024 (memory backstop) |

Saturation is the constant rejection, early and stable.

### 9.4 FR-20 (content only) and the held POST

FR-20 is **content**. Unknown and known-but-idle IDs return the
same status, headers and body (`constant-rejection`). AT-FR-20-a
and AT-FR-20-b assert that byte-identity; they do not assert
timing.

On `POST /v1/envelope/{id}` the reply hold makes completion time
reveal whether a live waiter is present. That is accepted
deliberately: the signal reaches only a caller already holding
the 256-bit rendezvous ID, so it is not sweepable. **Padding
unknown IDs to `ReplyHold` is rejected.** It would give an
unauthenticated flood a multi-second connection hold each,
against AT-FR-19-b.

AT-FR-20-c (P3) locks the other side of that choice: an unknown
ID is rejected before the body is read, allocates nothing, and
is never held for the reply window.

This amends the design document, not this spec alone. See
`remote-by-invite-only.md` FR-20 and the "No oracle (FR-20)"
paragraph, and §16 below.

---

## 10. Pairing (rv half)

FR-11, FR-39. Temporary waiters. Prior enrollment is required on
the computer side. The phone is unauthenticated.

### 10.1 Short code

8 Crockford characters (40 bits, 5 CSPRNG bytes), displayed as
`XXXX-XXXX`. Normalisation is the P1 invite parser's alphabet
rules (case fold, `O→0`, `I/L→1`, hyphen/space stripped) applied
to length 8. The code **indexes** a temporary waiter; it is not
a secret. Vector `pairing-code-8`.

QR and fragment-link also carry the client-minted RID and the
computer's static P-256 public key (§12). The typed path carries
only the code.

### 10.2 `POST /v1/pair/wait`

Authenticated, route `/v1/pair/wait`. JSON:

| field | type |
|---|---|
| `v`, `handle`, `challenge`, `sig` | as §4.3 / §5 |
| `code` | 8-char normalised (or grouped) short code |
| `id` | 64-char temp RID, client-minted |
| `reply` | no; hex of the sealed reply to the previous offer on this code. Same role as §9.2. Not in the signed message. |
| `max_ms` | no; default 60000, max 120000. Bounds this poll only. The waiter TTL is independently 120 s from first registration. Signatures do not cover `max_ms`. |

Response when an offer is delivered (`pair-wait-response`):

```
200
Content-Type: application/octet-stream
X-Content-Type-Options: nosniff
X-Rv-Challenge: <64 lowercase hex>

<raw offer envelope bytes, hex-decoded, unmodified>
```

Response when `max_ms` or the waiter TTL elapses with no offer
(`pair-wait-request`):

```
204
X-Content-Type-Options: nosniff
X-Rv-Challenge: <64 lowercase hex>
```

TTL of the temporary waiter: **120 s** from first registration,
measured with the injectable admission clock (not a wall timer).
The window is sized for a person, not for the protocol: unlock a
phone, open the link, read six digits off one screen and type
them on another. Widening it is paid for in guesses, and the price
is small — the code space is 10^8 and §10.3 caps offers at 30/s
process-wide, so a blind attacker gets 3600 tries per window
against 10^8 codes.
Same installation may re-register the same code until that expiry
(reconnect / cancel-and-retry). A different installation
presenting the same code is `rejection-collision` (constant 404).

### 10.3 `POST /v1/pair/offer`

Unauthenticated. JSON `{ "v": 1, "code": "…", "envelope": "<hex>" }`.
Body cap 8 KiB. Global rate limit **before** the body is treated
as a claim on a waiter: 30 offers per second across the whole
process, not per source. Exhaustion is `pair-rate-limit`:

```
429
Retry-After: 2
Content-Type: text/plain; charset=utf-8
X-Content-Type-Options: nosniff
```

Unknown, expired, or idle code: `constant-rejection`. A live
waiter receives the envelope bytes (decoded hex) as the
`pair/wait` 200 body, raw. The offer POST is held for
`ReplyHold` and returns the waiter's `reply` the same way as
§9.3.

An offer does **not** consume the pairing waiter. Consumption
happens at the first `reply` the computer submits, or at the 120 s
TTL — **not** at `pair/cancel` (§10.4, §10.6). A `reply` that
arrives after the offer POST has already completed (`ReplyHold`
elapsed, disconnect, or revoke) is dropped and does **not**
consume the code. An attacker who posts first is delivered to
the computer; SAS will not match; the computer does not reply and
re-waits with the same code; the legitimate phone can still
offer. That is the rv half of AT-FR-39-a.

Simultaneous offers: one pending slot. The second during
`ReplyHold` is `pair-rate-limit` or constant 404 (same as a full
slot). Resolution of attacker vs legitimate is SAS on the
clients (AT-FR-39-b, not rv).

### 10.4 `POST /v1/pair/cancel`

Authenticated, route `/v1/pair/cancel`. JSON:
`{ "v", "handle", "challenge", "sig", "code" }`.

Tears down the temporary waiter without treating the code as
consumed: the same installation may `pair/wait` again with that
code until the original 120 s expiry (FR-38). Response `204`.

### 10.5 Restart

rv holds pairing waiters in memory only. A restart is total
amnesia. In-flight pairings fail; the user generates a new code.

### 10.6 State machine (rv)

```
absent --pair/wait--> waiting(ttl=120s)
waiting --pair/offer--> holding(replyHold=4s)   [waiter still waiting]
holding --reply--> consumed (torn down)
holding --timeout / disconnect--> waiting (if ttl remains) else absent
waiting --pair/cancel--> absent (code reusable until original ttl)
waiting --ttl--> absent
any --unknown / unauth / rate--> constant 404 or 429; no transition
```

Single-use of the *code* is consumed at `reply` or `ttl`, not at
`offer` and not at `cancel`.

---

## 11. Pairing transcript and SAS

Client-side. rv never sees these bytes.

Computer holds a static ECDH P-256 key `X` (generated at first
`--remote`, stored with the installation identity, never sent to
rv) and the Ed25519 installation public key `I` already bound at
enroll. Device generates a static ECDH P-256 key `Y` at pair
time.

Transcript, 0x00-separated, no other framing. This is unambiguous
only because every variable-length field is UTF-8 that MUST NOT
contain 0x00 (`origin`, `code_normalised`, `rid_hex` — hex is
ASCII) and every binary field is fixed length (`X` and `Y` are
65 bytes uncompressed, `I` is 32 bytes, each nonce is 32 bytes).
A future variable-length binary field would make the transcript
ambiguous; do not add one without a length prefix.

```
"scimux-rv/pair/v1" || 0x00
|| origin || 0x00
|| code_normalised || 0x00
|| rid_hex || 0x00
|| X_uncompressed || 0x00      // 65 bytes, 0x04 || x || y
|| Y_uncompressed || 0x00
|| I || 0x00                   // 32-byte Ed25519 public key
|| offer_nonce || 0x00         // 32 bytes, device-chosen
|| reply_nonce                 // 32 bytes, computer-chosen
```

```
shared = ECDH(X, Y)
sas_key = HKDF-SHA-256(ikm=shared, salt="scimux-rv/sas/v1",
                       info=transcript, L=4)
sas = sprintf("%06d", uint32_be(sas_key) % 1000000)
```

~20 bits. Display on both sides. Never a password. Never
auto-confirmed. A mismatch is a failed pairing, not a retry of
the same transcript. Vectors `pairing-transcript-v1`,
`pairing-sas-6`.

QR / link carry `X`, `rid`, `code`, `origin` in the URL fragment
(`#…`), never sent to any server. Typed path carries only
`code`; a malicious rv can MITM that path, which is why SAS is
mandatory there.

### 11.1 Invite fragment

The fragment is `application/x-www-form-urlencoded`:

| key | value |
|---|---|
| `v` | `1`. Any other value is refused, never guessed at. |
| `o` | the rendezvous origin. Optional; when present it MUST equal the origin the page was served from, or the invite is refused. |
| `c` | the short code, grouped or not (§10.1 normalisation applies). |
| `r` | the 64-character lowercase-hex RID. |
| `x` | `X` uncompressed, hex — 130 characters beginning `04`. |

A fragment carrying none of `c`, `r`, `x` is simply not an invite
and is not an error. A fragment carrying some of them is refused.
These values MUST NOT appear in the query string: a query reaches
rv's access log, and §8 forbids the RID on that surface.

### 11.2 The typed path is deferred (V1)

Decided 2026-08-26. A device that typed only a short code has
neither `X` nor `rid`. Without `X` it cannot compute `ECDH(X, Y)`
and so has no digits to compare; without `rid` it cannot even open
the computer's `pair-reply`, whose AD is `origin || 0x00 || rid_hex`
(§12.2). §12.3 therefore describes a path no client can complete,
and a client that appeared to complete it would be showing digits
it had not derived — the exact failure §12.3 exists to prevent.

V1 clients pair from a link or QR only. Closing the gap means
carrying `x_pub` and `rid` in the `pair-reply` and unsealing that
reply on the typed path; it is a change to §12.1 and §12.3 and to
both implementations, and is deliberately not made here.

---

## 12. Envelope inner format and seal

Opaque to rv (`[]byte`). 4 kB cap is on the **sealed** blob.

### 12.1 Inner JSON

| field | type | notes |
|---|---|---|
| `v` | number | 1 |
| `type` | string | `session-offer`, `session-answer`, `pair-offer`, `pair-reply` |
| `sdp` | string | session types only; includes the DTLS fingerprint in the SDP |
| `fingerprint` | string | session types; `sha-256` hex pairs, must match the SDP |
| `device_pub` | string | pair-offer: `Y` uncompressed, hex |
| `offer_nonce` / `reply_nonce` | string | pair types; 32 bytes hex |
| `install_pub` | string | pair-reply: `I`, hex |

Vector `envelope-inner-offer`. FR-16: a hub that alters the SDP
causes DTLS failure because the fingerprint is inside the seal.

### 12.2 Seal

Ephemeral-static ECIES, P-256, HKDF-SHA-256, AES-256-GCM.
A **fresh** ephemeral private key is required for every seal.
Reusing one across messages reuses the 12-byte nonce space.

```
e        = ephemeral P-256 private   // fresh CSPRNG per seal
E        = e.Public (uncompressed, 65 bytes)
shared   = ECDH(e, recipient_static)
info     = "seal" || 0x00 || E || 0x00 || recipient_static
key      = HKDF-SHA-256(ikm=shared, salt="scimux-rv/envelope/v1",
                        info=info, L=32)
nonce    = 12 CSPRNG bytes
ad       = origin || 0x00 || rid_hex
ct       = AES-256-GCM-Seal(key, nonce, plaintext, ad)
sealed   = E || nonce || ct        // ct includes the 16-byte tag
```

`E` and the recipient's static public key are bound into the KDF
so a transplanted ephemeral cannot open a blob sealed to a
different recipient (unknown-key-share / cross-context reuse).
Recipient opens with its static private and `E`. Vector
`envelope-seal-p256` (`ephemeral_priv_hex` is a fixture).

Session offer: phone seals to computer `X`. Session answer: computer
seals to device `Y`. Pairing QR offer: phone seals to `X` from
the fragment. Pairing reply: computer seals to `Y` from the offer.

### 12.3 Typed-path offer is unsealed

On the **typed** pairing path the phone has only the short code.
It does not have `X`, so it cannot seal. The `pair-offer`
`envelope` field on that path is the inner JSON of §12.1 in
plaintext. rv therefore sees the device's public key, both
nonces if present, and — if a session-shaped body were sent —
the SDP.

That is a complete loss of confidentiality from the server on
this path. The design accepts a malicious rv here and **requires
SAS** (§11): pairing MUST NOT complete unless both sides derive
the same six digits and the human confirms them on each. Whether a
side displays its digits or requires them to be entered is that
client's decision; scimux displays on the device and requires
entry on the computer, so that a device which is not the user's
cannot be waved through by someone with nothing to compare. A
client that
auto-confirms, or that treats a typed-path pairing as
confidential from rv, is not implementing this spec.

QR, fragment-link, and every post-pairing session envelope are
sealed. The typed path is the only exception.

---

## 13. `/p` bootstrap inventory

FR-33. Closed enumerated GET list. The inventory is this table
and nothing else. A path, method, or trailing slash that is not
an exact row is `p-rejection` — the same bytes as
`constant-rejection` (§7): status 404, the same headers, the
ten-byte body, `Content-Length: 10`, no `Transfer-Encoding`, no
`Location`, no `Server`, no listing, no framework error, no
version banner. That comparison is made through a real HTTP
client (AT-FR-33-a), not a `ResponseRecorder`.

`/p` MUST NOT pin a client to a scimux version. The application
is fetched from the computer (FR-14, FR-40). A scimux version
string in a `/p` body, query, path, or header is a defect. Two
rv binaries stamped with different version strings MUST serve
byte-identical inventory responses.

| route | type | cache | integrity | notes |
|---|---|---|---|---|
| `GET /p` | `text/html; charset=utf-8` | `no-store` | `Digest: sha-256=:…:` of the HTML | the page shell: markup and one module `src`. No inline script — the CSP below forbids it. No application code. |
| `GET /p/start.js` | `application/javascript; charset=utf-8` | `no-store` | `Digest: sha-256=:…:` of the JS | the entry point: imports `page.js` and calls `start()`. Nothing else. |
| `GET /p/page.js` | `application/javascript; charset=utf-8` | `no-store` | `Digest: sha-256=:…:` of the JS | the pairing screens (NFR-12) and the page's wiring. |
| `GET /p/boot.js` | `application/javascript; charset=utf-8` | `no-store` | `Digest: sha-256=:…:` of the JS | pairing-to-handover orchestration and the FR-24 causes. Does not load the module graph; that graph arrives over the channel (FR-40). |
| `GET /p/pairing.js` | `application/javascript; charset=utf-8` | `no-store` | `Digest: sha-256=:…:` of the JS | pairing crypto: transcript, SAS, envelope seal (§11, §12). |
| `GET /p/rendezvous.js` | `application/javascript; charset=utf-8` | `no-store` | `Digest: sha-256=:…:` of the JS | the rendezvous HTTP client (§9, §10). |
| `GET /p/session.js` | `application/javascript; charset=utf-8` | `no-store` | `Digest: sha-256=:…:` of the JS | session offer/answer and the data channel (§9, §12). |
| `GET /p/store.js` | `application/javascript; charset=utf-8` | `no-store` | `Digest: sha-256=:…:` of the JS | per-installation storage namespace. |
| `GET /p/boot.css` | `text/css; charset=utf-8` | `no-store` | `Digest: sha-256=:…:` of the CSS | bootstrap styles. |
| `GET /p/codec.js` | `application/javascript; charset=utf-8` | `no-store` | `Digest: sha-256=:…:` of the JS | tunnel handshake, frames, records and payloads ([`tunnel-v2.md`](tunnel-v2.md) §2–§6). Protocol, not application code. |
| `GET /p/connection.js` | `application/javascript; charset=utf-8` | `no-store` | `Digest: sha-256=:…:` of the JS | tunnel request lifecycle and FR-24 states (`tunnel-v2.md` §7, §9). Protocol, not application code. |
| `GET /p/manifest.webmanifest` | `application/manifest+json; charset=utf-8` | `no-store` | `Digest: sha-256=:…:` of the JSON | the Home Screen app declaration. `display: standalone`; `start_url` and `scope` are both `/p`. |
| `GET /p/icon-180.png` | `image/png` | `no-store` | `Digest: sha-256=:…:` of the PNG | the `apple-touch-icon`. iOS takes the Home Screen icon from the link in the shell, not from the manifest. |
| `GET /p/icon-512.png` | `image/png` | `no-store` | `Digest: sha-256=:…:` of the PNG | the manifest icon, `purpose: "any maskable"`; artwork inset for launchers that crop. |

The inventory grew from three rows to five on 2026-08-24, when the
tunnel protocol moved into this repository, from five to ten on
2026-08-26, when the pairing client was built, to eleven on
2026-08-27, when the entry point became a row, and to fourteen on
2026-09-05, when the Home Screen manifest and its two icons became
rows. Before those three, adding `/p` to a Home Screen produced a
bookmark rather than an app: nothing in the shell declared one, so the
launcher opened a browser tab and captured a screenshot for the icon.

The entry point had been an inline
`<script type=module>`, which the CSP two paragraphs below forbids, so
`/p` rendered its "Loading…" placeholder and stopped there in every
browser. An inline exemption — `unsafe-inline`, a nonce, or a hash —
would have been the smaller diff and the wrong one: the inventory is the
served set, and the code that runs the page belongs in it, carrying a
`Digest` and closed over this table like every other row. "Closed" means
**enumerated and fail-closed**, not fixed at any number: a row is
added by amending this table and `specPInventory`, never by a
wildcard, a directory handler, or a route that reads from disk per
request.

Every module is served at the path its own relative imports resolve
to, so rv's half of the page needs no import map and no specifier
rewriting. The computer's graph is a different problem and another
repository's (`tunnel-v2.md` §8).

None of these modules is application code. They implement pairing,
the tunnel, and the FR-24 states, and they stop at §8's three fixed
constants. FR-33 is asserted over them as a closure property rather
than a word ban: every `import` in an inventory body resolves to
another row of this table, every `icons[].src` in the manifest is a row
of it, the manifest's `scope` is `/p` rather than `/` — a wider scope
would claim routes this table does not contain — and the only computer
route any of them names is §8's `/api/remote/bootstrap`, which is fetched over the
data channel and never from rv. What the browser does *after* the
channel is negotiated is fetched from the computer through those three
constants.

Each inventory response also carries:

- `X-Content-Type-Options: nosniff`
- `Content-Security-Policy: default-src 'self'; script-src 'self' blob:; style-src 'self' blob: 'unsafe-inline'; img-src 'self' blob: data:; worker-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'`
- `Referrer-Policy: no-referrer`
- `Cache-Control: no-store`
- `Digest: sha-256=:<base64 of SHA-256(body)>:` (RFC 9530)

`Digest` is a content hash of the embedded bytes. It is not a
scimux version and MUST NOT be derived from the linker-stamped
binary version. Changing an asset changes its digest; changing
the rv version string MUST NOT.

The `blob:` allowances are for computer bytes already fetched through the
authenticated tunnel and checked against the computer's manifest before an
object URL is minted. Inline and evaluated script remain forbidden. Inline
style is allowed because the computer UI uses style attributes for dynamic
layout and state; workers and objects remain forbidden.

POST, PUT, DELETE, PATCH on an inventory path are
`p-rejection`. `GET /p/` (trailing slash), `GET /p/index.html`,
`GET /p/app.js`, and every other `/p…` path are `p-rejection`.
Query strings on an exact inventory path are ignored (the path
is the inventory key). An unclean path (`/p/../p`) is the
constant 404 before ServeMux can 301.

### 13.1 Served bytes

`GET /p` (`p-page`) is the only inventory body small enough to pin
verbatim, and its vector carries these exact bytes. It contains no
`/js/` of the application, no `/api/`, no `index.html`, no import
map, and no version token:

```
<!doctype html>
<meta charset=utf-8>
<meta name=viewport content="width=device-width,initial-scale=1">
<title>Pair with scimux</title>
<link rel=stylesheet href="/p/boot.css">
<main id=app><h1>Pair with scimux</h1><p>Loading&#8230;</p></main>
<script type=module src="/p/start.js"></script>
```

(the file ends with a newline).

The ten asset rows are **not reproduced inline.** They are hundreds
of lines of protocol and pairing implementation, and pasting them
here would create a second copy to keep in sync with `web/` — the
drift this document exists to prevent. Their served bytes are exactly
the checked-in bytes of the matching file under `web/js/` and
`web/css/`, embedded at build time; each vector pins its
`body_sha256` and its `Digest` header, so a module that changes moves
its own row and nothing else. The behaviour they implement is
specified in this document and in [`tunnel-v2.md`](tunnel-v2.md).

The FR-33 properties that apply to every inventory row — no scimux
version token, no application code, imports closed over the table,
byte-identical across rv versions — are asserted by
`TestAT_FR_33_b_NoApplicationCode` and `TestAT_FR_33_c_NoScimuxVersionPin`.

---

## 14. Invite codes

Referenced only. See `internal/invite`: 15 CSPRNG bytes, 24
Crockford characters, grouped 6×4 at issue, verifier =
SHA-256(`scimux-rv/invite-verifier/v1` \|\| 0x00 \|\| normalised).
No server pepper. Vectors do not redefine this.

---

## 15. Test vectors

Directory: `internal/rv/testdata/vectors/*.json`.

Each object has `id`, `message` (a catalog `id`), `kind`, and
kind-specific fields. A file is a JSON array. Kinds:

| kind | how it is executed |
|---|---|
| `auth-message` | recompute the concatenation from literals |
| `ed25519` | sign and verify from the seed |
| `handle`, `rid`, `pairing-code` | recompute the encoding |
| `rid-collision-retry` | **test-only** seeded retry; not a minting rule |
| `envelope-seal` | seal and open. Field `ephemeral_priv_hex` is a fixture so the ciphertext is reproducible. A client MUST draw a fresh ephemeral per seal. |
| `pairing-transcript`, `pairing-sas` | recompute HKDF |
| `envelope-inner` | required JSON fields |
| `http` | if the request path is registered, **replay** through `newAdmissionHandler` and compare `dumpResponse` (status + sorted headers + body). Otherwise declarative. Expected responses for built admission routes are authored from §4, not recorded from the handler. `/p` inventory rows become replayed once those GET routes are registered. |
| `rejection` | as `http`, plus the dump MUST equal the live constant rejection. The expected 404 is authored from §7. |
| `stun` | recompute XOR-MAPPED-ADDRESS from the documented address, port and transaction id (§17); compare `response_hex`. |
| `stun-drop` | the request hex is documented as dropped; there is no response field. |

`docs/protocol/vectors.sha256` is SHA-256 of each `*.json` file,
sorted by base name, `hex  filename` per line. The Go test
recomputes it. The scimux WebCrypto runner MUST recompute it
against its copy of the directory and fail on mismatch. That is
the binding AT-NFR-14-a names between the two sides.

`WRITE_P0_VECTORS=1 go test ./internal/rv -run TestWriteP0Vectors`
regenerates **inputs** (keys, nonces, codes, entropy) and the
construction ciphertexts. It does not capture handler output as
an expected response.

---

## 16. Superseded text

| text | replacement |
|---|---|
| `remote-scimux.md`: rendezvous IDs are "free to mint" | Unauthenticated minting is gone. An enrolled installation mints; wait is admitted. The "random key held by both" wording stands. |
| `remote-scimux.md` §6: `GET /v1/wait/{id}`, invite token on wait | V1 is `POST /v1/wait` with a JSON body, admitted by signature over a fresh challenge. The ID is a field, not a path element, so it does not leak in access logs as a URL. Envelope keeps `{id}` in the path because FR-18 resolve-before-read needs it. |
| AT-NFR-17-a looking for `newRendezvousID` / `generateRID` in rv | Negative assertion: those identifiers must not exist in non-test rv source. Collision retry is vector `rid-collision-retry-test-only`. |
| `remote-by-invite-only.md` FR-20 and "No oracle": "indistinguishable in content and in timing" | FR-20 is content only. The FR-20 row and that paragraph now state the envelope-route timing exception and reject padding. Requirement lives in the design document; this spec records the amendment. §9.4, AT-FR-20-c. |
| A challenge fetched before a long poll remaining usable for the reply | Dead on arrival. Next challenge is `X-Rv-Challenge` on the wait response. |
| §9.3 item 3 read as the envelope POST creating a pending slot | The slot is created when a waiter registers. An envelope for an ID with no prior waiter is unknown: no body read, no allocation (FR-18, AT-FR-19-b). |
| §10.3 "Consumption happens at … or at `pair/cancel`" | Cancel does not consume. Single-use of the code is at `reply` or TTL (§10.4, §10.6). |
| P0 stub health payload `{ready, version}` | §18 allowlist. `version` stays; FR-09 counters join it. Per-installation and per-code fields stay forbidden. |

P3 revision of the parked `TestAT_NFR_17_a`: invert the grep;
move the collision-retry subtest to the vector already in this
phase.

---

## 17. STUN Binding

FR-08. This is the first UDP surface. HTTP body caps, header
timeouts and `Content-Length` do not apply.

**Implementation.** A hand-rolled Binding-only responder
(RFC 5389 / RFC 8489 Binding). NFR-01 forbids a STUN library.
The source document left this choice open; stdlib-only closes
it. The responder answers Binding Request and nothing else.

**Listeners.** Two sockets, one per family:

| flag | default | family |
|---|---|---|
| `-stun-addr` | `127.0.0.1:3478` | UDP IPv4 |
| `-stun6-addr` | `[::1]:3478` | UDP IPv6 |

An empty flag disables that family. A non-empty flag that
cannot bind fails startup. Port `0` is allowed (tests).
Deployment (P6) points both at the jail's public addresses.
The sockets are bounded by a fixed 1280-byte receive buffer
(IPv6 minimum MTU). A datagram that fills the buffer is
oversized: drop, no reply.

**Request.** 20-byte STUN header, big-endian:

| offset | size | field |
|---|---|---|
| 0 | 2 | message type; Binding Request is `0x0001` |
| 2 | 2 | message length (bytes after the header) |
| 4 | 4 | magic cookie `0x2112A442` |
| 8 | 12 | transaction id, echoed |

Attributes, if present, are ignored when they are
comprehension-optional (`0x8000`–`0xFFFF`). A comprehension-required
attribute other than those Binding does not need is not answered
with 420: the datagram is dropped. Attributes are walked as
4-byte-aligned TLVs; a malformed or truncated TLV is a drop.
Unknown methods, Binding
Indications, STUN responses, a bad magic cookie, a truncated
header, a length that does not match the datagram, and any
datagram larger than 1280 bytes are dropped. No error response
is ever sent. That is the amplification bound: the only
outgoing datagram is a Binding Success, 32 bytes (IPv4) or
44 bytes (IPv6). SOFTWARE, FINGERPRINT, MAPPED-ADDRESS and
any other attribute MUST NOT be emitted. A version string on
this path is a defect.

**Success.** Type `0x0101`. One attribute, XOR-MAPPED-ADDRESS
(`0x0020`), encoding the source address of the request:

- family `0x01` (IPv4) or `0x02` (IPv6)
- X-Port = port XOR `0x2112` (the top 16 bits of the cookie)
- X-Address IPv4 = address XOR the magic cookie
- X-Address IPv6 = address XOR (magic cookie ∥ transaction id)

Vector `stun-binding-success-v4` uses `192.0.2.1:3478` and
transaction id `000102030405060708090a0b`. Vector
`stun-binding-success-v6` uses `2001:db8::1:3478` and the same
id. Vector `stun-binding-request` is the 20-byte empty Binding
Request. Vector `stun-drop-truncated` is a 10-byte prefix.

`scimux-rv stun-probe host:port` sends one Binding Request and
prints the recovered XOR-MAPPED-ADDRESS. It is how AT-FR-08 is
reproduced on a deployed host.

Both the browser peer (rv `/p` composition) and the computer peer
(scimux answering path) derive this same unauthenticated STUN service
from the rendezvous origin: host of the origin, UDP port `3478`,
ignoring any HTTP/HTTPS web port. There is no TURN or relay.

---

## 18. Aggregate health

FR-09. Loopback-only, unauthenticated, reset on restart. The
socket is the confinement: `-health-addr` MUST be a loopback
host and startup refuses otherwise (§ existing `checkLoopback`).
A request that arrives on a non-loopback address cannot reach
this mux, asserted by binding a second listener (AT-FR-09-a),
never by inspecting a header.

`GET /health` is the only route on this mux. Anything else is
the constant 404. The public mux does not serve `/health`.

**Allowlist.** The body is one JSON object. These keys, and
no others. The test fails on an unexpected field
(AT-FR-09-b). `version` is the existing stub field, kept
because the operator needs the binary stamp; it is not an
installation identifier.

| key | type | meaning |
|---|---|---|
| `ready` | bool | process is serving |
| `version` | string | linker-stamped rv version |
| `waits` | number | live `POST /v1/wait` holders |
| `waits_max` | number | high-water of `waits` since start |
| `pairing_waits` | number | pairing-map residency (`len(pairs)`), the same quantity `MaxPairingWaits` / `MaxPairingResidents` count |
| `pairing_waits_max` | number | high-water of `pairing_waits` |
| `accepted` | number | `/v1/*` responses with 2xx |
| `rejected` | number | public constant 404s |
| `rate_limited` | number | public 429s |
| `envelope_bytes` | number | sum of accepted `POST /v1/envelope/{id}` body lengths |
| `replay_generation` | number | 1 after the startup Replay; increments when `ReplayIfStale` consumes new log bytes |

Integers are JSON numbers, non-negative, with no fractional
part. `/p` and `GET /` do not increment `accepted` or
`rejected`. Per-code and per-installation counters are
deferred (§6 of the design document) and MUST NOT appear.
No rendezvous ID, pairing code, handle, verifier, public key,
or IP may appear as a key or a value (AT-FR-09-c).

Vector `health-response` documents the zero-state object.
