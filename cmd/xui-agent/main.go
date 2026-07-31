package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/qqqasdwx/xui-agent/internal/agent"
	"github.com/qqqasdwx/xui-agent/internal/config"
	updatepkg "github.com/qqqasdwx/xui-agent/internal/update"
	"github.com/qqqasdwx/xui-agent/internal/xrayruntime"
)

var version = "dev"
var commit = "unknown"
var buildDate = "unknown"

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, updatepkg.ErrRestartRequired) {
			os.Exit(75)
		}
		slog.Error("xui-agent", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "run"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		command = args[0]
		args = args[1:]
	}
	switch command {
	case "run":
		return runAgent(args)
	case "enroll":
		return enrollAgent(args)
	case "status":
		return printStatus(args)
	case "init-config":
		return initConfig(args)
	case "xray-run":
		return runManagedXray(args)
	case "version":
		fmt.Printf("xui-agent %s (commit %s, built %s)\n", version, commit, buildDate)
		return nil
	case "prepare-update":
		return prepareUpdate(args)
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

func prepareUpdate(args []string) error {
	flags := flag.NewFlagSet("prepare-update", flag.ContinueOnError)
	stateDirectory := flags.String("state-directory", "/var/lib/xui-agent", "agent state directory")
	commandID := flags.String("command-id", "", "durable update command identifier")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("prepare-update does not accept positional arguments")
	}
	if !filepath.IsAbs(*stateDirectory) {
		return errors.New("state-directory must be an absolute path")
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate candidate binary: %w", err)
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		return fmt.Errorf("read candidate binary: %w", err)
	}
	manager, err := updatepkg.NewManager(*stateDirectory, "")
	if err != nil {
		return err
	}
	prepared, err := manager.InstallLocal(context.Background(), updatepkg.Request{
		CommandID: *commandID,
		Version:   version,
	}, binary)
	if err != nil {
		return err
	}
	fmt.Printf("prepared xui-agent %s\n", prepared)
	return nil
}

func runManagedXray(args []string) error {
	flags := flag.NewFlagSet("xray-run", flag.ContinueOnError)
	configPath := flags.String("config", "/etc/xui-agent/config.json", "path to the agent configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if !cfg.Xray.Managed() {
		return errors.New("xray-run requires xray.mode=managed")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return xrayruntime.RunManagedProcess(ctx, cfg.StateDirectory, cfg.Xray.BinaryPath)
}

func runAgent(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	configPath := flags.String("config", "/etc/xui-agent/config.json", "path to the agent configuration")
	showVersion := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	client, err := agent.NewClient(cfg, version)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return client.Run(ctx)
}

func enrollAgent(args []string) error {
	flags := flag.NewFlagSet("enroll", flag.ContinueOnError)
	configPath := flags.String("config", "/etc/xui-agent/config.json", "path to the agent configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	client, err := agent.NewClient(cfg, version)
	if err != nil {
		return err
	}
	id, err := client.Enroll(context.Background(), os.Getenv("XUI_AGENT_ENROLLMENT_TOKEN"))
	if err != nil {
		return err
	}
	fmt.Printf("enrolled node %d (%s)\n", id.NodeID, id.NodeName)
	return nil
}

func printStatus(args []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	configPath := flags.String("config", "/etc/xui-agent/config.json", "path to the agent configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	client, err := agent.NewClient(cfg, version)
	if err != nil {
		return err
	}
	status, err := client.Status(context.Background())
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(status)
}

func initConfig(args []string) error {
	flags := flag.NewFlagSet("init-config", flag.ContinueOnError)
	path := flags.String("config", "/etc/xui-agent/config.json", "path to write")
	serverURL := flags.String("server-url", "", "central 3x-ui URL, including its base path")
	stateDirectory := flags.String("state-directory", "/var/lib/xui-agent", "agent state directory")
	allowInsecure := flags.Bool("allow-insecure", false, "allow plain HTTP (testing only)")
	serverCertSHA256 := flags.String("server-cert-sha256", "", "optional server certificate SHA-256 fingerprint")
	xrayBinary := flags.String("xray-binary", "/usr/local/x-ui/bin/xray-linux-amd64", "Xray binary path")
	xrayMode := flags.String("xray-mode", config.XrayModeObserve, "Xray mode: observe or managed")
	xrayConfig := flags.String("xray-config", "/usr/local/x-ui/bin/config.json", "Xray config path")
	xrayPIDFile := flags.String("xray-pid-file", "", "optional Xray PID file path")
	force := flags.Bool("force", false, "replace an existing config")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg := config.Config{
		ServerURL:        *serverURL,
		StateDirectory:   *stateDirectory,
		AllowInsecure:    *allowInsecure,
		ServerCertSHA256: *serverCertSHA256,
		Update:           config.UpdateConfig{},
		Xray: config.XrayConfig{
			Mode:       *xrayMode,
			BinaryPath: *xrayBinary,
			ConfigPath: *xrayConfig,
			PIDFile:    *xrayPIDFile,
		},
	}
	if cfg.Xray.Managed() {
		cfg.Xray.BinaryPath = config.ManagedXrayBinaryPath(*stateDirectory)
		cfg.Xray.ConfigPath = ""
		cfg.Xray.PIDFile = ""
	}
	if err := config.Write(*path, cfg, *force); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", *path)
	return nil
}
