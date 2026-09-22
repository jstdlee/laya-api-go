package process

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCommandForConfig(t *testing.T) {
	cfg := Config{
		UVPath:       "/opt/uv",
		ProjectDir:   "/srv/laya",
		WorkerScript: "worker/laya_worker.py",
		SocketPath:   "/run/laya.sock",
		Preload:      "english,multilingual",
		Device:       "cpu",
	}
	got := commandArgs(cfg)
	want := []string{"run", "--project", "/srv/laya", "python", "worker/laya_worker.py", "--socket", "/run/laya.sock", "--preload", "english,multilingual", "--device", "cpu"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestStartWorkerWithFakeExecutable(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := filepath.Join(dir, "fake-uv")
	contents := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGS_FILE\"\nexit 0\n"
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		UVPath:       script,
		ProjectDir:   "/srv/laya",
		WorkerScript: "worker/laya_worker.py",
		SocketPath:   filepath.Join(dir, "worker.sock"),
		Preload:      "english,multilingual",
		Env:          []string{"ARGS_FILE=" + argsFile},
	}
	cmd, err := StartWorker(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if data, readErr := os.ReadFile(argsFile); readErr == nil {
			got := strings.Fields(string(data))
			if len(got) < 8 || got[0] != "run" || got[1] != "--project" || got[2] != "/srv/laya" {
				t.Fatalf("fake command args = %#v", got)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("fake worker command did not start")
}

func TestWaitReadyTimesOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := WaitReady(ctx, filepath.Join(t.TempDir(), "missing.sock")); err == nil {
		t.Fatal("expected readiness timeout")
	}
}

func TestCleanupSocketOnlyWhenOwned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if err := CleanupSocket(path, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unowned socket removed: %v", err)
	}
	if err := CleanupSocket(path, true); err != nil {
		t.Fatal(err)
	}
	listener.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("owned socket still exists: %v", err)
	}
}
