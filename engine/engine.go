package engine

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"quotient/engine/checks"
	"quotient/engine/config"
	"quotient/engine/db"
)

type ScoringEngine struct {
	Config                *config.ConfigSettings
	CredentialsMutex      map[uint]*sync.Mutex
	UptimePerService      map[uint]map[string]db.Uptime
	SlaPerService         map[uint]map[string]int
	EnginePauseWg         *sync.WaitGroup
	IsEnginePaused        bool
	CurrentRound          uint
	NextRoundStartTime    time.Time
	CurrentRoundStartTime time.Time
	Broker                *Broker

	// Config update handling
	configPath string

	// Number of worker goroutines (replaces runner replicas)
	NumWorkers int
}

func NewEngine(conf *config.ConfigSettings, configPath string) *ScoringEngine {
	// Ensure config is properly validated and runners are set up
	if err := conf.SetConfig(configPath); err != nil {
		panic(fmt.Sprintf("Failed to validate initial config: %v", err))
	}

	se := &ScoringEngine{
		Config:           conf,
		CredentialsMutex: make(map[uint]*sync.Mutex),
		UptimePerService: make(map[uint]map[string]db.Uptime),
		SlaPerService:    make(map[uint]map[string]int),
		NumWorkers:       5, // default, same as old docker-compose replicas
		configPath:       configPath,
	}

	// Start watching config file for changes
	if err := conf.WatchConfig(configPath); err != nil {
		slog.Error("Failed to start config watcher", "error", err)
	}

	return se
}

func (se *ScoringEngine) Start() {
	if t, err := db.GetLastRound(); err != nil {
		slog.Error("failed to get last round", "error", err)
	} else {
		se.CurrentRound = uint(t.ID) + 1
	}

	if err := db.LoadUptimes(&se.UptimePerService); err != nil {
		slog.Error("failed to load uptimes", "error", err)
	}

	if err := db.LoadSLAs(&se.SlaPerService, se.Config.MiscSettings.SlaThreshold); err != nil {
		slog.Error("failed to load SLAs", "error", err)
	}

	// load credentials
	err := se.LoadCredentials()
	if err != nil {
		slog.Error("failed to load credential files into teams", "error", err)
	}

	// start paused if configured
	se.IsEnginePaused = false
	se.EnginePauseWg = &sync.WaitGroup{}
	if se.Config.MiscSettings.StartPaused {
		se.IsEnginePaused = true
		se.EnginePauseWg.Add(1)
	}

	se.NextRoundStartTime = time.Time{}

	// Create and start the broker with worker goroutines
	se.Broker = NewBroker(se.NumWorkers)
	se.Broker.StartWorkers()

	// Subscribe for reset events
	subID, eventsCh := se.Broker.Subscribe()
	defer se.Broker.Unsubscribe(subID)

	// engine loop
	go func() {
		for {
			slog.Info("Queueing up for round", "round", se.CurrentRound)
			se.EnginePauseWg.Wait()
			select {
			case msg := <-eventsCh:
				slog.Info("Received message", "message", msg)
				if msg == "reset" {
					slog.Info("Engine loop reset event received while waiting, quitting...")
					return
				} else {
					continue
				}
			default:
				slog.Info("Starting round", "round", se.CurrentRound)
				se.CurrentRoundStartTime = time.Now()
				se.NextRoundStartTime = time.Now().Add(time.Duration(se.Config.MiscSettings.Delay) * time.Second)

				// run the round logic
				var err error
				switch se.Config.RequiredSettings.EventType {
				case "koth":
					err = se.koth()
				case "rvb":
					err = se.rvb()
				default:
					slog.Error("Unknown event type", "eventType", se.Config.RequiredSettings.EventType)
				}
				if err != nil {
					slog.Error("Round error. If this is a reset, ignore...", "error", err)
					return
				}

				slog.Info(fmt.Sprintf("Round %d complete", se.CurrentRound))
				se.CurrentRound++

				se.Broker.Publish("round_finish")
				slog.Info(fmt.Sprintf("Round %d will start in %s, sleeping...", se.CurrentRound, time.Until(se.NextRoundStartTime).String()))
				time.Sleep(time.Until(se.NextRoundStartTime))
			}
		}
	}()
	se.waitForReset()
	slog.Info("Restarting scoring...")
}

func (se *ScoringEngine) waitForReset() {
	subID, eventsCh := se.Broker.Subscribe()
	defer se.Broker.Unsubscribe(subID)

	for msg := range eventsCh {
		slog.Info("Received message", "message", msg)
		if msg == "reset" {
			slog.Info("Reset event received, quitting...")
			return
		}
	}
}

func (se *ScoringEngine) GetUptimePerService() map[uint]map[string]db.Uptime {
	return se.UptimePerService
}

// GetActiveTasks returns all active and recently completed tasks
func (se *ScoringEngine) GetActiveTasks() (map[string]any, error) {
	if se.Broker == nil {
		return map[string]any{
			"running":     []any{},
			"success":     []any{},
			"failed":      []any{},
			"all_runners": []any{},
		}, nil
	}
	return se.Broker.GetAllTaskStatuses(), nil
}

func (se *ScoringEngine) PauseEngine() {
	if !se.IsEnginePaused {
		se.EnginePauseWg.Add(1)
		se.IsEnginePaused = true
	}
}

func (se *ScoringEngine) ResumeEngine() {
	if se.IsEnginePaused {
		se.EnginePauseWg.Done()
		se.IsEnginePaused = false
	}
}

// ResetScores resets the engine to the initial state and stops the engine
func (se *ScoringEngine) ResetScores() error {
	slog.Info("Resetting scores and clearing task queues")

	// Reset the database
	if err := db.ResetScores(); err != nil {
		slog.Error("failed to reset scores", "error", err)
		return fmt.Errorf("failed to reset scores: %v", err)
	}

	// Publish reset event (workers and engine loop will see this)
	se.Broker.Publish("reset")

	// Stop the broker (drains queues, stops workers)
	se.Broker.Restart()

	// Reset engine state
	se.CurrentRound = 1
	se.UptimePerService = make(map[uint]map[string]db.Uptime)
	se.SlaPerService = make(map[uint]map[string]int)
	slog.Info("Scores reset and task queues cleared successfully")

	return nil
}

// perform a round of koth
func (se *ScoringEngine) koth() error {
	return nil
}

// perform a round of rvb
func (se *ScoringEngine) rvb() error {
	// reassign the next round start time with jitter
	randomJitter := rand.Intn(2*se.Config.MiscSettings.Jitter) - se.Config.MiscSettings.Jitter
	jitter := time.Duration(randomJitter) * time.Second
	se.NextRoundStartTime = time.Now().Add(time.Duration(se.Config.MiscSettings.Delay) * time.Second).Add(jitter)

	slog.Info(fmt.Sprintf("round should take %s", time.Until(se.NextRoundStartTime).String()))

	// Subscribe for reset events during this round
	subID, eventsCh := se.Broker.Subscribe()
	defer se.Broker.Unsubscribe(subID)

	// do rvb stuff
	teams, err := db.GetTeams()
	if err != nil {
		slog.Error("failed to get teams:", "error", err)
		return err
	}

	taskCount := 0

	slog.Debug("Starting service checks", "round", se.CurrentRound)
	allRunners := se.Config.AllChecks()
	slog.Info("Service check configuration",
		"boxes", len(se.Config.Box),
		"runners", len(allRunners))

	for _, box := range se.Config.Box {
		slog.Debug("Box configuration", "name", box.Name, "runners", len(box.Runners))
	}

	// 1) Enqueue tasks to the broker
	for _, team := range teams {
		if !team.Active {
			continue
		}
		for _, r := range allRunners {
			if !r.Runnable() {
				continue
			}
			enabled, err := db.IsTeamServiceEnabled(team.ID, r.GetName())
			if err != nil {
				slog.Error("failed to check service state", "team", team.ID, "service", r.GetName(), "error", err)
				continue
			}
			if !enabled {
				continue
			}

			data, err := json.Marshal(r)
			if err != nil {
				slog.Error("failed to marshal check definition", "error", err)
				continue
			}

			task := Task{
				TeamID:         team.ID,
				TeamIdentifier: team.Identifier,
				ServiceType:    r.GetType(),
				ServiceName:    r.GetName(),
				RoundID:        se.CurrentRound,
				Deadline:       se.NextRoundStartTime,
				Attempts:       r.GetAttempts(),
				CheckData:      data,
			}

			payload, err := json.Marshal(task)
			if err != nil {
				slog.Error("failed to marshal service task", "error", err)
				continue
			}
			se.Broker.EnqueueTask(payload)
			taskCount++
		}
	}
	slog.Info("Enqueued checks", "count", taskCount)

	// 2) Collect results from the broker
	results := make([]checks.Result, 0, taskCount)

	i := 0
	for i < taskCount {
		select {
		case msg := <-eventsCh:
			slog.Info("Received message", "message", msg)
			if msg == "reset" {
				slog.Info("Reset event received, quitting...")
				return fmt.Errorf("reset event received")
			}
			continue
		default:
			result, ok := se.Broker.CollectResult(se.NextRoundStartTime)
			if !ok {
				slog.Warn("Timeout waiting for results", "remaining", taskCount-i)
				results = []checks.Result{}
				goto done
			}
			if result.RoundID != se.CurrentRound {
				slog.Warn("Ignoring out of round result", "receivedRound", result.RoundID, "currentRound", se.CurrentRound)
				continue
			}
			results = append(results, result)
			i++
			slog.Debug("service check finished", "round_id", result.RoundID, "team_id", result.TeamID, "service_name", result.ServiceName, "result", result.Status, "debug", result.Debug, "error", result.Error)
		}
	}

done:
	// 3) Process all collected results
	se.processCollectedResults(results)
	return nil
}

func (se *ScoringEngine) processCollectedResults(results []checks.Result) {
	if len(results) == 0 {
		slog.Warn("No results collected for round", "round", se.CurrentRound)
		return
	}

	dbResults := []db.ServiceCheckSchema{}

	for _, result := range results {
		dbResults = append(dbResults, db.ServiceCheckSchema{
			TeamID:      result.TeamID,
			RoundID:     uint(se.CurrentRound),
			ServiceName: result.ServiceName,
			Points:      result.Points,
			Result:      result.Status,
			Error:       result.Error,
			Debug:       result.Debug,
		})
	}

	if len(dbResults) == 0 {
		slog.Warn("No results to process for the current round", "round", se.CurrentRound)
		return
	}

	// Save results to database
	round := db.RoundSchema{
		ID:        uint(se.CurrentRound),
		StartTime: se.CurrentRoundStartTime,
		Checks:    dbResults,
	}
	if _, err := db.CreateRound(round); err != nil {
		slog.Error("failed to create round:", "round", se.CurrentRound, "error", err)
		return
	}

	for _, result := range results {
		// Update uptime and SLA maps
		if _, ok := se.UptimePerService[result.TeamID]; !ok {
			se.UptimePerService[result.TeamID] = make(map[string]db.Uptime)
		}
		if _, ok := se.UptimePerService[result.TeamID][result.ServiceName]; !ok {
			se.UptimePerService[result.TeamID][result.ServiceName] = db.Uptime{}
		}
		newUptime := se.UptimePerService[result.TeamID][result.ServiceName]
		if result.Status {
			newUptime.PassedChecks++
		}
		newUptime.TotalChecks++
		se.UptimePerService[result.TeamID][result.ServiceName] = newUptime

		if _, ok := se.SlaPerService[result.TeamID]; !ok {
			se.SlaPerService[result.TeamID] = make(map[string]int)
		}
		if _, ok := se.SlaPerService[result.TeamID][result.ServiceName]; !ok {
			se.SlaPerService[result.TeamID][result.ServiceName] = 0
		}
		if result.Status {
			se.SlaPerService[result.TeamID][result.ServiceName] = 0
		} else {
			se.SlaPerService[result.TeamID][result.ServiceName]++
			if se.SlaPerService[result.TeamID][result.ServiceName] >= se.Config.MiscSettings.SlaThreshold {
				sla := db.SLASchema{
					TeamID:      result.TeamID,
					ServiceName: result.ServiceName,
					RoundID:     uint(se.CurrentRound),
					Penalty:     se.Config.MiscSettings.SlaPenalty,
				}
				db.CreateSLA(sla)
				se.SlaPerService[result.TeamID][result.ServiceName] = 0
			}
		}
	}

	slog.Debug("Successfully processed results for round", "round", se.CurrentRound, "total", len(dbResults))
}
