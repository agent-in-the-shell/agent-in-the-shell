# Design principles

## Focused executables, explicit collaboration contracts

Use executables with clear responsibilities as the units of composition, and
connect them through explicit contracts. Independent deployment does not prevent
collaboration, nor does collaboration require every capability to live in one
process.

The goal is not to maximize the number of binaries or minimize each binary's
size. It is to keep boundaries clear, contracts stable, and dependencies explicit.

### Multiple binaries can form one system

Terraform illustrates this distinction: its CLI is the core process, while
providers typically run as separate executables. The core manages their startup
and calls them through a plugin protocol over RPC. Separate binaries cooperate
as one system because their roles and communication contract are defined.

This is an architectural example, not a requirement to adopt Terraform's plugin
protocol or build a plugin framework here. Choose a CLI, an HTTP API, or a plugin
protocol according to the interaction the system actually needs.

### Boundaries in this repository

- **agent-shell and vendor CLIs:** agent-shell launches installed external tools.
  Adapters normalize arguments, results, and errors; the vendor CLI retains its
  own runtime requirements, credentials, and permission behavior.
- **agent-shell and agent-model:** these are independent capabilities, not an
  implicit core/plugin pair. The shell does not require or link the gateway.
  A workflow may compose their capabilities without making either service a
  mandatory dependency of the other.

### Review criteria

- Split a binary when it establishes a useful responsibility or deployment
  boundary, not merely to make individual binaries smaller.
- Document both build-time and runtime dependencies. A static binary may still
  invoke external executables or call external services.
- Define the contract at each collaboration boundary: inputs, outputs, errors,
  compatibility expectations, and ownership of process startup and shutdown
  where applicable.
- Keep cross-service integration at those boundaries instead of importing
  another service's implementation details.
- Introduce abstractions and orchestration only for concrete needs; multiple
  binaries alone do not justify a generic plugin framework.
- Do not confuse process separation with security isolation. In particular,
  agent-shell runs vendor CLIs with the caller's host permissions and environment;
  it is not a sandbox.

## Public transition: a clean compatibility baseline

The transition to the public development repository establishes a new baseline.
Backward compatibility with pre-transition versions is not a design constraint,
including versions already published by the previous export workflow. Choose the
intended public design rather than preserving legacy behavior to ease upgrades.

- CLI commands, API shapes, configuration keys, environment variables, file
  paths, and persisted formats may change without compatibility shims.
- Do not retain deprecated aliases, dual-format readers, legacy path fallbacks,
  or automatic startup migrations solely to support pre-transition versions.
- If existing files need conversion, provide at most a standalone, explicitly
  invoked conversion script. Keep legacy-format knowledge out of the normal
  application runtime; do not build an ongoing migration framework for this
  transition.
- A conversion script must preserve the source files by default, validate its
  input, and refuse to overwrite an existing destination unless explicitly
  requested. Document its supported input and output formats and how to run it.
- Document breaking changes and the new setup clearly. Removing compatibility
  requirements does not authorize silently deleting or overwriting user data.

This exception applies to the public transition, not every subsequent release.
Compatibility expectations between releases on the new public baseline must be
defined separately. The stable-contract principle above applies to that new
baseline; it does not require carrying forward the old one.
