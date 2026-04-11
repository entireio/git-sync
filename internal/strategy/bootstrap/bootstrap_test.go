package bootstrap

import (
	"errors"
	"testing"
)

func TestIsTargetBodyLimitError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "body exceeded size limit",
			err:  errors.New("body exceeded size limit 1048576"),
			want: true,
		},
		{
			name: "case insensitive body exceeded",
			err:  errors.New("Body Exceeded Size Limit 999"),
			want: true,
		},
		{
			name: "request body too large",
			err:  errors.New("request body is too large"),
			want: true,
		},
		{
			name: "payload too large",
			err:  errors.New("payload is too large for this endpoint"),
			want: true,
		},
		{
			name: "HTTP 413",
			err:  errors.New("server returned HTTP 413"),
			want: true,
		},
		{
			name: "unrelated error",
			err:  errors.New("connection refused"),
			want: false,
		},
		{
			name: "partial match body without too large",
			err:  errors.New("request body is fine"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isTargetBodyLimitError(tt.err)
			if got != tt.want {
				t.Errorf("isTargetBodyLimitError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestTargetBodyLimit(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int64
	}{
		{
			name: "nil error",
			err:  nil,
			want: 0,
		},
		{
			name: "extracts numeric limit from error",
			err:  errors.New("body exceeded size limit 1048576"),
			want: 1048576,
		},
		{
			name: "no limit in error message",
			err:  errors.New("connection refused"),
			want: 0,
		},
		{
			name: "limit with surrounding text",
			err:  errors.New("push target refs: body exceeded size limit 536870912 bytes"),
			want: 536870912,
		},
		{
			name: "case insensitive match",
			err:  errors.New("Body Exceeded Size Limit 2097152"),
			want: 2097152,
		},
		{
			name: "no number after pattern",
			err:  errors.New("body exceeded size limit"),
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := targetBodyLimit(tt.err)
			if got != tt.want {
				t.Errorf("targetBodyLimit(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}
