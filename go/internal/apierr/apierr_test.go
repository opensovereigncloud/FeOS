// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package apierr

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

// later is an error that asks for a retry after its duration.
type later time.Duration

func (l later) Error() string             { return "busy" }
func (l later) RetryAfter() time.Duration { return time.Duration(l) }

// RetryAfterOf takes the first RetryAfter in an error's chain, rounded up
// to seconds; without one, or with none positive, the default.
func TestRetryAfterOf(t *testing.T) {
	for _, c := range []struct {
		err  error
		want int
	}{
		{errors.New("no capacity"), 5},
		{later(time.Minute), 60},
		{fmt.Errorf("bash: ensure sandbox: %w", later(time.Minute)), 60},
		{later(1500 * time.Millisecond), 2},
		{later(0), 5},
		{nil, 5},
	} {
		if got := RetryAfterOf(c.err, 5); got != c.want {
			t.Errorf("%v: %d, want %d", c.err, got, c.want)
		}
	}
	w := httptest.NewRecorder()
	Write(w, New(502, "bash: busy").RetryAfter(RetryAfterOf(later(time.Minute), 5)))
	if w.Code != 502 || w.Header().Get("Retry-After") != "60" {
		t.Fatalf("%d Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
}
