package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/subaud-tools/muster/internal/client"
	"github.com/subaud-tools/muster/internal/daemon"
	"github.com/subaud-tools/muster/internal/mustertest"
	"github.com/subaud-tools/muster/internal/paths"
	"github.com/subaud-tools/muster/internal/store"
)

// TestMain builds the shared binary lazily (see builtBinary) and removes its
// temp directory once every test in this package has run.
func TestMain(m *testing.M) {
	code := m.Run()
	if binPath != "" {
		_ = os.RemoveAll(filepath.Dir(binPath))
	}
	os.Exit(code)
}

// builtBinary lazily builds the real muster binary once per test process
// (not per test) and returns its path, so exit-code/output behavior is
// verified end-to-end through main() itself — the layer unit tests inside
// internal/cli can't reach, since the exit-code split (UsageError → 2,
// everything else → 1) and the bare-invocation/serve-mcp-debug help
// interception all live here.
var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

func builtBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "muster-cli-test-")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "muster")
		cmd := exec.Command("go", "build", "-o", binPath, ".")
		cmd.Dir = "."
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = err
			t.Logf("go build output:\n%s", out)
		}
	})
	if buildErr != nil {
		t.Fatalf("building muster binary: %v", buildErr)
	}
	return binPath
}

// run execs the built binary with args and returns stdout, stderr, and the
// process exit code.
func run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	bin := builtBinary(t)
	cmd := exec.Command(bin, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code = 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("running %v: %v", args, err)
		}
	}
	return outBuf.String(), errBuf.String(), code
}

// Bare `muster` is the family usage error: the short grouped usage on
// stderr, exit 2 (`muster help` is the long form, on stdout).
func TestBareInvocationIsUsageError(t *testing.T) {
	out, errOut, code := run(t)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if out != "" || !strings.HasPrefix(errOut, "usage: muster <command>") {
		t.Errorf("stdout %q, stderr %q; want the usage on stderr", out, errOut)
	}
}

func TestHelpExitsZero(t *testing.T) {
	out, _, code := run(t, "help")
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "\nTalk\n") || !strings.Contains(out, "local multi-agent coordination bus") {
		t.Errorf("stdout missing the overview or grouped usage:\n%s", out)
	}
}

func TestUnknownCommandExitsTwo(t *testing.T) {
	_, errOut, code := run(t, "bogus")
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.HasPrefix(errOut, "muster: unknown command") {
		t.Errorf("stderr = %q, want a \"muster: unknown command\" prefix", errOut)
	}
}

func TestUnknownHelpCommandExitsTwo(t *testing.T) {
	_, errOut, code := run(t, "help", "bogus")
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut, `unknown command "bogus"`) {
		t.Errorf("stderr does not name the unknown command: %q", errOut)
	}
}

func TestVersionExitsZero(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		out, _, code := run(t, args...)
		if code != 0 {
			t.Errorf("%v: exit code = %d, want 0", args, code)
		}
		if !strings.HasPrefix(out, "muster ") {
			t.Errorf("%v: stdout = %q, want it to start with \"muster \"", args, out)
		}
	}
}

// TestMainOwnedCommandHelpExitsZero covers the commands main() routes itself
// rather than through tools.App's dispatch. They are also the ones whose help
// text can silently go missing, since dispatch never sees them — a Registry
// row is the only thing that gives them help.
func TestMainOwnedCommandHelpExitsZero(t *testing.T) {
	for _, name := range []string{"serve", "mcp", "lambda", "debug"} {
		for _, flag := range []string{"-h", "--help"} {
			out, _, code := run(t, name, flag)
			if code != 0 {
				t.Errorf("%s %s: exit code = %d, want 0", name, flag, code)
			}
			if !strings.Contains(out, "Usage: muster "+name) {
				t.Errorf("%s %s: stdout missing the command's help:\n%s", name, flag, out)
			}
		}
	}
}

// TestLambdaWithoutBuildTagExplainsItself pins the untagged half of the build
// -tag indirection (lambda_off.go). `muster lambda` is advertised in usage on
// every build, so on the binary devices actually run it has to say why it
// can't serve — not fall through to "unknown command", and not link the AWS
// SDK to answer. This test binary is built untagged, so it exercises exactly
// that path.
func TestLambdaWithoutBuildTagExplainsItself(t *testing.T) {
	_, errOut, code := run(t, "lambda")
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut, "-tags lambda") {
		t.Errorf("stderr = %q, want it to name the -tags lambda rebuild", errOut)
	}
}

// runEnv is run with extra environment variables, for the backend-selection
// tests below. Each entry is "KEY=value".
func runEnv(t *testing.T, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(builtBinary(t), args...)
	cmd.Env = append(os.Environ(), env...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running %v: %v", args, err)
		}
		code = exitErr.ExitCode()
	}
	return outBuf.String(), errBuf.String(), code
}

// TestUnknownBackendIsAnErrorNotAFallback: a typo in MUSTER_BACKEND must stop
// the daemon. Falling back to local would strand the device on a private bus
// while every other device talked to the hosted one, and nothing would say so.
func TestUnknownBackendIsAnErrorNotAFallback(t *testing.T) {
	_, errOut, code := runEnv(t, []string{"MUSTER_HOME=" + t.TempDir(), "MUSTER_BACKEND=hosted"}, "serve")
	if code == 0 {
		t.Fatal("an unknown backend must not start the daemon")
	}
	if !strings.Contains(errOut, "unknown MUSTER_BACKEND") {
		t.Errorf("stderr = %q, want it to name the unknown backend", errOut)
	}
}

func TestServeRepairsHomeDirectoryMode(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, _ = runEnv(t, []string{"MUSTER_HOME=" + home, "MUSTER_BACKEND=hosted"}, "serve")
	info, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("MUSTER_HOME mode = %#o, want 0700", got)
	}
}

// TestRemoteBackendRequiresItsURL: remote mode with no endpoint has nowhere to
// forward to, and must say which variable is missing rather than start and
// fail on the first op.
func TestRemoteBackendRequiresItsURL(t *testing.T) {
	_, errOut, code := runEnv(t, []string{"MUSTER_HOME=" + t.TempDir(), "MUSTER_BACKEND=remote"}, "serve")
	if code == 0 {
		t.Fatal("remote mode without a URL must not start the daemon")
	}
	if !strings.Contains(errOut, "MUSTER_REMOTE_URL") {
		t.Errorf("stderr = %q, want it to name MUSTER_REMOTE_URL", errOut)
	}
}

// TestRemoteBackendRequiresItsToken: the URL alone is not enough, and the
// missing-token message has to name the file the operator must write.
func TestRemoteBackendRequiresItsToken(t *testing.T) {
	_, errOut, code := runEnv(t, []string{
		"MUSTER_HOME=" + t.TempDir(),
		"MUSTER_BACKEND=remote",
		"MUSTER_REMOTE_URL=https://example.invalid/",
	}, "serve")
	if code == 0 {
		t.Fatal("remote mode without a token must not start the daemon")
	}
	if !strings.Contains(errOut, "remote-token") {
		t.Errorf("stderr = %q, want it to name the token file", errOut)
	}
}

// TestSecondServeLeavesFirstDaemonSocketOwned proves that an auto-spawned
// duplicate exits before it can unlink the live daemon's socket.
func TestSecondServeLeavesFirstDaemonSocketOwned(t *testing.T) {
	// ShortHome, not t.TempDir(): the daemon socket path must stay under the
	// unix sun_path ~104-char limit, which t.TempDir() exceeds on macOS.
	home, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", home)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(paths.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	first, err := daemon.Serve(paths.SocketPath(), s, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })

	lock, err := os.OpenFile(filepath.Join(home, "serve.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) })

	bin := builtBinary(t)
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "serve")
	cmd.Env = append(os.Environ(), "MUSTER_HOME="+home, client.AutospawnEnv+"=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			t.Fatalf("second serve did not exit while the first owned serve.lock: %s", out)
		}
		t.Fatalf("second serve: %v: %s", err, out)
	}

	conn, err := net.DialTimeout("unix", paths.SocketPath(), 100*time.Millisecond)
	if err != nil {
		t.Fatalf("first daemon socket was unlinked: %v", err)
	}
	_ = conn.Close()
}

// TestSupervisedServeWaitsThenTakesOver: a serve that was not auto-spawned (the
// launchd job) must not exit while another daemon holds serve.lock, because
// launchd restarts a clean exit forever. It waits, then serves the moment the
// holder lets go.
func TestSupervisedServeWaitsThenTakesOver(t *testing.T) {
	home, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", home)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(home, "serve.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(builtBinary(t), "serve")
	cmd.Env = append(os.Environ(), "MUSTER_HOME="+home)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		<-exited
	})

	select {
	case <-exited:
		t.Fatalf("supervised serve exited while the lock was held: %s", errBuf.String())
	case <-time.After(500 * time.Millisecond):
	}

	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", paths.SocketPath(), 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("supervised serve did not take over after the lock was released: %s", errBuf.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestWatchFileFiresWhenBinaryIsReplaced: an upgrade renames a new binary over
// the old path, and the daemon must notice so the new version starts.
func TestWatchFileFiresWhenBinaryIsReplaced(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "muster")
	if err := os.WriteFile(bin, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	changed := watchFile(bin, 10*time.Millisecond)

	select {
	case <-changed:
		t.Fatal("fired before the binary changed")
	case <-time.After(50 * time.Millisecond):
	}

	next := filepath.Join(dir, "muster.new")
	if err := os.WriteFile(next, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, bin); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("did not fire after the binary was replaced")
	}
}

// TestBareInvocationUnderLambdaRuntimeRoutesToLambda pins the dispatch
// decision the provided.al2023 runtime depends on. It execs the zip's
// `bootstrap` with no arguments, so a bare invocation inside Lambda is not a
// usage error — it is the function starting. Without the AWS_LAMBDA_FUNCTION_NAME
// branch every cold start prints usage and exits 2.
//
// This test binary is built UNTAGGED, so the branch lands in lambda_off.go's
// stub. That is exactly the observable that distinguishes the two paths: the
// stub's exit code is also 2, so the assertion has to be on stderr naming the
// rebuild, not on the code.
func TestBareInvocationUnderLambdaRuntimeRoutesToLambda(t *testing.T) {
	out, errOut, code := runEnv(t, []string{"AWS_LAMBDA_FUNCTION_NAME=muster-bus"})
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut, "-tags lambda") {
		t.Errorf("stderr = %q, want the lambda-mode stub message — a bare invocation "+
			"under the Lambda runtime must route to lambda mode", errOut)
	}
	if strings.Contains(out, "muster — local multi-agent coordination bus") {
		t.Errorf("stdout printed the usage banner; the Lambda runtime would log it "+
			"and exit on every cold start:\n%s", out)
	}
}

// TestBareInvocationOutsideLambdaStillPrintsUsage is the other half: the
// AWS_LAMBDA_FUNCTION_NAME branch must not change what `muster` with no
// arguments does anywhere else. TestBareInvocationExitsTwoOnStdout covers the
// inherited environment; this pins that an EMPTY value is also not the Lambda
// runtime, since that is how the variable would most plausibly get set by
// accident.
func TestBareInvocationOutsideLambdaStillPrintsUsage(t *testing.T) {
	_, errOut, code := runEnv(t, []string{"AWS_LAMBDA_FUNCTION_NAME="})
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.HasPrefix(errOut, "usage: muster") {
		t.Errorf("stderr missing the usage:\n%s", errOut)
	}
}

func TestDebugMissingOpStillExitsNonzero(t *testing.T) {
	// Sanity check that fixing debug's -h handling didn't disturb its
	// existing "no args" error path.
	_, errOut, code := run(t, "debug")
	if code == 0 {
		t.Error("expected non-zero exit for `muster debug` with no op")
	}
	if !strings.Contains(errOut, "usage: muster debug") {
		t.Errorf("stderr = %q, want the debug usage message", errOut)
	}
}
