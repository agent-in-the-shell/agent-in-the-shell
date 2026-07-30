# Agent in the Shell

> One binary per job. Simple, fast, minimal attack surface. Pick the pieces you
> need and combine them.

Agent in the Shell is a family of small, self-contained Go services for building
and running agents. Each service is a single static binary — no runtime, no
dependency hell — and does one job well.

## Services

### `agent-model`

OpenAI-compatible LLM gateway — multi-provider routing, fallback, cost tracking

## Install

Each service is available three ways.

### Direct (`go install`)

```sh
go install github.com/agent-in-the-shell/agent-in-the-shell/cmd/agent-model@latest
```

Prebuilt binaries for linux/darwin × amd64/arm64 are attached to every
[GitHub Release](https://github.com/agent-in-the-shell/agent-in-the-shell/releases).

### Docker

```sh
docker run --rm ghcr.io/agent-in-the-shell/agent-model:latest --help
```

### Homebrew

```sh
brew install agent-in-the-shell/tap/agent-model

```

## License

MIT — see [LICENSE](LICENSE).

---

_This repository is generated from a private monorepo by `release-export`. It is
a read-only mirror: issues are welcome, but pull requests are not accepted here._
