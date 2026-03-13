# Contributing to arkonis-operator

Thank you for your interest in contributing. This document covers how to get started, the development workflow, and the standards we expect from contributions.

## Before you start

- **Open an issue first** for any non-trivial change (new feature, refactor, or API change). This avoids wasted effort if the direction isn't a good fit.
- Bug fixes and documentation improvements can go straight to a PR.

## Development setup

**Prerequisites**

| Tool | Version |
|---|---|
| Go | 1.25+ (see `go.mod`) |
| kubectl | 1.31+ |
| kind or k3d | latest |
| kubebuilder | v4 |

```bash
# 1. Clone
git clone https://github.com/arkonis-dev/arkonis-operator.git
cd arkonis-operator

# 2. Start a full local environment (kind cluster + Redis + operator inside the cluster)
make dev ANTHROPIC_API_KEY=sk-ant-...

# Tear down when done
make dev-down
```

For faster iteration on controller code without rebuilding Docker images, you can run the operator on your host instead:

```bash
# Requires a cluster with CRDs and Redis already installed (e.g. from a previous make dev)
make run
```

## Project layout

```
api/v1alpha1/          # CRD type definitions — edit here first
internal/controller/   # One reconciler per Kind
runtime/agent/         # Binary that runs inside each agent pod
config/                # Kustomize manifests (CRDs, RBAC, samples)
cmd/main.go            # Operator entrypoint
```

## Making changes

### Changing a CRD type

1. Edit the relevant `api/v1alpha1/*_types.go` file.
2. Regenerate code and manifests:

   ```bash
   make generate   # regenerates zz_generated.deepcopy.go
   make manifests  # regenerates config/crd/ and config/rbac/
   ```

3. Update or add a sample CR in `config/samples/`.
4. Update the controller if the new field needs to be acted on.

### Adding a controller feature

1. Edit `internal/controller/<kind>_controller.go`.
2. Add or update tests in `internal/controller/<kind>_controller_test.go`.
3. Run tests: `make test`.

### Changing the agent runtime

The runtime lives in `runtime/agent/` and is built into a separate Docker image (`Dockerfile.agent`). It reads all config from environment variables injected by the operator (`AGENT_MODEL`, `AGENT_SYSTEM_PROMPT`, `AGENT_MCP_SERVERS`, `ANTHROPIC_API_KEY`, `TASK_QUEUE_URL`, etc.).

## Running tests

```bash
# Unit + controller tests (uses envtest, no cluster needed)
make test

# E2E tests (requires a running cluster and the operator image)
make test-e2e
```

Tests use [Ginkgo](https://onsi.github.io/ginkgo/) + [Gomega](https://onsi.github.io/gomega/).

## Code style

- Run `make lint` before submitting — the CI will block on lint failures.
- Follow standard Go conventions (`gofmt`, `goimports`).
- Keep reconcile functions focused: extract helpers for anything non-trivial.
- Never panic in a `Reconcile()` function.
- Use `ctrl.Result{}, err` for transient errors (triggers exponential backoff) and `ctrl.Result{}, nil` when done.

## Submitting a pull request

1. Fork the repo and create a branch from `main`.
2. Make your changes with focused commits.
3. Ensure `make test` and `make lint` both pass locally.
4. Open a PR against `main` with a clear description of what and why.
5. Link the related issue (if any) in the PR description.

We use **Rebase and merge** for all PRs to keep a linear history on `main`.

## Reporting bugs

Open a [GitHub issue](https://github.com/arkonis-dev/arkonis-operator/issues/new) with:

- Operator version (`kubectl get deployment -n arkonis-system`)
- Kubernetes version (`kubectl version`)
- The ArkonisDeployment / ArkonisService YAML (redact secrets)
- Relevant controller logs (`kubectl logs -n arkonis-system -l control-plane=controller-manager`)

## Security vulnerabilities

See [SECURITY.md](./SECURITY.md) — please do **not** open a public issue.

## License

By contributing, you agree that your contributions will be licensed under the [Apache 2.0 License](./LICENSE).
