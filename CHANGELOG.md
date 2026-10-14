# Changelog: Stagehand fork

The fork's releases, newest first. Tags are `vX.Y.Z-stagehand.N`, where `X.Y.Z` is the upstream
voxpupuli/webhook-go release the fork is based on and `N` counts the fork's releases on that base.
Upstream's own history is in its GitHub releases.

## v2.15.1-stagehand.1

The first release of the fork, based on upstream v2.15.1.

### Added

- **mTLS-only mode.** Setting `server.tls.client_ca` turns it on. Client certificates must chain to the Puppet CA, and their certname must be on `server.tls.allowed_certnames`. This is checked in `VerifyConnection`, so it also applies to resumed TLS sessions.
- **`server.tls.crl`.** A revoked leaf or intermediate certificate fails the handshake. The CRL must be signed by a CA in `client_ca`, is re-read within a minute of a change, and keeps the last good list if a new file does not parse.
- **`server.tls.allowed_fingerprints`.** An optional SHA-256 certificate pin.
- **The deploy API.**
  - `POST /api/v1/deploys` takes 1-20 environments, each with a required `expected_sha`, and `wait: true|false`.
  - `wait: false` answers `202 Accepted`, and you poll `GET /api/v1/deploys/{id}`.
  - `GET /api/v1/deploys?limit=N` lists the newest jobs.
- **Honest verdicts.** For r10k (`.r10k-deploy.json`) and g10k (`.g10k-deploy.json`), `deployed` needs:
  - exit 0;
  - a marker rewritten by this run;
  - `deploy_success: true`;
  - a signature equal to `expected_sha`.

  Every other case is `failed` with a named reason.
- **Job records.** They are stored under `deploys.state_dir/jobs` (mode 0600). They survive restarts, and the newest `deploys.max_jobs` are kept. A job a dead process left unfinished fails as `interrupted by a restart`.
- **One deploy at a time,** enforced by a lock file, also across processes.
- **Bounded runs.** Each environment run is bounded by `deploys.timeout`; the engine's whole process group is killed when it expires. Output is bounded by `deploys.output_limit`.
- **Clean shutdown.** In mTLS mode, SIGTERM stops a running engine, fails queued jobs and writes their records before exiting.
- **Release supply chain.**
  - Releases are built in GitHub Actions.
  - `checksums.txt` is signed keyless with cosign (`checksums.txt.sigstore.json`).
  - There is an SPDX SBOM per archive and package, and a build provenance attestation.
  - The container image `ghcr.io/puppet-stagehand/webhook-go` is cosign-signed.

### Changed

- Published builds are Linux amd64 and arm64 only (`.deb`, `.rpm`, `.tar.gz`).
- Package vendor and maintainer name the fork. The package name `webhook-go`, the config path `/etc/voxpupuli/webhook.yml` and the unit `webhook-go.service` are unchanged.
- The package version is `2.15.1~stagehand.1`, because deb and rpm turn the tag's `-stagehand.1` into `~stagehand.1`.

### Known limits

- **The mTLS deploy API needs Linux (or another Unix).** It uses flock and process groups. Other platforms still compile, but every deploy fails closed.
- **Unchanged upstream unit file.** It still has `KillMode=process` and an `ExecReload` that does nothing. The `stagehand::webhook` Puppet class adds a systemd drop-in (`KillMode=mixed`, `TimeoutStopSec=60`, no reload). If you install the package by hand, add the same drop-in. Never send SIGHUP: it ends the process without the clean shutdown.
- **Job ids** are 32 lower-case hex characters. Treat them as opaque.
