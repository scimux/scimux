# Remote integration invariants

Part of the invariant set in `AGENTS.md`. Read before changing remote access,
pairing, bootstrap, browser module imports, codec vectors, or dependencies in
`internal/remote/`. The wire contract is in `docs/protocol/rendezvous-v1.md`;
its vendoring procedure is in `docs/rendezvous-protocol-sync.md`. FR-xx/NFR-xx
identifiers retain their numbering from the archived remote-access requirements.

- **Viewer and service origins are independent configuration.** Official v2
  pairing links open `https://my.scimux.com/p`; fragment `o` names the
  cryptographic rendezvous service `https://rv.scimux.com`. `Config.Origin`
  remains the service identity bound into signatures, transcripts, persisted
  enrollment and STUN derivation. `Config.ViewerOrigin` chooses only the trusted
  page that receives the invitation. Changing one must never silently rewrite
  the other. Custom viewer origins are canonical HTTPS origins and survive the
  bounded web-child configuration pipe; an older pipe document defaults to the
  official viewer. The local HTTP listener does not gain CORS from this split.
- **The tunnel protocol is owned by scimux-connect, not by this repository.** The
  browser↔computer wire format is specified in scimux-connect's
  `docs/protocol/tunnel-v2.md`, and its **browser** implementation lives there
  too (`web/js/codec.js`, `web/js/connection.js`). Deployment asymmetry is the
  reason: the independent viewer reaches every browser on the next page load,
  while scimux binaries sit on many computers at many versions, so the side
  that must tolerate the spread holds the protocol. Never reintroduce a
  browser codec or channel transport here — it would arrive over the very
  channel it exists to create. Read the spec for the format; what binds here:
  - The computer half is `internal/remote/codec` plus `web/js/bootstrap.js`,
    which is the *loader*, not the transport. It ships with the binary because
    it knows this repository's module graph, which is what lets us restructure
    that graph with no viewer deployment. Vendoring it into the viewer would
    leave it stale and supply the code that checks this computer's own
    integrity values, inverting FR-40.
  - Three constants are protocol, not local names: `GET /api/remote/bootstrap`,
    `GET /js/bootstrap.js`, and the `bootstrap({channel, manifest,
    createObjectURL, installImportMap, importModule})` signature. Renaming or
    moving any of them is a MAJOR bump that breaks every deployed viewer.
  - The loader rewrites every specifier to the imported module's absolute
    `blob:` URL and installs **no** import map (a `blob:` URL has an opaque
    path, so nothing relative or root-absolute resolves against it). Hence
    `installImportMap` stays in the signature as protocol but is left
    uncalled, and **the browser module graph must stay acyclic** — a `blob:`
    graph cannot express a cycle, which is the named `bootstrap-cyclic` abort
    (AT-FR-40-f), never a partial boot.
  - Versioning is semantic and binds both halves: MAJOR must surface as a
    named FR-24 state saying which side is behind
    (`tunnel-version-mismatch`); MINOR is additive and must never escalate to
    the user. Keeping MINOR safe is a local obligation: ignore unknown frame
    types, record tags, rejection classes and trailing bytes instead of
    rejecting them, treat type/tag `0x00` as permanently reserved and always
    malformed, never reuse or retype a tag number, and add no count prefix
    anywhere so a claimed count cannot be lied about. The frozen 8-byte
    `SCMX` preamble never gains a field — capabilities grow in the hello, and
    payload fields are tagged records rather than positional.
- **`internal/remote/codec/testdata/vectors.json` is a published contract, not
  a local fixture.** `vectors_test.go` regenerates it from the production
  encoders and fails on drift; scimux-connect byte-copies it and decodes every
  vector with its browser codec. It is the only oracle either side has, so it
  must stay generated — never hand-edited, pretty-printed, or authored by
  reading the spec. Each vendoring lane is owned by the *receiving*
  repository: the manual procedure in `docs/rendezvous-protocol-sync.md`
  here, and `scripts/vendor-tunnel.sh` in scimux-connect;
  a single bidirectional "sync everything" entry point is deliberately absent,
  because it would eventually regenerate these vectors on the consuming side.
- **`PairingTTL` is a constant shared with a server this repository does not
  deploy.** `internal/remote.PairingTTL` and rv's `PairingWaitTTL` must hold
  the same value — **120 s** as of 2026-09-05 — and nothing enforces it; a
  disagreement is silent in the direction that matters (expire first here and
  the user is told a live code is dead; expire first there and the computer
  waits on a dead slot). The value is an economics decision (10^8 codes
  against rv's 30 offers/s is ~3600 blind tries per window), so change it in
  both repositories, with the arithmetic, or not at all. FR-11 carries it too.
- **Pairing is offered only where it could complete.** Minting is entirely
  local and asks the rendezvous for nothing, so a computer rv would not admit
  can still mint a perfect code and QR for a meeting that can never happen,
  which the user reads as a timing problem rather than "not enrolled".
  `internal/app.pairingAvailable` is therefore an **allowlist** — "enrolled"
  and "unavailable", nothing else — gating the mint, the session projection
  and the `can_pair` the menu reads. "unavailable" is in it because the mint
  starts the wait loop that clears a transient outage; refusing it would be
  self-sustaining. `""` (just unlinked) and the FR-30 broken states are out.
  The browser must not re-derive this from `hosted`: one copy of the policy,
  on the side that enforces it. `#m_pair` ships hidden and is revealed by the
  status read, leaving `#m_pair_note` to say why — a control that vanishes
  without a word reads as a bug.
- **Origin migration uses the existing unlink flow.** An enrollment saved for
  another service origin fails before authenticated outbound traffic. Restart
  against the saved old origin, unlink while it is available, then enroll with
  a fresh invite at the new service. Never rewrite `remote/state` in place or
  point scimux at a new data directory: unlink is remote-specific and preserves
  histories, settings and session workers.
