# Security Policy

## Reporting a vulnerability

Please report security vulnerabilities **privately**. Do not open a public issue
for a suspected vulnerability.

- Use GitHub's [private vulnerability reporting](https://github.com/agent-in-the-shell/agent-in-the-shell/security/advisories/new).

We aim to acknowledge reports within 3 business days and to provide a remediation
timeline after triage.

## Supported versions

Only the latest released version of each service receives security fixes.

## Scope

Each service is a single static Go binary with a deliberately small attack
surface. Dependencies are audited with `govulncheck` in CI on every push, and
the public build is verified to have zero private dependencies.
