# ark-operator

[![GitHub release](https://img.shields.io/github/v/release/arkonis-dev/ark-operator)](https://github.com/arkonis-dev/ark-operator/releases)
[![Go Report Card](https://goreportcard.com/badge/github.com/arkonis-dev/ark-operator)](https://goreportcard.com/report/github.com/arkonis-dev/ark-operator)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](./LICENSE)
[![Lint](https://github.com/arkonis-dev/ark-operator/actions/workflows/lint.yml/badge.svg)](https://github.com/arkonis-dev/ark-operator/actions/workflows/lint.yml)
[![Tests](https://github.com/arkonis-dev/ark-operator/actions/workflows/test.yml/badge.svg)](https://github.com/arkonis-dev/ark-operator/actions/workflows/test.yml)

**Orchestrate AI agents on Kubernetes.**  Define your agent(s) (model, prompt, tools) and the operator keeps it running, scaled, and within budget. Chain agents into pipelines with `ArkFlow`. Trigger them on a schedule or webhook with `ArkEvent`.

---

## Resources

| Resource      | What it does |
| ------------- | ------------ |
| `ArkAgent`    | Pool of agent replicas backed by a model, system prompt, and optional MCP tool servers |
| `ArkService`  | Routes tasks to available agents (round-robin, least-busy, random) |
| `ArkSettings` | Reusable config shared across agents: temperature, output format, prompt fragments |
| `ArkFlow`     | DAG of agent steps where outputs feed into inputs. Supports conditionals, loops, and timeouts |
| `ArkEvent`    | Fires flows on a cron schedule or HTTP webhook, with fan-out to multiple flows |
| `ArkMemory`   | Attaches a memory backend (in-context, Redis, or vector store) to an agent |

---

## Quick start

```bash
# Install the operator
kubectl apply -f https://github.com/arkonis-dev/ark-operator/releases/latest/download/install.yaml

# Deploy Redis (task queue)
kubectl apply -f https://raw.githubusercontent.com/arkonis-dev/ark-operator/main/config/prereqs/redis.yaml

# Add your API key
kubectl create secret generic arkonis-api-keys \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-... \
  --from-literal=TASK_QUEUE_URL=redis.agent-infra.svc.cluster.local:6379
```

---

## ark CLI — run flows locally

No cluster required. Same YAML, same output.

```bash
go install github.com/arkonis-dev/ark-operator/cmd/ark@latest

ark run quickstart.yaml --provider mock --watch
ark run quickstart.yaml --provider anthropic --watch
ark validate quickstart.yaml
```

Provider is auto-detected from the model name: `claude-*` → Anthropic, `gpt-*`/`o*` → OpenAI.
Pre-built binaries available on the [releases page](https://github.com/arkonis-dev/ark-operator/releases).

---

Full documentation: **[arkonis.dev](https://arkonis.dev)**

## License

Apache 2.0 — see [LICENSE](./LICENSE)
