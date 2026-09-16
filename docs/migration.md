# Migration from v0.1.1

This source revision adds agent-shell and expands agent-model. Consult the
release notes and assets for the version you install.

## CLI changes

- agent-model filter and schema have been removed. Existing JSONL filter
  pipelines need a replacement before upgrading; prompt is a single-request
  diagnostic, not a compatible JSONL filter.
- New gateway commands include prompt, limits, configure-models,
  configure-fallbacks and keys.
- agent-shell provides submit/run, install/doctor and process-registry commands.
  It executes vendor CLIs and does not require a gateway.

## Gateway and key management

The gateway adds /v1/responses and an optional employee portal. Employee keys
can use the documented inference and model-list endpoints; account, usage and
administration operations require operator authority.

The former /v1/keys/{id}/unrevoke route is removed. Use disable/enable for
reversible suspension. Revocation is permanent and retains history; delete
also revokes rather than erasing the audit trail. Service-key rotation keeps
the key ID and immediately invalidates the old secret. Legacy-key rotation
has different behavior; see the [key-management reference](agentmodel.md#runtime-virtual-keys-keys).

Back up configuration, credentials and a consistent SQLite database before
upgrading. Stop the service for a file-level backup or use SQLite's backup
facilities; copying only the main file while WAL writes are active can omit
recent data. Run migrate with the same config before starting the new binary.
Do not assume an older binary supports the migrated schema; retain the
pre-upgrade backup for rollback.

## Paths and portal configuration

Fresh installations resolve config and database paths through XDG directories
under heros/agentmodel. Existing legacy paths remain supported. Explicit db
and --config settings remain the most predictable upgrade path.

The portal is disabled by default. Set its own origin, identity issuer,
audience, email domain and administrator addresses. Set gateway_origin when
inference uses a different origin from the portal. Without it, the embedded
page uses its current origin; no organization-specific gateway or external
branding asset is embedded.
