package liveroll

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// logMOutput reads from r line by line and logs it using t.Log.
func logMOutput(t *testing.T, r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		t.Log("[CHILD] " + scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Logf("Error reading merged output: %v", err)
	}
}

func TestIntegration_Simple(t *testing.T) {
	// Dummy command for --pull: simply output "dummy".
	pullCmd := "echo dummy"
	// Dummy command for --id: output a fixed ID ("testid").
	idCmd := "echo testid"

	// --exec command: Launch plackup with a one-line Perl script.
	token := time.Now().UnixNano()
	execCmd := fmt.Sprintf("go run testutils/demohttpd/demohttpd.go -port <<PORT>> -content 'ok %v'", token)

	// Prepare liveroll command-line arguments.
	args := []string{
		"--interval", "10s",
		"--port", "4374",
		"--child-port1", "9101",
		"--child-port2", "9102",
		"--pull", pullCmd,
		"--id", idCmd,
		"--exec", execCmd,
		"--health-timeout", "30s",
	}

	// Assume liveroll binary is built and available as "./liveroll".
	cmd := exec.Command("./liveroll", args...)

	// Merge stdout and stderr by assigning stderr to stdout.
	cmd.Stderr = cmd.Stdout
	// Get the combined output stdoutPipe.
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("Failed to get combined stdout stdoutPipe: %v", err)
	}
	// Get the combined output stdoutPipe.
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("Failed to get combined stderr stderrPipe: %v", err)
	}

	// Start the liveroll process.
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start liveroll: %v", err)
	}

	// Log the merged output in real time.
	go logMOutput(t, stdoutPipe)
	go logMOutput(t, stderrPipe)

	t.Log("Wait for the initial setup.")
	time.Sleep(3 * time.Second)

	// Check that the reverse proxy is serving requests.
	resp, err := http.Get("http://localhost:4374/")
	if err != nil {
		t.Fatalf("Failed to perform GET on reverse proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected HTTP 200 from healthcheck, got %d", resp.StatusCode)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Errorf("Failed to read response body: %v", err)
	}
	body := strings.TrimSpace(string(bodyBytes))
	if !strings.HasPrefix(body, "ok") {
		t.Errorf("Expected response body to start with 'ok', got %q", body)
	}

	t.Logf("Clean up: terminate the liveroll process.")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("Failed to kill liveroll process: %v", err)
	}
	err = cmd.Wait()
	if err != nil {
		log.Printf("child process exited: %v", err.Error())
		t.Fatalf("Failed to wait for liveroll process: %v", err)
	}
}
