# Rendezvous protocol vector sync

This repository vendors the scimux-rv rendezvous protocol specification
and its test vectors. The current copy is taken from scimux-rv committed
revision ccb8ffe.

S1 is Gate B. These files are the protocol artifacts, not shipped product
logic. The Go and WebCrypto acceptance tests bind both sides to the same
`vectors.sha256`. There is no parallel `spec_version` field.

## Source revision

Name the scimux-rv Git object id that produced the vendored bytes. The
current source is committed revision **ccb8ffe**
(`ccb8ffeb237249bb490b0e44a87ab94dd7fb5837`). When rv regenerates
vectors, replace that oid with the new committed revision and record it
here in the same sentence form: “scimux-rv committed revision \<oid\>”.

## Byte-copy mappings

Vendoring is a **byte copy** of committed blobs from that revision. Do
not pretty-print, reorder keys, normalize newlines, regenerate vectors,
or copy a working-tree file that differs from the commit.

| rv path at the named revision | scimux destination |
|---|---|
| `docs/protocol/rendezvous-v1.md` | `docs/protocol/rendezvous-v1.md` |
| `docs/protocol/vectors.sha256` | `internal/remote/testdata/vectors.sha256` |
| `internal/rv/testdata/vectors/*.json` | `internal/remote/testdata/vectors/*.json` |

Copy every vector JSON file. Reformatting or reserialising JSON is
forbidden. A pretty-printer or a key reorder silently breaks
`vectors.sha256`, which is SHA-256 of the exact file bytes.

Extraction must come from Git, for example:

```
git -C /path/to/scimux-rv cat-file blob $(git -C /path/to/scimux-rv rev-parse ccb8ffe:docs/protocol/rendezvous-v1.md)
```

and the analogous `rev-parse <oid>:<path>` for the digest and each
JSON file. `git show` of a blob is acceptable only when the output is
byte-identical to `git cat-file blob`.

## Reading the vendored copy

The specification is a byte copy, so it is written for its own
repository and reads a little oddly here. Three things a reader of this
tree will notice, none of which is a defect and none of which may be
"fixed" in place -- an edit would break the byte copy, disagree with the
bytes rv publishes, and be reverted by the next sync:

- **Its relative links resolve in rv, not here.** `vectors.sha256` sits
  beside the spec there; here it is `internal/remote/testdata/vectors.sha256`,
  per the table above. `tunnel-v2.md` is rv's document and deliberately
  has no copy in this repository -- see the tunnel-protocol invariant in
  AGENTS.md for why that half lives there.
- **It cites two design documents this repository does not track.**
  `remote-scimux.md` and `remote-by-invite-only.md` are superseded plans,
  archived under untracked `attic/`. Every citation quotes the sentence it
  supersedes or relies on, so nothing in the spec depends on opening them;
  they are provenance for a decision, not a reference you must follow.
  This is the same arrangement AGENTS.md already records for the FR-xx and
  NFR-xx numbering, which those archived documents also originate.
- **It is therefore self-contained, and says so.** Its header claims a
  browser and a computer that have never read the scimux-rv repository can
  be implemented from it plus the vectors. That claim is the reason the
  dangling names above are tolerable: follow the quotation, not the
  filename.

## Future rv vector revision

Use the four steps below. Start from a source tree with no uncommitted
changes to the vendored paths. Pin the commit that last *touched* them,
enumerate its vector JSON files, and remove any destination vector that
is absent from that source revision. Run both verification commands.

1. Identify the committed scimux-rv Git OID that authored the new spec
   and vectors.
2. Byte-copy the specification, `vectors.sha256`, and every vector
   JSON from that commit using the table above.
3. Record the new concrete committed Git OID in this document (replace
   `ccb8ffe` in the source-revision paragraph). Do not leave the old
   oid in place as the current source.
4. Run both verification commands below. Both must pass against the
   newly copied bytes.

## Verification

After every synchronization, run both sides:

```
go test ./internal/remote -count=1
node --test web/test/rendezvous-protocol.test.js
```

The Go tests execute the specification catalog and every vector and
recompute `vectors.sha256`. The WebCrypto tests independently recompute
the same digest and execute the construction vectors. Either failing
means the copy is not current.
