package agentperm

import (
	"encoding/json"
	"testing"
)

func TestOptionJSONOmitsEmptyKind(t *testing.T) {
	b, err := json.Marshal(Option{Key: "1", Name: "Allow once"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"key":"1","name":"Allow once"}`
	if string(b) != want {
		t.Fatalf("json = %s, want %s", b, want)
	}
}

func TestOptionJSONEmitsKind(t *testing.T) {
	b, err := json.Marshal(Option{Key: "2", Name: "Always", Kind: "allow_always"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"key":"2","name":"Always","kind":"allow_always"}`
	if string(b) != want {
		t.Fatalf("json = %s, want %s", b, want)
	}
}

func TestPendingFieldShape(t *testing.T) {
	p := Pending{
		RequestID: "sess-a:4",
		Title:     "apply patch to main.go",
		ToolKind:  "edit",
		Reason:    "the file is outside the sandbox",
		Options: []Option{
			{Key: "1", Name: "Allow once", Kind: "allow"},
			{Key: "2", Name: "Reject", Kind: "reject"},
		},
	}
	// Conversion to an anonymous struct with the same exported fields is a
	// compile-time shape pin: a renamed, dropped, or extra field fails here.
	shape := struct {
		RequestID string
		Title     string
		ToolKind  string
		Reason    string
		Options   []Option
	}(p)
	if shape.RequestID != "sess-a:4" || shape.Title != "apply patch to main.go" ||
		shape.ToolKind != "edit" || shape.Reason != "the file is outside the sandbox" {
		t.Fatalf("pending fields = %+v", shape)
	}
	if len(shape.Options) != 2 || shape.Options[0] != (Option{Key: "1", Name: "Allow once", Kind: "allow"}) ||
		shape.Options[1] != (Option{Key: "2", Name: "Reject", Kind: "reject"}) {
		t.Fatalf("options = %+v", shape.Options)
	}
}
