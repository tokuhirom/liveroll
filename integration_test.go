package liveroll

import (
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

	// Redirect stdout and stderr for logging.
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("Failed to obtain stdout pipe: %v", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("Failed to obtain stderr pipe: %v", err)
	}

	// Start the liveroll process.
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start liveroll: %v", err)
	}

	// Log liveroll output for debugging.
	go func() {
		data, _ := io.ReadAll(stdoutPipe)
		t.Logf("liveroll stdout: %s", string(data))
	}()
	go func() {
		data, _ := io.ReadAll(stderrPipe)
		t.Logf("liveroll stderr: %s", string(data))
	}()

	// Allow time for liveroll to perform its initial update process and register the plackup test server.
	time.Sleep(15 * time.Second)

	// Check that the reverse proxy is serving requests.
	resp, err := http.Get("http://localhost:8080/heathz")
	if err != nil {
		t.Fatalf("Failed to perform GET on reverse proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected HTTP 200 from healthcheck, got %d", resp.StatusCode)
	}
	// Optionally, verify the response body contains the expected text (starting with "ok").
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Errorf("Failed to read response body: %v", err)
	}
	body := strings.TrimSpace(string(bodyBytes))
	if !strings.HasPrefix(body, "ok") {
		t.Errorf("Expected response body to start with 'ok', got %q", body)
	}

	// Optionally, force an update by sending a signal.
	// Here, we simulate a SIGHUP by sending an os.Interrupt signal (adjust if needed).
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("Failed to send signal to liveroll: %v", err)
	}

	// Wait for the update process to take effect.
	time.Sleep(15 * time.Second)

	// Re-check the reverse proxy health endpoint.
	resp, err = http.Get("http://localhost:8080/heathz")
	if err != nil {
		t.Fatalf("Failed to perform GET on reverse proxy after update: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected HTTP 200 from healthcheck after update, got %d", resp.StatusCode)
	}

	// Clean up: terminate the liveroll process.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("Failed to kill liveroll process: %v", err)
	}
	cmd.Wait()
}
