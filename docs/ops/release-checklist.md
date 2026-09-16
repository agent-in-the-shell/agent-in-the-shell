# Release checklist

This repository owns its source and release configuration.

## Before tagging

- Review the proposed diff, compatibility notes and contributor attribution.
- Verify no credentials, internal data or unrelated files enter the tree.
- Run gofmt, build, race tests, staticcheck, govulncheck and portal UI tests.
- Review dependency licenses and retain required notices in distributed artifacts.
- Run actionlint and validate the GoReleaser configuration.
- Review configuration defaults, authentication boundaries and database upgrades.
- Confirm the README and service documents match the release's available commands.
- Configure private vulnerability reporting, required PR checks and scoped
  HOMEBREW_TAP_GITHUB_TOKEN access to the public tap.

## Release

Tag the reviewed commit with the intended v-prefixed version. The workflow
runs shared CI on that commit, then GoReleaser, then install smoke tests.
A failed or cancelled CI blocks publishing.

GoReleaser publishes agent-model and agent-shell archives for Linux/macOS,
amd64/arm64; a container for agent-model; and both Homebrew formulae.
Tests receive read-only permissions, while publishing holds the required
contents/packages write permissions.

## After publishing

- Verify anonymous archive downloads and checksums.
- Verify go install at the release tag and Homebrew installation.
- Verify the agent-model image on both supported Linux architectures.
- Test a gateway request against a controlled upstream and an agent-shell run
  against a controlled local executable.
- Check the automatic smoke workflow. It runs after publication, so a failure
  means artifacts need investigation; it does not undo the release.
- Publish release notes describing migrations and known limitations.
