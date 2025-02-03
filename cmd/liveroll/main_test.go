package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRunCommand tests both the normal and error cases for runCommand.
func TestRunCommand(t *testing.T) {
	// Normal case: "echo hello" should succeed.
	if err := runCommand("echo hello"); err != nil {
		t.Errorf("Expected no error for 'echo hello', got: %v", err)
	}

	// Error case: the "false" command should exit with an error.
	if err := runCommand("false"); err == nil {
		t.Error("Expected error for 'false' command, but got nil")
	}
}

// TestRunCommandOutput tests that runCommandOutput returns the expected output.
func TestRunCommandOutput(t *testing.T) {
	out, err := runCommandOutput("echo hello")
	if err != nil {
		t.Errorf("Expected no error for 'echo hello', got: %v", err)
	}
	// Remove trailing newline and whitespace before comparing.
	if strings.TrimSpace(out) != "hello" {
		t.Errorf("Expected output 'hello', got: %q", out)
	}
}

// createTestLiveRoll creates a LiveRoll instance with test configuration
func createTestLiveRoll() *LiveRoll {
	lr := NewLiveRoll()
	lr.childPort1 = 9101
	lr.childPort2 = 9102
	lr.healthTimeout = 2 * time.Second
	return &lr
}

// TestSelectChildPort_NoChild tests port selection when no child process exists.
func TestSelectChildPort_NoChild(t *testing.T) {
	lr := createTestLiveRoll()

	port := lr.selectChildPort()
	if port != lr.childPort1 {
		t.Errorf("Expected port %d when no child exists, got %d", lr.childPort1, port)
	}
}

// TestSelectChildPort_OneChild tests port selection when one child process exists.
func TestSelectChildPort_OneChild(t *testing.T) {
	lr := createTestLiveRoll()
	lr.children[lr.childPort1] = &ChildProcess{id: "someid"}

	port := lr.selectChildPort()
	if port != lr.childPort2 {
		t.Errorf("Expected port %d when one child exists, got %d", lr.childPort2, port)
	}
}

// TestSelectChildPort_BothChildren_OneOld tests the behavior when both child processes exist and one is outdated.
func TestSelectChildPort_BothChildren_OneOld(t *testing.T) {
	lr := createTestLiveRoll()

	lr.currentIDMutex.Lock()
	lr.currentID = "current"
	lr.currentIDMutex.Unlock()

	lr.childrenMutex.Lock()
	lr.children[lr.childPort1] = &ChildProcess{id: "old"}
	lr.children[lr.childPort2] = &ChildProcess{id: "current"}
	lr.childrenMutex.Unlock()

	port := lr.selectChildPort()
	if port != lr.childPort1 {
		t.Errorf("Expected port %d to be selected (old process), got %d", lr.childPort1, port)
	}

	lr.childrenMutex.Lock()
	if _, exists := lr.children[lr.childPort1]; exists {
		t.Errorf("Expected child on port %d to be removed", lr.childPort1)
	}
	lr.childrenMutex.Unlock()
}

// TestSelectChildPort_BothChildren_Current tests the behavior when both child processes match the currentID.
func TestSelectChildPort_BothChildren_Current(t *testing.T) {
	lr := createTestLiveRoll()

	lr.currentIDMutex.Lock()
	lr.currentID = "current"
	lr.currentIDMutex.Unlock()

	lr.childrenMutex.Lock()
	lr.children[lr.childPort1] = &ChildProcess{id: "current"}
	lr.children[lr.childPort2] = &ChildProcess{id: "current"}
	lr.childrenMutex.Unlock()

	port := lr.selectChildPort()
	if port != lr.childPort1 {
		t.Errorf("Expected port %d to be selected, got %d", lr.childPort1, port)
	}

	lr.childrenMutex.Lock()
	if _, exists := lr.children[lr.childPort1]; exists {
		t.Errorf("Expected child on port %d to be removed", lr.childPort1)
	}
	lr.childrenMutex.Unlock()
}

// TestWaitForHealth_Success tests that waitForHealth succeeds when a 200 OK response is received.
func TestWaitForHealth_Success(t *testing.T) {
	lr := createTestLiveRoll()

	// Create a test HTTP server that always returns 200 OK.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	child := &ChildProcess{
		port:      12345, // Arbitrary port value
		healthURL: ts.URL,
	}

	if err := lr.waitForHealth(child); err != nil {
		t.Errorf("Expected health check to succeed, got error: %v", err)
	}
}

// TestWaitForHealth_Failure tests that waitForHealth fails when the health check does not return 200 OK.
func TestWaitForHealth_Failure(t *testing.T) {
	lr := createTestLiveRoll()

	// Create a test HTTP server that always returns 500 Internal Server Error.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	child := &ChildProcess{
		port:      12345,
		healthURL: ts.URL,
	}

	if err := lr.waitForHealth(child); err == nil {
		t.Error("Expected health check to fail, but it succeeded")
	}
}
