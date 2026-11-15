package s3

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Content-MD5 is S3's integrity header for bucket configuration, not a security use
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrConfig: an endpoint, bucket, region or credential that isn't
	// valid. The message names the field, never its value.
	ErrConfig = errors.New("s3: invalid configuration")
	// ErrKey: an object key outside the three prefixes or not made of ids.
	ErrKey = errors.New("s3: object key not allowed")
	// ErrNotFound: no such object.
	ErrNotFound = errors.New("s3: no such object")
	// ErrNoLifecycle: the bucket has no lifecycle configuration.
	ErrNoLifecycle = errors.New("s3: the bucket has no lifecycle configuration")
	// ErrTooLarge: a response larger than the client reads.
	ErrTooLarge = errors.New("s3: response too large")
)

// StatusError is an unexpected HTTP status; only the code is kept.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return "s3: unexpected status " + strconv.Itoa(e.Code) }

// uuidRE is a canonical lower-case uuid.
const uuidRE = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`

// objectKey is every key the service may touch (§6.1): ids only, in the
// three prefixes, the hospital first.
var objectKey = regexp.MustCompile(`^(quarantine/` + uuidRE + `/` + uuidRE + `|originals/` + uuidRE + `/` + uuidRE +
	`|pages/` + uuidRE + `/` + uuidRE + `/([1-9]|[1-4][0-9]|50))$`)

// AllowedKey reports whether key is one the service may read or write.
func AllowedKey(key string) bool { return objectKey.MatchString(key) }

var (
	bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	regionName = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	accessKey  = regexp.MustCompile(`^[A-Za-z0-9]{3,128}$`)
)

// MinSecretLen is the shortest secret access key accepted.
const MinSecretLen = 16

// Config is a client's configuration.
type Config struct {
	// Endpoint is scheme://host[:port] (path-style requests:
	// <endpoint>/<bucket>/<key>), with no credentials, path or query.
	Endpoint    string
	Bucket      string
	Region      string
	Credentials Credentials
	// HTTPClient is nil for the default: 60 s timeout, no proxy from the
	// environment, and no redirect followed (a redirect could carry a
	// signed request elsewhere).
	HTTPClient *http.Client
	// Now is nil for time.Now.
	Now func() time.Time
}

// Client is the object store client.
type Client struct {
	endpoint *url.URL
	bucket   string
	region   string
	creds    Credentials
	http     *http.Client
	now      func() time.Time
}

// ParseEndpoint checks an endpoint: http or https, a host, nothing else.
func ParseEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%w: endpoint", ErrConfig)
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, nil
}

// CheckNames returns the names (BUCKET, REGION, ACCESS_KEY_ID) of the
// set values that aren't valid; empty values are skipped.
func CheckNames(bucket, region, accessKeyID string) []string {
	var bad []string
	if bucket != "" && !bucketName.MatchString(bucket) {
		bad = append(bad, "BUCKET")
	}
	if region != "" && !regionName.MatchString(region) {
		bad = append(bad, "REGION")
	}
	if accessKeyID != "" && !accessKey.MatchString(accessKeyID) {
		bad = append(bad, "ACCESS_KEY_ID")
	}
	return bad
}

// CheckConfig checks everything but the HTTP client.
func CheckConfig(c Config) error {
	if _, err := ParseEndpoint(c.Endpoint); err != nil {
		return err
	}
	switch {
	case c.Bucket == "" || c.Region == "" || c.Credentials.AccessKeyID == "":
		return fmt.Errorf("%w: bucket, region and access key id are required", ErrConfig)
	case len(CheckNames(c.Bucket, c.Region, c.Credentials.AccessKeyID)) > 0:
		return fmt.Errorf("%w: %s", ErrConfig, strings.ToLower(strings.Join(CheckNames(c.Bucket, c.Region,
			c.Credentials.AccessKeyID), ", ")))
	case len(c.Credentials.Secret) < MinSecretLen:
		return fmt.Errorf("%w: secret access key", ErrConfig)
	}
	return nil
}

// New makes a client.
func New(c Config) (*Client, error) {
	if err := CheckConfig(c); err != nil {
		return nil, err
	}
	u, _ := ParseEndpoint(c.Endpoint)
	hc := c.HTTPClient
	if hc == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil
		hc = &http.Client{Timeout: 60 * time.Second, Transport: tr}
	}
	// Never follow a redirect, whatever client was given.
	h := *hc
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	now := c.Now
	if now == nil {
		now = time.Now
	}
	return &Client{endpoint: u, bucket: c.Bucket, region: c.Region, creds: c.Credentials, http: &h, now: now}, nil
}

// Bucket is the bucket's name.
func (c *Client) Bucket() string { return c.bucket }

// Region is the bucket's region.
func (c *Client) Region() string { return c.region }

// do signs and sends one request. key is "" for a bucket request; sub is a
// bucket subresource ("lifecycle", "versioning") or "".
func (c *Client) do(ctx context.Context, method, key, sub string, body []byte, hdr map[string]string) (*http.Response, error) {
	path := "/" + c.bucket
	if key != "" {
		path += "/" + key
	}
	u := *c.endpoint
	u.Path = path
	canonQuery := ""
	if sub != "" {
		u.RawQuery = sub
		canonQuery = sub + "="
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, fmt.Errorf("%w: request", ErrConfig)
	}
	req.ContentLength = int64(len(body))
	now := c.now()
	payload := sha256Hex(body)
	headers := map[string]string{"host": c.endpoint.Host, "x-amz-content-sha256": payload, "x-amz-date": amzDate(now)}
	for k, v := range hdr {
		headers[strings.ToLower(k)] = v
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + strings.TrimSpace(headers[k]) + "\n")
		if k != "host" {
			req.Header.Set(k, headers[k])
		}
	}
	signed := strings.Join(names, ";")
	canonical := method + "\n" + path + "\n" + canonQuery + "\n" + canonHeaders.String() + "\n" + signed + "\n" + payload
	sc := scope(shortDate(now), c.region)
	toSign := Algorithm + "\n" + amzDate(now) + "\n" + sc + "\n" + sha256Hex([]byte(canonical))
	sig := hmacSHA256(signingKey(c.creds.Secret, shortDate(now), c.region), toSign)
	req.Header.Set("Authorization", Algorithm+" Credential="+c.creds.AccessKeyID+"/"+sc+", SignedHeaders="+signed+
		", Signature="+fmt.Sprintf("%x", sig))
	req.Host = c.endpoint.Host
	resp, err := c.http.Do(req)
	if err != nil {
		// The transport's error can name the URL; keep only that it failed.
		return nil, errors.New("s3: the object store did not answer")
	}
	return resp, nil
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// object checks key and sends a request for it.
func (c *Client) object(ctx context.Context, method, key string, body []byte, hdr map[string]string) (*http.Response, error) {
	if !AllowedKey(key) {
		return nil, ErrKey
	}
	return c.do(ctx, method, key, "", body, hdr)
}

// Put stores body at key.
func (c *Client) Put(ctx context.Context, key string, body []byte, contentType string) error {
	resp, err := c.object(ctx, http.MethodPut, key, body, map[string]string{"content-type": contentType})
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return &StatusError{resp.StatusCode}
	}
	return nil
}

// Get returns the object's body; the caller closes it.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := c.object(ctx, http.MethodGet, key, nil, nil)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, nil
	case http.StatusNotFound:
		drain(resp)
		return nil, ErrNotFound
	}
	drain(resp)
	return nil, &StatusError{resp.StatusCode}
}

// Head returns the object's size.
func (c *Client) Head(ctx context.Context, key string) (int64, error) {
	resp, err := c.object(ctx, http.MethodHead, key, nil, nil)
	if err != nil {
		return 0, err
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.ContentLength, nil
	case http.StatusNotFound:
		return 0, ErrNotFound
	}
	return 0, &StatusError{resp.StatusCode}
}

// Delete removes the object (its current version; the bucket's lifecycle
// expires non-current ones). An object that isn't there is not an error.
func (c *Client) Delete(ctx context.Context, key string) error {
	resp, err := c.object(ctx, http.MethodDelete, key, nil, nil)
	if err != nil {
		return err
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return nil
	}
	return &StatusError{resp.StatusCode}
}

// maxConfigBytes bounds a bucket configuration read.
const maxConfigBytes = 64 << 10

// Lifecycle returns the bucket's lifecycle configuration (XML).
func (c *Client) Lifecycle(ctx context.Context) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, "", "lifecycle", nil, nil)
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrNoLifecycle
	default:
		return nil, &StatusError{resp.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxConfigBytes+1))
	if err != nil {
		return nil, errors.New("s3: cannot read the lifecycle configuration")
	}
	if len(b) > maxConfigBytes {
		return nil, ErrTooLarge
	}
	return b, nil
}

// The bucket-level writes below are for operators' tooling and the tests;
// the service never calls them, and its credential needn't allow them.

// CreateBucket creates the bucket (in the endpoint's default region).
func (c *Client) CreateBucket(ctx context.Context) error {
	return c.bucketPut(ctx, "", nil)
}

// PutVersioning switches the bucket's versioning on (§6.1).
func (c *Client) PutVersioning(ctx context.Context) error {
	return c.bucketPut(ctx, "versioning",
		[]byte(`<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>`))
}

// PutLifecycle sets the bucket's lifecycle configuration (XML).
func (c *Client) PutLifecycle(ctx context.Context, xml []byte) error {
	return c.bucketPut(ctx, "lifecycle", xml)
}

func (c *Client) bucketPut(ctx context.Context, sub string, body []byte) error {
	var hdr map[string]string
	if body != nil {
		sum := md5.Sum(body) //nolint:gosec // S3's Content-MD5 integrity header
		hdr = map[string]string{"content-md5": base64.StdEncoding.EncodeToString(sum[:]), "content-type": "application/xml"}
	}
	resp, err := c.do(ctx, http.MethodPut, "", sub, body, hdr)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return &StatusError{resp.StatusCode}
	}
	return nil
}
