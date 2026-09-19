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
workstation agent's `pair <url>` subcommand (`s3ntin3l8/branchdam-agent`
PR #243, landed on `main` after v1.12.1) does the heavy lifting:
parses the URL, refuses a group/world-readable existing config,
validates the credentials server-side via `POST /api/v1/agent/hello`,
and atomically writes `~/.config/branchdam-agent/config.yaml` at mode
0600. The Ansible layer is glue: vault -> pair CLI -> restart systemd
service.

## Operator one-shot setup

Three steps the operator runs by hand, exactly once per workstation:

1. **Pair via the SPA**: Settings -> Companion Pairing -> "Pair new
   device". The modal exposes three things:

   - A QR code (the mobile companion app scans this).
   - A plaintext API key, shown once. **Copy this.**
   - A `branchdam://` URL containing the URL-encoded
     `<server>`, `<key>`, and `<agent_id>` fields. **Copy this.**

   The `pair` subcommand is not in any released version yet -- it
   landed on `s3ntin3l8/branchdam-agent`'s `main` after the v1.12.1
   tag. Build from source or wait for v1.13 if you're on a released
   version.

2. **Pick the workstation this pair belongs to**. A given pair is
   bound to one workstation's Linux user account (the agent reads
   `~/.config/branchdam-agent/config.yaml` from the user that runs
   it, not from root).

3. **Store the URL in Ansible Vault**: `ansible-vault edit
   host_vars/<workstation>/vault.yml` and add:

   ```yaml
   vault_branchdam_pair_url: "branchdam://?server=https%3A%2F%2Fdam.example.com&key=<plaintext>&agent=dev-xxxxxxxx"
   branchdam_pair_agent_user: <the-linux-username-that-runs-the-agent>
   ```

   The plaintext key is the only secret in the URL -- keep the vault
   file mode 0600, encrypt with a strong vault password, and treat the
   file like any other credentials file. (The branchDAM server
   treats this plaintext as a "show once" secret; the operator has
   already accepted that exposure.)

## Ongoing Ansible-only flow

The playbook runs on each workstation and:

1. Asserts `branchdam_pair_agent_user` is set explicitly -- silently
   defaulting to root would write the credentials to `/root/.config/...`,
   which the workstation agent (running under a non-root user) never
   reads.
2. Validates the `vault_branchdam_pair_url` is set.
3. Confirms `branchdam-agent` is installed and reports a version.
4. Writes a starter config via `branchdam-agent init` (idempotent --
   skips if `config.yaml` already exists).
5. Runs `branchdam-agent pair -config <path> -timeout 10s <url>` to
   parse the URL, validate the credentials server-side, and atomically
   patch the config at mode 0600.
6. Restarts `branchdam-agent.service` if systemd is managing the
   agent on the host (configurable; set `branchdam_pair_agent_service: ""`
   to skip on hosts where the agent runs interactively).

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
  either via the agent's tray Settings UI or by hand-editing the
  config after `pair` has written its three fields. Templating the
  full config here would conflict with the pair CLI's own writes
  (and would force every secret into Ansible-managed YAML);
  leaving that to the operator matches the kubeadm-init model too
  (the bootstrap-token only carries the join secret; everything
  else is rendered by site-specific playbooks).

## Files

```
tools/ansible-playbooks/branchdam-pairing/
  playbook.yml   # the entry point (init -> pair -> restart)
  README.md      # this file
```

The playbook is reference material, not a vendored dependency --
copy it into your own Ansible repo's `playbooks/` tree and customize
the variable names / vault paths / service-restart handler for your
site. The shape (init -> pair CLI -> restart) is the load-bearing part.
