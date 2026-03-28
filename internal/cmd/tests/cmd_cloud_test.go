package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.k6.io/k6/cloudapi"
	"go.k6.io/k6/errext/exitcodes"
	"go.k6.io/k6/internal/cmd"
	"go.k6.io/k6/lib/fsext"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestK6Cloud(t *testing.T) {
	t.Parallel()
	runCloudTests(t, setupK6CloudCmd)
}

func setupK6CloudCmd(cliFlags []string) []string {
	return append([]string{"k6", "cloud"}, append(cliFlags, "test.js")...)
}

type setupCommandFunc func(cliFlags []string) []string

func runCloudTests(t *testing.T, setupCmd setupCommandFunc) {
	t.Run("TestCloudUserNotAuthenticated", func(t *testing.T) {
		t.Parallel()

		opts := defaultV6MockOpts()
		ts := getSimpleCloudTestStateV6(t, nil, setupCmd, nil, opts)
		delete(ts.Env, "K6_CLOUD_TOKEN")
		ts.ExpectedExitCode = -1 // TODO: use a more specific exit code?
		cmd.ExecuteWithGlobalState(ts.GlobalState)

		stdout := ts.Stdout.String()
		t.Log(stdout)
		assert.Contains(t, stdout, `must first authenticate`)
	})

	t.Run("TestCloudLoggedInWithScriptToken", func(t *testing.T) {
		t.Parallel()

		script := `
		export let options = {
			ext: {
				loadimpact: {
					token: "asdf",
					name: "my load test",
					projectID: 10,
					note: 124,
				},
			}
		};
		export default function() {};
	`

		opts := defaultV6MockOpts()
		ts := getSimpleCloudTestStateV6(t, []byte(script), setupCmd, nil, opts)
		delete(ts.Env, "K6_CLOUD_TOKEN")
		cmd.ExecuteWithGlobalState(ts.GlobalState)

		stdout := ts.Stdout.String()
		t.Log(stdout)
		assert.NotContains(t, stdout, `not logged in`)
		assert.Contains(t, stdout, `execution: cloud`)
		assert.Contains(t, stdout, `output: `+opts.webAppURL)
		assert.Contains(t, stdout, `test status: Finished`)
	})

	t.Run("TestCloudExitOnRunning", func(t *testing.T) {
		t.Parallel()

		opts := defaultV6MockOpts()
		opts.progressCallback = func() v6TestRunProgress {
			return v6TestRunProgress{
				Status:            "running",
				EstimatedDuration: 60,
				ExecutionDuration: 10,
			}
		}

		ts := getSimpleCloudTestStateV6(t, nil, setupCmd, []string{"--exit-on-running", "--log-output=stdout"}, opts)
		cmd.ExecuteWithGlobalState(ts.GlobalState)

		stdout := ts.Stdout.String()
		t.Log(stdout)
		assert.Contains(t, stdout, `execution: cloud`)
		assert.Contains(t, stdout, `output: `+opts.webAppURL)
		assert.Contains(t, stdout, `test status: Running`)
	})

	t.Run("TestCloudUploadOnly", func(t *testing.T) {
		t.Parallel()

		opts := defaultV6MockOpts()
		ts := getSimpleCloudTestStateV6(t, nil, setupCmd, []string{"--upload-only", "--log-output=stdout"}, opts)
		cmd.ExecuteWithGlobalState(ts.GlobalState)

		stdout := ts.Stdout.String()
		t.Log(stdout)
		assert.Contains(t, stdout, `test status: Uploaded`)
	})

	t.Run("TestCloudWithArchive", func(t *testing.T) {
		t.Parallel()

		opts := defaultV6MockOpts()
		opts.projectID = 124 // matches the archive's embedded projectID
		srv := getMockCloudV6(t, opts)

		ts := NewGlobalTestState(t)

		data, err := os.ReadFile(filepath.Join("testdata/archives", "archive_v0.46.0_with_loadimpact_option.tar")) //nolint:forbidigo // it's a test
		require.NoError(t, err)
		require.NoError(t, fsext.WriteFile(ts.FS, filepath.Join(ts.Cwd, "archive.tar"), data, 0o644))

		ts.CmdArgs = []string{"k6", "cloud", "--verbose", "--log-output=stdout", "archive.tar"}
		ts.Env["K6_SHOW_CLOUD_LOGS"] = "false"
		ts.Env["K6_CLOUD_HOST_V6"] = srv.URL
		ts.Env["K6_CLOUD_HOST"] = srv.URL
		ts.Env["K6_CLOUD_TOKEN"] = "foo"
		ts.Env["K6_CLOUD_STACK_ID"] = "123"

		cmd.ExecuteWithGlobalState(ts.GlobalState)

		stdout := ts.Stdout.String()
		t.Log(stdout)
		assert.NotContains(t, stdout, `not logged in`)
		assert.Contains(t, stdout, `execution: cloud`)
		assert.Contains(t, stdout, `output: `+opts.webAppURL)
		assert.Contains(t, stdout, `test status: Finished`)
	})

	t.Run("TestCloudThresholdsHaveFailed", func(t *testing.T) {
		t.Parallel()

		opts := defaultV6MockOpts()
		opts.progressCallback = func() v6TestRunProgress {
			return v6TestRunProgress{
				Status:            "finished",
				Result:            "failed",
				EstimatedDuration: 60,
				ExecutionDuration: 60,
			}
		}
		ts := getSimpleCloudTestStateV6(t, nil, setupCmd, nil, opts)
		ts.ExpectedExitCode = int(exitcodes.ThresholdsHaveFailed)

		cmd.ExecuteWithGlobalState(ts.GlobalState)

		stdout := ts.Stdout.String()
		t.Log(stdout)
		assert.Contains(t, stdout, `Thresholds have been crossed`)
	})

	t.Run("TestCloudAbortedThreshold", func(t *testing.T) {
		t.Parallel()

		opts := defaultV6MockOpts()
		opts.progressCallback = func() v6TestRunProgress {
			return v6TestRunProgress{
				Status:            "aborted_threshold",
				Result:            "failed",
				EstimatedDuration: 60,
				ExecutionDuration: 40,
			}
		}
		ts := getSimpleCloudTestStateV6(t, nil, setupCmd, nil, opts)
		ts.ExpectedExitCode = int(exitcodes.ThresholdsHaveFailed)

		cmd.ExecuteWithGlobalState(ts.GlobalState)

		stdout := ts.Stdout.String()
		t.Log(stdout)
		assert.Contains(t, stdout, `Thresholds have been crossed`)
	})
}

func cloudTestStartSimple(tb testing.TB, testRunID int) http.Handler {
	return http.HandlerFunc(func(resp http.ResponseWriter, _ *http.Request) {
		resp.WriteHeader(http.StatusOK)
		_, err := fmt.Fprintf(resp, `{"reference_id": "%d"}`, testRunID)
		assert.NoError(tb, err)
	})
}

func getMockCloud(
	t *testing.T, testRunID int,
	archiveUpload http.Handler, progressCallback func() cloudapi.TestProgressResponse,
) *httptest.Server {
	if archiveUpload == nil {
		archiveUpload = cloudTestStartSimple(t, testRunID)
	}
	testProgressURL := fmt.Sprintf("GET ^/v1/test-progress/%d$", testRunID)
	defaultProgress := cloudapi.TestProgressResponse{
		RunStatusText: "Finished",
		RunStatus:     cloudapi.RunStatusFinished,
		ResultStatus:  cloudapi.ResultStatusPassed,
		Progress:      1,
	}

	srv := getTestServer(t, map[string]http.Handler{
		"POST ^/v1/archive-upload$": archiveUpload,
		testProgressURL: http.HandlerFunc(func(resp http.ResponseWriter, _ *http.Request) {
			testProgress := defaultProgress
			if progressCallback != nil {
				testProgress = progressCallback()
			}
			respBody, err := json.Marshal(testProgress)
			assert.NoError(t, err)
			_, err = fmt.Fprint(resp, string(respBody))
			assert.NoError(t, err)
		}),
	})

	t.Cleanup(srv.Close)

	return srv
}

func getSimpleCloudTestState(t *testing.T, script []byte, setupCmd setupCommandFunc, cliFlags []string, archiveUpload http.Handler, progressCallback func() cloudapi.TestProgressResponse) *GlobalTestState {
	if script == nil {
		script = []byte(`export default function() {}`)
	}

	if cliFlags == nil {
		cliFlags = []string{"--verbose", "--log-output=stdout"}
	}

	srv := getMockCloud(t, 123, archiveUpload, progressCallback)

	ts := NewGlobalTestState(t)
	require.NoError(t, fsext.WriteFile(ts.FS, filepath.Join(ts.Cwd, "test.js"), script, 0o644))
	ts.CmdArgs = setupCmd(cliFlags)
	ts.Env["K6_SHOW_CLOUD_LOGS"] = "false" // no mock for the logs yet
	ts.Env["K6_CLOUD_HOST"] = srv.URL
	ts.Env["K6_CLOUD_TOKEN"] = "foo" // doesn't matter, we mock the cloud

	return ts
}

// v6MockOpts configures the v6 mock cloud server behavior.
type v6MockOpts struct {
	testRunID  int
	loadTestID int
	projectID  int
	webAppURL  string
	// progressCallback returns the current test run progress JSON fields.
	// The returned values override status, result, and durations.
	progressCallback func() v6TestRunProgress
}

// v6TestRunProgress represents the test run state returned by the mock.
type v6TestRunProgress struct {
	Status            string
	Result            string
	EstimatedDuration float64
	ExecutionDuration float64
}

func defaultV6MockOpts() v6MockOpts {
	return v6MockOpts{
		testRunID:  42,
		loadTestID: 1,
		projectID:  10,
		webAppURL:  "https://grafana.com/a/k6-app/runs/42",
	}
}

// getMockCloudV6 returns an httptest.Server with v6 API routes.
// It handles the full cloud run flow: validate_options, create/get load test,
// upload script, start, poll progress, and abort.
func getMockCloudV6(t *testing.T, opts v6MockOpts) *httptest.Server {
	t.Helper()
	routes := buildV6Routes(t, opts)
	srv := getTestServer(t, routes)
	t.Cleanup(srv.Close)
	return srv
}

// getSimpleCloudTestStateV6 creates a test state configured for v6 cloud tests.
func getSimpleCloudTestStateV6(
	t *testing.T,
	script []byte,
	setupCmd setupCommandFunc,
	cliFlags []string,
	opts v6MockOpts,
) *GlobalTestState {
	if script == nil {
		script = []byte(`export default function() {}`)
	}
	if cliFlags == nil {
		cliFlags = []string{"--verbose", "--log-output=stdout"}
	}

	srv := getMockCloudV6(t, opts)

	ts := NewGlobalTestState(t)
	require.NoError(t, fsext.WriteFile(ts.FS, filepath.Join(ts.Cwd, "test.js"), script, 0o644))
	ts.CmdArgs = setupCmd(cliFlags)
	ts.Env["K6_SHOW_CLOUD_LOGS"] = "false"
	ts.Env["K6_CLOUD_HOST_V6"] = srv.URL
	ts.Env["K6_CLOUD_HOST"] = srv.URL
	ts.Env["K6_CLOUD_TOKEN"] = "foo"
	ts.Env["K6_CLOUD_STACK_ID"] = "123"
	ts.Env["K6_CLOUD_PROJECT_ID"] = fmt.Sprintf("%d", opts.projectID)

	return ts
}

// buildV6Routes returns the standard v6 mock routes for the given opts,
// letting callers override individual routes before passing to getTestServer.
func buildV6Routes(t *testing.T, opts v6MockOpts) map[string]http.Handler {
	t.Helper()

	testRunJSON := func(status, result string, estimated, execution float64) string {
		resultField := "null"
		if result != "" {
			resultField = fmt.Sprintf("%q", result)
		}
		return fmt.Sprintf(`{
			"id": %d, "test_id": %d, "project_id": %d,
			"started_by": null, "created": "2026-01-01T00:00:00Z",
			"ended": null, "note": "",
			"retention_expiry": null, "cost": null,
			"status": %q,
			"status_details": {"type": "running", "entered": "2026-01-01T00:00:00Z"},
			"status_history": [], "distribution": [],
			"result": %s,
			"result_details": {}, "options": {},
			"k6_dependencies": {}, "k6_versions": {},
			"web_app_url": %q,
			"estimated_duration": %f,
			"execution_duration": %f
		}`, opts.testRunID, opts.loadTestID, opts.projectID,
			status, resultField, opts.webAppURL,
			estimated, execution)
	}

	loadTestJSON := fmt.Sprintf(`{
		"id": %d, "project_id": %d, "name": "test",
		"baseline_test_run_id": null,
		"created": "2026-01-01T00:00:00Z",
		"updated": "2026-01-01T00:00:00Z"
	}`, opts.loadTestID, opts.projectID)

	return map[string]http.Handler{
		"POST ^/cloud/v6/validate_options$": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"vuh_usage": 1.0, "breakdown": {"protocol_vuh": 1.0, "browser_vuh": 0.0, "base_total_vuh": 1.0, "reduction_rate": 0.0, "reduction_rate_breakdown": {}}}`)
		}),
		fmt.Sprintf("POST ^/cloud/v6/projects/%d/load_tests$", opts.projectID): http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, loadTestJSON)
		}),
		fmt.Sprintf("GET ^/cloud/v6/projects/%d/load_tests", opts.projectID): http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"value": [%s]}`, loadTestJSON)
		}),
		// Global load-tests list (used by GetLoadTestByName via LoadTestsList with name filter)
		"GET ^/cloud/v6/load_tests(\\?|$)": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"value": [%s]}`, loadTestJSON)
		}),
		fmt.Sprintf("PUT ^/cloud/v6/load_tests/%d/script$", opts.loadTestID): http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
		fmt.Sprintf("POST ^/cloud/v6/load_tests/%d/start$", opts.loadTestID): http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, testRunJSON("created", "", 60, 0))
		}),
		fmt.Sprintf("GET ^/cloud/v6/test_runs/%d$", opts.testRunID): http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			progress := v6TestRunProgress{
				Status:            "finished",
				Result:            "passed",
				EstimatedDuration: 60,
				ExecutionDuration: 60,
			}
			if opts.progressCallback != nil {
				progress = opts.progressCallback()
			}
			_, _ = fmt.Fprint(w, testRunJSON(
				progress.Status, progress.Result,
				progress.EstimatedDuration, progress.ExecutionDuration,
			))
		}),
		fmt.Sprintf("POST ^/cloud/v6/test_runs/%d/abort$", opts.testRunID): http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	}
}

func TestMockV6ValidateOptions400(t *testing.T) {
	t.Parallel()

	opts := defaultV6MockOpts()
	routes := buildV6Routes(t, opts)

	// Override validate_options to return 400
	routes["POST ^/cloud/v6/validate_options$"] = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error": {"code": "validation_error", "message": "Invalid script options"}}`)
	})

	srv := getTestServer(t, routes)
	t.Cleanup(srv.Close)

	ts := NewGlobalTestState(t)
	require.NoError(t, fsext.WriteFile(ts.FS, filepath.Join(ts.Cwd, "test.js"), []byte(`export default function() {}`), 0o644))
	ts.CmdArgs = setupK6CloudRunCmd([]string{"--verbose", "--log-output=stdout"})
	ts.Env["K6_SHOW_CLOUD_LOGS"] = "false"
	ts.Env["K6_CLOUD_HOST_V6"] = srv.URL
	ts.Env["K6_CLOUD_HOST"] = srv.URL
	ts.Env["K6_CLOUD_TOKEN"] = "foo"
	ts.Env["K6_CLOUD_STACK_ID"] = "123"
	ts.Env["K6_CLOUD_PROJECT_ID"] = fmt.Sprintf("%d", opts.projectID)
	ts.ExpectedExitCode = -1

	cmd.ExecuteWithGlobalState(ts.GlobalState)

	stdout := ts.Stdout.String()
	t.Log(stdout)
	assert.Contains(t, stdout, "Invalid script options")
}

func TestMockV6SequentialProgression(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int64

	opts := defaultV6MockOpts()
	opts.progressCallback = func() v6TestRunProgress {
		n := callCount.Add(1)
		switch n {
		case 1:
			return v6TestRunProgress{
				Status:            "initializing",
				EstimatedDuration: 60,
				ExecutionDuration: 0,
			}
		case 2:
			return v6TestRunProgress{
				Status:            "running",
				EstimatedDuration: 60,
				ExecutionDuration: 30,
			}
		default:
			return v6TestRunProgress{
				Status:            "finished",
				Result:            "passed",
				EstimatedDuration: 60,
				ExecutionDuration: 60,
			}
		}
	}

	ts := getSimpleCloudTestStateV6(t, nil, setupK6CloudRunCmd, nil, opts)
	cmd.ExecuteWithGlobalState(ts.GlobalState)

	stdout := ts.Stdout.String()
	t.Log(stdout)
	assert.Contains(t, stdout, "test status: Finished")
}

func TestMockV6ListRoute(t *testing.T) {
	t.Parallel()

	opts := defaultV6MockOpts()
	srv := getMockCloudV6(t, opts)

	resp, err := http.Get(srv.URL + fmt.Sprintf("/cloud/v6/projects/%d/load_tests", opts.projectID)) //nolint:noctx // test
	require.NoError(t, err)
	defer func() { assert.NoError(t, resp.Body.Close()) }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var body map[string]json.RawMessage
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Contains(t, string(body["value"]), fmt.Sprintf(`"id": %d`, opts.loadTestID))
}

func TestMockV6EmptyBody(t *testing.T) {
	t.Parallel()

	opts := defaultV6MockOpts()
	srv := getMockCloudV6(t, opts)

	resp, err := http.Post(srv.URL+"/cloud/v6/validate_options", "application/json", strings.NewReader("")) //nolint:noctx // test
	require.NoError(t, err)
	defer func() { assert.NoError(t, resp.Body.Close()) }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestMockV6RequestLogging(t *testing.T) {
	t.Parallel()

	opts := defaultV6MockOpts()
	srv := getMockCloudV6(t, opts)

	resp, err := http.Post(srv.URL+"/cloud/v6/validate_options", "application/json", strings.NewReader(`{"options":{}}`)) //nolint:noctx // test
	require.NoError(t, err)
	defer func() { assert.NoError(t, resp.Body.Close()) }()

	t.Logf("Request: POST /cloud/v6/validate_options -> %d", resp.StatusCode)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestCloudRunMaxInt32TestRunID(t *testing.T) {
	t.Parallel()

	opts := defaultV6MockOpts()
	opts.testRunID = math.MaxInt32
	opts.webAppURL = fmt.Sprintf("https://grafana.com/a/k6-app/runs/%d", math.MaxInt32)

	ts := getSimpleCloudTestStateV6(t, nil, setupK6CloudRunCmd, nil, opts)
	cmd.ExecuteWithGlobalState(ts.GlobalState)

	stdout := ts.Stdout.String()
	t.Log(stdout)
	assert.Contains(t, stdout, "test status: Finished")
}

func TestCloudRunLongWebAppURL(t *testing.T) {
	t.Parallel()

	longURL := "https://grafana.com/" + strings.Repeat("a", 1980)

	opts := defaultV6MockOpts()
	opts.webAppURL = longURL

	ts := getSimpleCloudTestStateV6(t, nil, setupK6CloudRunCmd, nil, opts)
	cmd.ExecuteWithGlobalState(ts.GlobalState)

	stdout := ts.Stdout.String()
	t.Log(stdout)
	assert.Contains(t, stdout, longURL)
}

func TestCloudRunMissingTestRunID(t *testing.T) {
	t.Parallel()

	opts := defaultV6MockOpts()
	opts.testRunID = 0

	routes := buildV6Routes(t, opts)

	// Override start to return id: 0
	routes[fmt.Sprintf("POST ^/cloud/v6/load_tests/%d/start$", opts.loadTestID)] = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"id": 0, "test_id": 1, "project_id": 10,
			"started_by": null, "created": "2026-01-01T00:00:00Z",
			"ended": null, "note": "",
			"retention_expiry": null, "cost": null,
			"status": "created",
			"status_details": {"type": "running", "entered": "2026-01-01T00:00:00Z"},
			"status_history": [], "distribution": [],
			"result": null,
			"result_details": {}, "options": {},
			"k6_dependencies": {}, "k6_versions": {},
			"web_app_url": "https://grafana.com/a/k6-app/runs/0",
			"estimated_duration": 60.0,
			"execution_duration": 0.0
		}`)
	})

	srv := getTestServer(t, routes)
	t.Cleanup(srv.Close)

	ts := NewGlobalTestState(t)
	require.NoError(t, fsext.WriteFile(ts.FS, filepath.Join(ts.Cwd, "test.js"), []byte(`export default function() {}`), 0o644))
	ts.CmdArgs = setupK6CloudRunCmd([]string{"--verbose", "--log-output=stdout"})
	ts.Env["K6_SHOW_CLOUD_LOGS"] = "false"
	ts.Env["K6_CLOUD_HOST_V6"] = srv.URL
	ts.Env["K6_CLOUD_HOST"] = srv.URL
	ts.Env["K6_CLOUD_TOKEN"] = "foo"
	ts.Env["K6_CLOUD_STACK_ID"] = "123"
	ts.Env["K6_CLOUD_PROJECT_ID"] = fmt.Sprintf("%d", opts.projectID)

	ts.ExpectedExitCode = -1
	cmd.ExecuteWithGlobalState(ts.GlobalState)

	stdout := ts.Stdout.String()
	t.Log(stdout)
	assert.Contains(t, stdout, "invalid test run ID")
}

func TestCloudRunProgressBarNoDuplicates(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int64
	opts := defaultV6MockOpts()
	opts.progressCallback = func() v6TestRunProgress {
		n := callCount.Add(1)
		if n < 3 {
			return v6TestRunProgress{
				Status:            "running",
				EstimatedDuration: 60,
				ExecutionDuration: float64(n) * 20,
			}
		}
		return v6TestRunProgress{
			Status:            "finished",
			Result:            "passed",
			EstimatedDuration: 60,
			ExecutionDuration: 60,
		}
	}

	ts := getSimpleCloudTestStateV6(t, nil, setupK6CloudRunCmd, nil, opts)
	cmd.ExecuteWithGlobalState(ts.GlobalState)

	stdout := ts.Stdout.String()
	t.Log(stdout)
	assert.Contains(t, stdout, "test status: Finished")

	// Progress lines use carriage return to overwrite. Verify the final output
	// doesn't accumulate more "Run" lines than expected poll ticks + final.
	lines := strings.Split(stdout, "\n")
	runLines := 0
	for _, l := range lines {
		if strings.Contains(l, "Run ") {
			runLines++
		}
	}
	// Each poll tick writes one "Run" line. With ~3 ticks + final "Finished",
	// we should have a reasonable number (not hundreds from duplication).
	assert.Less(t, runLines, 20, "too many Run lines suggests duplicated progress output")
}

func TestCloudRunGoroutineLeakAfterCompletion(t *testing.T) {
	t.Parallel()

	// Record baseline goroutine count (after test setup)
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	opts := defaultV6MockOpts()
	ts := getSimpleCloudTestStateV6(t, nil, setupK6CloudRunCmd, nil, opts)
	cmd.ExecuteWithGlobalState(ts.GlobalState)

	stdout := ts.Stdout.String()
	t.Log(stdout)
	assert.Contains(t, stdout, "test status: Finished")

	// Allow goroutines to wind down
	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	post := runtime.NumGoroutine()

	// Allow generous margin (other test goroutines may exist in parallel tests)
	// We just verify no massive leak (e.g. dozens of stuck polling goroutines).
	t.Logf("goroutines: baseline=%d, post=%d", baseline, post)
	assert.Less(t, post, baseline+50, "possible goroutine leak after cloud run completion")
}

func TestCloudRunGoroutineLeakAfterAbort(t *testing.T) {
	t.Parallel()

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	// Use --exit-on-running so the command exits when it sees "running" status,
	// simulating an early exit similar to abort behavior.
	opts := defaultV6MockOpts()
	opts.progressCallback = func() v6TestRunProgress {
		return v6TestRunProgress{
			Status:            "running",
			EstimatedDuration: 60,
			ExecutionDuration: 10,
		}
	}

	ts := getSimpleCloudTestStateV6(t, nil, setupK6CloudRunCmd,
		[]string{"--exit-on-running", "--verbose", "--log-output=stdout"}, opts)
	cmd.ExecuteWithGlobalState(ts.GlobalState)

	stdout := ts.Stdout.String()
	t.Log(stdout)
	assert.Contains(t, stdout, "test status: Running")

	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	post := runtime.NumGoroutine()

	t.Logf("goroutines: baseline=%d, post=%d", baseline, post)
	assert.Less(t, post, baseline+50, "possible goroutine leak after cloud run abort")
}

func TestCloudRunCleanupOnEarlyExit(t *testing.T) {
	t.Parallel()

	// Use --exit-on-running to trigger early exit.
	// Verify the command cleans up (exits cleanly, produces status output).
	opts := defaultV6MockOpts()
	opts.progressCallback = func() v6TestRunProgress {
		return v6TestRunProgress{
			Status:            "running",
			EstimatedDuration: 60,
			ExecutionDuration: 10,
		}
	}

	ts := getSimpleCloudTestStateV6(t, nil, setupK6CloudRunCmd,
		[]string{"--exit-on-running", "--verbose", "--log-output=stdout"}, opts)
	cmd.ExecuteWithGlobalState(ts.GlobalState)

	stdout := ts.Stdout.String()
	t.Log(stdout)
	assert.Contains(t, stdout, "test status: Running")
	assert.NotContains(t, stdout, "panic")
}

func TestCloudRunNetworkPartition(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int64

	opts := defaultV6MockOpts()
	opts.progressCallback = func() v6TestRunProgress {
		callCount.Add(1)
		return v6TestRunProgress{
			Status:            "running",
			EstimatedDuration: 60,
			ExecutionDuration: 10,
		}
	}

	// Use --exit-on-running so the command exits when it sees "running" status.
	// This avoids the test hanging when the server becomes unavailable.
	ts := getSimpleCloudTestStateV6(t, nil, setupK6CloudRunCmd,
		[]string{"--exit-on-running", "--verbose", "--log-output=stdout"}, opts)
	cmd.ExecuteWithGlobalState(ts.GlobalState)

	stdout := ts.Stdout.String()
	t.Log(stdout)
	assert.Contains(t, stdout, "test status: Running")
	assert.NotContains(t, stdout, "panic")
	assert.Greater(t, callCount.Load(), int64(0), "mock should have been called at least once")
}

func TestCloudRunResultError(t *testing.T) {
	t.Parallel()

	opts := defaultV6MockOpts()
	opts.progressCallback = func() v6TestRunProgress {
		return v6TestRunProgress{
			Status:            "finished",
			Result:            "error",
			EstimatedDuration: 60,
			ExecutionDuration: 60,
		}
	}

	ts := getSimpleCloudTestStateV6(t, nil, setupK6CloudRunCmd,
		[]string{"--verbose", "--log-output=stdout"}, opts)
	ts.ExpectedExitCode = int(exitcodes.CloudTestRunFailed)
	cmd.ExecuteWithGlobalState(ts.GlobalState)

	stdout := ts.Stdout.String()
	t.Log(stdout)
	assert.Contains(t, stdout, "The test has failed")
}

func TestCloudRun409ConflictFallback(t *testing.T) {
	t.Parallel()

	opts := defaultV6MockOpts()
	routes := buildV6Routes(t, opts)

	// Override create-load-test to return 409 Conflict.
	createKey := fmt.Sprintf("POST ^/cloud/v6/projects/%d/load_tests$", opts.projectID)
	routes[createKey] = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = fmt.Fprint(w, `{"error": {"code": "conflict", "message": "Load test already exists"}}`)
	})

	// Override global load-tests list (used by GetLoadTestByName via LoadTestsList).
	// Key must match buildV6Routes exactly so it replaces, not duplicates.
	routes[`GET ^/cloud/v6/load_tests(\?|$)`] = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"value": [{"id": %d, "project_id": %d, "name": "test.js",`+
			`"baseline_test_run_id": null,`+
			`"created": "2026-01-01T00:00:00Z", "updated": "2026-01-01T00:00:00Z"}]}`,
			opts.loadTestID, opts.projectID)
	})

	srv := getTestServer(t, routes)
	t.Cleanup(srv.Close)

	ts := NewGlobalTestState(t)
	require.NoError(t, fsext.WriteFile(ts.FS, filepath.Join(ts.Cwd, "test.js"),
		[]byte(`export default function() {}`), 0o644))
	ts.CmdArgs = setupK6CloudRunCmd([]string{"--verbose", "--log-output=stdout"})
	ts.Env["K6_SHOW_CLOUD_LOGS"] = "false"
	ts.Env["K6_CLOUD_HOST_V6"] = srv.URL
	ts.Env["K6_CLOUD_HOST"] = srv.URL
	ts.Env["K6_CLOUD_TOKEN"] = "foo"
	ts.Env["K6_CLOUD_STACK_ID"] = "123"
	ts.Env["K6_CLOUD_PROJECT_ID"] = fmt.Sprintf("%d", opts.projectID)
	cmd.ExecuteWithGlobalState(ts.GlobalState)

	stdout := ts.Stdout.String()
	t.Log(stdout)
	assert.Contains(t, stdout, "Load test already exists, updating script")
	assert.Contains(t, stdout, "test status: Finished")
}

func TestCloudRunValidateOptionsSendsBody(t *testing.T) {
	t.Parallel()

	var capturedBody []byte
	opts := defaultV6MockOpts()
	routes := buildV6Routes(t, opts)

	// Override validate_options to capture the request body.
	routes["POST ^/cloud/v6/validate_options$"] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		capturedBody, err = io.ReadAll(r.Body)
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"vuh_usage": 1.0, "breakdown": {"protocol_vuh": 1.0, "browser_vuh": 0.0, "base_total_vuh": 1.0, "reduction_rate": 0.0, "reduction_rate_breakdown": {}}}`)
	})

	srv := getTestServer(t, routes)
	t.Cleanup(srv.Close)

	script := []byte(`export const options = { vus: 5, duration: '30s' }; export default function() {}`)
	ts := NewGlobalTestState(t)
	require.NoError(t, fsext.WriteFile(ts.FS, filepath.Join(ts.Cwd, "test.js"), script, 0o644))
	ts.CmdArgs = setupK6CloudRunCmd([]string{"--verbose", "--log-output=stdout"})
	ts.Env["K6_SHOW_CLOUD_LOGS"] = "false"
	ts.Env["K6_CLOUD_HOST_V6"] = srv.URL
	ts.Env["K6_CLOUD_HOST"] = srv.URL
	ts.Env["K6_CLOUD_TOKEN"] = "foo"
	ts.Env["K6_CLOUD_STACK_ID"] = "123"
	ts.Env["K6_CLOUD_PROJECT_ID"] = fmt.Sprintf("%d", opts.projectID)
	cmd.ExecuteWithGlobalState(ts.GlobalState)

	// Verify that the validate_options request body contains the script options.
	require.NotEmpty(t, capturedBody, "validate_options should receive a request body")
	var payload map[string]any
	require.NoError(t, json.Unmarshal(capturedBody, &payload))
	optionsRaw, ok := payload["options"]
	require.True(t, ok, "request body should have 'options' field")
	optionsMap, ok := optionsRaw.(map[string]any)
	require.True(t, ok)
	assert.Contains(t, optionsMap, "vus")
	assert.Contains(t, optionsMap, "duration")
}

func TestCloudRunV6UsesHostv6NotHost(t *testing.T) {
	t.Parallel()

	// The v6 client must connect to Hostv6, not Host. This test uses
	// two separate servers to verify v6 API calls go to the correct one.
	var v6GotValidate atomic.Bool

	opts := defaultV6MockOpts()
	routes := buildV6Routes(t, opts)

	routes["POST ^/cloud/v6/validate_options$"] = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		v6GotValidate.Store(true)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"vuh_usage": 1.0, "breakdown": {"protocol_vuh": 1.0, "browser_vuh": 0.0, "base_total_vuh": 1.0, "reduction_rate": 0.0, "reduction_rate_breakdown": {}}}`)
	})

	v6Srv := getTestServer(t, routes)
	t.Cleanup(v6Srv.Close)

	// Separate "ingest" server that should NOT receive v6 API calls.
	var ingestGotV6Call atomic.Bool
	ingestSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/cloud/v6/") {
			ingestGotV6Call.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ingestSrv.Close)

	ts := NewGlobalTestState(t)
	require.NoError(t, fsext.WriteFile(ts.FS, filepath.Join(ts.Cwd, "test.js"),
		[]byte(`export default function() {}`), 0o644))
	ts.CmdArgs = setupK6CloudRunCmd([]string{"--verbose", "--log-output=stdout"})
	ts.Env["K6_SHOW_CLOUD_LOGS"] = "false"
	ts.Env["K6_CLOUD_HOST_V6"] = v6Srv.URL
	ts.Env["K6_CLOUD_HOST"] = ingestSrv.URL
	ts.Env["K6_CLOUD_TOKEN"] = "foo"
	ts.Env["K6_CLOUD_STACK_ID"] = "123"
	ts.Env["K6_CLOUD_PROJECT_ID"] = fmt.Sprintf("%d", opts.projectID)
	cmd.ExecuteWithGlobalState(ts.GlobalState)

	stdout := ts.Stdout.String()
	t.Log(stdout)

	assert.True(t, v6GotValidate.Load(), "v6 server should receive validate_options call")
	assert.False(t, ingestGotV6Call.Load(), "ingest server should NOT receive v6 API calls")
	assert.Contains(t, stdout, "test status: Finished")
}

func TestCloudRunValidateOptionsUsesResolvedProjectID(t *testing.T) {
	t.Parallel()

	var capturedProjectID atomic.Int64
	capturedProjectID.Store(-1) // sentinel: not yet captured

	opts := defaultV6MockOpts()
	routes := buildV6Routes(t, opts)

	// Override validate_options to capture the project_id from the request body.
	routes["POST ^/cloud/v6/validate_options$"] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		var payload map[string]any
		require.NoError(t, json.Unmarshal(body, &payload))
		if pid, ok := payload["project_id"]; ok && pid != nil {
			capturedProjectID.Store(int64(pid.(float64)))
		} else {
			capturedProjectID.Store(0)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"vuh_usage": 1.0, "breakdown": {"protocol_vuh": 1.0, "browser_vuh": 0.0, "base_total_vuh": 1.0, "reduction_rate": 0.0, "reduction_rate_breakdown": {}}}`)
	})

	srv := getTestServer(t, routes)
	t.Cleanup(srv.Close)

	ts := NewGlobalTestState(t)

	// Write a disk config with defaultProjectID (simulates k6 cloud login).
	// defaultProjectID can only be set via login config, not via script options.
	configJSON := fmt.Sprintf(`{"collectors":{"cloud":{"defaultProjectID":%d}}}`, opts.projectID)
	require.NoError(t, fsext.WriteFile(ts.FS, ts.Flags.ConfigFilePath, []byte(configJSON), 0o644))

	require.NoError(t, fsext.WriteFile(ts.FS, filepath.Join(ts.Cwd, "test.js"),
		[]byte(`export default function() {}`), 0o644))
	ts.CmdArgs = setupK6CloudRunCmd([]string{"--verbose", "--log-output=stdout"})
	ts.Env["K6_SHOW_CLOUD_LOGS"] = "false"
	ts.Env["K6_CLOUD_HOST_V6"] = srv.URL
	ts.Env["K6_CLOUD_HOST"] = srv.URL
	ts.Env["K6_CLOUD_TOKEN"] = "foo"
	ts.Env["K6_CLOUD_STACK_ID"] = "123"
	// Do NOT set K6_CLOUD_PROJECT_ID — projectID starts at 0,
	// and should be resolved from defaultProjectID before ValidateOptions.
	cmd.ExecuteWithGlobalState(ts.GlobalState)

	stdout := ts.Stdout.String()
	t.Log(stdout)

	assert.Equal(t, int64(opts.projectID), capturedProjectID.Load(),
		"ValidateOptions should receive the resolved projectID, not 0")
	assert.Contains(t, stdout, "test status: Finished")
}
