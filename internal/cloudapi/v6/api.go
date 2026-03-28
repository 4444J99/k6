package cloudapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"time"

	k6cloud "github.com/grafana/k6-cloud-openapi-client-go/k6"
)

// TestRunProgress holds the progress information for a cloud test run
// fetched from the v6 API.
type TestRunProgress struct {
	Status            string
	Result            string
	WebAppURL         string
	EstimatedDuration float64
	ExecutionDuration float64
}

// IsTerminal reports whether the test run status is a terminal state.
func (p TestRunProgress) IsTerminal() bool {
	switch p.Status {
	case "finished", "timed_out", "aborted_user", "aborted_system",
		"aborted_script_error", "aborted_threshold", "aborted_limit":
		return true
	default:
		return false
	}
}

// Progress computes execution_duration / estimated_duration,
// clamped to [0, 1]. Returns 0 if estimated_duration is zero or negative.
func (p TestRunProgress) Progress() float64 {
	if p.EstimatedDuration <= 0 {
		return 0
	}
	if p.ExecutionDuration < 0 {
		return 0
	}
	return math.Min(p.ExecutionDuration/p.EstimatedDuration, 1.0)
}

// ValidateToken calls the endpoint to validate the Client's token and returns the result.
func (c *Client) ValidateToken(stackURL string) (_ *k6cloud.AuthenticationResponse, err error) {
	if stackURL == "" {
		return nil, errors.New("stack URL is required to validate token")
	}

	if _, err := url.Parse(stackURL); err != nil {
		return nil, fmt.Errorf("invalid stack URL: %w", err)
	}

	ctx := context.WithValue(context.Background(), k6cloud.ContextAccessToken, c.token)
	req := c.apiClient.AuthorizationAPI.
		Auth(ctx).
		XStackUrl(stackURL)

	resp, httpRes, rerr := req.Execute()
	defer func() {
		if httpRes != nil {
			_, _ = io.Copy(io.Discard, httpRes.Body)
			if cerr := httpRes.Body.Close(); cerr != nil && err == nil {
				err = cerr
			}
		}
	}()

	if rerr != nil {
		var apiErr *k6cloud.GenericOpenAPIError
		if !errors.As(rerr, &apiErr) {
			return nil, fmt.Errorf("failed to validate token: %w", rerr)
		}
	}

	if err := CheckResponse(httpRes); err != nil {
		return nil, fmt.Errorf("failed to validate token: %w", err)
	}

	return resp, err
}

// isRetryableStatus returns true for transient HTTP errors worth retrying.
func isRetryableStatus(code int) bool {
	return code == http.StatusBadGateway || code == http.StatusServiceUnavailable
}

// FetchTestRun calls GET /cloud/v6/test_runs/{id} and returns the test run progress.
// It extracts web_app_url, estimated_duration, and execution_duration from
// AdditionalProperties since those fields are not yet typed in the OpenAPI spec.
// Transient 502/503 errors are retried up to MaxRetries times.
func (c *Client) FetchTestRun(ctx context.Context, testRunID int32) (*TestRunProgress, error) {
	var lastErr error
	for attempt := range c.retries + 1 {
		progress, httpStatus, err := c.fetchTestRunOnce(ctx, testRunID)
		if err == nil {
			return progress, nil
		}
		if !isRetryableStatus(httpStatus) {
			return nil, err
		}
		lastErr = err
		if attempt < c.retries {
			c.logger.WithField("attempt", attempt+1).
				Warnf("Transient %d error fetching test run, retrying...", httpStatus)
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("failed to fetch test run: %w", ctx.Err())
			case <-time.After(c.retryInterval):
			}
		}
	}
	return nil, lastErr
}

func (c *Client) fetchTestRunOnce(
	ctx context.Context, testRunID int32,
) (*TestRunProgress, int, error) {
	ctx = context.WithValue(ctx, k6cloud.ContextAccessToken, c.token)
	req := c.apiClient.TestRunsAPI.
		TestRunsRetrieve(ctx, testRunID).
		XStackId(int32(c.stackID)) //nolint:gosec // stackID range is validated at login

	resp, httpRes, rerr := req.Execute()
	defer func() {
		if httpRes != nil {
			_, _ = io.Copy(io.Discard, httpRes.Body)
			_ = httpRes.Body.Close()
		}
	}()

	statusCode := 0
	if httpRes != nil {
		statusCode = httpRes.StatusCode
	}

	if rerr != nil {
		var apiErr *k6cloud.GenericOpenAPIError
		if !errors.As(rerr, &apiErr) {
			return nil, statusCode, fmt.Errorf("failed to fetch test run: %w", rerr)
		}
	}

	if err := CheckResponse(httpRes); err != nil {
		return nil, statusCode, fmt.Errorf("failed to fetch test run: %w", err)
	}

	progress := &TestRunProgress{
		Status: resp.Status,
	}

	if resp.Result.IsSet() && resp.Result.Get() != nil {
		progress.Result = *resp.Result.Get()
	}

	ap := resp.AdditionalProperties
	if v, ok := ap["web_app_url"].(string); ok {
		progress.WebAppURL = v
	}
	if v, ok := ap["estimated_duration"].(float64); ok {
		progress.EstimatedDuration = v
	}
	if v, ok := ap["execution_duration"].(float64); ok {
		progress.ExecutionDuration = v
	}

	return progress, statusCode, nil
}

// StartTestRunResponse holds the result of starting a cloud test run.
type StartTestRunResponse struct {
	TestRunID int32
	WebAppURL string
}

// ValidateOptions calls POST /cloud/v6/validate_options to validate script options.
func (c *Client) ValidateOptions(ctx context.Context, options *k6cloud.ValidateOptionsRequest) error {
	if options == nil {
		options = &k6cloud.ValidateOptionsRequest{Options: k6cloud.Options{}}
	}
	ctx = context.WithValue(ctx, k6cloud.ContextAccessToken, c.token)
	req := c.apiClient.LoadTestsAPI.
		ValidateOptions(ctx).
		XStackId(int32(c.stackID)). //nolint:gosec // stackID range is validated at login
		ValidateOptionsRequest(options)

	_, httpRes, rerr := req.Execute()
	defer func() {
		if httpRes != nil {
			_, _ = io.Copy(io.Discard, httpRes.Body)
			_ = httpRes.Body.Close()
		}
	}()

	if rerr != nil {
		var apiErr *k6cloud.GenericOpenAPIError
		if !errors.As(rerr, &apiErr) {
			return fmt.Errorf("failed to validate options: %w", rerr)
		}
	}

	if err := CheckResponse(httpRes); err != nil {
		return fmt.Errorf("failed to validate options: %w", err)
	}

	return nil
}

// CreateLoadTest calls POST /cloud/v6/projects/{pid}/load_tests to create a new load test.
func (c *Client) CreateLoadTest(
	ctx context.Context, projectID int32, name string, script io.ReadCloser,
) (int32, error) {
	ctx = context.WithValue(ctx, k6cloud.ContextAccessToken, c.token)
	req := c.apiClient.LoadTestsAPI.
		ProjectsLoadTestsCreate(ctx, projectID).
		XStackId(int32(c.stackID)). //nolint:gosec // stackID range is validated at login
		Name(name).
		Script(script)

	resp, httpRes, rerr := req.Execute()
	defer func() {
		if httpRes != nil {
			_, _ = io.Copy(io.Discard, httpRes.Body)
			_ = httpRes.Body.Close()
		}
	}()

	if rerr != nil {
		var apiErr *k6cloud.GenericOpenAPIError
		if !errors.As(rerr, &apiErr) {
			return 0, fmt.Errorf("failed to create load test: %w", rerr)
		}
	}

	if err := CheckResponse(httpRes); err != nil {
		return 0, fmt.Errorf("failed to create load test: %w", err)
	}

	return resp.Id, nil
}

// UpdateScript calls PUT /cloud/v6/load_tests/{id}/script to upload a test script.
func (c *Client) UpdateScript(ctx context.Context, loadTestID int32, script io.ReadCloser) error {
	ctx = context.WithValue(ctx, k6cloud.ContextAccessToken, c.token)
	req := c.apiClient.LoadTestsAPI.
		LoadTestsScriptUpdate(ctx, loadTestID).
		XStackId(int32(c.stackID)). //nolint:gosec // stackID range is validated at login
		Body(script)

	httpRes, rerr := req.Execute()
	defer func() {
		if httpRes != nil {
			_, _ = io.Copy(io.Discard, httpRes.Body)
			_ = httpRes.Body.Close()
		}
	}()

	if rerr != nil {
		var apiErr *k6cloud.GenericOpenAPIError
		if !errors.As(rerr, &apiErr) {
			return fmt.Errorf("failed to update script: %w", rerr)
		}
	}

	if err := CheckResponse(httpRes); err != nil {
		return fmt.Errorf("failed to update script: %w", err)
	}

	return nil
}

// StartTestRun calls POST /cloud/v6/load_tests/{id}/start to start a test run.
// It extracts web_app_url from AdditionalProperties in the response.
func (c *Client) StartTestRun(ctx context.Context, loadTestID int32) (*StartTestRunResponse, error) {
	ctx = context.WithValue(ctx, k6cloud.ContextAccessToken, c.token)
	req := c.apiClient.LoadTestsAPI.
		LoadTestsStart(ctx, loadTestID).
		XStackId(int32(c.stackID)) //nolint:gosec // stackID range is validated at login

	resp, httpRes, rerr := req.Execute()
	defer func() {
		if httpRes != nil {
			_, _ = io.Copy(io.Discard, httpRes.Body)
			_ = httpRes.Body.Close()
		}
	}()

	if rerr != nil {
		var apiErr *k6cloud.GenericOpenAPIError
		if !errors.As(rerr, &apiErr) {
			return nil, fmt.Errorf("failed to start test run: %w", rerr)
		}
	}

	if err := CheckResponse(httpRes); err != nil {
		return nil, fmt.Errorf("failed to start test run: %w", err)
	}

	if resp.Id == 0 {
		return nil, fmt.Errorf("start test run returned invalid test run ID: 0")
	}

	result := &StartTestRunResponse{
		TestRunID: resp.Id,
	}

	if v, ok := resp.AdditionalProperties["web_app_url"].(string); ok {
		result.WebAppURL = v
	}

	return result, nil
}

// GetLoadTestByName lists load tests filtered by name and returns the ID of the
// one matching the given project. It returns an error when no match is found.
//
// TODO: Switch to the project-scoped ProjectsLoadTestsRetrieve endpoint with
// .Name(name) once k6-cloud-openapi-client-go PR #22 is merged. The current
// global LoadTestsList workaround is not project-scoped, so it may return
// results from other projects and only inspects the first page of results.
func (c *Client) GetLoadTestByName(
	ctx context.Context, projectID int32, name string,
) (int32, error) {
	ctx = context.WithValue(ctx, k6cloud.ContextAccessToken, c.token)
	req := c.apiClient.LoadTestsAPI.
		LoadTestsList(ctx).
		XStackId(int32(c.stackID)). //nolint:gosec // stackID range is validated at login
		Name(name).
		Top(500)

	resp, httpRes, rerr := req.Execute()
	defer func() {
		if httpRes != nil {
			_, _ = io.Copy(io.Discard, httpRes.Body)
			_ = httpRes.Body.Close()
		}
	}()

	if rerr != nil {
		var apiErr *k6cloud.GenericOpenAPIError
		if !errors.As(rerr, &apiErr) {
			return 0, fmt.Errorf("failed to list load tests: %w", rerr)
		}
	}

	if err := CheckResponse(httpRes); err != nil {
		return 0, fmt.Errorf("failed to list load tests: %w", err)
	}

	if resp == nil {
		return 0, fmt.Errorf("failed to list load tests: empty response")
	}

	for _, lt := range resp.Value {
		if lt.ProjectId == projectID && lt.Name == name {
			return lt.Id, nil
		}
	}
	return 0, fmt.Errorf("load test %q not found in project %d", name, projectID)
}

// AbortTestRun calls POST /cloud/v6/test_runs/{id}/abort to abort a running test.
func (c *Client) AbortTestRun(ctx context.Context, testRunID int32) error {
	ctx = context.WithValue(ctx, k6cloud.ContextAccessToken, c.token)
	req := c.apiClient.TestRunsAPI.
		TestRunsAbort(ctx, testRunID).
		XStackId(int32(c.stackID)) //nolint:gosec // stackID range is validated at login

	httpRes, rerr := req.Execute()
	defer func() {
		if httpRes != nil {
			_, _ = io.Copy(io.Discard, httpRes.Body)
			_ = httpRes.Body.Close()
		}
	}()

	if rerr != nil {
		var apiErr *k6cloud.GenericOpenAPIError
		if !errors.As(rerr, &apiErr) {
			return fmt.Errorf("failed to abort test run: %w", rerr)
		}
	}

	if err := CheckResponse(httpRes); err != nil {
		return fmt.Errorf("failed to abort test run: %w", err)
	}

	return nil
}
