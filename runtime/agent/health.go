package main

import (
	"context"
	"net/http"
	"strings"
	"time"
)

const defaultValidatorPrompt = "Reply with exactly one word: HEALTHY"

// ServeHealthProbe starts HTTP health endpoints on addr.
//
//   - GET /healthz — basic liveness: always 200 if the process is running.
//   - GET /readyz  — semantic readiness: sends a prompt to the Anthropic API and
//     checks that the response contains "HEALTHY". The prompt is taken from
//     AGENT_VALIDATOR_PROMPT (set via ArkonisDeployment.spec.livenessProbe.validatorPrompt),
//     falling back to a built-in default. This validates that the model and API key
//     are correctly configured before the pod accepts tasks.
func ServeHealthProbe(addr string, runner *Runner, validatorPrompt string) {
	if validatorPrompt == "" {
		validatorPrompt = defaultValidatorPrompt
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		result, err := runner.RunTask(ctx, Task{
			ID:     "health-probe",
			Prompt: validatorPrompt,
		})
		if err != nil || !strings.Contains(result, "HEALTHY") {
			w.WriteHeader(http.StatusServiceUnavailable)
			if err != nil {
				_, _ = w.Write([]byte("semantic check failed: " + err.Error()))
			} else {
				_, _ = w.Write([]byte("unexpected response: " + result))
			}
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})

	_ = http.ListenAndServe(addr, mux)
}
