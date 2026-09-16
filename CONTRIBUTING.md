# Contributing

Issues and pull requests are welcome in this repository.

## Local checks

Use the Go version declared in go.mod. Tests use fake upstreams and temporary
state by default; do not enable live provider tests for routine changes.

```sh
gofmt -l .
go build ./...
go test -race ./...
go install honnef.co/go/tools/cmd/staticcheck@2025.1.1
staticcheck ./...
go install golang.org/x/vuln/cmd/govulncheck@v1.6.0
govulncheck ./...
node --test services/agentmodel/api/portal_ui_test.mjs
```

Node.js 22 or newer is needed only for the portal's dependency-free UI tests.
Go builds do not require Node.js. Run actionlint when changing workflows.

## Pull requests

Describe the problem, resulting behavior and relevant validation. Update the
service reference, configuration examples and agent-facing usage document when
changing the CLI, API or persistence contract. Keep each PR focused on one change.
Link public issues when they provide useful context; explain design decisions
in the PR so readers do not need access to another repository.

For bugs, include the service/version, OS/architecture, reproduction command,
expected behavior and actual result. Replace credentials and personal data in
logs with placeholders.

## Security reports

Report suspected vulnerabilities privately following [SECURITY.md](SECURITY.md).
