package main

import (
	"bufio"
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
	case "access":
		return accessCommand(args[1:])
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

func accessCommand(args []string) error {
	if len(args) == 0 || args[0] != "set" {
		return errors.New("use barqctl access set")
	}
	set := flag.NewFlagSet("access set", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	dir := set.String("dir", "", "deployment directory")
	keyFile := set.String("key-file", "", "read the new operator API key from a private file")
	if err := set.Parse(args[1:]); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("access set does not take a key argument; paste it on stdin or use --key-file")
	}
	var raw string
	if *keyFile != "" {
		data, err := os.ReadFile(*keyFile)
		if err != nil {
			return err
		}
		raw = string(data)
	} else {
		fmt.Fprint(os.Stderr, "Paste the new one-time API key, then press Enter: ")
		line, err := bufio.NewReader(io.LimitReader(os.Stdin, 4097)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		raw = line
	}
	if err := deployment.SetAPIKey(*dir, raw); err != nil {
		return err
	}
	fmt.Println("Local operator key updated. Run barqctl doctor to check it.")
	return nil
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
	if set.NArg() > 1 {
		return errors.New("logs accepts at most one service: core, control, or edge")
	}
	service := ""
	if set.NArg() == 1 {
		service = set.Arg(0)
		switch service {
		case "core", "control", "edge":
		default:
			return fmt.Errorf("unknown service %q; use core, control, or edge", service)
		}
	}
	composeArgs := []string{"logs", "--tail", fmt.Sprint(*tail)}
	if *follow {
		composeArgs = append(composeArgs, "--follow")
	}
	if service != "" {
		composeArgs = append(composeArgs, service)
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
	if len(args) > 0 {
		switch args[0] {
		case "configure":
			return configureRemoteBackupCommand(args[1:])
		case "check":
			return checkRemoteBackupCommand(args[1:])
		case "schedule":
			return scheduleRemoteBackupCommand(args[1:])
		}
	}
	set := flag.NewFlagSet("backup", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	dir := set.String("dir", "", "deployment directory")
	output := set.String("output", "", "backup directory")
	remote := set.Bool("remote", false, "encrypt and upload to the configured S3 repository")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("backup does not take positional arguments")
	}
	if *remote {
		if *output != "" {
			return errors.New("--output cannot be used with --remote")
		}
		result, err := deployment.RemoteBackup(context.Background(), deployment.RemoteBackupOptions{Dir: *dir, Stdout: os.Stdout, Stderr: os.Stderr})
		if err != nil {
			return err
		}
		fmt.Println("Encrypted backup uploaded. Local copy:", result.LocalPath)
		return nil
	}
	result, err := deployment.Backup(context.Background(), deployment.BackupOptions{Dir: *dir, Destination: *output, Stdout: os.Stdout, Stderr: os.Stderr})
	if err != nil {
		return err
	}
	fmt.Println("Backup created:", result.Path)
	return nil
}

func configureRemoteBackupCommand(args []string) error {
	set := flag.NewFlagSet("backup configure", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	dir := set.String("dir", "", "deployment directory")
	repository := set.String("repository", "", "restic S3 URL, for example s3:https://s3.example.com/bucket/barq")
	region := set.String("region", "us-east-1", "S3 region")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("backup configure does not take positional arguments")
	}
	result, err := deployment.ConfigureRemoteBackup(context.Background(), deployment.ConfigureRemoteBackupOptions{
		Dir: *dir, Repository: *repository, Region: *region,
		AccessKey:    firstEnvironment("BARQ_BACKUP_ACCESS_KEY", "AWS_ACCESS_KEY_ID"),
		SecretKey:    firstEnvironment("BARQ_BACKUP_SECRET_KEY", "AWS_SECRET_ACCESS_KEY"),
		SessionToken: firstEnvironment("BARQ_BACKUP_SESSION_TOKEN", "AWS_SESSION_TOKEN"),
		Password:     firstEnvironment("BARQ_BACKUP_PASSWORD", "RESTIC_PASSWORD"),
		Stdout:       os.Stdout, Stderr: os.Stderr,
	})
	if err != nil {
		return err
	}
	fmt.Println("Encrypted remote backup configured.")
	fmt.Println("Copy this recovery key to a separate password manager:", result.RecoveryKeyPath)
	return nil
}

func checkRemoteBackupCommand(args []string) error {
	set := flag.NewFlagSet("backup check", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	dir := set.String("dir", "", "deployment directory")
	restoreTest := set.Bool("restore-test", false, "download and verify the latest backup")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("backup check does not take positional arguments")
	}
	if err := deployment.CheckRemoteBackup(context.Background(), deployment.RemoteCheckOptions{
		Dir: *dir, RestoreTest: *restoreTest, Stdout: os.Stdout, Stderr: os.Stderr,
	}); err != nil {
		return err
	}
	if *restoreTest {
		fmt.Println("Remote repository and full restore test passed.")
	} else {
		fmt.Println("Remote repository check passed.")
	}
	return nil
}

func scheduleRemoteBackupCommand(args []string) error {
	set := flag.NewFlagSet("backup schedule", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	dir := set.String("dir", "", "deployment directory")
	dailyAt := set.String("daily-at", "03:00", "daily backup time in 24-hour HH:MM format")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("backup schedule does not take positional arguments")
	}
	result, err := deployment.InstallBackupSchedule(context.Background(), deployment.BackupScheduleOptions{
		Dir: *dir, DailyAt: *dailyAt, Stdout: os.Stdout, Stderr: os.Stderr,
	})
	if err != nil {
		return err
	}
	fmt.Println("Daily encrypted backups enabled:", result.DailyTimer)
	fmt.Println("Weekly full restore tests enabled:", result.CheckTimer)
	return nil
}

func restoreCommand(args []string) error {
	set := flag.NewFlagSet("restore", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	dir := set.String("dir", "", "deployment directory")
	backup := set.String("backup", "", "backup directory to restore")
	snapshot := set.String("snapshot", "", "encrypted remote snapshot ID or latest")
	yes := set.Bool("yes", false, "confirm replacement of current data")
	if err := set.Parse(args); err != nil {
		return err
	}
	if (*backup == "") == (*snapshot == "") {
		return errors.New("use exactly one of --backup or --snapshot")
	}
	if !*yes {
		return errors.New("restore replaces current data; rerun with --yes")
	}
	var result deployment.RestoreResult
	var err error
	if *snapshot != "" {
		result, err = deployment.RestoreRemoteBackup(context.Background(), deployment.RemoteRestoreOptions{
			Dir: *dir, Snapshot: *snapshot, Stdout: os.Stdout, Stderr: os.Stderr,
		})
	} else {
		result, err = deployment.Restore(context.Background(), deployment.RestoreOptions{
			Dir: *dir, Backup: *backup, Stdout: os.Stdout, Stderr: os.Stderr, SafetyBackup: true,
		})
	}
	if err != nil {
		return err
	}
	fmt.Println("Restore completed. Safety backup:", result.SafetyBackup)
	return nil
}

func firstEnvironment(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
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
		Dir: *dir, Version: *release, Stdout: os.Stdout, Stderr: os.Stderr, Verify: verifyRelease,
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
		Dir: *dir, Domain: *domain, Version: *release, ControlImage: *controlImage, CoreImage: *coreImage, Force: *force, Verify: verifyRelease,
	})
	if err != nil {
		return err
	}
	fmt.Printf("Barq deployment created in %s\n", result.Dir)
	fmt.Printf("Global control admin API key: %s\n", result.APIKey)
	fmt.Println("Save this key now. It is also stored in the private .env file.")
	fmt.Println("Next: barqctl up")
	return nil
}

func verifyRelease(release deployment.Release) error {
	return deployment.VerifyRelease(context.Background(), release, nil, io.Discard, os.Stderr)
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
  barqctl logs --tail 200 --follow core
  barqctl access set
  barqctl backup
  barqctl backup --remote
  barqctl backup configure --repository s3:https://s3.example.com/bucket/barq
  barqctl backup schedule --daily-at 03:00

Commands:
  init      Create the deployment, secrets, and JWT key pair
  up        Start or update the deployment
  status    Show service health
  open      Open the control plane
  logs      Show logs for all services, or core, control, or edge
  doctor    Check configuration, health, disk, and backups
  access    Update the local operator API key after rotation
  backup    Create, upload, and check encrypted backups
  restore   Restore a verified backup after making a safety backup
  upgrade   Back up and switch to a fixed-digest release
  rollback  Back up and return to the previous release
  version   Print the barqctl version
`) + "\n")
}
