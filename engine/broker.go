package engine

import (
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"quotient/engine/checks"
)

// Broker replaces Redis with in-process channels and goroutines.
// It handles task dispatch, result collection, event signaling,
// and task status tracking -- all without any external dependency.
type Broker struct {
	tasks   chan []byte        // task queue (replaces Redis "tasks" list)
	results chan checks.Result // result queue (replaces Redis "results" list)

	// Event pub/sub (replaces Redis pub/sub on "events" channel)
	eventMu     sync.RWMutex
	eventSubs   map[int]chan string
	eventNextID int

	// Task status tracking for admin UI (replaces Redis "task:*" keys)
	taskStatusMu sync.RWMutex
	taskStatuses map[string]checks.Result // key = "task:{round}:{team}:{type}:{name}"

	// Worker pool
	numWorkers int
	stopOnce   sync.Once
	stopCh     chan struct{}
}

// NewBroker creates a broker with the given number of worker goroutines.
func NewBroker(numWorkers int) *Broker {
	if numWorkers <= 0 {
		numWorkers = 5
	}
	return &Broker{
		tasks:        make(chan []byte, 4096),
		results:      make(chan checks.Result, 4096),
		eventSubs:    make(map[int]chan string),
		taskStatuses: make(map[string]checks.Result),
		numWorkers:   numWorkers,
		stopCh:       make(chan struct{}),
	}
}

// StartWorkers launches the worker goroutines that consume tasks and produce results.
func (b *Broker) StartWorkers() {
	for i := 0; i < b.numWorkers; i++ {
		workerID := fmt.Sprintf("worker-%d", i)
		go b.worker(workerID)
	}
	slog.Info("Started worker pool", "workers", b.numWorkers)
}

// Stop signals all workers to drain and stop.
func (b *Broker) Stop() {
	b.stopOnce.Do(func() {
		close(b.stopCh)
	})
}

// Restart resets the broker for a new engine cycle (e.g. after a reset).
func (b *Broker) Restart() {
	b.Stop()

	// Drain any leftover items
	b.drainTasks()
	b.drainResults()
	b.clearTaskStatuses()

	// Close all event subscribers
	b.eventMu.Lock()
	for id, ch := range b.eventSubs {
		close(ch)
		delete(b.eventSubs, id)
	}
	b.eventMu.Unlock()

	// Reset stop channel and counters
	b.stopOnce = sync.Once{}
	b.stopCh = make(chan struct{})
	b.tasks = make(chan []byte, 4096)
	b.results = make(chan checks.Result, 4096)
}

// --- Task Queue ---

// EnqueueTask pushes a serialized task onto the queue.
func (b *Broker) EnqueueTask(payload []byte) {
	select {
	case b.tasks <- payload:
	case <-b.stopCh:
	}
}

// CollectResult blocks until a result is available or the context-like deadline fires.
// Returns the result and true, or zero-value and false on timeout/stop.
func (b *Broker) CollectResult(deadline time.Time) (checks.Result, bool) {
	timeout := time.Until(deadline)
	if timeout <= 0 {
		// Try non-blocking
		select {
		case r := <-b.results:
			return r, true
		default:
			return checks.Result{}, false
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-b.results:
		return r, true
	case <-timer.C:
		return checks.Result{}, false
	case <-b.stopCh:
		return checks.Result{}, false
	}
}

// --- Event Pub/Sub ---

// Subscribe returns a channel that receives event messages, and an ID to unsubscribe.
func (b *Broker) Subscribe() (int, <-chan string) {
	b.eventMu.Lock()
	defer b.eventMu.Unlock()
	id := b.eventNextID
	b.eventNextID++
	ch := make(chan string, 16)
	b.eventSubs[id] = ch
	return id, ch
}

// Unsubscribe removes a subscriber.
func (b *Broker) Unsubscribe(id int) {
	b.eventMu.Lock()
	defer b.eventMu.Unlock()
	if ch, ok := b.eventSubs[id]; ok {
		close(ch)
		delete(b.eventSubs, id)
	}
}

// Publish sends an event to all current subscribers.
func (b *Broker) Publish(event string) {
	b.eventMu.RLock()
	defer b.eventMu.RUnlock()
	for _, ch := range b.eventSubs {
		select {
		case ch <- event:
		default:
			// subscriber is slow, drop the message rather than block
		}
	}
}

// --- Task Status Tracking (for admin UI) ---

// SetTaskStatus stores the current status of a task.
func (b *Broker) SetTaskStatus(key string, status checks.Result, ttl time.Duration) {
	b.taskStatusMu.Lock()
	b.taskStatuses[key] = status
	b.taskStatusMu.Unlock()

	// Auto-expire after TTL
	if ttl > 0 {
		go func() {
			timer := time.NewTimer(ttl)
			defer timer.Stop()
			select {
			case <-timer.C:
				b.taskStatusMu.Lock()
				delete(b.taskStatuses, key)
				b.taskStatusMu.Unlock()
			case <-b.stopCh:
			}
		}()
	}
}

// GetAllTaskStatuses returns a snapshot of all task statuses, categorized for the admin API.
func (b *Broker) GetAllTaskStatuses() map[string]any {
	b.taskStatusMu.RLock()
	defer b.taskStatusMu.RUnlock()

	result := map[string]any{
		"running":     []any{},
		"success":     []any{},
		"failed":      []any{},
		"all_runners": []any{},
	}

	runnersSet := make(map[string]struct{})
	for _, ts := range b.taskStatuses {
		if ts.RunnerID != "" {
			runnersSet[ts.RunnerID] = struct{}{}
		}
		if statusKey := ts.StatusText; statusKey == "running" || statusKey == "success" || statusKey == "failed" {
			result[statusKey] = append(result[statusKey].([]any), ts)
		}
	}
	for runnerID := range runnersSet {
		result["all_runners"] = append(result["all_runners"].([]any), runnerID)
	}

	return result
}

func (b *Broker) clearTaskStatuses() {
	b.taskStatusMu.Lock()
	b.taskStatuses = make(map[string]checks.Result)
	b.taskStatusMu.Unlock()
}

// --- Worker (replaces runner/runner.go) ---

func (b *Broker) worker(workerID string) {
	slog.Info("Worker started", "id", workerID)
	for {
		select {
		case <-b.stopCh:
			slog.Info("Worker stopping", "id", workerID)
			return
		case payload, ok := <-b.tasks:
			if !ok {
				return
			}
			b.handleTask(workerID, payload)
		}
	}
}

func (b *Broker) handleTask(workerID string, payload []byte) {
	var task Task
	if err := json.Unmarshal(payload, &task); err != nil {
		slog.Error("Worker: invalid task format", "error", err, "worker", workerID)
		return
	}

	slog.Debug("Worker received task",
		"worker", workerID,
		"round", task.RoundID,
		"team", task.TeamID,
		"service", task.ServiceType,
		"name", task.ServiceName,
	)

	runner, err := createRunner(&task)
	if err != nil {
		slog.Error("Worker: failed to create runner", "error", err, "worker", workerID)
		// Push a failed result so the engine doesn't hang waiting
		b.results <- checks.Result{
			TeamID:      task.TeamID,
			ServiceName: task.ServiceName,
			ServiceType: task.ServiceType,
			RoundID:     task.RoundID,
			Status:      false,
			Error:       fmt.Sprintf("failed to create runner: %v", err),
			RunnerID:    workerID,
			StatusText:  "failed",
		}
		return
	}

	taskKey := fmt.Sprintf("task:%d:%d:%s:%s", task.RoundID, task.TeamID, task.ServiceType, task.ServiceName)

	result := checks.Result{
		TeamID:      task.TeamID,
		ServiceName: task.ServiceName,
		ServiceType: task.ServiceType,
		RoundID:     task.RoundID,
		Status:      false,
		RunnerID:    workerID,
		StartTime:   time.Now().Format(time.RFC3339),
		StatusText:  "running",
	}

	// Set "running" status for admin UI
	b.SetTaskStatus(taskKey, result, time.Until(task.Deadline))

	resultsChan := make(chan checks.Result, 1)

	for i := range task.Attempts {
		log.Printf("[Worker %s] Running check: RoundID=%d TeamID=%d ServiceType=%s ServiceName=%s Attempt=%d",
			workerID, task.RoundID, task.TeamID, task.ServiceType, task.ServiceName, i+1)

		go runner.Run(task.TeamID, task.TeamIdentifier, task.RoundID, resultsChan)

		timer := time.NewTimer(time.Until(task.Deadline))
		select {
		case result = <-resultsChan:
			timer.Stop()
			result.TeamID = task.TeamID
			result.ServiceName = task.ServiceName
			result.ServiceType = task.ServiceType
			result.RoundID = task.RoundID
		case <-timer.C:
			result.Status = false
			result.Debug = "round ended before check completed"
			result.Error = "timeout"
			result.TeamID = task.TeamID
			result.ServiceName = task.ServiceName
			result.ServiceType = task.ServiceType
			result.RoundID = task.RoundID
		case <-b.stopCh:
			timer.Stop()
			return
		}

		if result.Status || time.Now().After(task.Deadline) {
			break
		}
	}

	// Set final status for admin UI
	result.EndTime = time.Now().Format(time.RFC3339)
	result.RunnerID = workerID
	result.StatusText = map[bool]string{true: "success", false: "failed"}[result.Status]
	b.SetTaskStatus(taskKey, result, 3*time.Minute)

	// Send result to engine
	select {
	case b.results <- result:
	case <-b.stopCh:
	}
}

// createRunner mirrors the logic from runner/runner.go.
func createRunner(task *Task) (checks.Runner, error) {
	var runner checks.Runner

	switch task.ServiceType {
	case "Custom":
		runner = &checks.Custom{}
	case "Dns":
		runner = &checks.Dns{}
	case "Ftp":
		runner = &checks.Ftp{}
	case "Imap":
		runner = &checks.Imap{}
	case "Ldap":
		runner = &checks.Ldap{}
	case "Ping":
		runner = &checks.Ping{}
	case "Pop3":
		runner = &checks.Pop3{}
	case "Rdp":
		runner = &checks.Rdp{}
	case "Smb":
		runner = &checks.Smb{}
	case "Smtp":
		runner = &checks.Smtp{}
	case "Sql":
		runner = &checks.Sql{}
	case "Ssh":
		runner = &checks.Ssh{}
	case "Tcp":
		runner = &checks.Tcp{}
	case "Vnc":
		runner = &checks.Vnc{}
	case "Web":
		runner = &checks.Web{}
	case "WinRM":
		runner = &checks.WinRM{}
	default:
		return nil, fmt.Errorf("unknown service type: %s", task.ServiceType)
	}

	if err := json.Unmarshal(task.CheckData, runner); err != nil {
		return nil, fmt.Errorf("failed to unmarshal check data: %w", err)
	}

	return runner, nil
}

// --- Helpers ---

func (b *Broker) drainTasks() {
	for {
		select {
		case <-b.tasks:
		default:
			return
		}
	}
}

func (b *Broker) drainResults() {
	for {
		select {
		case <-b.results:
		default:
			return
		}
	}
}

// getRunnerID returns a stable ID for this process (like the old runner binary did).
func getRunnerID() string {
	id := os.Getenv("RUNNER_ID")
	if id != "" {
		return id
	}
	hostname, err := os.Hostname()
	if err != nil {
		return "embedded"
	}
	return strings.TrimSpace(hostname)
}
