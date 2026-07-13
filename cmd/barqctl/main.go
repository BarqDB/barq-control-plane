package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/barqdb/barq-server/internal/deployment"
)

var version = "main"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "barqctl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("command is required")
	}
	switch args[0] {
	case "init":
		return initCommand(args[1:])
	case "up":
		return composeCommand("up", args[1:], "up", "-d", "--wait")
	case "status":
		return composeCommand("status", args[1:], "ps")
	case "open":
		return openCommand(args[1:])
	case "logs":
		return logsCommand(args[1:])
	case "doctor":
		return doctorCommand(args[1:])
	case "backup":
		return backupCommand(args[1:])
	case "restore":
		return restoreCommand(args[1:])
	case "upgrade":
		return upgradeCommand(args[1:])
	case "rollback":
		return rollbackCommand(args[1:])
	case "version", "--version", "-version":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func logsCommand(args []string) error {
	set := flag.NewFlagSet("logs", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	dir := set.String("dir", "", "deployment directory")
	follow := set.Bool("follow", false, "keep following new log lines")
	tail := set.Int("tail", 200, "number of recent lines")
	if err := set.Parse(args); err != nil {
		return err
	}
	composeArgs := []string{"logs", "--tail", fmt.Sprint(*tail)}
	if *follow {
		composeArgs = append(composeArgs, "--follow")
	}
	return deployment.Compose(context.Background(), *dir, os.Stdout, os.Stderr, composeArgs...)
}

func doctorCommand(args []string) error {
	set := flag.NewFlagSet("doctor", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	dir := set.String("dir", "", "deployment directory")
	if err := set.Parse(args); err != nil {
		return err
	}
	checks, err := deployment.Doctor(context.Background(), deployment.DoctorOptions{Dir: *dir, Stdout: io.Discard, Stderr: io.Discard})
	for _, check := range checks {
		icon := "✓"
		if check.Status == deployment.CheckWarn {
			icon = "!"
		} else if check.Status == deployment.CheckFail {
			icon = "x"
		}
		fmt.Printf("%s %-20s %s\n", icon, check.Name, check.Detail)
	}
	return err
}

func backupCommand(args []string) error {
	set := flag.NewFlagSet("backup", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	dir := set.String("dir", "", "deployment directory")
	output := set.String("output", "", "backup directory")
	if err := set.Parse(args); err != nil {
		return err
	}
	result, err := deployment.Backup(context.Background(), deployment.BackupOptions{Dir: *dir, Destination: *output, Stdout: os.Stdout, Stderr: os.Stderr})
	if err != nil {
		return err
	}
	fmt.Println("Backup created:", result.Path)
	return nil
}

func restoreCommand(args []string) error {
	set := flag.NewFlagSet("restore", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	dir := set.String("dir", "", "deployment directory")
	backup := set.String("backup", "", "backup directory to restore")
	yes := set.Bool("yes", false, "confirm replacement of current data")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *backup == "" {
		return errors.New("--backup is required")
	}
	if !*yes {
		return errors.New("restore replaces current data; rerun with --yes")
	}
	result, err := deployment.Restore(context.Background(), deployment.RestoreOptions{
		Dir: *dir, Backup: *backup, Stdout: os.Stdout, Stderr: os.Stderr, SafetyBackup: true,
	})
	if err != nil {
		return err
	}
	fmt.Println("Restore completed. Safety backup:", result.SafetyBackup)
	return nil
}

func upgradeCommand(args []string) error {
	set := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	dir := set.String("dir", "", "deployment directory")
	release := set.String("release", "", "target release, for example v1.2.0")
	if err := set.Parse(args); err != nil {
		return err
	}
	result, err := deployment.Upgrade(context.Background(), deployment.UpgradeOptions{
		Dir: *dir, Version: *release, Stdout: os.Stdout, Stderr: os.Stderr,
	})
	if err != nil {
		return err
	}
	fmt.Printf("Upgraded %s to %s. Safety backup: %s\n", result.From, result.To, result.BackupPath)
	return nil
}

func rollbackCommand(args []string) error {
	set := flag.NewFlagSet("rollback", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	dir := set.String("dir", "", "deployment directory")
	if err := set.Parse(args); err != nil {
		return err
	}
	result, err := deployment.Rollback(context.Background(), deployment.RollbackOptions{
		Dir: *dir, Stdout: os.Stdout, Stderr: os.Stderr,
	})
	if err != nil {
		return err
	}
	fmt.Printf("Rolled back %s to %s. Safety backup: %s\n", result.From, result.To, result.BackupPath)
	return nil
}

func initCommand(args []string) error {
	set := flag.NewFlagSet("init", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	domain := set.String("domain", "", "public hostname for Barq")
	dir := set.String("dir", "", "deployment directory (default: $BARQ_HOME or ~/.barq)")
	release := set.String("release", version, "matching Core and control-plane image tag")
	controlImage := set.String("control-image", "", "override the control-plane image")
	coreImage := set.String("core-image", "", "override the Core image")
	force := set.Bool("force", false, "write into a non-empty directory")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("init does not take positional arguments")
	}
	result, err := deployment.Init(deployment.InitOptions{
		Dir: *dir, Domain: *domain, Version: *release, ControlImage: *controlImage, CoreImage: *coreImage, Force: *force,
	})
	if err != nil {
		return err
	}
	fmt.Printf("Barq deployment created in %s\n", result.Dir)
	fmt.Printf("Control API key: %s\n", result.APIKey)
	fmt.Println("Save this key now. It is also stored in the private .env file.")
	fmt.Println("Next: barqctl up")
	return nil
}

func composeCommand(name string, args []string, composeArgs ...string) error {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	dir := set.String("dir", "", "deployment directory")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("%s does not take positional arguments", name)
	}
	return deployment.Compose(context.Background(), *dir, os.Stdout, os.Stderr, composeArgs...)
}

func openCommand(args []string) error {
	set := flag.NewFlagSet("open", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	dir := set.String("dir", "", "deployment directory")
	printOnly := set.Bool("print", false, "print the URL without opening a browser")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("open does not take positional arguments")
	}
	if *printOnly {
		manifest, err := deployment.LoadManifest(*dir)
		if err != nil {
			return err
		}
		fmt.Println(manifest.URL)
		return nil
	}
	opened, err := deployment.Open(*dir)
	if err != nil {
		fmt.Println(opened)
		return err
	}
	fmt.Println(opened)
	return nil
}

func usage() {
	fmt.Print(strings.TrimSpace(`
barqctl manages a self-hosted Barq deployment.

Usage:
  barqctl init --domain db.example.com
  barqctl up
  barqctl status
  barqctl open
  barqctl doctor
  barqctl backup

Commands:
  init      Create the deployment, secrets, and JWT key pair
  up        Start or update the deployment
  status    Show service health
  open      Open the control plane
  logs      Show service logs
  doctor    Check configuration, health, disk, and backups
  backup    Create a consistent local backup
  restore   Restore a verified backup after making a safety backup
  upgrade   Back up and switch to a fixed-digest release
  rollback  Back up and return to the previous release
  version   Print the barqctl version
`) + "\n")
}
