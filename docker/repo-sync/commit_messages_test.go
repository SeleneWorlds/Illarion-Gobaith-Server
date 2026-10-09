package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGeneratedCommitMessages(t *testing.T) {
	for _, tc := range []struct {
		name, response, want string
		status               int
	}{
		{"generated", `{"status":"completed","output":[{"type":"reasoning"},{"type":"message","content":[{"type":"output_text","text":"Update server data"}]}]}`, "Update server data", 200},
		{"http error", `secret response`, defaultCommitMessage, 429},
		{"invalid JSON", `{`, defaultCommitMessage, 200},
		{"empty", `{"status":"completed","output":[]}`, defaultCommitMessage, 200},
		{"incomplete", `{"status":"incomplete","output":[{"type":"message","content":[{"type":"output_text","text":"Partial"}]}]}`, defaultCommitMessage, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := fixture(t, true)
			ctx := context.Background()
			calls := 0
			r.messages = &commitMessages{key: "test-key", model: "test-model", client: &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.String() != "https://api.openai.com/v1/responses" || req.Method != "POST" || req.Header.Get("Authorization") != "Bearer test-key" {
					t.Error("invalid API request")
				}
				var body struct {
					Model, Input string
					Store        bool
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body.Model != "test-model" || body.Store || !strings.Contains(body.Input, "+server edit") || !strings.Contains(body.Input, "new.txt") || !strings.Contains(body.Input, "deleted file") {
					t.Errorf("incorrect staged context: %+v", body)
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.response)), Header: make(http.Header)}, nil
			})}}
			if err := r.initialize(ctx); err != nil {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Fatal("API called without changes")
			}
			write(t, filepath.Join(r.path, "new.txt"), "server edit\n")
			command(t, "-C", r.path, "rm", "old.txt")
			if err := r.persist(ctx); err != nil {
				t.Fatal(err)
			}
			if got := command(t, "--git-dir", r.url, "log", "-1", "--format=%B"); got != tc.want {
				t.Fatalf("message = %q, want %q", got, tc.want)
			}
			if err := r.persist(ctx); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("API calls = %d", calls)
			}
		})
	}
}

func TestCommitMessageInputBounds(t *testing.T) {
	input := strings.Repeat("€", 30000)
	got := boundedText(input, 64000)
	if !strings.HasSuffix(got, "\n[truncated]") || strings.ContainsRune(got, '\ufffd') || len(got) > 64000+len("\n[truncated]") {
		t.Fatal("invalid truncation")
	}
}
