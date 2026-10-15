package api

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"

	"advance/internal/ledger"
	"advance/internal/underwriting"
)

// state is the on-disk snapshot. It is a plain JSON file so the demo survives
// restarts without a database; the ledger is rebuilt by replaying its entries.
type state struct {
	Invoices  []*Invoice                            `json:"invoices"`
	Histories map[string]underwriting.ClientHistory `json:"histories"`
	Events    []string                              `json:"events"`
	Ledger    []ledger.Entry                        `json:"ledger"`
}

// persistLocked writes the snapshot atomically. Callers must hold s.mu.
func (s *Server) persistLocked() {
	if s.opts.DataFile == "" {
		return
	}
	st := state{Histories: s.histories, Events: s.events, Ledger: s.led.Entries()}
	for _, inv := range s.invoices {
		st.Invoices = append(st.Invoices, inv)
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		log.Printf("persist: %v", err)
		return
	}
	tmp := s.opts.DataFile + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.opts.DataFile), 0o755); err != nil {
		log.Printf("persist: %v", err)
		return
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("persist: %v", err)
		return
	}
	if err := os.Rename(tmp, s.opts.DataFile); err != nil {
		log.Printf("persist: %v", err)
	}
}

// LoadState restores a snapshot if the data file exists. Call it once at startup.
func (s *Server) LoadState() error {
	if s.opts.DataFile == "" {
		return nil
	}
	b, err := os.ReadFile(s.opts.DataFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, inv := range st.Invoices {
		s.invoices[inv.ID] = inv
		if inv.Status == StatusAdvancing { // a crash mid-advance: let the user retry
			inv.Status = StatusSent
		}
	}
	for k, v := range st.Histories {
		s.histories[k] = v
	}
	for _, id := range st.Events {
		s.events = append(s.events, id)
		s.eventSet[id] = true
	}
	return s.led.Replay(st.Ledger)
}
