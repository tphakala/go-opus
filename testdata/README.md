# testdata

Test fixtures for go-opus. The large, license-encumbered conformance payloads
here are fetched on demand and sha256-verified, never committed.

## Conformance test vectors

The official RFC 6716 / RFC 8251 Opus conformance vectors live under
`testdata/vectors/` after fetching. They are gitignored (see the repo
`.gitignore`): only the fetch script and the checksums are tracked.

Fetch and verify them with either:

```sh
task vectors
# or, directly:
scripts/fetch-vectors.sh
```

The script downloads the RFC 8251 reference archive from opus-codec.org,
verifies its sha256 against `testdata/vectors.sha256` before extracting, and
unpacks it to `testdata/vectors/opus_newvectors/` (12 bitstreams
`testvector01.bit` .. `testvector12.bit`, each with stereo `.dec` and mono
`m.dec` reference decoder outputs). It is idempotent: an archive that is
already present and verifies is not re-downloaded, and an extracted set is left
in place.

Environment toggles:

- `FETCH_RFC6716=1` also fetches the original RFC 6716 archive to
  `testdata/vectors/opus_testvectors/`. Its bitstreams are byte-identical to the
  RFC 8251 set; only the `.dec` reference outputs differ (pre-errata), so
  conformance runs against the RFC 8251 references and this archive is optional.
- `FORCE=1` re-extracts even when a vector set is already present.

See `docs/test-vectors.md` for the per-vector census (modes, mode transitions,
bandwidths, frame sizes) and the archive URLs and checksums.
