package openapi_client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
)

// APIClient represents an OpenAPI 3.x HTTP client
type APIClient struct {
	baseURL    string
	httpClient *http.Client
	spec       *openapi3.T
	router     routers.Router
	headers    map[string]string
}

// ClientOption allows customizing the API client
type ClientOption func(*APIClient)

// NewAPIClient creates a new API client from OpenAPI 3.x specification
func NewAPIClient(ctx context.Context, specification string, baseURL string, options ...ClientOption) (*APIClient, error) {
	// Load and parse OpenAPI specification
	loader := openapi3.NewLoader()
	spec, err := loader.LoadFromData([]byte(specification))
	if err != nil {
		return nil, fmt.Errorf("failed to load OpenAPI spec: %w", err)
	}

	// Validate the specification
	if err := spec.Validate(ctx); err != nil {
		return nil, fmt.Errorf("invalid OpenAPI spec: %w", err)
	}

	// Create router
	router, err := gorillamux.NewRouter(spec)
	if err != nil {
		return nil, fmt.Errorf("failed to create router: %w", err)
	}

	client := &APIClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Transport: http.DefaultTransport,
		},
		spec:    spec,
		router:  router,
		headers: make(map[string]string),
	}

	// Apply options
	for _, option := range options {
		option(client)
	}

	return client, nil
}

// WithHeader adds a default header to all requests
func WithHeader(key, value string) ClientOption {
	return func(c *APIClient) {
		c.headers[key] = value
	}
}

// WithHTTPClient sets a custom HTTP client
func WithHTTPClient(httpClient *http.Client) ClientOption {
	return func(c *APIClient) {
		c.httpClient = httpClient
	}
}

// RequestParams holds parameters for an API request
type RequestParams struct {
	Path   map[string]string
	Query  url.Values
	Header http.Header
	Body   interface{}
}

// APIResponse wraps the HTTP response
type APIResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// Call makes an API call using the operation ID from the OpenAPI spec
func (c *APIClient) Call(ctx context.Context, operationID string, params *RequestParams) (*APIResponse, error) {
	// Find operation in spec
	var operation *openapi3.Operation
	var path string
	var method string

	for p, pathItem := range c.spec.Paths.Map() {
		for m, op := range pathItem.Operations() {
			if op.OperationID == operationID {
				operation = op
				path = p
				method = m
				break
			}
		}
		if operation != nil {
			break
		}
	}

	if operation == nil {
		return nil, fmt.Errorf("operation ID %q not found in specification", operationID)
	}

	// Build URL with path parameters
	u := c.baseURL + path
	if params != nil {
		for param, value := range params.Path {
			u = strings.ReplaceAll(u, fmt.Sprintf("{%s}", param), value)
		}
	}

	// Prepare request body
	var bodyReader io.Reader
	if params != nil && params.Body != nil {
		bodyBytes, err := json.Marshal(params.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(bodyBytes)
	}

	// Create request
	req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set query parameters
	if params != nil && len(params.Query) > 0 {
		req.URL.RawQuery = params.Query.Encode()
	}

	// Set default headers
	for key, value := range c.headers {
		req.Header.Set(key, value)
	}

	// Set request-specific headers
	if params != nil && len(params.Header) > 0 {
		for key, values := range params.Header {
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
	}

	// Set content type if body is present
	if params != nil && params.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// Validate request
	route, routeParams, err := c.router.FindRoute(req)
	if err != nil {
		return nil, fmt.Errorf("invalid route: %w", err)
	}

	requestValidationInput := &openapi3filter.RequestValidationInput{
		Request:    req,
		PathParams: routeParams,
		Route:      route,
		Options: &openapi3filter.Options{
			AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
		},
	}

	if err := openapi3filter.ValidateRequest(ctx, requestValidationInput); err != nil {
		return nil, fmt.Errorf("request validation failed: %w", err)
	}

	// Perform request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Validate response
	responseValidationInput := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: requestValidationInput,
		Status:                 resp.StatusCode,
		Header:                 resp.Header,
	}
	if len(body) > 0 {
		responseValidationInput.SetBodyBytes(body)
	}

	if err := openapi3filter.ValidateResponse(ctx, responseValidationInput); err != nil {
		return nil, fmt.Errorf("response validation failed: %w", err)
	}

	return &APIResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       body,
	}, nil
}

// UnmarshalResponse unmarshals the response body into the given target
func (r *APIResponse) UnmarshalResponse(target interface{}) error {
	return json.Unmarshal(r.Body, target)
}
