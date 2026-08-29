package app

import (
	"errors"
	"testing"
)

func TestListenHost(t *testing.T) {
	cases := []struct {
		name    string
		addr    string
		want    string
		wantErr error
		anyErr  bool
	}{
		{
			name:    "empty address is empty host",
			addr:    "",
			want:    "",
			wantErr: errEmptyHost,
		},
		{
			name: "port-only address is wildcard bind",
			addr: ":8787",
			want: "",
		},
		{
			name: "IPv4 host discards port",
			addr: "127.0.0.1:8787",
			want: "127.0.0.1",
		},
		{
			name: "bracketed IPv6 with port",
			addr: "[::1]:8787",
			want: "::1",
		},
		{
			name: "bare host without port",
			addr: "localhost",
			want: "localhost",
		},
		{
			name: "bracketed IPv6 without port",
			addr: "[::1]",
			want: "::1",
		},
		{
			name:    "unclosed IPv6 bracket is malformed",
			addr:    "[::1",
			want:    "",
			wantErr: errMalformedHost,
		},
		{
			name:    "lone open bracket is malformed",
			addr:    "[",
			want:    "",
			wantErr: errMalformedHost,
		},
		{
			name:    "empty brackets are malformed",
			addr:    "[]",
			want:    "",
			wantErr: errMalformedHost,
		},
		{
			name:   "bare IPv6 is dynamic error",
			addr:   "::1",
			want:   "",
			anyErr: true,
		},
		{
			name:   "colon-rich address is dynamic error",
			addr:   "a:b:c",
			want:   "",
			anyErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := listenHost(tc.addr)
			if got != tc.want {
				t.Errorf("listenHost(%q) host = %q, want %q", tc.addr, got, tc.want)
			}
			if tc.anyErr {
				if err == nil {
					t.Errorf("listenHost(%q) err = nil, want non-nil", tc.addr)
				}
				return
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("listenHost(%q) err = %v, want %v", tc.addr, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Errorf("listenHost(%q) err = %v, want nil", tc.addr, err)
			}
		})
	}
}
