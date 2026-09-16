# Agent in the Shell

Small Go tools for model access and local coding agents.

- **agent-model** is an OpenAI-compatible gateway with multi-provider routing,
  fallback, cost accounting, virtual keys and an optional employee portal.
- **agent-shell** runs the coding-agent CLIs already installed on your machine
  through one interface, with ordered fallback and normalized results.
  It works independently of agent-model and does not provide a sandbox.

## Build and try this revision

Use the Go toolchain declared in [go.mod](go.mod).

```sh
git clone https://github.com/agent-in-the-shell/agent-in-the-shell.git
cd agent-in-the-shell
go build -o build/ ./cmd/agent-model ./cmd/agent-shell
./build/agent-model --help
./build/agent-shell --help
```

These instructions describe the checked-out source. Published releases may
not yet contain every command documented here.

### Model gateway

The [minimal configuration](examples/agent-model.yaml) configures one
OpenAI-compatible upstream. Set the upstream model to one your account can use.

```sh
cp examples/agent-model.yaml config.yaml
export AGENT_MODEL_TOKEN=$(openssl rand -hex 32)
export OPENAI_API_KEY='YOUR_PROVIDER_API_KEY'
./build/agent-model migrate --config config.yaml
./build/agent-model serve --config config.yaml
```

In a second terminal, set the same gateway token and send a request:

```sh
export AGENT_MODEL_TOKEN='THE_GATEWAY_TOKEN_FROM_THE_FIRST_TERMINAL'
curl --fail-with-body http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $AGENT_MODEL_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"model":"default","messages":[{"role":"user","content":"Hello"}]}'
```

This request uses your provider account and may incur charges. See the
[gateway reference](docs/agentmodel.md) and
[complete configuration example](cmd/agent-model/example_config.yaml) for
routing, authentication, persistence and the optional portal.

### Coding-agent CLI

Install and authenticate at least one supported vendor CLI, then:

```sh
./build/agent-shell doctor
./build/agent-shell submit --agent claude --dry-run "summarize this repo"
./build/agent-shell submit --agent claude "summarize this repo"
```

The final command invokes the installed agent using its own credentials and
permissions. See [agent-shell usage and security model](docs/agentshell.md).

## Releases

[GitHub Releases](https://github.com/agent-in-the-shell/agent-in-the-shell/releases)
list the binaries available for each version. Check the release assets before
choosing a service; agent-shell is new in this source revision.

The release configuration builds Linux/macOS archives for amd64/arm64 and
Homebrew formulae for both services. Only agent-model has a container image,
because agent-shell needs the vendor executables installed on its host.

```sh
go install github.com/agent-in-the-shell/agent-in-the-shell/cmd/agent-model@latest
docker run --rm ghcr.io/agent-in-the-shell/agent-model:latest --help
brew install agent-in-the-shell/tap/agent-model
```

Agent-facing usage documents live in [skills/agentmodel](skills/agentmodel/SKILL.md)
and [skills/agentshell](skills/agentshell/SKILL.md) and ship with new release archives.

## Development

Issues and pull requests are welcome. This repository is the development home;
see [CONTRIBUTING.md](CONTRIBUTING.md), [migration notes](docs/migration.md),
and the [release checklist](docs/ops/release-checklist.md).

## License

[MIT](LICENSE). Existing copyright and contributor attribution are retained.
