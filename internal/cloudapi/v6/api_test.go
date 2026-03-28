package cloudapi

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	k6cloud "github.com/grafana/k6-cloud-openapi-client-go/k6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.k6.io/k6/internal/lib/testutils"
)

func newTestClient(t *testing.T, url string) *Client {
	t.Helper()
	c, err := NewClient(testutils.NewLogger(t), "test-token", url, "1.0", time.Second)
	require.NoError(t, err)
	require.NoError(t, c.SetStackID(123))
	return c
}

func testRunJSON(status, result, webAppURL string, estimated, execution float64) string {
	resultField := "null"
	if result != "" {
		resultField = fmt.Sprintf("%q", result)
	}
	webAppField := ""
	if webAppURL != "" {
		webAppField = fmt.Sprintf(`,"web_app_url": %q`, webAppURL)
	}
	return fmt.Sprintf(`{
		"id": 42, "test_id": 1, "project_id": 10,
		"started_by": null, "created": "2026-01-01T00:00:00Z",
		"ended": null, "note": "",
		"retention_expiry": null, "cost": null,
		"status": %q,
		"status_details": {"type": "running", "entered": "2026-01-01T00:00:00Z"},
		"status_history": [], "distribution": [],
		"result": %s,
		"result_details": {}, "options": {},
		"k6_dependencies": {}, "k6_versions": {},
		"estimated_duration": %f,
		"execution_duration": %f
		%s
	}`, status, resultField, estimated, execution, webAppField)
}

func TestValidateToken(t *testing.T) {
	t.Parallel()

	t.Run("successful token validation", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Verify the authorization header
			authHeader := r.Header.Get("Authorization")
			assert.Equal(t, "Bearer test-token", authHeader)

			// Verify the stack URL
			stackURL := r.Header.Get("X-Stack-Url")
			assert.Equal(t, stackURL, "https://stack.grafana.net")

			w.Header().Add("Content-Type", "application/json")
			fprint(t, w, `{
				"stack_id": 123,
				"default_project_id": 456
			}`)
		}))
		defer server.Close()

		client, err := NewClient(testutils.NewLogger(t), "test-token", server.URL, "1.0", 1*time.Second)
		require.NoError(t, err)

		resp, err := client.ValidateToken("https://stack.grafana.net")
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Equal(t, int32(123), resp.StackId)
		assert.Equal(t, int32(456), resp.DefaultProjectId)
	})

	t.Run("unauthorized token should fail", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Add("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fprint(t, w, `{
				"error": {
					"code": "error",
					"message": "Invalid token"
				}
			}`)
		}))
		defer server.Close()

		client, err := NewClient(testutils.NewLogger(t), "invalid-token", server.URL, "1.0", 1*time.Second)
		require.NoError(t, err)

		resp, err := client.ValidateToken("https://stack.grafana.net")
		assert.Error(t, err)
		assert.Nil(t, resp)
		assert.Contains(t, err.Error(), "(401/error) Invalid token")
	})

	t.Run("network error should fail", func(t *testing.T) {
		t.Parallel()
		// Use an invalid URL to simulate network error
		client, err := NewClient(testutils.NewLogger(t), "test-token", "http://invalid-url-that-does-not-exist", "1.0", 1*time.Second)
		require.NoError(t, err)

		resp, err := client.ValidateToken("https://stack.grafana.net")
		assert.Error(t, err)
		assert.Nil(t, resp)
	})

	t.Run("missing stack URL should fail", func(t *testing.T) {
		t.Parallel()
		client, err := NewClient(testutils.NewLogger(t), "test-token", "http://example.com", "1.0", 1*time.Second)
		require.NoError(t, err)

		resp, err := client.ValidateToken("")
		assert.Error(t, err)
		assert.Nil(t, resp)
		assert.Equal(t, "stack URL is required to validate token", err.Error())
	})

	t.Run("invalid stack URL should fail", func(t *testing.T) {
		t.Parallel()
		client, err := NewClient(testutils.NewLogger(t), "test-token", "http://example.com", "1.0", 1*time.Second)
		require.NoError(t, err)

		resp, err := client.ValidateToken("://invalid-url")
		assert.Error(t, err)
		assert.Nil(t, resp)
		assert.Contains(t, err.Error(), "invalid stack URL")
	})
}

func TestFetchTestRun(t *testing.T) {
	t.Parallel()

	t.Run("returns progress from running test run", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Contains(t, r.URL.Path, "/cloud/v6/test_runs/42")
			assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
			assert.Equal(t, "123", r.Header.Get("X-Stack-Id"))

			w.Header().Set("Content-Type", "application/json")
			fprint(t, w, testRunJSON("running", "", "https://grafana.com/runs/42", 60, 30))
		}))
		defer server.Close()

		client := newTestClient(t, server.URL)

		progress, err := client.FetchTestRun(t.Context(), 42)
		require.NoError(t, err)
		assert.Equal(t, "running", progress.Status)
		assert.Equal(t, "", progress.Result)
		assert.Equal(t, 0.5, progress.Progress())
		assert.Equal(t, "https://grafana.com/runs/42", progress.WebAppURL)
	})

	t.Run("finished test run with failed result", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fprint(t, w, testRunJSON("finished", "failed", "https://grafana.com/runs/42", 60, 60))
		}))
		defer server.Close()

		client := newTestClient(t, server.URL)

		progress, err := client.FetchTestRun(t.Context(), 42)
		require.NoError(t, err)
		assert.Equal(t, "finished", progress.Status)
		assert.Equal(t, "failed", progress.Result)
		assert.Equal(t, 1.0, progress.Progress())
		assert.True(t, progress.IsTerminal())
	})

	t.Run("all terminal statuses are recognized", func(t *testing.T) {
		t.Parallel()

		statuses := []string{
			"finished", "timed_out", "aborted_user", "aborted_system",
			"aborted_script_error", "aborted_threshold", "aborted_limit",
		}
		for _, s := range statuses {
			p := TestRunProgress{Status: s}
			assert.True(t, p.IsTerminal(), "status %q should be terminal", s)
		}

		nonTerminal := []string{"created", "validated", "queued", "initializing", "running"}
		for _, s := range nonTerminal {
			p := TestRunProgress{Status: s}
			assert.False(t, p.IsTerminal(), "status %q should NOT be terminal", s)
		}
	})

	t.Run("HTTP errors", func(t *testing.T) {
		t.Parallel()

		for _, tc := range []struct {
			code int
			body string
		}{
			{500, `{"error":{"code":"internal_error","message":"Something went wrong"}}`},
			{401, `{"error":{"code":"unauthorized","message":"Invalid token"}}`},
			{403, `{"error":{"code":"forbidden","message":"Access denied"}}`},
		} {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.code)
				fprint(t, w, tc.body)
			}))
			defer server.Close()

			client := newTestClient(t, server.URL)

			progress, err := client.FetchTestRun(t.Context(), 42)
			assert.Error(t, err)
			assert.Nil(t, progress)
			assert.Contains(t, err.Error(), "failed to fetch test run")
		}
	})

	t.Run("context cancellation returns error", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fprint(t, w, `{}`)
		}))
		defer server.Close()

		client := newTestClient(t, server.URL)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		progress, err := client.FetchTestRun(ctx, 42)
		assert.Error(t, err)
		assert.Nil(t, progress)
	})

	t.Run("connection refused returns error gracefully", func(t *testing.T) {
		t.Parallel()

		client, err := NewClient(testutils.NewLogger(t), "test-token", "http://127.0.0.1:1", "1.0", 1*time.Second)
		require.NoError(t, err)
		require.NoError(t, client.SetStackID(123))

		progress, err := client.FetchTestRun(t.Context(), 42)
		assert.Error(t, err)
		assert.Nil(t, progress)
	})

	t.Run("response timeout is handled gracefully", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			time.Sleep(5 * time.Second)
		}))
		defer server.Close()

		client, err := NewClient(testutils.NewLogger(t), "test-token", server.URL, "1.0", 100*time.Millisecond)
		require.NoError(t, err)
		require.NoError(t, client.SetStackID(123))

		progress, err := client.FetchTestRun(t.Context(), 42)
		assert.Error(t, err)
		assert.Nil(t, progress)
	})

	t.Run("max int32 test run ID", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Contains(t, r.URL.Path, "/cloud/v6/test_runs/2147483647")

			w.Header().Set("Content-Type", "application/json")
			fprint(t, w, testRunJSON("running", "", "", 0, 0))
		}))
		defer server.Close()

		client := newTestClient(t, server.URL)

		progress, err := client.FetchTestRun(t.Context(), math.MaxInt32)
		require.NoError(t, err)
		assert.Equal(t, "running", progress.Status)
	})

	t.Run("very long web_app_url", func(t *testing.T) {
		t.Parallel()

		longURL := "https://grafana.com/" + strings.Repeat("a", 1980)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fprint(t, w, testRunJSON("running", "", longURL, 0, 0))
		}))
		defer server.Close()

		client := newTestClient(t, server.URL)

		progress, err := client.FetchTestRun(t.Context(), 42)
		require.NoError(t, err)
		assert.Equal(t, longURL, progress.WebAppURL)
	})

	t.Run("large response with many additional properties", func(t *testing.T) {
		t.Parallel()

		var b strings.Builder
		for i := range 100 {
			fmt.Fprintf(&b, `"extra_field_%d": "value%d",`, i, i)
		}
		extraFields := b.String()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fprint(t, w, `{
				"id": 42, "test_id": 1, "project_id": 10,
				"started_by": null, "created": "2026-01-01T00:00:00Z",
				"ended": null, "note": "",
				"retention_expiry": null, "cost": null,
				"status": "running",
				"status_details": {"type": "running", "entered": "2026-01-01T00:00:00Z"},
				"status_history": [], "distribution": [],
				"result": null,
				"result_details": {}, "options": {},
				"k6_dependencies": {}, "k6_versions": {},
				"web_app_url": "https://grafana.com/runs/42",
				"estimated_duration": 60.0,
				"execution_duration": 30.0,
				`+extraFields+`
				"last_extra": "done"
			}`)
		}))
		defer server.Close()

		client := newTestClient(t, server.URL)

		progress, err := client.FetchTestRun(t.Context(), 42)
		require.NoError(t, err)
		assert.Equal(t, "running", progress.Status)
		assert.Equal(t, "", progress.Result)
		assert.Equal(t, "https://grafana.com/runs/42", progress.WebAppURL)
		assert.Equal(t, 0.5, progress.Progress())
	})
}

func TestFetchTestRunRetries(t *testing.T) {
	t.Parallel()

	for _, code := range []int{http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprintf("retries on transient %d error", code), func(t *testing.T) {
			t.Parallel()

			var callCount atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				n := callCount.Add(1)
				if n == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(code)
					fprint(t, w, `{"error": {"code": "transient", "message": "transient"}}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fprint(t, w, testRunJSON("running", "", "", 60, 30))
			}))
			defer server.Close()

			client, err := NewClient(testutils.NewLogger(t), "test-token", server.URL, "1.0", 5*time.Second)
			require.NoError(t, err)
			require.NoError(t, client.SetStackID(123))

			progress, err := client.FetchTestRun(t.Context(), 42)
			require.NoError(t, err)
			assert.Equal(t, "running", progress.Status)
			assert.Equal(t, int32(2), callCount.Load(), "should have retried once after %d", code)
		})
	}
}

func TestStartTestRun(t *testing.T) {
	t.Parallel()

	t.Run("returns test run ID and web_app_url", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Contains(t, r.URL.Path, "/cloud/v6/load_tests/1/start")

			w.Header().Set("Content-Type", "application/json")
			fprint(t, w, testRunJSON("created", "", "https://grafana.com/a/k6-app/runs/42", 0, 0))
		}))
		defer server.Close()

		client := newTestClient(t, server.URL)

		result, err := client.StartTestRun(t.Context(), 1)
		require.NoError(t, err)
		assert.Equal(t, int32(42), result.TestRunID)
		assert.Equal(t, "https://grafana.com/a/k6-app/runs/42", result.WebAppURL)
	})

	t.Run("missing web_app_url returns empty string", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fprint(t, w, testRunJSON("created", "", "", 0, 0))
		}))
		defer server.Close()

		client := newTestClient(t, server.URL)

		result, err := client.StartTestRun(t.Context(), 1)
		require.NoError(t, err)
		assert.Equal(t, int32(42), result.TestRunID)
		assert.Equal(t, "", result.WebAppURL)
	})

	t.Run("server error returns wrapped error", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fprint(t, w, `{"error": {"code": "bad_request", "message": "Invalid load test"}}`)
		}))
		defer server.Close()

		client := newTestClient(t, server.URL)

		result, err := client.StartTestRun(t.Context(), 1)
		assert.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "failed to start test run")
	})

	t.Run("zero test run ID returns error", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fprint(t, w, `{
				"id": 0, "test_id": 1, "project_id": 10,
				"started_by": null, "created": "2026-01-01T00:00:00Z",
				"ended": null, "note": "",
				"retention_expiry": null, "cost": null,
				"status": "created",
				"status_details": {"type": "running", "entered": "2026-01-01T00:00:00Z"},
				"status_history": [], "distribution": [],
				"result": null,
				"result_details": {}, "options": {},
				"k6_dependencies": {}, "k6_versions": {}
			}`)
		}))
		defer server.Close()

		client := newTestClient(t, server.URL)

		result, err := client.StartTestRun(t.Context(), 1)
		assert.Nil(t, result)
		assert.ErrorContains(t, err, "invalid test run ID")
	})
}

func TestAbortTestRun(t *testing.T) {
	t.Parallel()

	t.Run("successful abort", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Contains(t, r.URL.Path, "/cloud/v6/test_runs/42/abort")
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		client := newTestClient(t, server.URL)

		err := client.AbortTestRun(t.Context(), 42)
		assert.NoError(t, err)
	})

	t.Run("server error returns wrapped error", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			fprint(t, w, `{"error": {"code": "not_found", "message": "Test run not found"}}`)
		}))
		defer server.Close()

		client := newTestClient(t, server.URL)

		err := client.AbortTestRun(t.Context(), 999)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to abort test run")
	})
}

func TestValidateOptions(t *testing.T) {
	t.Parallel()

	t.Run("successful validation", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Contains(t, r.URL.Path, "/cloud/v6/validate_options")

			w.Header().Set("Content-Type", "application/json")
			fprint(t, w, `{"vuh_usage": 1.5, "breakdown": {"protocol_vuh": 0.5, "browser_vuh": 0.5, "base_total_vuh": 1.5, "reduction_rate": 0.0, "reduction_rate_breakdown": {}}}`)
		}))
		defer server.Close()

		client := newTestClient(t, server.URL)

		err := client.ValidateOptions(t.Context(), &k6cloud.ValidateOptionsRequest{Options: k6cloud.Options{}})
		assert.NoError(t, err)
	})

	t.Run("HTTP 400 returns validation error with details", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fprint(t, w, `{"error": {"code": "validation_error", "message": "Invalid script options"}}`)
		}))
		defer server.Close()

		client := newTestClient(t, server.URL)

		err := client.ValidateOptions(t.Context(), &k6cloud.ValidateOptionsRequest{Options: k6cloud.Options{}})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to validate options")
		assert.Contains(t, err.Error(), "Invalid script options")
	})
}

func TestProgress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		progress TestRunProgress
		expected float64
	}{
		{
			name:     "normal progress",
			progress: TestRunProgress{EstimatedDuration: 100.0, ExecutionDuration: 50.0},
			expected: 0.5,
		},
		{
			name:     "complete",
			progress: TestRunProgress{EstimatedDuration: 60.0, ExecutionDuration: 60.0},
			expected: 1.0,
		},
		{
			name:     "clamped to 1 when execution exceeds estimated",
			progress: TestRunProgress{EstimatedDuration: 30.0, ExecutionDuration: 90.0},
			expected: 1.0,
		},
		{
			name:     "zero estimated returns zero",
			progress: TestRunProgress{EstimatedDuration: 0.0, ExecutionDuration: 10.0},
			expected: 0,
		},
		{
			name:     "negative estimated returns zero",
			progress: TestRunProgress{EstimatedDuration: -5.0, ExecutionDuration: 10.0},
			expected: 0,
		},
		{
			name:     "missing estimated returns zero",
			progress: TestRunProgress{ExecutionDuration: 10.0},
			expected: 0,
		},
		{
			name:     "missing execution returns zero",
			progress: TestRunProgress{EstimatedDuration: 60.0},
			expected: 0,
		},
		{
			name:     "both missing returns zero",
			progress: TestRunProgress{},
			expected: 0,
		},
		{
			name:     "negative execution returns zero",
			progress: TestRunProgress{EstimatedDuration: 60.0, ExecutionDuration: -10.0},
			expected: 0,
		},
		{
			name:     "zero execution returns zero progress",
			progress: TestRunProgress{EstimatedDuration: 60.0, ExecutionDuration: 0.0},
			expected: 0,
		},
		{
			name:     "very small durations",
			progress: TestRunProgress{EstimatedDuration: 0.001, ExecutionDuration: 0.0005},
			expected: 0.5,
		},
		{
			name:     "very large durations",
			progress: TestRunProgress{EstimatedDuration: 86400.0, ExecutionDuration: 43200.0},
			expected: 0.5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, tt.progress.Progress())
		})
	}
}

func TestSetStackIDValidation(t *testing.T) {
	t.Parallel()

	newClient := func(t *testing.T) *Client {
		t.Helper()
		c, err := NewClient(testutils.NewLogger(t), "test-token", "http://example.com", "1.0", 1*time.Second)
		require.NoError(t, err)
		return c
	}

	t.Run("valid stack ID", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, newClient(t).SetStackID(123))
	})

	t.Run("max int32 boundary", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, newClient(t).SetStackID(math.MaxInt32))
	})

	t.Run("zero is valid", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, newClient(t).SetStackID(0))
	})

	t.Run("negative is invalid", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, newClient(t).SetStackID(-1))
	})

	t.Run("exceeds MaxInt32", func(t *testing.T) {
		t.Parallel()
		err := newClient(t).SetStackID(math.MaxInt32 + 1)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "out of valid int32 range")
	})
}

func TestIdempotencyKeyInjected(t *testing.T) {
	t.Parallel()

	t.Run("POST request gets idempotency key", func(t *testing.T) {
		t.Parallel()

		var gotHeader string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHeader = r.Header.Get("K6-Idempotency-Key")
			w.Header().Set("Content-Type", "application/json")
			fprint(t, w, testRunJSON("created", "", "https://grafana.com/runs/42", 0, 0))
		}))
		defer server.Close()

		client := newTestClient(t, server.URL)

		_, err := client.StartTestRun(t.Context(), 1)
		require.NoError(t, err)
		assert.NotEmpty(t, gotHeader, "POST should have K6-Idempotency-Key header")
		assert.Len(t, gotHeader, 16, "key should be 16 hex chars")
	})

	t.Run("GET request has no idempotency key", func(t *testing.T) {
		t.Parallel()

		var gotHeader string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHeader = r.Header.Get("K6-Idempotency-Key")
			w.Header().Set("Content-Type", "application/json")
			fprint(t, w, testRunJSON("running", "", "https://grafana.com/runs/42", 60, 30))
		}))
		defer server.Close()

		client := newTestClient(t, server.URL)

		_, err := client.FetchTestRun(t.Context(), 42)
		require.NoError(t, err)
		assert.Empty(t, gotHeader, "GET should NOT have K6-Idempotency-Key header")
	})

	t.Run("keys are unique per request", func(t *testing.T) {
		t.Parallel()

		var keys []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			keys = append(keys, r.Header.Get("K6-Idempotency-Key"))
			w.Header().Set("Content-Type", "application/json")
			fprint(t, w, testRunJSON("created", "", "https://grafana.com/runs/42", 0, 0))
		}))
		defer server.Close()

		client := newTestClient(t, server.URL)

		_, err := client.StartTestRun(t.Context(), 1)
		require.NoError(t, err)
		_, err = client.StartTestRun(t.Context(), 1)
		require.NoError(t, err)

		require.Len(t, keys, 2)
		assert.NotEqual(t, keys[0], keys[1], "each request should have a unique idempotency key")
	})
}

func TestGetLoadTestByName(t *testing.T) {
	t.Parallel()

	t.Run("found", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "/cloud/v6/load_tests", r.URL.Path)
			assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
			assert.Equal(t, "123", r.Header.Get("X-Stack-Id"))
			assert.Equal(t, "my-test", r.URL.Query().Get("name"))

			w.Header().Set("Content-Type", "application/json")
			fprint(t, w, `{"value": [
				{"id": 5, "project_id": 10, "name": "my-test",
				 "baseline_test_run_id": null,
				 "created": "2026-01-01T00:00:00Z", "updated": "2026-01-01T00:00:00Z"}
			]}`)
		}))
		defer server.Close()

		client := newTestClient(t, server.URL)

		id, err := client.GetLoadTestByName(t.Context(), 10, "my-test")
		require.NoError(t, err)
		assert.Equal(t, int32(5), id)
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fprint(t, w, `{"value": []}`)
		}))
		defer server.Close()

		client := newTestClient(t, server.URL)

		_, err := client.GetLoadTestByName(t.Context(), 10, "nonexistent")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})

	t.Run("server error", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			fprint(t, w, `{"error": {"code": "internal", "message": "server error"}}`)
		}))
		defer server.Close()

		client := newTestClient(t, server.URL)

		_, err := client.GetLoadTestByName(t.Context(), 10, "my-test")
		assert.Error(t, err)
	})
}

func fprint(t *testing.T, w io.Writer, format string) int {
	n, err := fmt.Fprint(w, format)
	require.NoError(t, err)
	return n
}
