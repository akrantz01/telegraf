package influxdb_v2

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/plugins/serializers/influx"
)

type APIError struct {
	StatusCode  int
	Title       string
	Description string
}

func (e APIError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("%s: %s", e.Title, e.Description)
	}
	return e.Title
}

type BucketNotFoundError struct {
	APIError
	Bucket string
}

const (
	defaultRequestTimeout   = time.Second * 5
	defaultMaxWait          = 60 // seconds
	errStringBucketNotFound = "not found: bucket"
)

// orgIDResponse is the response body from the /orgs endpoint
type orgIDResponse struct {
	Orgs []orgInfo `json:"orgs"`
}

type orgInfo struct {
	Id string `json:"id"`
}

// createBucketRequest is the payload used for creating a bucket
type createBucketRequest struct {
	Name  string `json:"name"`
	OrgID string `json:"orgID"`
	// TODO: support custom retention rule
}

type HTTPConfig struct {
	URL                *url.URL
	Token              string
	Organization       string
	Bucket             string
	BucketTag          string
	ExcludeBucketTag   bool
	SkipBucketCreation bool
	Timeout            time.Duration
	Headers            map[string]string
	Proxy              *url.URL
	UserAgent          string
	ContentEncoding    string
	TLSConfig          *tls.Config

	Serializer *influx.Serializer
}

type httpClient struct {
	ContentEncoding    string
	Timeout            time.Duration
	Headers            map[string]string
	Organization       string
	Bucket             string
	BucketTag          string
	ExcludeBucketTag   bool
	SkipBucketCreation bool

	client               *http.Client
	createBucketExecuted map[string]bool
	serializer           *influx.Serializer
	url                  *url.URL
	retryTime            time.Time
	retryCount           int
}

func NewHTTPClient(config *HTTPConfig) (*httpClient, error) {
	if config.URL == nil {
		return nil, ErrMissingURL
	}

	timeout := config.Timeout
	if timeout == 0 {
		timeout = defaultRequestTimeout
	}

	userAgent := config.UserAgent
	if userAgent == "" {
		userAgent = internal.ProductToken()
	}

	var headers = make(map[string]string, len(config.Headers)+2)
	headers["User-Agent"] = userAgent
	headers["Authorization"] = "Token " + config.Token
	for k, v := range config.Headers {
		headers[k] = v
	}

	var proxy func(*http.Request) (*url.URL, error)
	if config.Proxy != nil {
		proxy = http.ProxyURL(config.Proxy)
	} else {
		proxy = http.ProxyFromEnvironment
	}

	serializer := config.Serializer
	if serializer == nil {
		serializer = influx.NewSerializer()
	}

	var transport *http.Transport
	switch config.URL.Scheme {
	case "http", "https":
		transport = &http.Transport{
			Proxy:           proxy,
			TLSClientConfig: config.TLSConfig,
		}
	case "unix":
		transport = &http.Transport{
			Dial: func(_, _ string) (net.Conn, error) {
				return net.DialTimeout(
					config.URL.Scheme,
					config.URL.Path,
					timeout,
				)
			},
		}
	default:
		return nil, fmt.Errorf("unsupported scheme %q", config.URL.Scheme)
	}

	client := &httpClient{
		serializer: serializer,
		client: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
		createBucketExecuted: make(map[string]bool),
		url:                  config.URL,
		ContentEncoding:      config.ContentEncoding,
		Timeout:              timeout,
		Headers:              headers,
		Organization:         config.Organization,
		Bucket:               config.Bucket,
		BucketTag:            config.BucketTag,
		ExcludeBucketTag:     config.ExcludeBucketTag,
		SkipBucketCreation:   config.SkipBucketCreation,
	}
	return client, nil
}

// URL returns the origin URL that this client connects too.
func (c *httpClient) URL() string {
	return c.url.String()
}

type genericRespError struct {
	Code      string
	Message   string
	Line      *int32
	MaxLength *int32
}

func (g genericRespError) Error() string {
	errString := fmt.Sprintf("%s: %s", g.Code, g.Message)
	if g.Line != nil {
		return fmt.Sprintf("%s - line[%d]", errString, g.Line)
	} else if g.MaxLength != nil {
		return fmt.Sprintf("%s - maxlen[%d]", errString, g.MaxLength)
	}
	return errString
}

func (c *httpClient) Write(ctx context.Context, metrics []telegraf.Metric) error {
	if c.retryTime.After(time.Now()) {
		return errors.New("retry time has not elapsed")
	}

	batches := make(map[string][]telegraf.Metric)
	if c.BucketTag == "" {
		err := c.writeBatch(ctx, c.Bucket, metrics)
		if err != nil {
			return err
		}
	} else {
		for _, metric := range metrics {
			bucket, ok := metric.GetTag(c.BucketTag)
			if !ok {
				bucket = c.Bucket
			}

			if _, ok := batches[bucket]; !ok {
				batches[bucket] = make([]telegraf.Metric, 0)
			}

			if c.ExcludeBucketTag {
				// Avoid modifying the metric in case we need to retry the request.
				metric = metric.Copy()
				metric.Accept()
				metric.RemoveTag(c.BucketTag)
			}

			batches[bucket] = append(batches[bucket], metric)
		}

		for bucket, batch := range batches {
			if !c.SkipBucketCreation && !c.createBucketExecuted[bucket] {
				if err := c.CreateBucket(ctx, bucket); err != nil {
					log.Printf("W! [outputs.influxdb_v2] When writing to [%s]: bucket %q creation failed: %v\n", c.URL(), bucket, err)
				}
			}

			err := c.writeBatch(ctx, bucket, batch)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (c *httpClient) writeBatch(ctx context.Context, bucket string, metrics []telegraf.Metric) error {
	loc, err := makeWriteURL(*c.url, c.Organization, bucket)
	if err != nil {
		return err
	}

	reader, err := c.requestBodyReader(metrics)
	if err != nil {
		return err
	}
	defer reader.Close()

	req, err := c.makeWriteRequest(loc, reader)
	if err != nil {
		return err
	}

	resp, err := c.client.Do(req.WithContext(ctx))
	if err != nil {
		internal.OnClientError(c.client, err)
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case
		// this is the expected response:
		http.StatusNoContent,
		// but if we get these we should still accept it as delivered:
		http.StatusOK,
		http.StatusCreated,
		http.StatusAccepted,
		http.StatusPartialContent,
		http.StatusMultiStatus,
		http.StatusAlreadyReported:
		c.retryCount = 0
		return nil
	}

	writeResp := &genericRespError{}
	err = json.NewDecoder(resp.Body).Decode(writeResp)
	desc := writeResp.Error()
	if err != nil {
		desc = resp.Status
	}

	switch resp.StatusCode {
	case
		// request was malformed:
		http.StatusBadRequest,
		// request was too large:
		http.StatusRequestEntityTooLarge,
		// request was received but server refused to process it due to a semantic problem with the request.
		// for example, submitting metrics outside the retention period.
		// Clients should *not* repeat the request and the metrics should be dropped.
		http.StatusUnprocessableEntity,
		http.StatusNotAcceptable:
		log.Printf("E! [outputs.influxdb_v2] Failed to write metric (will be dropped: %s): %s\n", resp.Status, desc)
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("failed to write metric (%s): %s", resp.Status, desc)
	case http.StatusTooManyRequests,
		http.StatusServiceUnavailable,
		http.StatusBadGateway,
		http.StatusGatewayTimeout:
		// ^ these handle the cases where the server is likely overloaded, and may not be able to say so.
		c.retryCount++
		retryDuration := c.getRetryDuration(resp.Header)
		c.retryTime = time.Now().Add(retryDuration)
		log.Printf("W! [outputs.influxdb_v2] Failed to write; will retry in %s. (%s)\n", retryDuration, resp.Status)
		return fmt.Errorf("waiting %s for server before sending metric again", retryDuration)
	}

	if strings.Contains(desc, errStringBucketNotFound) {
		return &BucketNotFoundError{
			APIError: APIError{
				StatusCode:  resp.StatusCode,
				Title:       resp.Status,
				Description: desc,
			},
			Bucket: bucket,
		}
	}

	// if it's any other 4xx code, the client should not retry as it's the client's mistake.
	// retrying will not make the request magically work.
	if len(resp.Status) > 0 && resp.Status[0] == '4' {
		log.Printf("E! [outputs.influxdb_v2] Failed to write metric (will be dropped: %s): %s\n", resp.Status, desc)
		return nil
	}

	// This is only until platform spec is fully implemented. As of the
	// time of writing, there is no error body returned.
	if xErr := resp.Header.Get("X-Influx-Error"); xErr != "" {
		desc = fmt.Sprintf("%s; %s", desc, xErr)
	}

	return &APIError{
		StatusCode:  resp.StatusCode,
		Title:       resp.Status,
		Description: desc,
	}
}

// getOrgID retrieves the organization's ID from its name
func (c *httpClient) getOrgID(ctx context.Context) (string, error) {
	loc, err := makeOrgIDURL(*c.url, c.Organization)
	if err != nil {
		return "", err
	}

	req, err := c.makeAPIRequest("GET", loc, nil)
	if err != nil {
		return "", err
	}

	resp, err := c.client.Do(req.WithContext(ctx))
	if err != nil {
		internal.OnClientError(c.client, err)
		return "", err
	}
	defer resp.Body.Close()

	body, err := c.validateResponse(resp.Body)
	if err != nil {
		return "", &APIError{
			StatusCode:  resp.StatusCode,
			Title:       resp.Status,
			Description: "An error response was received while attempting to get the organization's ID for: " + c.Organization + ". Error: " + err.Error(),
		}
	}

	orgIDResp := &orgIDResponse{}
	if err := json.NewDecoder(body).Decode(orgIDResp); err != nil {
		return "", err
	}

	if resp.StatusCode == 200 && len(orgIDResp.Orgs) == 1 {
		return orgIDResp.Orgs[0].Id, nil
	}

	return "", fmt.Errorf("failed to get ID for org %q (do you have org-level read permissions?)", c.Organization)
}

// CreateBucket creates a new bucket in the configured organization if it doesn't already exist
func (c *httpClient) CreateBucket(ctx context.Context, bucket string) error {
	orgId, err := c.getOrgID(ctx)
	if err != nil {
		return err
	}

	loc, err := makeCreateURL(*c.url)
	if err != nil {
		return err
	}

	bodyBytes, err := json.Marshal(createBucketRequest{
		Name:  bucket,
		OrgID: orgId,
	})
	if err != nil {
		return err
	}
	body := bytes.NewBuffer(bodyBytes)

	req, err := c.makeAPIRequest("POST", loc, body)
	if err != nil {
		return err
	}

	resp, err := c.client.Do(req.WithContext(ctx))
	if err != nil {
		internal.OnClientError(c.client, err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 201 {
		c.createBucketExecuted[bucket] = true
		return nil
	}

	createResp := &genericRespError{}
	err = json.NewDecoder(resp.Body).Decode(createResp)
	desc := createResp.Error()
	if err != nil {
		desc = resp.Status
	}

	return &APIError{
		StatusCode:  resp.StatusCode,
		Title:       resp.Status,
		Description: desc,
	}
}

// Implementation from influxdb v1 output
func (c *httpClient) validateResponse(response io.ReadCloser) (io.ReadCloser, error) {
	bodyBytes, err := ioutil.ReadAll(response)
	if err != nil {
		return nil, err
	}
	defer response.Close()

	originalResponse := ioutil.NopCloser(bytes.NewBuffer(bodyBytes))

	// Empty response is valid.
	if response == http.NoBody || len(bodyBytes) == 0 || bodyBytes == nil {
		return originalResponse, nil
	}

	if valid := json.Valid(bodyBytes); !valid {
		err = errors.New(string(bodyBytes))
	}

	return originalResponse, err
}

// retryDuration takes the longer of the Retry-After header and our own back-off calculation
func (c *httpClient) getRetryDuration(headers http.Header) time.Duration {
	// basic exponential backoff (x^2)/40 (denominator to widen the slope)
	// at 40 denominator, it'll take 35 retries to hit the max defaultMaxWait of 30s
	backoff := math.Pow(float64(c.retryCount), 2) / 40

	// get any value from the header, if available
	retryAfterHeader := float64(0)
	retryAfterHeaderString := headers.Get("Retry-After")
	if len(retryAfterHeaderString) > 0 {
		var err error
		retryAfterHeader, err = strconv.ParseFloat(retryAfterHeaderString, 64)
		if err != nil {
			// there was a value but we couldn't parse it? guess minimum 10 sec
			retryAfterHeader = 10
		}
	}
	// take the highest value from both, but not over the max wait.
	retry := math.Max(backoff, retryAfterHeader)
	retry = math.Min(retry, defaultMaxWait)
	return time.Duration(retry) * time.Second
}

func (c *httpClient) makeWriteRequest(url string, body io.Reader) (*http.Request, error) {
	var err error

	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	c.addHeaders(req)

	if c.ContentEncoding == "gzip" {
		req.Header.Set("Content-Encoding", "gzip")
	}

	return req, nil
}

func (c *httpClient) makeAPIRequest(method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	c.addHeaders(req)

	if c.ContentEncoding == "gzip" {
		req.Header.Set("Content-Encoding", "gzip")
	}

	return req, nil
}

// requestBodyReader warp io.Reader from influx.NewReader to io.ReadCloser, which is usefully to fast close the write
// side of the connection in case of error
func (c *httpClient) requestBodyReader(metrics []telegraf.Metric) (io.ReadCloser, error) {
	reader := influx.NewReader(metrics, c.serializer)

	if c.ContentEncoding == "gzip" {
		rc, err := internal.CompressWithGzip(reader)
		if err != nil {
			return nil, err
		}

		return rc, nil
	}

	return ioutil.NopCloser(reader), nil
}

func (c *httpClient) addHeaders(req *http.Request) {
	for header, value := range c.Headers {
		req.Header.Set(header, value)
	}
}

func makeWriteURL(loc url.URL, org, bucket string) (string, error) {
	params := url.Values{}
	params.Set("bucket", bucket)
	params.Set("org", org)

	switch loc.Scheme {
	case "unix":
		loc.Scheme = "http"
		loc.Host = "127.0.0.1"
		loc.Path = "/api/v2/write"
	case "http", "https":
		loc.Path = path.Join(loc.Path, "/api/v2/write")
	default:
		return "", fmt.Errorf("unsupported scheme: %q", loc.Scheme)
	}
	loc.RawQuery = params.Encode()
	return loc.String(), nil
}

func makeCreateURL(loc url.URL) (string, error) {
	switch loc.Scheme {
	case "unix":
		loc.Scheme = "http"
		loc.Host = "127.0.0.1"
		loc.Path = "/api/v2/buckets"
	case "http", "https":
		loc.Path = path.Join(loc.Path, "/api/v2/buckets")
	default:
		return "", fmt.Errorf("unsupported scheme: %q", loc.Scheme)
	}
	return loc.String(), nil
}

func makeOrgIDURL(loc url.URL, orgName string) (string, error) {
	params := url.Values{}
	params.Set("org", orgName)
	params.Set("limit", "1")

	switch loc.Scheme {
	case "unix":
		loc.Scheme = "http"
		loc.Host = "127.0.0.1"
		loc.Path = "/api/v2/orgs"
	case "http", "https":
		loc.Path = path.Join(loc.Path, "/api/v2/orgs")
	default:
		return "", fmt.Errorf("unsupported scheme: %q", loc.Scheme)
	}
	loc.RawQuery = params.Encode()
	return loc.String(), nil
}

func (c *httpClient) Close() {
	c.client.CloseIdleConnections()
}
