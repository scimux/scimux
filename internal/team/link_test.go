package team

import (
	"strings"
	"testing"
)

func TestParseJoinLinkAcceptsHTTPAndHTTPS(t *testing.T) {
	cases := []struct {
		name string
		in   string
		base string
		code string
	}{
		{"http", "http://relay.example/join#abc", "http://relay.example", "abc"},
		{"https", "https://relay.example/join#abc", "https://relay.example", "abc"},
		{"http port", "http://relay.example:8080/join#abc", "http://relay.example:8080", "abc"},
		{"https port", "https://relay.example:8080/join#abc", "https://relay.example:8080", "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, code, err := ParseJoinLink(tc.in)
			if err != nil {
				t.Fatalf("ParseJoinLink(%q) error = %v", tc.in, err)
			}
			if base != tc.base || code != tc.code {
				t.Fatalf("ParseJoinLink(%q) = %q, %q; want %q, %q", tc.in, base, code, tc.base, tc.code)
			}
		})
	}
}

func TestParseJoinLinkTrimsSurroundingSpace(t *testing.T) {
	base, code, err := ParseJoinLink(" \t http://relay.example/join#abc \n")
	if err != nil {
		t.Fatal(err)
	}
	if base != "http://relay.example" || code != "abc" {
		t.Fatalf("got %q, %q", base, code)
	}
}

func TestParseJoinLinkKeepsHyphensAndSpacesInTheCode(t *testing.T) {
	const code = "ab-cd ef GH"
	base, got, err := ParseJoinLink("http://relay.example/join#" + code)
	if err != nil {
		t.Fatal(err)
	}
	if base != "http://relay.example" || got != code {
		t.Fatalf("got base %q code %q", base, got)
	}
}

func TestParseJoinLinkLengthLimit(t *testing.T) {
	valid := "http://relay.example/join#abc"
	padded := valid + strings.Repeat(" ", 512-len(valid))
	if len(padded) != 512 {
		t.Fatalf("fixture length = %d", len(padded))
	}
	base, code, err := ParseJoinLink(padded)
	if err != nil {
		t.Fatal(err)
	}
	if base != "http://relay.example" || code != "abc" {
		t.Fatalf("512-byte input = %q, %q", base, code)
	}
	if _, _, err := ParseJoinLink(padded + " "); err == nil {
		t.Fatal("513-byte input was accepted")
	}
}

func TestParseJoinLinkRejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"scheme", "ftp://relay.example/join#abc"},
		{"empty host", "http:///join#abc"},
		{"userinfo", "http://user:secret@relay.example/join#abc"},
		{"query", "http://relay.example/join?x=1#abc"},
		{"empty query mark", "http://relay.example/join?#abc"},
		{"empty fragment", "http://relay.example/join#"},
		{"missing fragment", "http://relay.example/join"},
		{"other path", "http://relay.example/other#abc"},
		{"trailing slash", "http://relay.example/join/#abc"},
		{"space inside", "http://relay.example/join #abc"},
		{"space in host", "http://relay.example /join#abc"},
		{"empty port", "http://relay.example:/join#abc"},
		{"code too long", "http://relay.example/join#" + strings.Repeat("a", 65)},
		{"code character", "http://relay.example/join#ab_cd"},
		{"empty", ""},
		{"only space", " \n\t "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, code, err := ParseJoinLink(tc.in)
			if err == nil {
				t.Fatalf("ParseJoinLink accepted %q as %q, %q", tc.in, base, code)
			}
			if base != "" || code != "" {
				t.Fatalf("values alongside error: %q, %q", base, code)
			}
		})
	}
}

func TestParseJoinLinkErrorOmitsTheInput(t *testing.T) {
	const marker = "UNIQUE-INPUT-MARKER"
	_, _, err := ParseJoinLink("http://relay.example/join#" + marker)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "relay.example") {
		t.Fatalf("error text contains the input: %v", err)
	}
}
