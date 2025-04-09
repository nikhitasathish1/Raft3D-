package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	"github.com/hashicorp/raft-boltdb"
	"github.com/hashicorp/go-hclog"
)
// Data structures
type Printer struct {
	ID      string `json:"id"`
	Company string `json:"company"`
	Model   string `json:"model"`
}

type Filament struct {
	ID                    string `json:"id"`
	Type                  string `json:"type"`
	Color                 string `json:"color"`
	TotalWeightInGrams    int    `json:"total_weight_in_grams"`
	RemainingWeightInGrams int    `json:"remaining_weight_in_grams"`
}

type PrintJob struct {
	ID                string `json:"id"`
	PrinterID         string `json:"printer_id"`
	FilamentID        string `json:"filament_id"`
	Filepath          string `json:"filepath"`
	PrintWeightInGrams int    `json:"print_weight_in_grams"`
	Status            string `json:"status"`
}

// FSM implements the raft.FSM interface
type FSM struct {
	mu          sync.Mutex
	printers    map[string]Printer
	filaments   map[string]Filament
	printJobs   map[string]PrintJob
}

// Apply applies a Raft log entry to the FSM
func (f *FSM) Apply(log *raft.Log) interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()

	var command struct {
		Operation string      `json:"operation"`
		Data      interface{} `json:"data"`
	}

	if err := json.Unmarshal(log.Data, &command); err != nil {
		return fmt.Errorf("failed to unmarshal command: %v", err)
	}

	switch command.Operation {
	case "add_printer":
		var printer Printer
		data, _ := json.Marshal(command.Data)
		json.Unmarshal(data, &printer)
		f.printers[printer.ID] = printer
	case "add_filament":
		var filament Filament
		data, _ := json.Marshal(command.Data)
		json.Unmarshal(data, &filament)
		f.filaments[filament.ID] = filament
	case "add_print_job":
		var printJob PrintJob
		data, _ := json.Marshal(command.Data)
		json.Unmarshal(data, &printJob)
		f.printJobs[printJob.ID] = printJob
	case "update_print_job_status":
		var update struct {
			JobID  string `json:"job_id"`
			Status string `json:"status"`
		}
		data, _ := json.Marshal(command.Data)
		json.Unmarshal(data, &update)

		if job, exists := f.printJobs[update.JobID]; exists {
			// Validate status transition
			valid := false
			switch update.Status {
			case "Running":
				valid = job.Status == "Queued"
			case "Done":
				valid = job.Status == "Running"
			case "Canceled":
				valid = job.Status == "Queued" || job.Status == "Running"
			}

			if valid {
				job.Status = update.Status
				f.printJobs[update.JobID] = job

				// If status is Done, reduce filament weight
				if update.Status == "Done" {
					if filament, exists := f.filaments[job.FilamentID]; exists {
						filament.RemainingWeightInGrams -= job.PrintWeightInGrams
						f.filaments[job.FilamentID] = filament
					}
				}
			}
		}
	}

	return nil
}
func (f *FSM) handleHealth(w http.ResponseWriter, r *http.Request, raftNode *raft.Raft, nodeID string) {
    status := map[string]interface{}{
        "status":     "healthy",
        "node_id":    nodeID,
        "leader":     string(raftNode.Leader()),
        "raft_state": raftNode.State().String(),
    }
    
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(status)
}
// Snapshot returns a snapshot of the FSM's state
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	snapshot := &FSMSnapshot{
		printers:  make(map[string]Printer),
		filaments: make(map[string]Filament),
		printJobs: make(map[string]PrintJob),
	}

	for k, v := range f.printers {
		snapshot.printers[k] = v
	}
	for k, v := range f.filaments {
		snapshot.filaments[k] = v
	}
	for k, v := range f.printJobs {
		snapshot.printJobs[k] = v
	}

	return snapshot, nil
}

// Restore restores an FSM from a snapshot
func (f *FSM) Restore(rc io.ReadCloser) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	var snapshot FSMSnapshot
	if err := json.NewDecoder(rc).Decode(&snapshot); err != nil {
		return err
	}

	f.printers = snapshot.printers
	f.filaments = snapshot.filaments
	f.printJobs = snapshot.printJobs

	return nil
}

// FSMSnapshot implements the raft.FSMSnapshot interface
type FSMSnapshot struct {
	printers  map[string]Printer
	filaments map[string]Filament
	printJobs map[string]PrintJob
}

// Persist saves the FSM snapshot
func (f *FSMSnapshot) Persist(sink raft.SnapshotSink) error {
	err := json.NewEncoder(sink).Encode(f)
	if err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

// Release releases resources
func (f *FSMSnapshot) Release() {}

// API handlers
func (f *FSM) handleAddPrinter(w http.ResponseWriter, r *http.Request, raftNode *raft.Raft) {
	var printer Printer
	if err := json.NewDecoder(r.Body).Decode(&printer); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	command := struct {
		Operation string      `json:"operation"`
		Data      interface{} `json:"data"`
	}{
		Operation: "add_printer",
		Data:      printer,
	}

	commandBytes, err := json.Marshal(command)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if raftNode.State() != raft.Leader {
		leader := raftNode.Leader()
		if leader == "" {
			http.Error(w, "No leader available", http.StatusServiceUnavailable)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("http://%s/api/v1/printers", leader), http.StatusTemporaryRedirect)
		return
	}

	applyFuture := raftNode.Apply(commandBytes, 5*time.Second)
	if err := applyFuture.Error(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(printer)
}

func (f *FSM) handleGetPrinters(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	printers := make([]Printer, 0, len(f.printers))
	for _, printer := range f.printers {
		printers = append(printers, printer)
	}

	json.NewEncoder(w).Encode(printers)
}

func setupRaft(nodeID, raftAddr, raftDir string) (*raft.Raft, *FSM, error) {
	config := raft.DefaultConfig()
	config.LocalID = raft.ServerID(nodeID)
	
	// Tuned timeout settings
	config.HeartbeatTimeout = 1000 * time.Millisecond
	config.ElectionTimeout = 1000 * time.Millisecond
	config.LeaderLeaseTimeout = 500 * time.Millisecond
	config.CommitTimeout = 50 * time.Millisecond
	
	// Enhanced logging
	config.Logger = hclog.New(&hclog.LoggerOptions{
		Name:   "raft",
		Level:  hclog.Info,
		Output: os.Stderr,
	})

	fsm := &FSM{
		printers:  make(map[string]Printer),
		filaments: make(map[string]Filament),
		printJobs: make(map[string]PrintJob),
	}

	// Create BoltDB store
	store, err := raftboltdb.NewBoltStore(filepath.Join(raftDir, "raft.db"))
	if err != nil {
		return nil, nil, fmt.Errorf("boltdb.NewBoltStore: %v", err)
	}

	// Create snapshot store
	snapshots, err := raft.NewFileSnapshotStore(raftDir, 2, os.Stderr)
	if err != nil {
		return nil, nil, fmt.Errorf("raft.NewFileSnapshotStore: %v", err)
	}

	// Create TCP transport
	tcpAddr, err := net.ResolveTCPAddr("tcp", raftAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("ResolveTCPAddr: %v", err)
	}

	transport, err := raft.NewTCPTransport(raftAddr, tcpAddr, 3, 10*time.Second, os.Stderr)
	if err != nil {
		return nil, nil, fmt.Errorf("raft.NewTCPTransport: %v", err)
	}

	// Create Raft instance
	raftNode, err := raft.NewRaft(config, fsm, store, store, snapshots, transport)
	if err != nil {
		return nil, nil, fmt.Errorf("raft.NewRaft: %v", err)
	}

	// Cluster configuration with all peers
	configuration := raft.Configuration{
		Servers: []raft.Server{
			{
				ID:      raft.ServerID("node1"),
				Address: raft.ServerAddress("node1:7000"),
			},
			{
				ID:      raft.ServerID("node2"),
				Address: raft.ServerAddress("node2:7000"),
			},
			{
				ID:      raft.ServerID("node3"),
				Address: raft.ServerAddress("node3:7000"),
			},
		},
	}
	
	// Bootstrap cluster if this is the first node
	if nodeID == "node1" {
		raftNode.BootstrapCluster(configuration)
	} else {
		// For other nodes, wait a bit and add them as voters
		time.Sleep(5 * time.Second)
		raftNode.AddVoter(
			raft.ServerID(nodeID),
			raft.ServerAddress(raftAddr),
			0, 0,
		)
	}

	return raftNode, fsm, nil
}
func main() {
	nodeID := os.Getenv("NODE_ID")
	if nodeID == "" {
		nodeID = "node1"
	}

	raftAddr := os.Getenv("RAFT_ADDR")
	if raftAddr == "" {
		raftAddr = "127.0.0.1:7000"
	}

	httpAddr := os.Getenv("HTTP_ADDR")
	if httpAddr == "" {
		httpAddr = "127.0.0.1:8000"
	}

	raftDir := os.Getenv("RAFT_DIR")
	if raftDir == "" {
		raftDir = "./raft-data"
	}

	if err := os.MkdirAll(raftDir, 0700); err != nil {
		log.Fatalf("failed to create raft directory: %v", err)
	}

	raftNode, fsm, err := setupRaft(nodeID, raftAddr, raftDir)
	if err != nil {
		log.Fatalf("failed to setup raft: %v", err)
	}

	// Setup HTTP server endpoints
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if raftNode.State() == raft.Shutdown {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		fmt.Fprintf(w, `{"node":"%s","state":"%s"}`, nodeID, raftNode.State())
	})
	// http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
	// 	status := map[string]interface{}{
	// 		"status":     "healthy",
	// 		"node_id":    nodeID,
	// 		"leader":     string(raftNode.Leader()),
	// 		"raft_state": raftNode.State().String(),
	// 	}
	// 	w.Header().Set("Content-Type", "application/json")
	// 	json.NewEncoder(w).Encode(status)
	// })

	http.HandleFunc("/api/v1/printers", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			fsm.handleAddPrinter(w, r, raftNode)
		case http.MethodGet:
			fsm.handleGetPrinters(w, r)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Wait for stable cluster formation
	for i := 0; i < 10; i++ {
		if raftNode.Leader() != "" {
			break
		}
		time.Sleep(1 * time.Second)
	}

	log.Printf("Node %s initialized. Cluster state: Leader=%s, Current State=%s", 
		nodeID, raftNode.Leader(), raftNode.State())
	log.Printf("HTTP server listening on %s", httpAddr)
	log.Fatal(http.ListenAndServe(httpAddr, nil))
}