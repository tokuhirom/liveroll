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

type LiveRoll struct {
	pullCmdStr      string
	idCmdStr        string
	execCmdStr      string
	interval        time.Duration
	healthcheckPath string
	listenPort      int
	childPort1      int
	childPort2      int
	healthTimeout   time.Duration

	// current image ID (output from the --id command)
	currentID      string
	currentIDMutex sync.Mutex

	// Manage child processes (key: assigned child process port)
	children      map[int]*ChildProcess
	childrenMutex sync.Mutex

	// Reverse proxy using oxy round-robin load balancer
	lb *roundrobin.RoundRobin
	// Backend URLs management (key: child process port)
	backendURLs      map[int]*url.URL
	backendURLsMutex sync.Mutex

	updateChan        chan bool
	inShutdownProcess bool
}

// ChildProcess represents a launched child process.
type ChildProcess struct {
	port      int
	id        string // output from the --id command
	cmd       *exec.Cmd
	healthURL string // e.g., "http://localhost:<port><healthcheckPath>"
}

func NewLiveRoll() LiveRoll {
	return LiveRoll{
		children:          make(map[int]*ChildProcess),
		backendURLs:       make(map[int]*url.URL),
		updateChan:        make(chan bool, 1),
		inShutdownProcess: false,
	}
}

func main() {
	liveRoll := NewLiveRoll()

	// Define flags
	flag.StringVar(&liveRoll.pullCmdStr, "pull", "", "Command to pull the new artifact")
	flag.StringVar(&liveRoll.idCmdStr, "id", "", "Command to output the version or ID of the pulled artifact (printed to STDOUT)")
	flag.StringVar(&liveRoll.execCmdStr, "exec", "", "Command to launch the child process (supports template variables)")
	flag.DurationVar(&liveRoll.interval, "interval", 60*time.Second, "Interval between update checks")
	flag.StringVar(&liveRoll.healthcheckPath, "healthcheck", "/heathz", "Path for the healthcheck endpoint")
	flag.IntVar(&liveRoll.listenPort, "port", 8080, "Port on which the reverse proxy listens")
	flag.IntVar(&liveRoll.childPort1, "child-port1", 9101, "Child process listen port 1")
	flag.IntVar(&liveRoll.childPort2, "child-port2", 9102, "Child process listen port 2")
	flag.DurationVar(&liveRoll.healthTimeout, "health-timeout", 30*time.Second, "Healthcheck timeout")
	flag.Parse()

	if liveRoll.pullCmdStr == "" || liveRoll.idCmdStr == "" || liveRoll.execCmdStr == "" {
		log.Fatal("Required flags --pull, --id, and --exec must be specified")
	}

	liveRoll.Run()
}

func (liveRoll *LiveRoll) Run() {
	// Initialize the oxy round-robin proxy
	fwd := forward.New(false)
	var err error
	liveRoll.lb, err = roundrobin.New(fwd)
	if err != nil {
		log.Fatalf("Failed to create roundrobin proxy: %v", err)
	}
	bufferHandler, err := buffer.New(liveRoll.lb, buffer.Retry(`IsNetworkError() && Attempts() < 2`))
	if err != nil {
		log.Fatalf("Failed to create buffer handler: %v", err)
	}

	// Signal handling (SIGHUP: restart; SIGTERM/SIGINT: shutdown)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)

	// update process loop
	go liveRoll.updateLoop()

	// Start the reverse proxy HTTP server
	go func() {
		addr := fmt.Sprintf(":%d", liveRoll.listenPort)
		log.Printf("Starting reverse proxy on %s", addr)
		if err := http.ListenAndServe(addr, bufferHandler); err != nil {
			log.Fatalf("Reverse proxy server terminated: %v", err)
		}
	}()

	// On first run, always execute the update process
	liveRoll.triggerUpdate(true)

	// Ticker for periodic updates
	log.Printf("Starting update loop with interval %v", liveRoll.interval)
	ticker := time.NewTicker(liveRoll.interval)
	defer ticker.Stop()

	// Main loop: handle signals and periodic update events
	for {
		select {
		case sig := <-sigs:
			switch sig {
			case syscall.SIGHUP:
				log.Println("Received SIGHUP. Forcing restart process.")
				liveRoll.triggerUpdate(true)
			case syscall.SIGTERM, syscall.SIGINT:
				log.Println("Received SIGTERM/SIGINT. Terminating child processes and shutting down.")
				liveRoll.shutdown()
				return
			}
		case <-ticker.C:
			log.Println("Update interval elapsed. Checking for updates.")
			liveRoll.triggerUpdate(false)
		}
	}
}

// updateLoop listens for update requests and triggers the update process.
func (liveRoll *LiveRoll) updateLoop() {
	for forced := range liveRoll.updateChan {
		log.Printf("Processing update request(forced=%v)\n", forced)
		if err := liveRoll.updateProcess(forced); err != nil {
			log.Printf("Update process failed: %v(forced=%v)", err, forced)
		}
	}
}

// triggerUpdate sends a signal to the update channel to trigger an update process.
func (liveRoll *LiveRoll) triggerUpdate(forced bool) {
	if liveRoll.inShutdownProcess {
		log.Println("Ignoring update request during shutdown process")
		return
	}
	liveRoll.updateChan <- forced
}

// shutdown sends SIGTERM to all child processes and exits the program.
func (liveRoll *LiveRoll) shutdown() {
	liveRoll.childrenMutex.Lock()
	defer liveRoll.childrenMutex.Unlock()

	// don't accept any more updates
	log.Printf("Shutting down. Waiting for child processes to exit.")
	liveRoll.inShutdownProcess = true

	sendSignalForAllChildren := func(signal syscall.Signal) {
		for port, child := range liveRoll.children {
			log.Printf("Sending signal %v to child process on port %d, pid=%s", signal, port, child.id)
			if child.cmd != nil && child.cmd.Process != nil {
				err := child.cmd.Process.Signal(signal)
				if err != nil {
					log.Printf("Failed to send signal %v to child process on port %d: %v", signal, port, err)
				}
			}
		}
	}

	waitAllChildren := func() bool {
		// Non-blocking wait for child processes using waitpid(-1, WNOHANG)
		for i := 0; i < 300; i++ {
			log.Print("Waiting for child processes to exit")
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
			if pid <= 0 {
				// No more child processes to wait for
				log.Println("All child processes exited")
				return true
			}
			if err != nil {
				log.Printf("Error waiting for child processes: %v", err)
				return false
			}
			log.Printf("Child process (pid=%d) exited", pid)
			time.Sleep(100 * time.Millisecond) // Small delay to avoid CPU overload
		}

		log.Printf("Timeout waiting for child processes to exit")
		return false
	}

	log.Printf("Sending SIGTERM to all child processes")
	sendSignalForAllChildren(syscall.SIGTERM)

	log.Println("Wait for all child processes to exit")

	if !waitAllChildren() {
		log.Println("Force killing all child processes")
		sendSignalForAllChildren(syscall.SIGKILL)

		waitAllChildren()
	}

	os.Exit(0)
}

// updateProcess executes the pull and id commands and launches a new child process if needed.
// If forced is true, the update process is executed even if the new ID matches the current ID.
func (liveRoll *LiveRoll) updateProcess(forced bool) error {
	log.Println("Starting update process")
	// 1. Execute the pull command
	if err := runCommand(liveRoll.pullCmdStr); err != nil {
		return fmt.Errorf("pull command failed: %v", err)
	}
	log.Println("Pull command executed successfully")

	// 2. Execute the id command to obtain the new ID
	newID, err := runCommandOutput(liveRoll.idCmdStr)
	if err != nil {
		return fmt.Errorf("id command failed: %v", err)
	}
	newID = strings.TrimSpace(newID)
	log.Printf("New ID: %s", newID)

	liveRoll.currentIDMutex.Lock()
	current := liveRoll.currentID
	liveRoll.currentIDMutex.Unlock()

	if !forced && newID == current {
		log.Println("ID unchanged. No update required.")
		return nil
	}

	// 3. Determine available port for the child process
	portToUse := liveRoll.selectChildPort()
	if portToUse == 0 {
		return fmt.Errorf("no available port for launching a child process")
	}
	log.Printf("Assigning port %d for new child process", portToUse)

	// 4. Launch the child process (perform template substitution on the exec command)
	child, err := liveRoll.startChildProcess(portToUse, newID)
	if err != nil {
		return fmt.Errorf("failed to launch child process: %v", err)
	}

	// 5. Perform healthcheck (wait until a HTTP 200 response is received)
	if err := liveRoll.waitForHealth(child); err != nil {
		log.Printf("Healthcheck failed for child process on port %d: %v", portToUse, err)
		killChild(child)
		return fmt.Errorf("healthcheck failed: %v", err)
	}
	log.Printf("Child process on port %d passed healthcheck", portToUse)

	// 6. Register the child process and add it to the reverse proxy backend list
	liveRoll.childrenMutex.Lock()
	liveRoll.children[portToUse] = child
	liveRoll.childrenMutex.Unlock()
	liveRoll.addBackend(child)

	// 7. Update the currentID
	liveRoll.currentIDMutex.Lock()
	liveRoll.currentID = newID
	liveRoll.currentIDMutex.Unlock()

	// 8. Terminate old child processes (those with an ID different from newID)
	liveRoll.removeStaleChildren(newID, portToUse)

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
func (liveRoll *LiveRoll) selectChildPort() int {
	liveRoll.childrenMutex.Lock()
	defer liveRoll.childrenMutex.Unlock()

	_, exists1 := liveRoll.children[liveRoll.childPort1]
	_, exists2 := liveRoll.children[liveRoll.childPort2]
	if !exists1 {
		return liveRoll.childPort1
	}
	if !exists2 {
		return liveRoll.childPort2
	}

	// Both ports are in use. Terminate the one that does not match the current ID.
	liveRoll.currentIDMutex.Lock()
	current := liveRoll.currentID
	liveRoll.currentIDMutex.Unlock()
	if liveRoll.children[liveRoll.childPort1].id != current {
		log.Printf("Both ports in use. Terminating process on port %d", liveRoll.childPort1)
		killChild(liveRoll.children[liveRoll.childPort1])
		delete(liveRoll.children, liveRoll.childPort1)
		liveRoll.removeBackendByPort(liveRoll.childPort1)
		return liveRoll.childPort1
	}
	if liveRoll.children[liveRoll.childPort2].id != current {
		log.Printf("Both ports in use. Terminating process on port %d", liveRoll.childPort2)
		killChild(liveRoll.children[liveRoll.childPort2])
		delete(liveRoll.children, liveRoll.childPort2)
		liveRoll.removeBackendByPort(liveRoll.childPort2)
		return liveRoll.childPort2
	}

	// If both processes are current, arbitrarily terminate the one on childPort1.
	log.Printf("Both child processes are current. Terminating process on port %d", liveRoll.childPort1)
	killChild(liveRoll.children[liveRoll.childPort1])
	delete(liveRoll.children, liveRoll.childPort1)
	liveRoll.removeBackendByPort(liveRoll.childPort1)
	return liveRoll.childPort1
}

// startChildProcess performs template substitution on the exec command and launches the child process.
func (liveRoll *LiveRoll) startChildProcess(port int, newID string) (*ChildProcess, error) {
	// Replace template variables <<PORT>> and <<HEALTHCHECK>> in execCmdStr.
	cmdStr := strings.ReplaceAll(liveRoll.execCmdStr, "<<PORT>>", fmt.Sprintf("%d", port))
	cmdStr = strings.ReplaceAll(cmdStr, "<<HEALTHCHECK>>", liveRoll.healthcheckPath)
	log.Printf("Child process launch command: %s", cmdStr)
	cmd := exec.Command("sh", "-c", cmdStr)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Launch the child process.
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	healthURL := fmt.Sprintf("http://localhost:%d%s", port, liveRoll.healthcheckPath)
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
		liveRoll.childrenMutex.Lock()
		delete(liveRoll.children, port)
		liveRoll.childrenMutex.Unlock()
		liveRoll.removeBackend(ch)
	}(child)

	return child, nil
}

// waitForHealth waits until the child process's healthcheck endpoint returns HTTP 200.
func (liveRoll *LiveRoll) waitForHealth(child *ChildProcess) error {
	interval := 1 * time.Second
	deadline := time.Now().Add(liveRoll.healthTimeout)
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
func (liveRoll *LiveRoll) removeStaleChildren(newID string, newPort int) {
	liveRoll.childrenMutex.Lock()
	defer liveRoll.childrenMutex.Unlock()
	for port, child := range liveRoll.children {
		if port != newPort && child.id != newID {
			log.Printf("Terminating old child process on port %d", port)
			killChild(child)
			delete(liveRoll.children, port)
			liveRoll.removeBackend(child)
		}
	}
}

// addBackend adds the child process's address to the reverse proxy.
func (liveRoll *LiveRoll) addBackend(child *ChildProcess) {
	liveRoll.backendURLsMutex.Lock()
	defer liveRoll.backendURLsMutex.Unlock()
	urlStr := fmt.Sprintf("http://localhost:%d", child.port)
	u, err := url.Parse(urlStr)
	if err != nil {
		log.Printf("Failed to parse backend URL %s: %v", urlStr, err)
		return
	}
	// Add to the oxy round-robin load balancer.
	err = liveRoll.lb.UpsertServer(u)
	if err != nil {
		log.Printf("[ERROR} Failed to add backend to load balancer: %v", err)
	}
	liveRoll.backendURLs[child.port] = u
	log.Printf("Added backend for port %d", child.port)
}

// removeBackend removes the child process's backend from the reverse proxy.
func (liveRoll *LiveRoll) removeBackend(child *ChildProcess) {
	liveRoll.removeBackendByPort(child.port)
}

func (liveRoll *LiveRoll) removeBackendByPort(port int) {
	liveRoll.backendURLsMutex.Lock()
	defer liveRoll.backendURLsMutex.Unlock()
	if u, ok := liveRoll.backendURLs[port]; ok {
		err := liveRoll.lb.RemoveServer(u)
		if err != nil {
			log.Print("[ERROR] Failed to remove backend from load balancer: ", err)
		}
		delete(liveRoll.backendURLs, port)
		log.Printf("Removed backend for port %d", port)
	}
}
