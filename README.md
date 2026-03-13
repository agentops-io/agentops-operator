# arkonis-operator

> Kubernetes-native AI agent infrastructure. Deploy, scale, and manage agentic workloads the same way you manage any other workload.

[![GitHub release](https://img.shields.io/github/v/release/arkonis-dev/arkonis-operator)](https://github.com/arkonis-dev/arkonis-operator/releases)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](./LICENSE)
[![Lint](https://github.com/arkonis-dev/arkonis-operator/actions/workflows/lint.yml/badge.svg)](https://github.com/arkonis-dev/arkonis-operator/actions/workflows/lint.yml)
[![Tests](https://github.com/arkonis-dev/arkonis-operator/actions/workflows/test.yml/badge.svg)](https://github.com/arkonis-dev/arkonis-operator/actions/workflows/test.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/arkonis-dev/arkonis-operator)](https://goreportcard.com/report/github.com/arkonis-dev/arkonis-operator)
[![Go version](https://img.shields.io/github/go-mod/go-version/arkonis-dev/arkonis-operator)](./go.mod)
[![GitHub stars](https://img.shields.io/github/stars/arkonis-dev/arkonis-operator?style=social)](https://github.com/arkonis-dev/arkonis-operator/stargazers)

arkonis-operator extends Kubernetes with AI agents as first-class resources. Declare an agent with a model, system prompt, and MCP tool servers — the operator handles scheduling, scaling, and semantic health checks. Everything else works exactly like standard Kubernetes: GitOps, RBAC, namespaces, `kubectl`.

```bash
kubectl apply -f research-agent.yaml
# agentdeployment.arkonis.dev/research-agent created

kubectl get aodep
# NAME              MODEL                      REPLICAS   READY   AGE
# research-agent    claude-sonnet-4-20250514   5          5       2m
```

## Install

**Prerequisites:** Kubernetes 1.31+, kubectl

```bash
# 1. Install the operator
kubectl apply -f https://github.com/arkonis-dev/arkonis-operator/releases/latest/download/install.yaml

# 2. Deploy Redis task queue
kubectl apply -f https://raw.githubusercontent.com/arkonis-dev/arkonis-operator/main/config/prereqs/redis.yaml

# 3. Create the API key secret (one per namespace)
kubectl create secret generic arkonis-api-keys \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-... \
  --from-literal=TASK_QUEUE_URL=redis.agent-infra.svc.cluster.local:6379

# 4. Deploy your first agent
kubectl apply -f https://raw.githubusercontent.com/arkonis-dev/arkonis-operator/main/config/samples/arkonis_v1alpha1_arkondeployment.yaml
```

Full documentation: **[arkonis.dev](https://arkonis.dev)**

## Contributing

Contributions welcome. Open an issue before starting significant work. See [CONTRIBUTING.md](./CONTRIBUTING.md) for guidelines.

## License

Apache 2.0 — see [LICENSE](./LICENSE)
