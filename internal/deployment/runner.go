package deployment

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

type Runner interface {
	Run(context.Context, string, io.Reader, io.Writer, io.Writer, []string, string, ...string) error
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir string, stdin io.Reader, stdout, stderr io.Writer, environment []string, name string, args ...string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("%s is required but was not found", name)
	}
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	command.Env = append(os.Environ(), environment...)
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func runCompose(ctx context.Context, runner Runner, dir string, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	commandArgs := []string{"compose", "--env-file", ".env", "-f", "compose.yaml"}
	commandArgs = append(commandArgs, args...)
	if err := runner.Run(ctx, dir, stdin, stdout, stderr, nil, "docker", commandArgs...); err != nil {
		return fmt.Errorf("docker compose: %w", err)
	}
	return nil
}

func defaultRunner(runner Runner) Runner {
	if runner != nil {
		return runner
	}
	return ExecRunner{}
}

func defaultWriter(writer io.Writer) io.Writer {
	if writer != nil {
		return writer
	}
	return io.Discard
}

func processInput() io.Reader { return os.Stdin }
