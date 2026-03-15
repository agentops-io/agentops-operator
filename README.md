# ark-operator

[![GitHub release](https://img.shields.io/github/v/release/arkonis-dev/ark-operator)](https://github.com/arkonis-dev/ark-operator/releases)
[![Go Report Card](https://goreportcard.com/badge/github.com/arkonis-dev/ark-operator)](https://goreportcard.com/report/github.com/arkonis-dev/ark-operator)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](./LICENSE)
[![Lint](https://github.com/arkonis-dev/ark-operator/actions/workflows/lint.yml/badge.svg)](https://github.com/arkonis-dev/ark-operator/actions/workflows/lint.yml)
[![Tests](https://github.com/arkonis-dev/ark-operator/actions/workflows/test.yml/badge.svg)](https://github.com/arkonis-dev/ark-operator/actions/workflows/test.yml)

**AI agents as Kubernetes resources.** `ArkAgent` is to an LLM agent what `Deployment` is to a container: declare it, and the operator handles scheduling, scaling, health checks, and cost limits.

---

## The idea

Running agents in production means solving the same problems you already solved for services: _how many instances are running, are they healthy, how do I roll out a change, who can deploy to production?_

ark-operator doesn't reinvent that. It extends Kubernetes so your agents live alongside everything else: same GitOps pipeline, same RBAC, same `kubectl`.

```text
container image     →   model + system prompt + MCP tools
Deployment          →   ArkAgent
Service             →   ArkService
ConfigMap           →   ArkSettings
CronJob / Ingress   →   ArkEvent
(no equivalent)     →   ArkFlow  (multi-step agent pipeline)
```

---

## Resources

| Resource      | What it does                                                                                             |
| ------------- | -------------------------------------------------------------------------------------------------------- |
| `ArkAgent`    | A pool of agent replicas backed by a model, system prompt (inline or from a ConfigMap/Secret), and optional MCP tool servers |
| `ArkService`  | Routes tasks to available agent instances (round-robin, least-busy, random)                              |
| `ArkSettings` | Reusable config shared across agents: temperature, output format, prompt fragments                       |
| `ArkFlow`     | A DAG of agent steps. Outputs of one step feed into the next. Supports conditionals, loops, and timeouts |
| `ArkEvent`    | Fires flows on a schedule or HTTP webhook. One event can fan out to multiple flows in parallel           |
| `ArkMemory`   | Attaches a memory backend (in-context, Redis, or vector store) to an agent                               |

---

## Example

A research agent that fires on a webhook, runs a two-step pipeline, and caps daily spend:

System prompts can be inline or loaded from a file — useful when your instructions are long enough to live in their own file:

```yaml
# prompts/researcher.md (tracked in git, referenced by name)
apiVersion: v1
kind: ConfigMap
metadata:
  name: researcher-prompt
data:
  prompt.md: |
    You are a research agent working inside a Kubernetes cluster.
    Your job is to gather accurate, well-cited information on the topic you are given.

    Guidelines:
    - Always cite primary sources when available.
    - Prefer recent publications (last 2 years).
    - If you are uncertain, say so explicitly rather than guessing.
    - Keep responses concise: facts over prose.
---
apiVersion: arkonis.dev/v1alpha1
kind: ArkAgent
metadata:
  name: researcher
spec:
  model: claude-sonnet-4-20250514
  systemPromptRef:
    configMapKeyRef:
      name: researcher-prompt
      key: prompt.md
  limits:
    maxDailyTokens: 500000 # scales to 0 when the 24h window is exhausted
---
apiVersion: arkonis.dev/v1alpha1
kind: ArkFlow
metadata:
  name: research-pipeline-template
spec:
  steps:
    - name: research
      arkAgent: researcher
      prompt: "Research this topic: {{ .input.topic }}"
    - name: summarize
      arkAgent: researcher
      dependsOn: [research]
      prompt: "Summarize in 3 bullet points: {{ .steps.research.output }}"
  output: "{{ .steps.summarize.output }}"
---
apiVersion: arkonis.dev/v1alpha1
kind: ArkEvent
metadata:
  name: research-trigger
spec:
  type: webhook
  targets:
    - pipeline: research-pipeline-template
```

Fire it from anywhere:

```bash
# Get the webhook URL and token the operator generated
WEBHOOK_URL=$(kubectl get arkevent research-trigger -o jsonpath='{.status.webhookURL}')
TOKEN=$(kubectl get secret arkevent-research-trigger-token -o jsonpath='{.data.token}' | base64 -d)

curl -X POST "$WEBHOOK_URL/fire" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"input": {"topic": "Kubernetes operator patterns"}}'

# Watch it run
kubectl get arkflows -w
# NAME                              PHASE     TOKENS   STARTED   COMPLETED
# research-pipeline-20250314-abc1   Running   0        5s
# research-pipeline-20250314-abc1   Succeeded 1842     8s        12s
```

---

## ark — local development CLI

Run ArkFlow pipelines on your laptop without a Kubernetes cluster or Redis. The same YAML files that work with `kubectl apply -f` work unchanged with `ark run`.

```bash
# Pre-built binary — download from the releases page, or:
go install github.com/arkonis-dev/ark-operator/cmd/ark@latest

# Try it immediately — no API key needed:
ark run quickstart.yaml --provider mock --watch
```

```
$ ark run --help

Run an ArkFlow defined in a multi-document YAML file.
The file may contain ArkAgent, ArkFlow, and other resource definitions —
only ArkAgent and ArkFlow documents are used; everything else is ignored.

Examples:
  ark run quickstart.yaml
  ark run quickstart.yaml --provider mock
  ark run quickstart.yaml --provider openai --watch
  ark run quickstart.yaml --watch --output json
  ark run pipeline.yaml --input '{"topic":"Kubernetes operators"}'

Usage:
  ark run <file> [flags]

Flags:
      --dry-run           Validate YAML and print a summary without executing
      --input string      Flow input as a JSON object, e.g. '{"topic":"Kubernetes"}'
      --no-mcp            Skip MCP tool server connections
      --output string     Output format: text or json (default "text")
      --provider string   LLM provider: auto, anthropic, openai, or mock (default "auto")
      --watch             Stream step-by-step output as the flow executes
```

Provider defaults to `auto`: `claude-*` → Anthropic, `gpt-*`/`o*` → OpenAI. Set `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` in your environment.

---

## Install (operator)

**Prerequisites:** Kubernetes 1.31+, Redis (for the task queue)

```bash
# 1. Install the operator
kubectl apply -f https://github.com/arkonis-dev/ark-operator/releases/latest/download/install.yaml

# 2. Deploy Redis
kubectl apply -f https://raw.githubusercontent.com/arkonis-dev/ark-operator/main/config/prereqs/redis.yaml

# 3. Create the API key secret (one per namespace)
kubectl create secret generic arkonis-api-keys \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-... \
  --from-literal=TASK_QUEUE_URL=redis.agent-infra.svc.cluster.local:6379
```

For a full working example with agents, flows, and a webhook trigger in one apply:

```bash
kubectl apply -f https://raw.githubusercontent.com/arkonis-dev/ark-operator-example/main/quickstart.yaml
```

Full documentation: **[arkonis.dev](https://arkonis.dev)**

---

## Contributing

Contributions welcome. Open an issue before starting significant work. See [CONTRIBUTING.md](./CONTRIBUTING.md) for guidelines.

## License

Apache 2.0: see [LICENSE](./LICENSE)
