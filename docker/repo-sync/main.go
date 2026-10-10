package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type repository struct {
	path, url, branch, key, knownHosts string
	writable                           bool
	messages                           *commitMessages
	mu                                 sync.Mutex
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func (r *repository) git(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	prefix := []string{"-c", "safe.directory=" + r.path, "-c", "credential.helper="}
	if r.key != "" {
		prefix = append(prefix, "-c", "core.sshCommand="+r.sshCommand())
	}
	cmd := exec.CommandContext(ctx, "git", append(prefix, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (r *repository) sshCommand() string {
	return "ssh -F /dev/null -i " + shellQuote(r.key) +
		" -o BatchMode=yes -o IdentitiesOnly=yes -o IdentityAgent=none" +
		" -o StrictHostKeyChecking=yes -o GlobalKnownHostsFile=/dev/null" +
		" -o UserKnownHostsFile=" + shellQuote(r.knownHosts)
}

// Give SSH a private runtime file containing the raw multiline environment value.
func writeKey(value, destination string) error {
	key := []byte(value)
	if len(strings.TrimSpace(string(key))) == 0 {
		return fmt.Errorf("deploy key is required")
	}
	key = []byte(strings.ReplaceAll(string(key), "\r\n", "\n"))
	if key[len(key)-1] != '\n' {
		key = append(key, '\n')
	}
	return os.WriteFile(destination, key, 0600)
}

func (r *repository) commit(ctx context.Context) error {
	if !r.writable {
		return fmt.Errorf("repository is pull-only")
	}
	if _, err := r.git(ctx, "-C", r.path, "add", "--all", "--", ".", ":(top,exclude).selenelock"); err != nil {
		return err
	}
	changes, err := r.git(ctx, "-C", r.path, "diff", "--cached", "--name-only")
	if err != nil || changes == "" {
		return err
	}
	message := defaultCommitMessage
	if r.messages != nil {
		diff, generationErr := r.git(ctx, "-C", r.path, "diff", "--cached", "--no-ext-diff", "--no-textconv", "--unified=3")
		if generationErr == nil {
			message, generationErr = r.messages.generate(ctx, changes, diff)
		}
		if generationErr != nil {
			log.Printf("commit message generation failed: %v; using default message", generationErr)
			message = defaultCommitMessage
		}
	}
	_, err = r.git(ctx, "-C", r.path, "commit", "-m", message)
	return err
}

func (r *repository) pull(ctx context.Context) error {
	return r.withDirectoryLock(func() error { return r.pullLocked(ctx) })
}

// Callers hold the directory lock until Git has finished changing the checkout.
func (r *repository) pullLocked(ctx context.Context) error {
	if r.writable {
		return r.rebase(ctx)
	}
	_, err := r.git(ctx, "-C", r.path, "pull", "--ff-only", "origin", r.branch)
	return err
}

func (r *repository) withDirectoryLock(operation func() error) (err error) {
	path := filepath.Join(r.path, ".selenelock")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return fmt.Errorf("create server write lock: %w", err)
	}
	defer func() {
		if removeErr := os.Remove(path); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("remove server write lock: %w", removeErr))
		}
	}()
	if err := file.Close(); err != nil {
		return fmt.Errorf("close server write lock: %w", err)
	}
	return operation()
}

func (r *repository) rebase(ctx context.Context) error {
	gitDir, err := r.git(ctx, "-C", r.path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	inProgress := func() bool {
		for _, name := range []string{"rebase-merge", "rebase-apply"} {
			if _, err := os.Stat(filepath.Join(gitDir, name)); err == nil {
				return true
			}
		}
		return false
	}
	if inProgress() {
		return fmt.Errorf("rebase already in progress in %s; resolve it manually", r.path)
	}
	if _, err := r.git(ctx, "-C", r.path, "fetch", "origin", "refs/heads/"+r.branch); err != nil {
		return err
	}
	if _, err := r.git(ctx, "-C", r.path, "rebase", "--no-autostash", "FETCH_HEAD"); err != nil {
		if inProgress() {
			// Cleanup must still run when the original operation was cancelled.
			if _, abortErr := r.git(context.WithoutCancel(ctx), "-C", r.path, "rebase", "--abort"); abortErr != nil {
				return fmt.Errorf("rebase failed: %w; abort failed: %v", err, abortErr)
			}
			return fmt.Errorf("rebase failed and was aborted: %w", err)
		}
		return fmt.Errorf("rebase failed: %w", err)
	}
	return nil
}

func (r *repository) initialize(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := os.Stat(filepath.Join(r.path, ".git")); os.IsNotExist(err) {
		args := []string{"clone"}
		if r.branch != "" {
			args = append(args, "--branch", r.branch, "--single-branch")
		}
		args = append(args, "--", r.url, r.path)
		if _, err := r.git(ctx, args...); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return r.withDirectoryLock(func() error { return r.initializeLocked(ctx) })
}

func (r *repository) initializeLocked(ctx context.Context) error {
	branch, err := r.git(ctx, "-C", r.path, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return err
	}
	if r.branch != "" && r.branch != branch {
		return fmt.Errorf("checkout branch %q differs from configured branch %q", branch, r.branch)
	}
	r.branch = branch
	if r.writable {
		for key, value := range map[string]string{"user.name": env("GIT_AUTHOR_NAME", "Selene Server"), "user.email": env("GIT_AUTHOR_EMAIL", "selene@localhost")} {
			if _, err := r.git(ctx, "-C", r.path, "config", key, value); err != nil {
				return err
			}
		}
		if err := r.commit(ctx); err != nil {
			return err
		}
	}
	return r.pullLocked(ctx)
}

func (r *repository) persist(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.withDirectoryLock(func() error { return r.persistLocked(ctx) })
}

func (r *repository) persistLocked(ctx context.Context) error {
	if err := r.commit(ctx); err != nil {
		return err
	}
	// Always push: a previous attempt may have committed successfully but failed to push.
	_, err := r.git(ctx, "-C", r.path, "push", "origin", "HEAD:refs/heads/"+r.branch)
	if err == nil {
		return nil
	}
	if rebaseErr := r.rebase(ctx); rebaseErr != nil {
		return fmt.Errorf("push failed: %v; sync failed: %w", err, rebaseErr)
	}
	_, err = r.git(ctx, "-C", r.path, "push", "origin", "HEAD:refs/heads/"+r.branch)
	return err
}

type service struct {
	ready                  atomic.Bool
	scripts                *repository
	secret, repositoryName string
	pulls                  chan struct{}
}

func (s *service) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		if !s.ready.Load() {
			http.Error(w, "initial sync pending", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /webhooks/illarion-gobaith", s.webhook)
	return mux
}

func (s *service) webhook(w http.ResponseWriter, r *http.Request) {
	if s.secret == "" {
		http.Error(w, "webhook is disabled", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		http.Error(w, "invalid or oversized payload", http.StatusRequestEntityTooLarge)
		return
	}
	signature, err := hex.DecodeString(strings.TrimPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256="))
	mac := hmac.New(sha256.New, []byte(s.secret))
	mac.Write(body)
	if err != nil || !strings.HasPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256=") || !hmac.Equal(signature, mac.Sum(nil)) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	if event == "ping" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if event != "push" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var payload struct {
		Ref        string `json:"ref"`
		Deleted    bool   `json:"deleted"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if !s.ready.Load() {
		http.Error(w, "initial sync pending", http.StatusServiceUnavailable)
		return
	}
	if payload.Repository.FullName != s.repositoryName || payload.Ref != "refs/heads/"+s.scripts.branch || payload.Deleted {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Coalesce bursts. The worker fetches the latest branch tip, not a payload-supplied URL or commit.
	select {
	case s.pulls <- struct{}{}:
	default:
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *service) run(ctx context.Context, data *repository, interval time.Duration) {
	for {
		err := data.initialize(ctx)
		if err == nil {
			err = s.scripts.initialize(ctx)
		}
		if err == nil {
			break
		}
		log.Printf("initial sync failed: %v; retrying in 10s", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
	s.ready.Store(true)
	log.Print("initial sync complete; ready")
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := data.persist(ctx); err != nil {
					log.Printf("data persistence failed (will retry): %v", err)
				}
			}
		}
	}()
	defer workers.Wait()
	for {
		select {
		case <-ctx.Done():
			s.ready.Store(false)
			return
		case <-s.pulls:
			// Retry failures without requiring another webhook delivery.
			for {
				s.scripts.mu.Lock()
				err := s.scripts.pull(ctx)
				s.scripts.mu.Unlock()
				if err == nil {
					log.Print("illarion-gobaith pull complete; restart Selene to load updated scripts")
					break
				}
				log.Printf("scripts pull failed (will retry): %v", err)
				select {
				case <-ctx.Done():
					s.ready.Store(false)
					return
				case <-time.After(10 * time.Second):
				}
			}
		}
	}
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			client := &http.Client{Timeout: time.Second}
			response, err := client.Get("http://127.0.0.1:8080/ready")
			if err != nil {
				os.Exit(1)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				os.Exit(1)
			}
			return
		default:
			log.Fatal("unknown command")
		}
	}
	seconds, err := strconv.Atoi(env("SYNC_INTERVAL_SECONDS", "300"))
	if err != nil || seconds <= 0 || seconds > 86400 {
		log.Fatal("SYNC_INTERVAL_SECONDS must be between 1 and 86400")
	}
	keyDirectory, err := os.MkdirTemp("", "repo-sync-keys-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(keyDirectory)
	dataKey, scriptsKey := filepath.Join(keyDirectory, "data"), filepath.Join(keyDirectory, "scripts")
	if err := writeKey(os.Getenv("DATA_SSH_KEY"), dataKey); err != nil {
		os.RemoveAll(keyDirectory)
		log.Fatalf("DATA_SSH_KEY: %v", err)
	}
	if err := writeKey(os.Getenv("SCRIPTS_SSH_KEY"), scriptsKey); err != nil {
		os.RemoveAll(keyDirectory)
		log.Fatalf("SCRIPTS_SSH_KEY: %v", err)
	}
	// Git and SSH subprocesses only need key paths, not the original secret values.
	os.Unsetenv("DATA_SSH_KEY")
	os.Unsetenv("SCRIPTS_SSH_KEY")
	var messages *commitMessages
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		messages = &commitMessages{key: key, model: env("OPENAI_MODEL", "gpt-4.1-mini"), client: &http.Client{Timeout: 30 * time.Second}}
	}
	os.Unsetenv("OPENAI_API_KEY")
	knownHosts := env("SSH_KNOWN_HOSTS_FILE", "/etc/repo-sync/known_hosts")
	data := &repository{path: "/data", url: env("DATA_REPOSITORY", "git@github.com:SeleneWorlds/Illarion-Gobaith-Data.git"), branch: os.Getenv("DATA_BRANCH"), key: dataKey, knownHosts: knownHosts, writable: true}
	data.messages = messages
	scripts := &repository{path: "/scripts", url: env("SCRIPTS_REPOSITORY", "git@github.com:SeleneWorlds/Illarion-Gobaith-Scripts.git"), branch: os.Getenv("SCRIPTS_BRANCH"), key: scriptsKey, knownHosts: knownHosts}
	s := &service{scripts: scripts, secret: os.Getenv("WEBHOOK_SECRET"), repositoryName: "SeleneWorlds/Illarion-Gobaith-Scripts", pulls: make(chan struct{}, 1)}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	server := &http.Server{Addr: ":8080", Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	done := make(chan struct{})
	go func() { defer close(done); s.run(ctx, data, time.Duration(seconds)*time.Second) }()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	log.Print("repository sync listening on :8080")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	<-done
}
