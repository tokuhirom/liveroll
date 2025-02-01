package liveroll

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestIntegration_Plackup runs an end-to-end integration test using plackup as the child process.
// It uses dummy commands for --pull and --id, and verifies that liveroll correctly starts the
// test server via plackup, registers it in the reverse proxy, and forwards HTTP requests.
func TestIntegration_Plackup(t *testing.T) {
	// Dummy command for --pull: simply output "dummy".
	pullCmd := "echo dummy"
	// Dummy command for --id: output a fixed ID ("testid").
	idCmd := "echo testid"

	// --exec command: Launch plackup with a one-line Perl script.
	// The command is:
	//   plackup -p <<PORT>> -e 'my $t=time(); sub { [200, [], [qq{ok $t}]] }'
	// To pass this command as a Go string, escape the $ characters.
	execCmd := "plackup -p <<PORT>> -e 'sub { [200, [], [qq{ok}]] }'"

	// Prepare liveroll command-line arguments.
	args := []string{
		"--interval", "10s",
		"--port", "8080",
		"--child-port1", "9101",
		"--child-port2", "9102",
		"--pull", pullCmd,
		"--id", idCmd,
		"--exec", execCmd,
		"--health-timeout", "30s",
	}

	// Assume liveroll binary is built and available as "./liveroll".
	cmd := exec.Command("./liveroll", args...)
	// Instead of using asynchronous goroutines, capture stdout and stderr in buffers.
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	// Start the liveroll process.
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start liveroll: %v\nStdout:\n%s\nStderr:\n%s", err, stdoutBuf.String(), stderrBuf.String())
	}

	// Allow time for liveroll to perform its initial update process and register the plackup test server.
	time.Sleep(15 * time.Second)

	// Check that the reverse proxy is serving requests.
	resp, err := http.Get("http://localhost:8080/heathz")
	if err != nil {
		t.Fatalf("Failed to perform GET on reverse proxy: %v\nStdout:\n%s\nStderr:\n%s", err, stdoutBuf.String(), stderrBuf.String())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected HTTP 200 from healthcheck, got %d\nStdout:\n%s\nStderr:\n%s", resp.StatusCode, stdoutBuf.String(), stderrBuf.String())
	}
	// Optionally, verify the response body contains the expected text (starting with "ok").
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Errorf("Failed to read response body: %v\nStdout:\n%s\nStderr:\n%s", err, stdoutBuf.String(), stderrBuf.String())
	}
	body := strings.TrimSpace(string(bodyBytes))
	if !strings.HasPrefix(body, "ok") {
		t.Errorf("Expected response body to start with 'ok', got %q\nStdout:\n%s\nStderr:\n%s", body, stdoutBuf.String(), stderrBuf.String())
	}

	// Optionally, force an update by sending a signal.
	// Here, we simulate a SIGHUP by sending an os.Interrupt signal (adjust if needed).
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("Failed to send signal to liveroll: %v\nStdout:\n%s\nStderr:\n%s", err, stdoutBuf.String(), stderrBuf.String())
	}

	// Wait for the update process to take effect.
	time.Sleep(15 * time.Second)

	// Re-check the reverse proxy health endpoint.
	resp, err = http.Get("http://localhost:8080/heathz")
	if err != nil {
		t.Fatalf("Failed to perform GET on reverse proxy after update: %v\nStdout:\n%s\nStderr:\n%s", err, stdoutBuf.String(), stderrBuf.String())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected HTTP 200 from healthcheck after update, got %d\nStdout:\n%s\nStderr:\n%s", resp.StatusCode, stdoutBuf.String(), stderrBuf.String())
	}

	// Clean up: terminate the liveroll process.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("Failed to kill liveroll process: %v\nStdout:\n%s\nStderr:\n%s", err, stdoutBuf.String(), stderrBuf.String())
	}
	cmd.Wait()
}
