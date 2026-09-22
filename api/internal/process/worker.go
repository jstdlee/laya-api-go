package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"laya-api/internal/rpc"
)

type Config struct {
	UVPath       string
	ProjectDir   string
	WorkerScript string
	SocketPath   string
	Preload      string
	Device       string
	Env          []string
}

func StartWorker(ctx context.Context, cfg Config) (*exec.Cmd, error) {
	if cfg.UVPath == "" {
		cfg.UVPath = "uv"
	}
	if cfg.ProjectDir == "" || cfg.WorkerScript == "" || cfg.SocketPath == "" {
		return nil, errors.New("worker project, script, and socket are required")
	}
	args := commandArgs(cfg)
	cmd := exec.CommandContext(ctx, cfg.UVPath, args...)
	cmd.Env = append(os.Environ(), cfg.Env...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func commandArgs(cfg Config) []string {
	args := []string{"run", "--project", cfg.ProjectDir, "python", cfg.WorkerScript, "--socket", cfg.SocketPath}
	if cfg.Preload != "" {
		args = append(args, "--preload", cfg.Preload)
	}
	if cfg.Device != "" {
		args = append(args, "--device", cfg.Device)
	}
	return args
}

func WaitReady(ctx context.Context, socketPath string) error {
	if socketPath == "" {
		return errors.New("worker socket is required")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		probeCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		client := rpc.NewClient(socketPath, 250*time.Millisecond)
		err := client.Ready(probeCtx)
		_ = client.Close()
		cancel()
		if err == nil {
			return nil
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func CleanupSocket(path string, owned bool) error {
	if !owned || path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove non-socket path %q", path)
	}
	return os.Remove(filepath.Clean(path))
}
