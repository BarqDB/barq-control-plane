package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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

Commands:
  init      Create the deployment, secrets, and JWT key pair
  up        Start or update the deployment
  status    Show service health
  open      Open the control plane
  version   Print the barqctl version
`) + "\n")
}
