// Package client talks to the muster daemon, spawning it if needed.
package client

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/subaud-tools/muster/internal/proto"
)

// AutospawnEnv marks a `muster serve` that a client started because the socket
// did not answer. Such a daemon exits when another one already holds the lock;
// a serve started any other way (the launchd job) waits for the lock instead,
// so a supervisor that restarts clean exits never spins.
const AutospawnEnv = "MUSTER_AUTOSPAWNED"

// Call sends one request, dialing socketPath and spawning `muster serve` if the
// socket is dead. Returns the daemon's response.
func Call(socketPath string, req proto.Request) (proto.Response, error) {
	conn, err := dialOrSpawn(socketPath)
	if err != nil {
		return proto.Response{}, err
	}
	defer func() { _ = conn.Close() }()

	line, err := json.Marshal(req)
	if err != nil {
		return proto.Response{}, err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return proto.Response{}, err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	if !sc.Scan() {
		return proto.Response{}, fmt.Errorf("no response from daemon")
	}
	var resp proto.Response
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		return proto.Response{}, err
	}
	return resp, nil
}

func dialOrSpawn(socketPath string) (net.Conn, error) {
	c, dialErr := net.Dial("unix", socketPath)
	if dialErr == nil {
		return c, nil
	}
	if os.Getenv("MUSTER_NO_AUTOSPAWN") != "" {
		// Test/debug escape hatch: os.Executable() resolves to whatever binary
		// is currently running, which under `go test` is the compiled test
		// binary, not `muster`. Spawning that with "serve" recursively re-runs
		// the whole test suite as a detached child (a real fork bomb observed
		// in CI) — any test that exercises a genuinely-unreachable daemon must
		// set this to skip the spawn and just surface the dial failure.
		return nil, dialErr
	}
	// Socket dead: spawn the daemon and wait for it to bind.
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, "serve")
	cmd.Env = append(os.Environ(), AutospawnEnv+"=1")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	_ = cmd.Process.Release()
	for i := 0; i < 50; i++ { // up to ~5s
		if c, err := net.Dial("unix", socketPath); err == nil {
			return c, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("daemon did not start within timeout")
}
