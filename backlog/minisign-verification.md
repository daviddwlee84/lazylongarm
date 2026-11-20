# minisign verification of Longarm release binaries

Status: research. Priority P2.

Today `internal/release` downloads `SHA256SUMS` and the asset from the same
GitHub release over HTTPS. That detects corruption and a tampered mirror, not a
compromised release. Upstream signs every asset with minisign (`<asset>.minisig`,
trusted comment binds file name + version), but the public key is a CI secret
built into longarmd (`internal/update.PublicKey`, docs/ci-cd.md §2.1); it is
not published in the repository.

Options:
1. Pin the key extracted once from a verified longarmd (`strings | grep ^RW`)
   and ship it in lazylongarm; fail closed when a release is signed by another key.
2. Ask upstream to publish the key (README / `longarm version --json`).

Implementation: `aead.dev/minisign` (same library as upstream), verify
signature + trusted comment `longarm <name> <version>` before caching.
