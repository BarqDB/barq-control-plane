package dataplane

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// TestCoreBarqIntegration proves the production adapter uses real customer
// Barq files and can read an API write after the C++ process restarts.
func TestCoreBarqIntegration(t *testing.T) {
	bin := os.Getenv("BARQ_CORE_SERVER_BIN")
	if bin == "" {
		t.Skip("set BARQ_CORE_SERVER_BIN to run the real Core contract test")
	}
	root := t.TempDir()
	port := freePort(t)
	secret := "core-contract-secret"
	endpoint := "http://127.0.0.1:" + strconv.Itoa(port)

	start := func() *exec.Cmd {
		cmd := exec.Command(bin,
			"--root-dir", root,
			"--allow-unsigned-tokens",
			"--host", "127.0.0.1",
			"--port", strconv.Itoa(port),
			"--internal-api-secret", secret,
			"--log-level", "error",
		)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		client, err := NewHTTPDataPlane(endpoint, secret, nil)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			_, err = client.Health(ctx)
			cancel()
			if err == nil {
				return cmd
			}
			time.Sleep(25 * time.Millisecond)
		}
		_ = cmd.Process.Kill()
		t.Fatal("C++ data plane did not become ready")
		return nil
	}
	stop := func(cmd *exec.Cmd) {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("C++ data plane stopped with an error: %v", err)
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatal("C++ data plane did not stop")
		}
	}

	client, _ := NewHTTPDataPlane(endpoint, secret, nil)
	cmd := start()
	schema, err := client.ApplySchema(context.Background(), SchemaRequest{
		Scope: Scope{Tenant: "tenant-a", Database: "main"}, Version: 1,
		Manifest: json.RawMessage(`{"objects":[{"name":"Task","primary_key":{"name":"id","type":"string"},"properties":[{"name":"id","type":"string"},{"name":"title","type":"string"},{"name":"done","type":"bool"}]}]}`),
	})
	if err != nil || !schema.Applied {
		t.Fatalf("real Barq schema apply failed: %+v %v", schema, err)
	}
	written, err := client.WriteObject(context.Background(), WriteRequest{
		Scope: Scope{Tenant: "tenant-a", Database: "main"}, Operation: WriteCreate,
		Type: "Task", PrimaryKey: "one", Data: map[string]any{"title": "First", "done": false},
	})
	if err != nil || written.Object == nil || written.Object.ETag == "" {
		t.Fatalf("real Barq write failed: %+v %v", written, err)
	}
	read, err := client.ReadObject(context.Background(), ReadRequest{
		Scope: Scope{Tenant: "tenant-a", Database: "main"}, Type: "Task", PrimaryKey: "one",
	})
	if err != nil || read.Data["title"] != "First" || read.ETag != written.Object.ETag {
		t.Fatalf("real Barq read failed: %+v %v", read, err)
	}
	stop(cmd)

	cmd = start()
	defer stop(cmd)
	restarted, err := client.ReadObject(context.Background(), ReadRequest{
		Scope: Scope{Tenant: "tenant-a", Database: "main"}, Type: "Task", PrimaryKey: "one",
	})
	if err != nil || restarted.Data["title"] != "First" {
		t.Fatalf("real Barq restart read failed: %+v %v", restarted, err)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
