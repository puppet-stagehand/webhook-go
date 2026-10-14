# webhook-go, Stagehand fork: Maintaining

How the fork is kept current with upstream and how a release is cut and verified. Budget: about 1-2 hours a
month, plus an unscheduled re-release when upstream or a Go dependency ships a security fix (Phase 57.6
research, A13).

## Branch model (FK-01)

| Branch or tag | What it is |
|---------------|------------|
| `master` | A mirror of upstream `voxpupuli/webhook-go` `master`. Never commit to it. |
| `stagehand` | The fork's default branch: an upstream release tag plus the fork's commits on top. All fork work lands here (via pull request once branch protection is on). |
| `vX.Y.Z` | Upstream's tags, carried by the fork. Never released from here (the release workflow ignores them). |
| `vX.Y.Z-stagehand.N` | The fork's releases. `X.Y.Z` is the upstream base; `N` starts at 1 for each new base. |

**Current base: upstream tag `v2.15.1`** (`e271c52`). The upstream watch reads the line below; keep its format
when you rebase.

<!-- upstream-base: v2.15.1 -->

The Go module path stays `github.com/voxpupuli/webhook-go`. Renaming it would touch every import and turn
each rebase into a conflict across the tree.

## What the fork touches upstream

New, fork-only files (rarely conflict):

- `api/deploys.go`, `lib/jobs/**`, `server/tls.go`, `server/crl.go`, their tests and `testdata`;
- `docs/stagehand/**`, `CHANGELOG.md`, `.github/workflows/release.yml`, `.github/workflows/upstream-watch.yml`.

Upstream files the fork edits (where rebase conflicts land):

| File | Fork change | Conflict risk |
|------|-------------|---------------|
| `server/server.go` | `Init`'s mTLS branch became `serveMTLS` with signal handling | high |
| `server/router.go` | `NewRouter` wraps `newRouter(ctx)`; one re-indented block | high |
| `config/config.go` | Stagehand `server.tls.*` and `deploys.*` keys and validation | medium |
| `.goreleaser.yml` | Linux-only builds, fork nfpm metadata, signs, sboms, docker and release owner | medium |
| `README.md` | the "Stagehand fork" section above upstream's text | low |
| `build/webhook.yml` | a commented Stagehand block appended | low |
| `.github/workflows/go.yml` | `stagehand` added to the trigger lists | low |

`.github/workflows/release.yml` replaced upstream's. If upstream changes theirs, take only what still applies
(for example a Go version bump) and keep the fork's tag filter, pinned SHAs and cosign steps.

## Rebase onto a new upstream tag

1. Fetch: `git fetch upstream --tags`, then read the upstream release notes for the new tag (`vA.B.C`).
2. Update the mirror: `git checkout master && git merge --ff-only upstream/master && git push origin master`.
3. Branch: `git checkout -b rebase/vA.B.C stagehand`.
4. Rebase: `git rebase --onto vA.B.C v<old base> rebase/vA.B.C` (today `v<old base>` is `v2.15.1`). Resolve
   conflicts in the files above. Keep upstream's behaviour when mTLS is off, and the fork's when it is on.
5. Update `<!-- upstream-base: ... -->` above and the base named in `README.md`.
6. Test: `go vet ./... && go test -count=1 -race ./...`, then
   `goreleaser release --snapshot --clean --skip=sign,sbom,docker,publish`.
7. Push the branch and open a pull request into `stagehand`. CI (`go.yml`) must pass.
8. Merge. A rebase rewrites history, so `stagehand` moves by force-push here, and only here. Announce it.
   Keep the old head as a tag: `git tag stagehand-before-vA.B.C <old head>`. Old release tags keep pointing
   at their commits, so published releases stay verifiable.
9. Cut `vA.B.C-stagehand.1` (next section) and close the upstream-watch issue.

## Cut a release

Only from `stagehand`; the workflow refuses a tag that is not on that branch.

1. Add a section to `CHANGELOG.md` for the new tag, and commit it on `stagehand`.
2. Check CI is green for that commit:
   `gh run list --repo puppet-stagehand/webhook-go --branch stagehand --limit 1`.
3. Tag and push:

   ```sh
   git tag -a vX.Y.Z-stagehand.N -m "webhook-go Stagehand fork vX.Y.Z-stagehand.N (D-09 fork, D-10 signed release)"
   git push origin vX.Y.Z-stagehand.N
   ```

4. Watch it: `gh run watch --repo puppet-stagehand/webhook-go`. The `release` workflow:
   - tests;
   - builds `webhook-go_<version>_linux_{amd64,arm64}.{deb,rpm,tar.gz}`;
   - writes `checksums.txt` and signs it into `checksums.txt.sigstore.json`;
   - writes an `.sbom.json` per artifact;
   - pushes and signs `ghcr.io/puppet-stagehand/webhook-go:<version>`;
   - creates the GitHub Release;
   - verifies the signature;
   - attests provenance.
5. Verify from a clean directory (next section), then update the pinned URL and sha256 in the
   `stagehand-module` guide (`docs/stagehand-webhook.md`) and in any Hiera that pins the old release.

A failed release is not a forgotten step. If the workflow fails after the GitHub Release was created, delete
the release and the tag before retrying (`gh release delete vX.Y.Z-stagehand.N --cleanup-tag`). Fix, then
re-tag the same name only if nothing was published; otherwise bump `N`.

## Verify a release

```sh
TAG=vX.Y.Z-stagehand.N
gh release download "$TAG" --repo puppet-stagehand/webhook-go \
  --pattern 'checksums.txt*' --pattern '*.deb' --pattern '*.rpm'
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/puppet-stagehand/webhook-go/.github/workflows/release.yml@refs/tags/${TAG}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum -c --ignore-missing checksums.txt
gh attestation verify webhook-go_*_linux_amd64.deb --repo puppet-stagehand/webhook-go
cosign verify ghcr.io/puppet-stagehand/webhook-go:${TAG#v} \
  --certificate-identity "https://github.com/puppet-stagehand/webhook-go/.github/workflows/release.yml@refs/tags/${TAG}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Always use the exact `--certificate-identity`, never a regexp over the whole repository. A regexp such as
`^https://github.com/puppet-stagehand/webhook-go/` would also accept a signature from any other workflow or
branch in the fork. The release workflow's own verify step uses this exact identity, so keep the docs in step
with it.

Only `release.yml` may have `id-token: write`. A second workflow with it could mint a signature for its own
identity, and an inexact verify command would accept that. Review any workflow change that adds it.

## Package versions

nfpm turns the tag's pre-release part into a tilde: `v2.15.1-stagehand.1` becomes package version
`2.15.1~stagehand.1`. Tildes sort lower, so:
- `2.15.1~stagehand.1` is below upstream `2.15.1`;
- `2.15.1~stagehand.2` is above `2.15.1~stagehand.1`;
- `2.16.0~stagehand.1` is above both.

`stagehand::webhook` installs from the pinned file, so the upstream comparison never matters in practice.

## Dropped build targets (RL-02)

Upstream also builds Windows amd64, FreeBSD amd64/arm64 and Linux arm v6/v7, as archives and a Docker image.
The fork builds Linux amd64 and arm64 only, because those are the module's platforms and the deploy API needs
Unix. To add a target back, edit `builds:` in `.goreleaser.yml` (`goos`, `goarch` and, for arm, `goarm`), add
an `ignore:` for pairs that should not build, and run the snapshot build. Every new target adds artifacts to
`checksums.txt`, so it is signed with no other change. Windows builds compile, but every deploy fails closed.

## Upstream watch (RL-04)

`.github/workflows/upstream-watch.yml` runs every Monday, and on demand from the Actions tab. It compares
upstream's latest release with the `upstream-base` line above. If upstream is newer and no issue with that
title is open, it opens "Upstream webhook-go vA.B.C released". Triage that issue within a week:
- read the release notes;
- if they contain a security fix, rebase and release now;
- otherwise rebase within the month.

Dependabot-style CVEs in modules the fork does not use (chatops, VCS SDKs) are accepted risk AR-57.6-10.
The watch plus this procedure bound how long they stay.

## Repository settings (recommended)

These are applied by an org admin, not by the workflows:
- `stagehand` is the default branch;
- branch protection on `stagehand` (pull request plus green `Go` check, no force-push except during a rebase);
- a tag ruleset that limits who may create `v*-stagehand.*` tags.

The release workflow's ancestry check proves a tag is on `stagehand`, not that the commit was reviewed. The
branch protection and tag ruleset close that gap.
