# branchDAM Pairing Ansible Playbook (reference)

A reference playbook for fully-automated Companion Pairing deployment
of a [branchDAM workstation
agent](https://github.com/s3ntin3l8/branchdam-agent). Mirrors the
"one-shot operator setup, ongoing Ansible-only flow" pattern that
`kubeadm init`'s bootstrap-token uses: a single human step at install
time mints the credentials the playbook needs; every subsequent run is
unattended.

**Companion Pairing** is branchDAM's per-device API-key mechanism
(issue #396 on the server, `docs/mobile.md` §4 on the server). Each
paired workstation gets a unique `agent_id` (`dev-xxxxxxxx`) and a
plaintext key minted server-side. The agent signs requests with the
key directly; the server validates against a per-device HMAC derived
from the same key (issue #453 PR C). Replacing the previous
server-wide `BRANCHDAM_AGENT_API_KEY` shared-secret path.

This playbook does NOT touch the env-var shared-secret path. The
workstation agent (`branchdam-agent` v1.12+) pairs via a
`branchdam://?server=...&key=...&agent=...` URL emitted by the
server's Companion Pairing modal (see also
`branchdam-agent pair <url>` -- the agent-side CLI for the same flow,
added in `s3ntin3l8/branchdam-agent#243`).

## Operator one-shot setup

Three steps the operator runs by hand, exactly once per deployment:

1. **Pair via the SPA**: Settings -> Companion Pairing -> "Pair new
   device". The modal exposes three things:

   - A QR code (the mobile companion app scans this).
   - A plaintext API key, shown once. **Copy this.**
   - A `branchdam://` URL containing the URL-encoded
     `<server>`, `<key>`, and `<agent_id>` fields. **Copy this.**

2. **Pick one workstation per pair**: identify the workstation you
   want to register. A given pair is bound to one workstation's
   `agentId` config (the agent uses that id as `body.AgentID` on every
   request).

3. **Store the URL in Ansible Vault**: `ansible-vault edit
   host_vars/<workstation>/vault.yml` and add:

   ```yaml
   vault_branchdam_pair_url: "branchdam://?server=https%3A%2F%2Fdam.example.com&key=<plaintext>&agent=dev-xxxxxxxx"
   ```

   The plaintext key is the only secret in the URL -- keep the vault
   file mode 0600, encrypt with a strong vault password, and treat the
   file like any other credentials file. (The branchDAM server
   treats this plaintext as a "show once" secret; the operator has
   already accepted that exposure.)

## Ongoing Ansible-only flow

The playbook runs on each workstation and:

1. Validates the `vault_branchdam_pair_url` by parsing the URL into
   its three required fields (`server`, `key`, `agent_id`); fails
   closed on a malformed or missing key.
2. Reaches the server with a one-shot `Hello()` to confirm the key is
   still active (revoked, rotated, or wiped server-side since the last
   pair would surface as a 401).
3. Drops the three fields into `~/.config/branchdam-agent/config.yaml`
   via `branchdam-agent pair <url>` (the agent-side CLI), which writes
   the file atomically at mode 0600 and refuses a pre-existing
   group/world-readable config.
4. Restarts any running `branchdam-agent` tray/ingest service so the
   new credentials are picked up (the agent reads config at startup).

After step 1, the operator never touches the workstation again --
key rotation on the server is handled by the SPA: rotate the
pairing, get a new URL, paste into the vault, re-run the playbook.

## What's NOT in this playbook

- **Admin PATs / unattended pairing mint** -- tracked as issue #453
  PR E. Once that ships, the operator-side setup shrinks to "drop a
  PAT into the vault" and the playbook mints pairings itself; this
  reference playbook's flow stays the same downstream.
- **Server-side installation** -- this playbook only handles the
  workstation agent side. The branchDAM server itself installs via
  the standard server-deployment playbook (out of scope here).
- **PathMappings / archiveRoot / localEditRoot** -- those are
  workstation-side settings the operator configures separately,
  not part of the pairing flow.

## Files

```
tools/ansible-playbooks/branchdam-pairing/
  playbook.yml                          # the entry point
  templates/
    agent-config.yaml.j2                 # rendered into ~/.config/branchdam-agent/config.yaml
    branchdam-server.hints.yaml.j2       # optional operator-side hints file
  README.md                             # this file
```

The playbook is reference material, not a vendored dependency --
copy it into your own Ansible repo's `playbooks/` tree and customize
the variable names / vault paths / service-restart handler for your
site. The shape (vault URL -> pair CLI -> service restart) is the
load-bearing part.
