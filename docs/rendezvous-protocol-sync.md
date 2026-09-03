# Rendezvous protocol vector sync

This repository vendors the scimux-rv rendezvous protocol specification
and its test vectors. The current copy is taken from scimux-rv committed
revision a6ba05b.

S1 is Gate B. These files are the protocol artifacts, not shipped product
logic. The Go and WebCrypto acceptance tests bind both sides to the same
`vectors.sha256`. There is no parallel `spec_version` field.

## Source revision

Name the scimux-rv Git object id that produced the vendored bytes. The
current source is committed revision **a6ba05b**
(`a6ba05bac03a42fd15ab25e0a8b5b66003bc9610`). When rv regenerates
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
git -C /path/to/scimux-rv cat-file blob $(git -C /path/to/scimux-rv rev-parse a6ba05b:docs/protocol/rendezvous-v1.md)
```

and the analogous `rev-parse <oid>:<path>` for the digest and each
JSON file. `git show` of a blob is acceptable only when the output is
byte-identical to `git cat-file blob`.

## Future rv vector revision

1. Identify the committed scimux-rv Git OID that authored the new spec
   and vectors.
2. Byte-copy the specification, `vectors.sha256`, and every vector
   JSON from that commit using the table above.
3. Record the new concrete committed Git OID in this document (replace
   `a6ba05b` in the source-revision paragraph). Do not leave the old
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
