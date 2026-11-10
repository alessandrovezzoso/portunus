package sip

import (
	"errors"
	"testing"
	"time"

	"github.com/emiago/diago"
	sipmsg "github.com/emiago/sipgo/sip"
)

func TestNextBackoff(t *testing.T) {
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60}
	var wait time.Duration
	for i, w := range want {
		wait = nextBackoff(wait)
		if wait != w*time.Second {
			t.Errorf("step %d: wait = %v, want %v", i, wait, w*time.Second)
		}
	}
}

func TestStatusCode(t *testing.T) {
	rejected := &diago.RegisterResponseError{RegisterRes: &sipmsg.Response{StatusCode: 403}, Msg: "403 Forbidden"}

	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"network error", errors.New("i/o timeout"), 0},
		{"rejected", rejected, 403},
		// diago returns a pointer whose type has a value-receiver Error
		// method, which go vet rejects as a %w operand, hence errors.Join.
		{"wrapped rejection", errors.Join(errors.New("register"), rejected), 403},
		{"no response", &diago.RegisterResponseError{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusCode(tt.err); got != tt.want {
				t.Errorf("statusCode = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestIsAuthRejection(t *testing.T) {
	for status, want := range map[int]bool{
		0: false, 401: true, 403: true, 407: true,
		404: false, 408: false, 500: false, 503: false,
	} {
		if got := isAuthRejection(status); got != want {
			t.Errorf("isAuthRejection(%d) = %v, want %v", status, got, want)
		}
	}
}
