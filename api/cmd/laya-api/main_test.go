package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseConfigDefaultsFromProjectExecutable(t *testing.T) {
	project := t.TempDir()
	if err := writeProjectFixture(project); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseConfig(nil, filepath.Join(project, "api", "laya-api"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.host != "127.0.0.1" || cfg.port != 8011 {
		t.Fatalf("listen defaults = %s:%d", cfg.host, cfg.port)
	}
	if cfg.projectDir != project {
		t.Fatalf("project dir = %q, want %q", cfg.projectDir, project)
	}
	if cfg.workerScript != filepath.Join(project, "worker", "laya_worker.py") {
		t.Fatalf("worker script = %q", cfg.workerScript)
	}
	if cfg.preload != "english,multilingual" || cfg.workerTimeout != 60*time.Second {
		t.Fatalf("worker defaults = preload %q timeout %s", cfg.preload, cfg.workerTimeout)
	}
}

func TestParseConfigOverridesAndValidation(t *testing.T) {
	cfg, err := parseConfig([]string{
		"--host", "0.0.0.0",
		"--port", "9123",
		"--project-dir", "/srv/laya",
		"--worker-script", "custom/worker.py",
		"--worker-socket", "/run/user/1000/laya.sock",
		"--uv", "/opt/uv",
		"--preload", "english",
		"--device", "cpu",
		"--worker-timeout", "3s",
		"--no-spawn-worker",
	}, "/tmp/laya-api")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.host != "0.0.0.0" || cfg.port != 9123 || cfg.uvPath != "/opt/uv" || cfg.device != "cpu" {
		t.Fatalf("overrides not applied: %#v", cfg)
	}
	if cfg.workerScript != "/srv/laya/custom/worker.py" || !cfg.noSpawnWorker {
		t.Fatalf("worker overrides = %#v", cfg)
	}
	if cfg.workerTimeout != 3*time.Second {
		t.Fatalf("timeout = %s", cfg.workerTimeout)
	}
	if _, err := parseConfig([]string{"--port", "0"}, "/tmp/laya-api"); err == nil {
		t.Fatal("expected invalid port error")
	}
}

func writeProjectFixture(project string) error {
	if err := os.MkdirAll(filepath.Join(project, "worker"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(project, "pyproject.toml"), []byte("[project]\nname='laya'\n"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(project, "worker", "laya_worker.py"), []byte("print('worker')\n"), 0o644)
}
