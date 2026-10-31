# Security Policy

npm-switchyard reads the Docker API, accepts authenticated connections from partner hosts, answers DNS
queries and forwards TCP and UDP traffic between hosts. That makes it security-sensitive by nature, and
reports are taken seriously.

## Reporting a vulnerability

**Please report vulnerabilities privately; do not open a public issue, pull request, or discussion for a
security problem.**

Use **GitHub's private vulnerability reporting**:

1. Go to the repository's **Security** tab ->
   **[Report a vulnerability](https://github.com/motwok/npm-switchyard/security/advisories/new)**.
2. Describe the issue with enough detail to reproduce and assess it.

This keeps the report private between you and the maintainer and integrates with GitHub Security
Advisories for a coordinated fix and disclosure.

### What to include

- A clear description of the vulnerability and its **impact**.
- **Steps to reproduce** (or a proof of concept).
- Affected **version** (image tag or digest) and the **platforms and Docker versions** of the hosts
  involved.
- Any suggested mitigation, if you have one.

## What to expect

This is a single-maintainer project, so responses are best-effort rather than bound to a fixed SLA. You
can expect an acknowledgement of your report, an assessment, and, for confirmed issues, a coordinated fix
and disclosure. Credit is given to reporters who want it.

Please give a reasonable amount of time for a fix before any public disclosure.

## Supported versions

Only the **latest release** (`:latest`) receives security fixes. Older releases are not patched; please
update to the newest one.

| Version | Supported |
|---|---|
| Latest release (`:latest`) | yes |
| Older releases | no |

## Automatic dependency releases

Dependency updates are merged automatically by Dependabot once the build and the smoke test pass, and
**every such merge that changes the image is a release**. Security fixes therefore reach `:latest`
quickly, but a vulnerable or compromised dependency would be shipped automatically as well. If you want
to review updates first, pin a version tag instead of `:latest`. Details and mitigations:
[RELEASING.md](RELEASING.md#dependency-updates-are-released-automatically).

## Scope

npm-switchyard deliberately does things that can look alarming but are by design:

- it mounts the **Docker socket** (Linux) or the **Docker named pipe** (Windows) and uses the Docker API
  read-only to find the containers that share a network with it,
- on Linux it runs with **`NET_ADMIN`** added, to add address aliases to its own interface,
- it runs a **DNS server** for names of the form `<container>.<port>.<partner>.switchyard`,
- it **listens on ports on demand** and forwards TCP and UDP traffic to partner hosts,
- it accepts **connections from configured partners** and forwards them to containers that share a
  network with it (its import network excluded),
- `/metrics` exposes **partner names and the container, compose service and stack names of targets**.
  Do not publish its port to untrusted networks.

These documented behaviors are **not** vulnerabilities on their own. Genuine security issues include, for
example: forwarding to any target that does not share a network with the agent on the exporting host
(including targets reached by resolving a name via DNS); accepting connections from an unauthenticated
or unconfigured partner; re-exporting names imported from another partner; any write or control call
against the Docker API; execution of untrusted or attacker-controlled code; or disclosure of data beyond
what is documented for `/metrics`.
