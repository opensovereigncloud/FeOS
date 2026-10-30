// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The examples of AWS's documentation of Signature Version 4 for S3
// (Authenticating Requests: Using the Authorization Header).
func TestSignAWSExamples(t *testing.T) {
	c := &Client{Region: "us-east-1", AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

	get, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	get.Header.Set("Range", "bytes=0-9")
	c.sign(get, nil, at)
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := get.Header.Get("Authorization"); got != want {
		t.Errorf("GET:\n got %s\nwant %s", got, want)
	}

	body := []byte("Welcome to Amazon S3.")
	put, _ := http.NewRequest(http.MethodPut, "https://examplebucket.s3.amazonaws.com/test%24file.text", bytes.NewReader(body))
	put.Header.Set("Date", "Fri, 24 May 2013 00:00:00 GMT")
	put.Header.Set("X-Amz-Storage-Class", "REDUCED_REDUNDANCY")
	c.sign(put, body, at)
	want = "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class, " +
		"Signature=98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd"
	if got := put.Header.Get("Authorization"); got != want {
		t.Errorf("PUT:\n got %s\nwant %s", got, want)
	}
}

// fakeBucket is a path-style bucket that checks the shape of each
// request's signature.
type fakeBucket struct {
	t       *testing.T
	mu      sync.Mutex
	objects map[string][]byte
}

func (f *fakeBucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	date := r.Header.Get("X-Amz-Date")
	if len(date) != 16 || !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AK/"+date[:8]+"/auto/s3/aws4_request, SignedHeaders=") ||
		!strings.Contains(auth, "host;") || !strings.Contains(auth, "x-amz-content-sha256;x-amz-date") ||
		!strings.Contains(auth, ", Signature=") || len(auth[strings.Index(auth, "Signature=")+10:]) != 64 {
		f.t.Errorf("%s %s: bad authorization %q (date %q)", r.Method, r.URL.Path, auth, date)
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	key, ok := strings.CutPrefix(r.URL.Path, "/bucket/")
	if !ok {
		http.Error(w, "no such bucket", http.StatusNotFound)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		f.objects[key] = b
	case http.MethodGet:
		b, ok := f.objects[key]
		if !ok {
			http.Error(w, "NoSuchKey", http.StatusNotFound)
			return
		}
		_, _ = w.Write(b)
	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	}
}

func TestClient(t *testing.T) {
	f := &fakeBucket{t: t, objects: map[string][]byte{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c, err := New(srv.URL, "auto", "bucket", "AK", "SK")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := "pre/logs/t1/s_0123/0.ndjson.gz"
	if err := c.Put(ctx, key, []byte("hello"), "application/gzip"); err != nil {
		t.Fatal(err)
	}
	b, err := c.Get(ctx, key, 1<<20)
	if err != nil || string(b) != "hello" {
		t.Fatalf("Get = %q, %v", b, err)
	}
	if _, err := c.Get(ctx, key, 2); err == nil {
		t.Error("Get beyond the limit succeeded")
	}
	if err := c.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, key); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if _, err := c.Get(ctx, key, 1<<20); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete: %v", err)
	}
}

func TestNewRefusals(t *testing.T) {
	for _, tc := range []struct{ endpoint, region, bucket, ak, sk string }{
		{"ftp://x", "auto", "b", "a", "s"},
		{"https://x/path", "auto", "b", "a", "s"},
		{"https://x", "", "b", "a", "s"},
		{"https://x", "auto", "a/b", "a", "s"},
		{"https://x", "auto", "b", "", "s"},
	} {
		if _, err := New(tc.endpoint, tc.region, tc.bucket, tc.ak, tc.sk); err == nil {
			t.Errorf("New(%+v) succeeded", tc)
		}
	}
}
