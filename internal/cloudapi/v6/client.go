package cloudapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	k6cloud "github.com/grafana/k6-cloud-openapi-client-go/k6"
	"github.com/sirupsen/logrus"
)

const (
	// RetryInterval is the default cloud request retry interval
	RetryInterval = 500 * time.Millisecond
	// MaxRetries specifies max retry attempts
	MaxRetries = 3
)

// Client handles communication with the k6 Cloud API.
type Client struct {
	apiClient *k6cloud.APIClient
	token     string
	stackID   int64
	baseURL   string

	logger logrus.FieldLogger

	retries       int
	retryInterval time.Duration
}

// NewClient return a new client for the cloud API
func NewClient(logger logrus.FieldLogger, token, host, version string, timeout time.Duration) (*Client, error) {
	if token == "" {
		return nil, fmt.Errorf("token is required to create cloud API client")
	}

	cfg := &k6cloud.Configuration{
		DefaultHeader: make(map[string]string),
		UserAgent:     "k6cloud/" + version,
		Servers: k6cloud.ServerConfigurations{
			{
				URL:         host,
				Description: "Global k6 Cloud API.",
			},
		},
		OperationServers: map[string]k6cloud.ServerConfigurations{},
		HTTPClient: &http.Client{
			Timeout:   timeout,
			Transport: &idempotencyTransport{base: http.DefaultTransport},
		},
	}

	c := &Client{
		apiClient:     k6cloud.NewAPIClient(cfg),
		token:         token,
		baseURL:       fmt.Sprintf("%s/cloud/v6", host),
		retries:       MaxRetries,
		retryInterval: RetryInterval,
		logger:        logger,
	}
	return c, nil
}

// SetStackID sets the stack ID for the client.
// It returns an error if the value overflows int32.
func (c *Client) SetStackID(stackID int64) error {
	if stackID < 0 || stackID > math.MaxInt32 {
		return fmt.Errorf("stack ID %d is out of valid int32 range [0, %d]", stackID, math.MaxInt32)
	}
	c.stackID = stackID
	return nil
}

// BaseURL returns configured host.
func (c *Client) BaseURL() string {
	return c.baseURL
}

// CheckResponse checks the parsed response.
// It returns nil if the code is in the successful range,
// otherwise it tries to parse the body and return a parsed error.
func CheckResponse(r *http.Response) error {
	if r == nil {
		return errUnknown
	}

	if c := r.StatusCode; c >= 200 && c <= 299 {
		return nil
	}

	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}

	var payload ResponseError
	if err := json.Unmarshal(data, &payload); err != nil {
		if r.StatusCode == http.StatusUnauthorized {
			return errNotAuthenticated
		}
		if r.StatusCode == http.StatusForbidden {
			return errNotAuthorized
		}
		return fmt.Errorf(
			"unexpected HTTP error from %s: %d %s",
			r.Request.URL,
			r.StatusCode,
			http.StatusText(r.StatusCode),
		)
	}
	payload.Response = r
	return payload
}

const k6IdempotencyKeyHeader = "K6-Idempotency-Key"

// idempotencyTransport wraps an http.RoundTripper to inject
// a unique K6-Idempotency-Key header on mutation requests.
type idempotencyTransport struct {
	base http.RoundTripper
}

func (t *idempotencyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		// read-only — no idempotency key needed
	default:
		if req.Header.Get(k6IdempotencyKeyHeader) == "" {
			req.Header.Set(k6IdempotencyKeyHeader, randomStrHex())
		}
	}
	return t.base.RoundTrip(req)
}

// randomStrHex returns a 16-character hex string from crypto/rand.
func randomStrHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
