package health

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/arkonis-dev/ark-operator/internal/agent/queue"
	"github.com/arkonis-dev/ark-operator/internal/agent/runner"
)

// ServeProbe starts HTTP health endpoints on addr.
//
//   - GET /healthz — basic liveness: always 200 if the process is running.
//   - GET /readyz  — readiness check. Behaviour depends on whether a
//     validatorPrompt is set (controlled by ArkAgent.spec.livenessProbe):
//   - No prompt (type: ping): returns 200 immediately — process-level readiness only.
//   - Prompt set (type: semantic): calls the LLM API and checks that the
//     response contains "HEALTHY" before returning 200.
func ServeProbe(addr string, r *runner.Runner, validatorPrompt string) {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, req *http.Request) {
		// When no validator prompt is configured (probe type: ping), skip the LLM
		// call and return ready immediately — the process is up and that's enough.
		if validatorPrompt == "" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready"))
			return
		}

		ctx, cancel := context.WithTimeout(req.Context(), 15*time.Second)
		defer cancel()

		result, _, err := r.RunTask(ctx, queue.Task{
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
