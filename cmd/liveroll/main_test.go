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

// TestSelectChildPort_NoChild tests port selection when no child process exists.
func TestSelectChildPort_NoChild(t *testing.T) {
	// Reset global state.
	children = make(map[int]*ChildProcess)
	currentID = ""

	port := selectChildPort()
	if port != childPort1 {
		t.Errorf("Expected port %d when no child exists, got %d", childPort1, port)
	}
}

// TestSelectChildPort_OneChild tests port selection when one child process exists.
func TestSelectChildPort_OneChild(t *testing.T) {
	// Reset global state.
	children = make(map[int]*ChildProcess)
	currentID = ""
	// Create a child process on childPort1.
	children[childPort1] = &ChildProcess{id: "someid"}

	port := selectChildPort()
	if port != childPort2 {
		t.Errorf("Expected port %d when one child exists, got %d", childPort2, port)
	}
}

// TestSelectChildPort_BothChildren_OneOld tests the behavior when both child processes exist and one is outdated.
func TestSelectChildPort_BothChildren_OneOld(t *testing.T) {
	// Reset global state.
	children = make(map[int]*ChildProcess)
	currentID = "current"
	// childPort1 is outdated; childPort2 matches the currentID.
	children[childPort1] = &ChildProcess{id: "old"}
	children[childPort2] = &ChildProcess{id: "current"}

	port := selectChildPort()
	if port != childPort1 {
		t.Errorf("Expected port %d to be selected (old process), got %d", childPort1, port)
	}
	// The child process on childPort1 should have been removed.
	if _, exists := children[childPort1]; exists {
		t.Errorf("Expected child on port %d to be removed", childPort1)
	}
}

// TestSelectChildPort_BothChildren_Current tests the behavior when both child processes match the currentID.
func TestSelectChildPort_BothChildren_Current(t *testing.T) {
	// Reset global state.
	children = make(map[int]*ChildProcess)
	currentID = "current"
	children[childPort1] = &ChildProcess{id: "current"}
	children[childPort2] = &ChildProcess{id: "current"}

	port := selectChildPort()
	// In this scenario, childPort1 should be arbitrarily terminated.
	if port != childPort1 {
		t.Errorf("Expected port %d to be selected, got %d", childPort1, port)
	}
	if _, exists := children[childPort1]; exists {
		t.Errorf("Expected child on port %d to be removed", childPort1)
	}
}

// TestWaitForHealth_Success tests that waitForHealth succeeds when a 200 OK response is received.
func TestWaitForHealth_Success(t *testing.T) {
	// Create a test HTTP server that always returns 200 OK.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	child := &ChildProcess{
		port:      12345, // Arbitrary port value.
		healthURL: ts.URL,
	}

	// Temporarily set healthTimeout for testing.
	oldTimeout := healthTimeout
	healthTimeout = 2 * time.Second
	defer func() { healthTimeout = oldTimeout }()

	if err := waitForHealth(child); err != nil {
		t.Errorf("Expected health check to succeed, got error: %v", err)
	}
}

// TestWaitForHealth_Failure tests that waitForHealth fails when the health check does not return 200 OK.
func TestWaitForHealth_Failure(t *testing.T) {
	// Create a test HTTP server that always returns 500 Internal Server Error.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	child := &ChildProcess{
		port:      12345,
		healthURL: ts.URL,
	}

	// Temporarily set healthTimeout for testing.
	oldTimeout := healthTimeout
	healthTimeout = 2 * time.Second
	defer func() { healthTimeout = oldTimeout }()

	if err := waitForHealth(child); err == nil {
		t.Error("Expected health check to fail, but it succeeded")
	}
}
