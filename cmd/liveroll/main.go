package main

import (
	"flag"
	"fmt"
	"github.com/vulcand/oxy/v2/buffer"
	"github.com/vulcand/oxy/v2/forward"
	"github.com/vulcand/oxy/v2/roundrobin"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Command-line flags
var (
	pullCmdStr      string
	idCmdStr        string
	execCmdStr      string
	interval        time.Duration
	healthcheckPath string
	listenPort      int
	childPort1      int
	childPort2      int
	healthTimeout   time.Duration
)

// Global state
var (
	// current image ID (output from the --id command)
	currentID      string
	currentIDMutex sync.Mutex

	// Manage child processes (key: assigned child process port)
	children      = make(map[int]*ChildProcess)
	childrenMutex sync.Mutex

	// Reverse proxy using oxy round-robin load balancer
	lb *roundrobin.RoundRobin
	// Backend URLs management (key: child process port)
	backendURLs      = make(map[int]*url.URL)
	backendURLsMutex sync.Mutex
)

// ChildProcess represents a launched child process.
type ChildProcess struct {
	port      int
	id        string // output from the --id command
	cmd       *exec.Cmd
	healthURL string // e.g., "http://localhost:<port><healthcheckPath>"
}

func main() {
	// Define flags
	flag.StringVar(&pullCmdStr, "pull", "", "Command to pull the new artifact")
	flag.StringVar(&idCmdStr, "id", "", "Command to output the version or ID of the pulled artifact (printed to STDOUT)")
	flag.StringVar(&execCmdStr, "exec", "", "Command to launch the child process (supports template variables)")
	flag.DurationVar(&interval, "interval", 60*time.Second, "Interval between update checks")
	flag.StringVar(&healthcheckPath, "healthcheck", "/heathz", "Path for the healthcheck endpoint")
	flag.IntVar(&listenPort, "port", 8080, "Port on which the reverse proxy listens")
	flag.IntVar(&childPort1, "child-port1", 9101, "Child process listen port 1")
	flag.IntVar(&childPort2, "child-port2", 9102, "Child process listen port 2")
	flag.DurationVar(&healthTimeout, "health-timeout", 30*time.Second, "Healthcheck timeout")
	flag.Parse()

	if pullCmdStr == "" || idCmdStr == "" || execCmdStr == "" {
		log.Fatal("Required flags --pull, --id, and --exec must be specified")
	}

	// Initialize the oxy round-robin proxy
	fwd := forward.New(false)
	lb, err := roundrobin.New(fwd)
	if err != nil {
		log.Fatalf("Failed to create roundrobin proxy: %v", err)
	}
	bufferHandler, err := buffer.New(lb, buffer.Retry(`IsNetworkError() && Attempts() < 2`))
	if err != nil {
		log.Fatalf("Failed to create buffer handler: %v", err)
	}

	// Signal handling (SIGHUP: restart; SIGTERM/SIGINT: shutdown)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)

	// Start the reverse proxy HTTP server
	go func() {
		addr := fmt.Sprintf(":%d", listenPort)
		log.Printf("Starting reverse proxy on %s", addr)
		if err := http.ListenAndServe(addr, bufferHandler); err != nil {
			log.Fatalf("Reverse proxy server terminated: %v", err)
		}
	}()

	// On first run, always execute the update process
	go func() {
		if err := updateProcess(true); err != nil {
			log.Printf("Initial update failed: %v", err)
		}
	}()

	// Ticker for periodic updates
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Main loop: handle signals and periodic update events
	for {
		select {
		case sig := <-sigs:
			switch sig {
			case syscall.SIGHUP:
				log.Println("Received SIGHUP. Forcing restart process.")
				go func() {
					if err := updateProcess(true); err != nil {
						log.Printf("Update triggered by SIGHUP failed: %v", err)
					}
				}()
			case syscall.SIGTERM, syscall.SIGINT:
				log.Println("Received SIGTERM/SIGINT. Terminating child processes and shutting down.")
				shutdown()
				return
			}
		case <-ticker.C:
			log.Println("Update interval elapsed. Checking for updates.")
			go func() {
				if err := updateProcess(false); err != nil {
					log.Printf("Periodic update failed: %v", err)
				}
			}()
		}
	}
}

// shutdown sends SIGTERM to all child processes and exits the program.
func shutdown() {
	childrenMutex.Lock()
	defer childrenMutex.Unlock()
	for port, child := range children {
		log.Printf("Terminating child process on port %d", port)
		if child.cmd != nil && child.cmd.Process != nil {
			child.cmd.Process.Signal(syscall.SIGTERM)
		}
	}
	os.Exit(0)
}

// updateProcess executes the pull and id commands and launches a new child process if needed.
// If forced is true, the update process is executed even if the new ID matches the current ID.
func updateProcess(forced bool) error {
	log.Println("Starting update process")
	// 1. Execute the pull command
	if err := runCommand(pullCmdStr); err != nil {
		return fmt.Errorf("pull command failed: %v", err)
	}
	log.Println("Pull command executed successfully")

	// 2. Execute the id command to obtain the new ID
	newID, err := runCommandOutput(idCmdStr)
	if err != nil {
		return fmt.Errorf("id command failed: %v", err)
	}
	newID = strings.TrimSpace(newID)
	log.Printf("New ID: %s", newID)

	currentIDMutex.Lock()
	current := currentID
	currentIDMutex.Unlock()

	if !forced && newID == current {
		log.Println("ID unchanged. No update required.")
		return nil
	}

	// 3. Determine available port for the child process
	portToUse := selectChildPort()
	if portToUse == 0 {
		return fmt.Errorf("no available port for launching a child process")
	}
	log.Printf("Assigning port %d for new child process", portToUse)

	// 4. Launch the child process (perform template substitution on the exec command)
	child, err := startChildProcess(portToUse, newID)
	if err != nil {
		return fmt.Errorf("failed to launch child process: %v", err)
	}

	// 5. Perform healthcheck (wait until a HTTP 200 response is received)
	if err := waitForHealth(child); err != nil {
		log.Printf("Healthcheck failed for child process on port %d: %v", portToUse, err)
		killChild(child)
		return fmt.Errorf("healthcheck failed: %v", err)
	}
	log.Printf("Child process on port %d passed healthcheck", portToUse)

	// 6. Register the child process and add it to the reverse proxy backend list
	childrenMutex.Lock()
	children[portToUse] = child
	childrenMutex.Unlock()
	addBackend(child)

	// 7. Update the currentID
	currentIDMutex.Lock()
	currentID = newID
	currentIDMutex.Unlock()

	// 8. Terminate old child processes (those with an ID different from newID)
	removeStaleChildren(newID, portToUse)

	return nil
}

// runCommand executes a command using "sh -c".
func runCommand(cmdStr string) error {
	log.Printf("Executing command: %s", cmdStr)
	cmd := exec.Command("sh", "-c", cmdStr)
	// Output stdout and stderr to the current process
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runCommandOutput executes a command and returns its stdout as a string.
func runCommandOutput(cmdStr string) (string, error) {
	log.Printf("Executing command: %s", cmdStr)
	cmd := exec.Command("sh", "-c", cmdStr)
	out, err := cmd.Output()
	return string(out), err
}

// selectChildPort determines which port to assign to a new child process.
// If one port is free, it returns that port. If both are in use, it terminates
// the one that does not match the currentID or, if both match, arbitrarily terminates one.
func selectChildPort() int {
	childrenMutex.Lock()
	defer childrenMutex.Unlock()
	_, exists1 := children[childPort1]
	_, exists2 := children[childPort2]
	if !exists1 {
		return childPort1
	}
	if !exists2 {
		return childPort2
	}
	currentIDMutex.Lock()
	current := currentID
	currentIDMutex.Unlock()
	if children[childPort1].id != current {
		log.Printf("Both ports in use. Terminating process on port %d", childPort1)
		killChild(children[childPort1])
		delete(children, childPort1)
		removeBackendByPort(childPort1)
		return childPort1
	}
	if children[childPort2].id != current {
		log.Printf("Both ports in use. Terminating process on port %d", childPort2)
		killChild(children[childPort2])
		delete(children, childPort2)
		removeBackendByPort(childPort2)
		return childPort2
	}
	// If both processes are current, arbitrarily terminate the one on childPort1.
	log.Printf("Both child processes are current. Terminating process on port %d", childPort1)
	killChild(children[childPort1])
	delete(children, childPort1)
	removeBackendByPort(childPort1)
	return childPort1
}

// startChildProcess performs template substitution on the exec command and launches the child process.
func startChildProcess(port int, newID string) (*ChildProcess, error) {
	// Replace template variables <<PORT>> and <<HEALTHCHECK>> in execCmdStr.
	cmdStr := strings.ReplaceAll(execCmdStr, "<<PORT>>", fmt.Sprintf("%d", port))
	cmdStr = strings.ReplaceAll(cmdStr, "<<HEALTHCHECK>>", healthcheckPath)
	log.Printf("Child process launch command: %s", cmdStr)
	cmd := exec.Command("sh", "-c", cmdStr)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Launch the child process.
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	healthURL := fmt.Sprintf("http://localhost:%d%s", port, healthcheckPath)
	child := &ChildProcess{
		port:      port,
		id:        newID,
		cmd:       cmd,
		healthURL: healthURL,
	}

	// Start a goroutine to monitor the child process termination.
	go func(ch *ChildProcess) {
		err := cmd.Wait()
		if err != nil {
			log.Printf("Child process on port %d terminated abnormally: %v", port, err)
		} else {
			log.Printf("Child process on port %d terminated normally", port)
		}
		// On termination, remove the child from global management and the reverse proxy.
		childrenMutex.Lock()
		delete(children, port)
		childrenMutex.Unlock()
		removeBackend(ch)
	}(child)

	return child, nil
}

// waitForHealth waits until the child process's healthcheck endpoint returns HTTP 200.
func waitForHealth(child *ChildProcess) error {
	interval := 1 * time.Second
	deadline := time.Now().Add(healthTimeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(child.healthURL)
		if err == nil {
			// Discard the response body.
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		log.Printf("Healthcheck failed for port %d. Retrying in %v", child.port, interval)
		time.Sleep(interval)
	}
	return fmt.Errorf("healthcheck timed out")
}

// killChild sends a termination signal to the child process.
func killChild(child *ChildProcess) {
	if child.cmd != nil && child.cmd.Process != nil {
		log.Printf("Force killing child process on port %d", child.port)
		_ = child.cmd.Process.Kill()
	}
}

// removeStaleChildren terminates child processes that do not have the newID.
func removeStaleChildren(newID string, newPort int) {
	childrenMutex.Lock()
	defer childrenMutex.Unlock()
	for port, child := range children {
		if port != newPort && child.id != newID {
			log.Printf("Terminating old child process on port %d", port)
			killChild(child)
			delete(children, port)
			removeBackend(child)
		}
	}
}

// addBackend adds the child process's address to the reverse proxy.
func addBackend(child *ChildProcess) {
	backendURLsMutex.Lock()
	defer backendURLsMutex.Unlock()
	urlStr := fmt.Sprintf("http://localhost:%d", child.port)
	u, err := url.Parse(urlStr)
	if err != nil {
		log.Printf("Failed to parse backend URL %s: %v", urlStr, err)
		return
	}
	// Add to the oxy round-robin load balancer.
	err = lb.UpsertServer(u)
	if err != nil {
		log.Printf("[ERROR} Failed to add backend to load balancer: %v", err)
	}
	backendURLs[child.port] = u
	log.Printf("Added backend for port %d", child.port)
}

// removeBackend removes the child process's backend from the reverse proxy.
func removeBackend(child *ChildProcess) {
	removeBackendByPort(child.port)
}

func removeBackendByPort(port int) {
	backendURLsMutex.Lock()
	defer backendURLsMutex.Unlock()
	if u, ok := backendURLs[port]; ok {
		err := lb.RemoveServer(u)
		if err != nil {
			log.Print("[ERROR] Failed to remove backend from load balancer: ", err)
		}
		delete(backendURLs, port)
		log.Printf("Removed backend for port %d", port)
	}
}
