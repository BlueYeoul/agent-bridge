package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var ErrHelp = errors.New("help requested")

type Config struct {
	RemotePath string
	Target     string
	Launch     string
	Port       int
	PortSet    bool
	LaunchArgs []string
}

func Usage() string {
	return `Usage:
  agent-bridge <remote_server_path> --target <user@hostname> --launch <agent_name> [--port <ssh_port>] [-- <agent_args>...]

Examples:
  agent-bridge ./workspace/my-project --target devbox --launch claude
  agent-bridge /srv/app --target user@example.com --port 2222 --launch aider -- --model sonnet
`
}

func Parse(args []string) (Config, error) {
	cfg := Config{Port: 22}

	for i := 0; i < len(args); i++ {
		arg := args[i]

		if arg == "--" {
			cfg.LaunchArgs = append(cfg.LaunchArgs, args[i+1:]...)
			break
		}
		if arg == "-h" || arg == "--help" {
			return Config{}, ErrHelp
		}

		if strings.HasPrefix(arg, "--target=") {
			cfg.Target = strings.TrimPrefix(arg, "--target=")
			continue
		}
		if arg == "--target" {
			i++
			if i >= len(args) {
				return Config{}, fmt.Errorf("--target requires a value")
			}
			cfg.Target = args[i]
			continue
		}

		if strings.HasPrefix(arg, "--launch=") {
			cfg.Launch = strings.TrimPrefix(arg, "--launch=")
			continue
		}
		if arg == "--launch" {
			i++
			if i >= len(args) {
				return Config{}, fmt.Errorf("--launch requires a value")
			}
			cfg.Launch = args[i]
			continue
		}

		if strings.HasPrefix(arg, "--port=") {
			port, err := parsePort(strings.TrimPrefix(arg, "--port="))
			if err != nil {
				return Config{}, err
			}
			cfg.Port = port
			cfg.PortSet = true
			continue
		}
		if arg == "--port" {
			i++
			if i >= len(args) {
				return Config{}, fmt.Errorf("--port requires a value")
			}
			port, err := parsePort(args[i])
			if err != nil {
				return Config{}, err
			}
			cfg.Port = port
			cfg.PortSet = true
			continue
		}

		if strings.HasPrefix(arg, "-") {
			return Config{}, fmt.Errorf("unknown flag %q", arg)
		}
		if cfg.RemotePath != "" {
			return Config{}, fmt.Errorf("unexpected positional argument %q", arg)
		}
		cfg.RemotePath = arg
	}

	if cfg.RemotePath == "" {
		return Config{}, fmt.Errorf("remote_server_path is required")
	}
	if cfg.Target == "" {
		return Config{}, fmt.Errorf("--target is required")
	}
	if cfg.Launch == "" {
		return Config{}, fmt.Errorf("--launch is required")
	}

	return cfg, nil
}

func parsePort(raw string) (int, error) {
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("--port must be a number between 1 and 65535")
	}
	return port, nil
}
