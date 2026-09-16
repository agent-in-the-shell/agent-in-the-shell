# AgentModel

An OpenAI-compatible gateway with multi-provider routing, fallback, cost
accounting, virtual keys and an optional employee portal.

Use the [project quickstart](../../README.md#model-gateway),
[service reference](../../docs/agentmodel.md), and
[configuration example](../../cmd/agent-model/example_config.yaml).

Provider implementations live under provider/, shared HTTP types under wire/,
and the standalone Go HTTP client under gateway/.
