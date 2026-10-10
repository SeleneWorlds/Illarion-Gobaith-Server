package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func command(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func fixture(t *testing.T, writable bool) (*repository, string) {
	t.Helper()
	root := t.TempDir()
	remote, seed := filepath.Join(root, "remote.git"), filepath.Join(root, "seed")
	command(t, "init", "--bare", "--initial-branch=main", remote)
	command(t, "init", "--initial-branch=main", seed)
	write(t, filepath.Join(seed, "old.txt"), "initial")
	command(t, "-C", seed, "add", ".")
	command(t, "-C", seed, "commit", "-m", "Initial")
	command(t, "-C", seed, "remote", "add", "origin", remote)
	command(t, "-C", seed, "push", "-u", "origin", "main")
	return &repository{path: filepath.Join(root, "checkout"), url: remote, writable: writable}, seed
}

func write(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
}

func request(s *service, body, event, secret string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/illarion-gobaith", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	recorder := httptest.NewRecorder()
	s.handler().ServeHTTP(recorder, req)
	return recorder
}

const pushPayload = `{"ref":"refs/heads/main","repository":{"full_name":"SeleneWorlds/Illarion-Gobaith-Scripts"}}`

func TestWebhookAndReadiness(t *testing.T) {
	s := &service{scripts: &repository{branch: "main"}, secret: "secret", repositoryName: "SeleneWorlds/Illarion-Gobaith-Scripts", pulls: make(chan struct{}, 1)}
	ready := func() int {
		recorder := httptest.NewRecorder()
		s.handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/ready", nil))
		return recorder.Code
	}
	if ready() != 503 {
		t.Fatal("new process must not be ready")
	}
	if request(s, pushPayload, "push", "secret").Code != 503 {
		t.Fatal("must not enqueue during initialization")
	}
	s.ready.Store(true)
	if ready() != 200 {
		t.Fatal("initialized process must be ready")
	}
	cases := []struct {
		name, body, event, secret string
		code                      int
	}{
		{"bad signature", pushPayload, "push", "wrong", 401},
		{"invalid JSON", "{", "push", "secret", 400},
		{"ping", "{}", "ping", "secret", 200},
		{"other event", pushPayload, "issues", "secret", 204},
		{"other branch", strings.ReplaceAll(pushPayload, "heads/main", "heads/other"), "push", "secret", 204},
		{"other repo", strings.ReplaceAll(pushPayload, "Illarion-Gobaith-Scripts", "Illarion-Gobaith-Data"), "push", "secret", 204},
		{"deleted branch", strings.Replace(pushPayload, `{"ref"`, `{"deleted":true,"ref"`, 1), "push", "secret", 204},
		{"valid", pushPayload, "push", "secret", 202},
		{"coalesced", pushPayload, "push", "secret", 202},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := request(s, c.body, c.event, c.secret).Code; got != c.code {
				t.Fatalf("got %d, want %d", got, c.code)
			}
		})
	}
	if len(s.pulls) != 1 {
		t.Fatal("requests should coalesce")
	}
	s.secret = ""
	if request(s, pushPayload, "push", "").Code != 503 {
		t.Fatal("missing secret must disable webhooks")
	}
	fresh := &service{}
	if fresh.ready.Load() {
		t.Fatal("readiness must not survive a new service")
	}
}

func TestPersistRetriesPushAndPreservesEdits(t *testing.T) {
	r, _ := fixture(t, true)
	ctx := context.Background()
	if err := r.initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(r.path, "old.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(r.path, "new.txt"), "server edit")
	command(t, "-C", r.path, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
	if err := r.persist(ctx); err == nil {
		t.Fatal("push should fail")
	}
	if got := command(t, "-C", r.path, "rev-list", "--count", "HEAD"); got != "2" {
		t.Fatalf("edit must remain committed: %s", got)
	}
	command(t, "-C", r.path, "remote", "set-url", "origin", r.url)
	if err := r.persist(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.persist(ctx); err != nil {
		t.Fatal(err)
	}
	if got := command(t, "--git-dir", r.url, "ls-tree", "--name-only", "HEAD"); got != "new.txt" {
		t.Fatal(got)
	}
	if got := command(t, "--git-dir", r.url, "rev-list", "--count", "HEAD"); got != "2" {
		t.Fatal("empty commit was created")
	}
	write(t, filepath.Join(r.path, "new.txt"), "edits left on restart")
	if err := r.initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if got := command(t, "-C", r.path, "rev-list", "--count", "HEAD"); got != "3" {
		t.Fatal("restart edits were not saved")
	}
}

func TestScriptsPullOnlyAndDivergence(t *testing.T) {
	r, seed := fixture(t, false)
	ctx := context.Background()
	if err := r.initialize(ctx); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(seed, "old.txt"), "remote change")
	command(t, "-C", seed, "add", ".")
	command(t, "-C", seed, "commit", "-m", "Remote update")
	command(t, "-C", seed, "push")
	if err := r.pull(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.persist(ctx); err == nil {
		t.Fatal("scripts must never commit or push")
	}
	write(t, filepath.Join(r.path, "old.txt"), "local conflicting edit")
	write(t, filepath.Join(seed, "old.txt"), "another remote edit")
	command(t, "-C", seed, "add", ".")
	command(t, "-C", seed, "commit", "-m", "Conflict")
	command(t, "-C", seed, "push")
	if err := r.pull(ctx); err == nil {
		t.Fatal("conflicting local changes should block pull")
	}
	contents, _ := os.ReadFile(filepath.Join(r.path, "old.txt"))
	if string(contents) != "local conflicting edit" {
		t.Fatal("local edits were discarded")
	}
}

func TestDataRebaseRemoteChanges(t *testing.T) {
	for _, startup := range []bool{false, true} {
		t.Run(fmt.Sprintf("startup=%v", startup), func(t *testing.T) {
			r, seed := fixture(t, true)
			ctx := context.Background()
			if err := r.initialize(ctx); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(r.path, "local.txt"), "server edit")
			write(t, filepath.Join(seed, "remote.txt"), "remote edit")
			command(t, "-C", seed, "add", ".")
			command(t, "-C", seed, "commit", "-m", "Remote update")
			command(t, "-C", seed, "push")
			remoteHead := command(t, "-C", seed, "rev-parse", "HEAD")
			if startup {
				if err := r.initialize(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.persist(ctx); err != nil {
				t.Fatal(err)
			}
			if parent := command(t, "-C", r.path, "rev-parse", "HEAD^"); parent != remoteHead {
				t.Fatal("local commit was not rebased onto the remote tip")
			}
			if files := command(t, "--git-dir", r.url, "ls-tree", "--name-only", "HEAD"); files != "local.txt\nold.txt\nremote.txt" {
				t.Fatalf("missing edits: %s", files)
			}
		})
	}
}

func TestDataRebaseConflictAborts(t *testing.T) {
	r, seed := fixture(t, true)
	ctx := context.Background()
	if err := r.initialize(ctx); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(r.path, "old.txt"), "local edit")
	if err := r.commit(ctx); err != nil {
		t.Fatal(err)
	}
	localHead := command(t, "-C", r.path, "rev-parse", "HEAD")
	write(t, filepath.Join(seed, "old.txt"), "remote edit")
	command(t, "-C", seed, "add", ".")
	command(t, "-C", seed, "commit", "-m", "Remote conflict")
	command(t, "-C", seed, "push")
	remoteHead := command(t, "-C", seed, "rev-parse", "HEAD")
	for attempt := 0; attempt < 2; attempt++ {
		if err := r.persist(ctx); err == nil || !strings.Contains(err.Error(), "rebase failed and was aborted") {
			t.Fatalf("expected aborted rebase error, got %v", err)
		}
		if head := command(t, "-C", r.path, "rev-parse", "HEAD"); head != localHead {
			t.Fatal("abort did not restore local commits")
		}
		if contents, err := os.ReadFile(filepath.Join(r.path, "old.txt")); err != nil || string(contents) != "local edit" {
			t.Fatal("abort did not restore local contents")
		}
		if status := command(t, "-C", r.path, "status", "--porcelain"); status != "" {
			t.Fatalf("checkout left dirty: %s", status)
		}
		if head := command(t, "--git-dir", r.url, "rev-parse", "HEAD"); head != remoteHead {
			t.Fatal("conflicting changes were pushed")
		}
	}
}

func TestWorkerPullsWebhookAndShutsDown(t *testing.T) {
	data, _ := fixture(t, true)
	scripts, seed := fixture(t, false)
	s := &service{scripts: scripts, secret: "secret", repositoryName: "SeleneWorlds/Illarion-Gobaith-Scripts", pulls: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.run(ctx, data, time.Hour) }()
	await(t, func() bool { return s.ready.Load() })
	write(t, filepath.Join(seed, "old.txt"), "webhook update")
	command(t, "-C", seed, "add", ".")
	command(t, "-C", seed, "commit", "-m", "Webhook update")
	command(t, "-C", seed, "push")
	if code := request(s, pushPayload, "push", "secret").Code; code != 202 {
		t.Fatal(code)
	}
	await(t, func() bool {
		contents, _ := os.ReadFile(filepath.Join(scripts.path, "old.txt"))
		return string(contents) == "webhook update"
	})
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown timed out")
	}
	if s.ready.Load() {
		t.Fatal("stopped worker must not be ready")
	}
}

func await(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(fmt.Errorf("timed out waiting for worker"))
}

func TestDeployKeysAndSSHOptions(t *testing.T) {
	root := t.TempDir()
	key := filepath.Join(root, "private copy")
	if err := writeKey("private key\r\nmultiline contents\r\n", key); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(key)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("key mode: %o", info.Mode().Perm())
	}
	contents, err := os.ReadFile(key)
	if err != nil || string(contents) != "private key\nmultiline contents\n" {
		t.Fatal("multiline key contents differ")
	}
	if err := writeKey("", key); err == nil {
		t.Fatal("empty key should fail")
	}
	if err := writeKey("key without trailing newline", key); err != nil {
		t.Fatal(err)
	}
	contents, _ = os.ReadFile(key)
	if string(contents) != "key without trailing newline\n" {
		t.Fatal("missing final newline")
	}
	r := &repository{key: key, knownHosts: filepath.Join(root, "known'hosts")}
	// Execute the command through a shell as Git does, inspecting argv without SSH.
	command := strings.Replace(r.sshCommand(), "ssh ", `printf '%s\n' `, 1)
	output, err := exec.Command("sh", "-c", command).Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{key, "BatchMode=yes", "IdentitiesOnly=yes", "IdentityAgent=none", "StrictHostKeyChecking=yes", "GlobalKnownHostsFile=/dev/null", "UserKnownHostsFile=" + r.knownHosts} {
		if !strings.Contains(string(output), value+"\n") {
			t.Fatalf("missing SSH argument %q: %s", value, output)
		}
	}
	other := &repository{key: filepath.Join(root, "scripts"), knownHosts: r.knownHosts}
	if other.sshCommand() == r.sshCommand() {
		t.Fatal("repositories must select separate keys")
	}
}
