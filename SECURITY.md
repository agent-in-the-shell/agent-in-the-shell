# Security policy

## Reporting

Report suspected vulnerabilities privately through
[GitHub private vulnerability reporting](https://github.com/agent-in-the-shell/agent-in-the-shell/security/advisories/new).
Do not include credentials or private prompts in a public issue.
If private reporting is unavailable, open an issue requesting a private contact
without disclosing vulnerability details.

Maintainers triage reports and coordinate fixes and disclosure with the reporter.

## Supported versions

Only the latest released version receives security fixes.

## Boundaries

- agent-model holds provider credentials. Protect its configuration, token
  directories, audit database and optional content logs. Keep the master token
  restricted to operators. The employee portal is disabled by default and
  requires explicit identity and origin configuration.
- agent-shell runs installed vendor CLIs with the caller's host permissions and
  environment. It is not a sandbox. Prompts and agent output may be recorded in
  its local process registry.
- CI runs govulncheck before publishing. A passing dependency scan is not a
  guarantee that application behavior is free of vulnerabilities.

See [gateway security](docs/agentmodel.md) and
[agent-shell security model](docs/agentshell.md#security-model) for details.
