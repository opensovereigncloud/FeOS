// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package s3 is the sink's object store client: PUT, GET and DELETE of
// one object in a bucket of an S3 API (AWS, Cloudflare R2, MinIO), at
// path-style URLs (<endpoint>/<bucket>/<key>), signed with AWS Signature
// Version 4 with the standard library.
package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ErrNotFound is the error of Get for an object that does not exist.
var ErrNotFound = errors.New("object not found")

// maxError bounds the part of an error answer read for its message.
const maxError = 4 << 10

// emptySHA256 is the SHA-256 of an empty payload, in hex.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Client is a bucket. Endpoint is the API's base URL (https://…, no
// bucket in the host); Region is the signing region ("auto" for R2).
type Client struct {
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	// HTTP is the client of the requests; nil: one with a 60 s timeout.
	HTTP *http.Client
	// now is the signing clock, replaceable by tests.
	now func() time.Time
}

// New returns a client of a bucket.
func New(endpoint, region, bucket, accessKey, secretKey string) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return nil, fmt.Errorf("bucket endpoint %q is not an http(s) URL without a path", endpoint)
	}
	if region == "" || bucket == "" || strings.Contains(bucket, "/") {
		return nil, errors.New("a bucket needs a region and a name without a slash")
	}
	if accessKey == "" || secretKey == "" {
		return nil, errors.New("a bucket needs an access key and a secret key")
	}
	return &Client{
		Endpoint: strings.TrimSuffix(endpoint, "/"), Region: region, Bucket: bucket,
		AccessKey: accessKey, SecretKey: secretKey,
		HTTP: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// Put stores body at key with the content type given.
func (c *Client) Put(ctx context.Context, key string, body []byte, contentType string) error {
	h := http.Header{}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	resp, err := c.do(ctx, http.MethodPut, key, body, h)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode/100 != 2 {
		return statusError(http.MethodPut, key, resp)
	}
	return nil
}

// Get reads the object at key, at most limit bytes (an object larger is
// an error); ErrNotFound when it does not exist.
func (c *Client) Get(ctx context.Context, key string, limit int64) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, key, nil, nil)
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("GET %s: %w", key, ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(http.MethodGet, key, resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", key, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("GET %s: object larger than %d bytes", key, limit)
	}
	return b, nil
}

// Delete removes the object at key; one that does not exist is no error.
func (c *Client) Delete(ctx context.Context, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, key, nil, nil)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return statusError(http.MethodDelete, key, resp)
	}
	return nil
}

// ObjectURL is the path-style URL of key.
func (c *Client) ObjectURL(key string) string {
	return c.Endpoint + "/" + escapePath(c.Bucket) + "/" + escapePath(key)
}

func (c *Client) do(ctx context.Context, method, key string, body []byte, h http.Header) (*http.Response, error) {
	if key == "" {
		return nil, errors.New("empty object key")
	}
	req, err := http.NewRequestWithContext(ctx, method, c.ObjectURL(key), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range h {
		req.Header[k] = vs
	}
	if body == nil {
		req.Body, req.GetBody, req.ContentLength = http.NoBody, nil, 0
	}
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	c.sign(req, body, now().UTC())
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, key, err)
	}
	return resp, nil
}

// sign adds the headers of Signature Version 4 to req: x-amz-date,
// x-amz-content-sha256 (the payload's hash) and Authorization, signing
// host and every header req has.
func (c *Client) sign(req *http.Request, body []byte, t time.Time) {
	amzDate := t.Format("20060102T150405Z")
	date := t.Format("20060102")
	payload := emptySHA256
	if len(body) > 0 {
		sum := sha256.Sum256(body)
		payload = hex.EncodeToString(sum[:])
	}
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payload)

	signed := map[string]string{"host": req.URL.Host}
	for k, vs := range req.Header {
		signed[strings.ToLower(k)] = strings.TrimSpace(strings.Join(vs, ","))
	}
	names := make([]string, 0, len(signed))
	for k := range signed {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + signed[k] + "\n")
	}
	signedHeaders := strings.Join(names, ";")
	canonical := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		canonicalQuery(req.URL.Query()),
		canonHeaders.String(),
		signedHeaders,
		payload,
	}, "\n")
	scope := date + "/" + c.Region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	key := hmacSHA256([]byte("AWS4"+c.SecretKey), date)
	key = hmacSHA256(key, c.Region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKey+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+sig)
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// canonicalQuery is the query in SigV4's canonical form: sorted, every
// name and value escaped.
func canonicalQuery(q url.Values) string {
	var parts []string
	for k, vs := range q {
		for _, v := range vs {
			parts = append(parts, escape(k)+"="+escape(v))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

// escapePath escapes every segment of a key as SigV4 wants it, keeping
// the slashes.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = escape(s)
	}
	return strings.Join(segs, "/")
}

// escape is SigV4's URI encoding: everything but A-Z, a-z, 0-9, '-', '.',
// '_' and '~' as %XX, upper case.
func escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func statusError(method, key string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxError))
	return fmt.Errorf("%s %s: %s: %s", method, key, resp.Status, strings.TrimSpace(string(b)))
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}
