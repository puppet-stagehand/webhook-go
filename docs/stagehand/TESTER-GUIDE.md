# webhook-go, Stagehand fork: Tester guide

Manual checks for a lab Puppet primary running the fork in mTLS mode. Run them after installing a new release
or changing the config. Each step lists the expected result; anything else is a defect, so record it as an
issue on https://github.com/puppet-stagehand/webhook-go/issues.

The fork's automated suite (`go test ./...`, run in CI on every push to `stagehand`) already covers these
behaviours against fakes. These steps prove them on a real primary, with real r10k, git and Puppet CA.

## Before you start

You need:
- a lab primary with the fork installed. The `stagehand::webhook` class does it; see the module's
  `docs/stagehand-webhook.md`.
- a control repository with a `production` branch.
- a client certificate signed by the lab CA for an allowed certname (`stagehand-code-manager` below). Create
  it on the primary with `puppetserver ca generate --certnames stagehand-code-manager`.

Set these in a shell on the machine you test from:

```sh
URL=https://<primary fqdn>:8088
CA=/etc/puppetlabs/puppet/ssl/certs/ca.pem
CERT=/etc/puppetlabs/puppet/ssl/certs/stagehand-code-manager.pem
KEY=/etc/puppetlabs/puppet/ssl/private_keys/stagehand-code-manager.pem
SHA=$(git ls-remote <control repo url> refs/heads/production | cut -f1)
```

Check the service is up: `systemctl is-active webhook-go` prints `active`, and
`webhook-go --version` prints `webhook-go version 2.15.1-stagehand.1` (or the release under test).

## 1. A client without a certificate is refused

```sh
curl -sS --cacert $CA $URL/health
```

**Expected:** curl fails with a TLS error (for example `alert certificate required` or `handshake failure`).
No HTTP status is printed.

## 2. A certificate whose certname is not on the list is refused

Generate a second certificate (`puppetserver ca generate --certnames not-allowed`) and use it:

```sh
curl -sS --cacert $CA --cert <not-allowed cert> --key <not-allowed key> $URL/health
```

**Expected:** a TLS handshake error, no HTTP status. With `$CERT`/`$KEY` instead, `/health` answers 200.

## 3. A revoked certificate is refused after the CRL refresh

1. Confirm `$CERT` works (step 2's last line).
2. Revoke it: `puppetserver ca revoke --certname stagehand-code-manager`.
3. Wait 65 seconds. The service re-reads the CRL at most once a minute.
4. Repeat the `/health` call with `$CERT`.

**Expected:** a TLS handshake error. This assumes `server.tls.crl` is the CA's
`/etc/puppetlabs/puppetserver/ca/ca_crl.pem`, the default on the CA host. With the agent copy, run
`puppet agent -t` first.

Afterwards, clean up and issue a new certificate:
- `puppetserver ca clean --certname stagehand-code-manager`;
- then generate the certificate again.

If `allowed_fingerprints` is set, update it to the new certificate's fingerprint.

## 4. A deploy of a real environment returns the commit the branch points at

```sh
curl -sS --cacert $CA --cert $CERT --key $KEY -X POST $URL/api/v1/deploys \
  -H 'Content-Type: application/json' \
  -d "{\"environments\":[{\"name\":\"production\",\"expected_sha\":\"$SHA\"}],\"wait\":true}"
```

**Expected:** HTTP 200, and the body shows:
- `"state":"deployed"`;
- `results[0].deployed_sha` equal to `$SHA`;
- `marker_present: true`;
- `marker_deploy_success: true`;
- `error: ""`.

On the primary, `/etc/puppetlabs/code/environments/production/.r10k-deploy.json` has `signature` equal to
`$SHA`.

## 5. A wrong expected SHA returns failed

Repeat step 4 with `expected_sha` set to a different 40-hex commit (for example the branch's previous commit,
`git rev-parse production~1` in a clone).

**Expected:** HTTP 200 (the deploy ran) with `"state":"failed"` and
`results[0].error` reading `deployed <the branch's real SHA>, expected <the SHA you sent>`.

## 6. Two deploys at once run one after the other

Start two deploys within a second of each other with `"wait":false`:

```sh
for i in 1 2; do
  curl -sS --cacert $CA --cert $CERT --key $KEY -X POST $URL/api/v1/deploys \
    -H 'Content-Type: application/json' \
    -d "{\"environments\":[{\"name\":\"production\",\"expected_sha\":\"$SHA\"}],\"wait\":false}" &
done; wait
```

**Expected:** both answer 202 with an `id`. Poll each with
`curl ... $URL/api/v1/deploys/<id>` until both are `deployed`. The second job's `results[0].started_at` is
**at or after** the first job's `results[0].finished_at`; the runs never overlap.

## 7. A malformed request never reaches the engine

Send `"name":"prod;rm"` or a 39-character SHA.

**Expected:** HTTP 422 with `field` naming the bad field. `GET /api/v1/deploys?limit=1` shows no new job for it.

## 8. A restart mid-deploy is reported honestly

1. Start a deploy with `"wait":false` against a large environment.
2. While it reads `deploying`, run `systemctl restart webhook-go`.
3. Poll the job.

**Expected:** `state: failed`. The error reads `engine stopped: the deploy service is shutting down` (a clean
stop), or `interrupted by a restart` (if the process was killed).

## Recording results

Note the release tag, the primary's OS and the r10k version, plus pass or fail for each step, in the phase's UAT
record. Any step you cannot run goes in the console repo's `.planning/HUMAN-TESTS-DEFERRED.md` with the reason.
