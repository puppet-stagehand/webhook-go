# webhook-go, Stagehand fork: User guide

This guide is for anyone who runs Puppet and wants Stagehand to deploy Puppet code to a primary server. You
do not need to know r10k, mTLS or Go to follow it. Words in **bold** the first time they appear are explained
in the glossary at the end.

## The short version

1. Puppet code lives in a git repository, the **control repository**. Each branch is an **environment**.
2. Something has to copy a branch onto the Puppet primary before Puppet can use it. That something is a
   **deploy engine**: **r10k** or **g10k**.
3. This service runs on the primary and starts the deploy engine when Stagehand asks it to. It then checks
   the engine's work and says, honestly, whether the environment now holds the commit Stagehand asked for.
4. Only Stagehand can ask, because the service only talks to clients that hold a specific Puppet
   certificate.

## What r10k and g10k do

Think of the control repository as a recipe book and the primary as a kitchen. r10k (Ruby) and g10k (Go)
are the two delivery drivers. You tell a driver "bring me the `production` branch". The driver:

1. fetches that branch;
2. fetches every module its `Puppetfile` lists;
3. puts them all in `/etc/puppetlabs/code/environments/production`;
4. writes a small receipt file in that directory: `.r10k-deploy.json` (r10k) or `.g10k-deploy.json` (g10k).
   The receipt, the **deploy marker**, says which commit was delivered and whether it worked.

## What this service adds

Upstream webhook-go waits for a git host (GitHub, GitLab and so on) to call it, then starts r10k. It
believes r10k's exit code. Exit codes can lie: g10k exits 0 even when the branch does not exist. This fork
adds a mode for Stagehand with three differences.

- **Who may call it.** Only a client with a certificate from your Puppet CA, whose name is on an allow-list.
  This is **mTLS**: both sides show a certificate. A revoked certificate is refused. You can also pin the
  exact certificate.
- **What "deployed" means.** An environment is `deployed` only if all four hold:
  1. the engine exited 0;
  2. this run rewrote the deploy marker (an old receipt does not count);
  3. the marker says the deploy succeeded;
  4. the marker's commit equals the `expected_sha` the caller sent.

  Anything else is `failed`, with the reason in plain words.
- **A record of every deploy.** Each request becomes a **job** with an id. Its record is a file on disk that
  survives restarts, and Stagehand reads it back.

## How to install it

The easy way is the Puppet class `stagehand::webhook` from the `stagehand-module`. It installs git and r10k,
installs this package from a pinned URL and sha256, writes the config below, adds a systemd drop-in and
starts the service. Its own guide is `docs/stagehand-webhook.md` in that module. It gives the exact release
URLs, sha256 values and the one-time signature check.

By hand:

1. Download the `.deb` or `.rpm` for your CPU and `checksums.txt` plus `checksums.txt.sigstore.json` from the
   release page, https://github.com/puppet-stagehand/webhook-go/releases.
2. Check the signature once (it needs `cosign` 2.4 or later; 3.x recommended):

   ```sh
   TAG=v2.15.1-stagehand.1
   cosign verify-blob --bundle checksums.txt.sigstore.json \
     --certificate-identity "https://github.com/puppet-stagehand/webhook-go/.github/workflows/release.yml@refs/tags/${TAG}" \
     --certificate-oidc-issuer https://token.actions.githubusercontent.com \
     checksums.txt
   sha256sum -c --ignore-missing checksums.txt
   ```

   `Verified OK` and one `OK` line per downloaded file mean the files are exactly what the fork's release
   workflow built for that tag. Anything else: stop, and do not install.
3. Install: `dpkg -i webhook-go_2.15.1-stagehand.1_linux_amd64.deb` or
   `rpm -Uvh webhook-go_2.15.1-stagehand.1_linux_amd64.rpm`.
4. Edit `/etc/voxpupuli/webhook.yml` (the commented Stagehand block at its end), add the systemd drop-in from
   "Stopping and restarting", then run `systemctl enable --now webhook-go`.

The package installs `/usr/bin/webhook-go`, `/etc/voxpupuli/webhook.yml` (your edits survive upgrades) and
`/etc/systemd/system/webhook-go.service`. Its version reads `2.15.1~stagehand.1`. Package managers turn the
`-` into `~`, which sorts the fork's releases in the right order.

**Linux only.** The deploy API needs file locks and process groups, so releases are built for Linux amd64 and
arm64 only.

## Settings

All settings live in `/etc/voxpupuli/webhook.yml`. The ones below are the fork's. Upstream's settings (for
example `r10k.config_path`, `r10k.verbose`, `chatops.*`) work as upstream documents them.

| Setting | Default | What it does |
|---------|---------|--------------|
| `server.port` | `4000` | The port to listen on. `stagehand::webhook` uses `8088`. |
| `server.tls.client_ca` | `""` (off) | The Puppet CA bundle, usually `/etc/puppetlabs/puppet/ssl/certs/ca.pem`. **Setting it switches on mTLS-only mode.** |
| `server.tls.allowed_certnames` | `[]` | The certificate names allowed to call the API, for example `stagehand-code-manager`. Required (at least one, no blanks) in mTLS mode. |
| `server.tls.certificate` | `""` | This server's own certificate. Use the host's Puppet certificate. Required in mTLS mode. |
| `server.tls.key` | `""` | The key for that certificate. Required in mTLS mode. |
| `server.tls.crl` | `""` (no check) | The **certificate revocation list** (CRL). See "How revocation works". |
| `server.tls.allowed_fingerprints` | `[]` (no pin) | When set, the client certificate's SHA-256 fingerprint must also be listed. Format: 32 colon-separated hex pairs, as `puppetserver ca list --all` prints them, for example `AB:CD:...:EF`. Lower case is accepted. A malformed entry stops the service from starting. |
| `deploys.basedir` | `/etc/puppetlabs/code/environments` | Where environments and their deploy markers live. Must be absolute. |
| `deploys.state_dir` | `/var/lib/webhook-go` | Where job records (`jobs/<id>.json`, mode 0600), the deploy lock (`deploy.lock`) and one lock file per running process (`instances/`) live. Created 0700 if missing. Keep it owned by root and private. |
| `deploys.max_jobs` | `500` | How many job records to keep. The oldest finished ones beyond this are deleted. |
| `deploys.timeout` | `15m` | How long one environment may take. When it runs out, the engine and every process it started are stopped. |
| `deploys.output_limit` | `262144` | How many bytes of engine output to keep per environment (the end is kept). |
| `r10k.command_path` | `/opt/puppetlabs/puppetserver/bin/r10k` | The engine binary. **On Puppet Core use `/opt/puppetlabs/puppet/bin/r10k`.** With g10k, the g10k binary. |
| `r10k.use_g10k_commands` | `false` | `true` uses g10k instead of r10k. |

The service refuses to start, with a message naming the setting, if:
- `client_ca` is set but `allowed_certnames` is empty;
- the certificate or key is missing;
- the CRL does not parse or was not signed by a CA in `client_ca`;
- a fingerprint is malformed;
- a deploy setting is invalid.

In mTLS mode, upstream's git-host routes (`/api/v1/r10k/*`) are switched off. Leave `server.protected` off:
the certificate is the login.

## The API

Every call needs the allowed client certificate. Examples use `curl` with the client's Puppet certificate;
`stagehand-code-manager` is the usual certname.

```sh
CERT=/etc/puppetlabs/puppet/ssl/certs/stagehand-code-manager.pem
KEY=/etc/puppetlabs/puppet/ssl/private_keys/stagehand-code-manager.pem
CA=/etc/puppetlabs/puppet/ssl/certs/ca.pem
URL=https://primary.example.com:8088

curl --cert $CERT --key $KEY --cacert $CA -X POST $URL/api/v1/deploys \
  -H 'Content-Type: application/json' \
  -d '{"environments":[{"name":"production","expected_sha":"0123456789abcdef0123456789abcdef01234567"}],"wait":false}'
```

**Request** (`POST /api/v1/deploys`, at most 64 KiB):

- `environments`: 1 to 20 entries, no duplicates. `name` is lower-case letters, digits and `_`.
  `expected_sha` is the full 40-character lower-case commit the branch should be at. It is required, so
  there is no "deploy whatever is there" mode.
- `wait`: required.
  - `false` answers `202 Accepted` at once with the queued job and a `Location` header; you then poll
    `GET /api/v1/deploys/<id>`.
  - `true` answers `200` when the deploy has finished.

**Result** (`GET /api/v1/deploys/<id>`):

```json
{
  "id": "9f2c4d1e8a7b4c3d2e1f0a9b8c7d6e5f",
  "engine": "r10k",
  "state": "deployed",
  "created_at": "2026-10-13T20:18:00.1Z",
  "started_at": "2026-10-13T20:18:00.1Z",
  "finished_at": "2026-10-13T20:18:04.3Z",
  "results": [{
    "environment": "production",
    "state": "deployed",
    "exit_code": 0,
    "expected_sha": "0123456789abcdef0123456789abcdef01234567",
    "deployed_sha": "0123456789abcdef0123456789abcdef01234567",
    "marker_present": true,
    "marker_deploy_success": true,
    "duration_ms": 4120,
    "output": "INFO\t -> Deploying environment ...",
    "output_truncated": false,
    "error": ""
  }]
}
```

`GET /api/v1/deploys?limit=N` (N from 1 to 100, default 20) lists the newest jobs. Ids are 32 hex characters;
treat them as opaque. Early test builds used UUIDs.

### States

| State | Meaning |
|-------|---------|
| `queued` | Accepted; waiting for the deploy lock (another deploy is running). |
| `deploying` | The engine is running. |
| `deployed` | Every environment passed all four checks. |
| `failed` | At least one environment did not. Its `error` says why. |

Decide on `state` only, never on `exit_code`. `exit_code` is `-1` when the engine did not start.

### Errors

| `error` | What it means | What to do |
|---------|---------------|------------|
| `engine exited N: <stderr tail>` | The engine failed. The tail shows its last lines. | Read the tail: often a missing branch, an unreachable git remote or a bad module ref. |
| `no deploy marker` | No receipt was written. g10k does this, exiting 0, for a branch that does not exist. | Check the branch name. |
| `marker not rewritten by this run` | The receipt is old, so the engine did nothing. | Check the remote is reachable and the branch exists. |
| `marker reports deploy_success false` | The engine says it failed. | Read `output`. |
| `deployed <x>, expected <y>` | The environment is at a different commit than asked. | Someone pushed in between: fetch the new SHA and deploy again. |
| `engine timed out after 15m0s` | The run took longer than `deploys.timeout` and was stopped. | Look for a hung git remote; raise the timeout if the environment is huge. |
| `engine did not start: ...` | The engine binary is missing or not executable. | Check `r10k.command_path`. |
| `interrupted by a restart` | The service restarted while the job was in flight. | Deploy again. The environment may be half-updated. |
| `not run: the deploy service is shutting down` / `engine stopped: ...` | The service was stopping. | Deploy again once it is back. |
| `not run: deploy lock ...` | The lock could not be taken. | Check `state_dir` permissions. |

HTTP answers:
- `422` names the bad field in the request.
- `400` means the JSON is malformed.
- `404` means an unknown id, or one pruned beyond `max_jobs`.
- `503` with `Retry-After: 30` means the service is shutting down or 100 jobs are already waiting; that job's
  record reads `failed`.

A revoked, unlisted or unpinned certificate gets no HTTP answer at all. The TLS handshake fails.

## Why it runs as root

r10k and g10k write into `/etc/puppetlabs/code/environments` and into Puppet's caches, and r10k may reach git
over SSH with root's key. The packaged unit has no `User=`, so it runs as root, like Code Manager's deploys on
PE. In return, the attack surface is kept small:
- only mTLS clients from your CA on the allow-list get in;
- engines are started with an argument list, never a shell;
- environment names and SHAs are checked twice;
- state files are 0600 in a 0700 directory.

## How revocation works

When you revoke a certificate (`puppetserver ca revoke --certname <name>`), the CA adds it to its CRL. This
service re-reads the CRL file within a minute of the file changing, and from then on refuses that certificate,
including on resumed TLS sessions. If a new CRL file does not parse, the last good list is kept and an error
is logged.

Which file to point `server.tls.crl` at:
- **On the CA host:** `/etc/puppetlabs/puppetserver/ca/ca_crl.pem`. A revoke takes effect within a minute.
- **Elsewhere:** `/etc/puppetlabs/puppet/ssl/crl.pem`, the agent's copy. It is only refreshed when the Puppet
  agent runs, so a revoke can take up to one agent run interval (30 minutes by default) to reach this service.

`stagehand::webhook` picks the right file for you.

## Stopping and restarting

Use `systemctl stop|restart webhook-go`. On SIGTERM the service stops a running engine with every process it
started, fails queued jobs and writes their records before exiting (up to 30 seconds). Two upstream unit-file
settings work against that, so add this drop-in (`stagehand::webhook` does it for you):

```ini
# /etc/systemd/system/webhook-go.service.d/50-stagehand.conf
[Service]
KillMode=mixed
TimeoutStopSec=60
ExecReload=
```

Then run `systemctl daemon-reload`. Never `systemctl reload` or send SIGHUP: Go ends the process without the
clean shutdown, and the next start fails the job as `interrupted by a restart`. To change settings, edit the
file and restart.

## Glossary

| Word | Meaning |
|------|---------|
| control repository | The git repository holding your Puppet code; each branch is an environment. |
| environment | One branch of the control repository, deployed to its own directory on the primary. |
| deploy engine | The tool that copies a branch and its modules onto the primary: r10k or g10k. |
| r10k / g10k | Two deploy engines. r10k is Puppet's Ruby tool; g10k is a faster Go reimplementation. |
| deploy marker | The receipt file the engine writes in an environment: `.r10k-deploy.json` or `.g10k-deploy.json`. |
| mTLS | Mutual TLS: both the client and the server prove who they are with certificates. |
| certname | The name inside a Puppet certificate (its Subject CN), for example `stagehand-code-manager`. |
| CRL | Certificate revocation list: the CA's list of certificates that must no longer be trusted. |
| fingerprint | A SHA-256 hash of one exact certificate, used to pin it. |
| job | One deploy request with an id; its record says how each environment went. |
| cosign | A tool that signs files and checks signatures. Here it proves a release was built by this fork's release workflow. |
| sha256 | A 64-character fingerprint of a file. If even one byte changes, so does the sha256. |
