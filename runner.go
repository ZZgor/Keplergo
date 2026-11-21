package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Runner executes an external command and returns its stdout.
// Entries in env (KEY=VALUE) are added on top of the current environment.
type Runner func(ctx context.Context, env []string, name string, args ...string) ([]byte, error)

const (
	smiTimeout      = 8 * time.Second
	settingsTimeout = 12 * time.Second
)

// execRunner does not wait for a child that ignores the kill signal: a GPU
// that hangs can leave nvidia-smi stuck in the kernel, and the tool must
// still be able to report that and go on restoring defaults.
func execRunner(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return stdout.Bytes(), commandError(name, err, stderr.String(), stdout.String())
		}
		return stdout.Bytes(), nil
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("%s: %w", name, ctx.Err())
	}
}

func commandError(name string, err error, stderr, stdout string) error {
	var parts []string
	for _, s := range []string{stderr, stdout} {
		if l := firstLine(s); l != "" {
			parts = append(parts, clip(l, 200))
		}
	}
	if len(parts) == 0 {
		return fmt.Errorf("%s: %w", name, err)
	}
	return fmt.Errorf("%s: %s: %w", name, strings.Join(parts, " | "), err)
}
