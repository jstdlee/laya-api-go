package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"laya-api/internal/httpapi"
	"laya-api/internal/process"
	"laya-api/internal/rpc"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	return runWithArgs(os.Args[1:], os.Args[0])
}

type config struct {
	host          string
	port          int
	workerSocket  string
	projectDir    string
	workerScript  string
	uvPath        string
	preload       string
	device        string
	apiKey        string
	workerTimeout time.Duration
	noSpawnWorker bool
}

func parseConfig(args []string, executablePath string) (config, error) {
	projectDir := defaultProjectDir(executablePath)
	flags := flag.NewFlagSet("laya-api", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var cfg config
	flags.StringVar(&cfg.host, "host", "127.0.0.1", "HTTP listen host")
	flags.IntVar(&cfg.port, "port", 8011, "HTTP listen port")
	flags.StringVar(&cfg.workerSocket, "worker-socket", "/tmp/laya-api.sock", "private worker Unix socket")
	flags.StringVar(&cfg.projectDir, "project-dir", projectDir, "Python project directory used by uv")
	flags.StringVar(&cfg.workerScript, "worker-script", "", "worker script path; defaults to <project-dir>/worker/laya_worker.py")
	flags.StringVar(&cfg.uvPath, "uv", "uv", "uv executable")
	flags.StringVar(&cfg.preload, "preload", "english,multilingual", "comma-separated Laya models to preload")
	flags.StringVar(&cfg.device, "device", "", "optional Laya/PyTorch device")
	flags.StringVar(&cfg.apiKey, "api-key", "", "optional bearer token required by the public API")
	flags.DurationVar(&cfg.workerTimeout, "worker-timeout", 60*time.Second, "request and worker readiness timeout")
	flags.BoolVar(&cfg.noSpawnWorker, "no-spawn-worker", false, "do not start Python; use an already-running worker")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	if flags.NArg() != 0 {
		return config{}, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if cfg.host == "" {
		return config{}, errors.New("host must not be empty")
	}
	if cfg.port < 1 || cfg.port > 65535 {
		return config{}, fmt.Errorf("port must be between 1 and 65535")
	}
	if cfg.workerSocket == "" {
		return config{}, errors.New("worker-socket must not be empty")
	}
	if cfg.projectDir == "" {
		return config{}, errors.New("project-dir must not be empty")
	}
	if cfg.workerTimeout <= 0 {
		return config{}, errors.New("worker-timeout must be positive")
	}
	if cfg.workerScript == "" {
		cfg.workerScript = filepath.Join(cfg.projectDir, "worker", "laya_worker.py")
	} else if !filepath.IsAbs(cfg.workerScript) {
		cfg.workerScript = filepath.Join(cfg.projectDir, cfg.workerScript)
	}
	cfg.projectDir, _ = filepath.Abs(cfg.projectDir)
	cfg.workerScript, _ = filepath.Abs(cfg.workerScript)
	return cfg, nil
}

func defaultProjectDir(executablePath string) string {
	if executablePath != "" {
		if absolute, err := filepath.Abs(executablePath); err == nil {
			exeDir := filepath.Dir(absolute)
			candidate := filepath.Dir(exeDir)
			if isLayaProject(candidate) {
				return candidate
			}
		}
	}
	if cwd, err := os.Getwd(); err == nil && isLayaProject(cwd) {
		return cwd
	}
	return "."
}

func isLayaProject(path string) bool {
	_, pyprojectErr := os.Stat(filepath.Join(path, "pyproject.toml"))
	_, workerErr := os.Stat(filepath.Join(path, "worker", "laya_worker.py"))
	return pyprojectErr == nil && workerErr == nil
}

func runWithArgs(args []string, executablePath string) error {
	cfg, err := parseConfig(args, executablePath)
	if err != nil {
		return err
	}

	client := rpc.NewClient(cfg.workerSocket, cfg.workerTimeout)
	handler := httpapi.NewServer(client, cfg.apiKey, 8<<20, cfg.workerTimeout)
	httpServer := &http.Server{
		Addr:              net.JoinHostPort(cfg.host, strconv.Itoa(cfg.port)),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.workerTimeout + 10*time.Second,
		WriteTimeout:      cfg.workerTimeout + 10*time.Second,
		IdleTimeout:       60 * time.Second,
	}

	workerContext, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	var workerCmd *exec.Cmd
	workerOwnedSocket := false
	if !cfg.noSpawnWorker {
		if err := ensureSocketAvailable(cfg.workerSocket); err != nil {
			return err
		}
		workerCmd, err = process.StartWorker(workerContext, process.Config{
			UVPath:       cfg.uvPath,
			ProjectDir:   cfg.projectDir,
			WorkerScript: cfg.workerScript,
			SocketPath:   cfg.workerSocket,
			Preload:      cfg.preload,
			Device:       cfg.device,
		})
		if err != nil {
			return fmt.Errorf("start laya worker: %w", err)
		}
		workerOwnedSocket = true
	}

	serverErrors := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
			return
		}
		serverErrors <- nil
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	var serveErr error
	normalShutdown := false
	select {
	case sig := <-signals:
		_ = sig
		normalShutdown = true
	case serveErr = <-serverErrors:
	}

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	if err := httpServer.Shutdown(shutdownContext); serveErr == nil && err != nil {
		serveErr = err
	}
	_ = client.Close()
	stopWorker()
	if workerCmd != nil {
		serveErr = waitWorker(workerCmd, serveErr, normalShutdown)
	}
	if err := process.CleanupSocket(cfg.workerSocket, workerOwnedSocket); serveErr == nil && err != nil {
		serveErr = err
	}
	return serveErr
}

func ensureSocketAvailable(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("worker socket path already exists: %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect worker socket %q: %w", path, err)
	}
	return nil
}

func waitWorker(cmd *exec.Cmd, prior error, expectedStop bool) error {
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case err := <-wait:
		if prior == nil && err != nil && !expectedStop {
			return fmt.Errorf("laya worker stopped: %w", err)
		}
		return prior
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		if err := <-wait; prior == nil && err != nil && !expectedStop {
			return fmt.Errorf("laya worker stopped: %w", err)
		}
		return prior
	}
}
