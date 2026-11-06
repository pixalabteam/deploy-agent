package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// InstanceState describes the container running in one "slot" (one instance of an
// HTTP service, or a worker). Color alternates "blue"/"green" on every deploy so the
// new container name never collides with the old one (still running during the switch).
type InstanceState struct {
	Color         string `json:"color"`
	ContainerName string `json:"container_name"`
	Port          int    `json:"port,omitempty"` // 0 for workers (no exposed port)
	Image         string `json:"image"`
}

type ServiceState struct {
	Instances []InstanceState `json:"instances"`
	// Count overrides the configured instance_count once it was changed from the
	// admin UI; 0 means "never changed, use the config value".
	Count int `json:"count,omitempty"`
}

type State struct {
	// Projects maps project name -> service name -> state.
	Projects map[string]map[string]*ServiceState `json:"projects"`

	mu   sync.Mutex `json:"-"`
	path string     `json:"-"`
}

func LoadState(path string) (*State, error) {
	s := &State{path: path}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state file: %w", err)
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("parse state file: %w", err)
	}
	return s, nil
}

// save writes the state to disk atomically (write a tmp file, then rename) so a
// half-written state file is never left behind if the agent is killed mid-write.
func (s *State) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write state tmp file: %w", err)
	}
	return os.Rename(tmp, s.path)
}

// service returns the state of (project, service), creating it when absent.
// Callers hold s.mu.
func (s *State) service(project, service string) *ServiceState {
	if s.Projects == nil {
		s.Projects = map[string]map[string]*ServiceState{}
	}
	if s.Projects[project] == nil {
		s.Projects[project] = map[string]*ServiceState{}
	}
	if s.Projects[project][service] == nil {
		s.Projects[project][service] = &ServiceState{}
	}
	return s.Projects[project][service]
}

// Instance returns the current state of slot i, defaulting to "no container yet"
// (first deploy on a clean host) when nothing was ever saved.
func (s *State) Instance(project, service string, i int) InstanceState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if svc := s.Projects[project][service]; svc != nil && i < len(svc.Instances) {
		return svc.Instances[i]
	}
	return InstanceState{}
}

func (s *State) SetInstance(project, service string, i int, inst InstanceState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	svc := s.service(project, service)
	for len(svc.Instances) <= i {
		svc.Instances = append(svc.Instances, InstanceState{})
	}
	svc.Instances[i] = inst
	return s.save()
}

func (s *State) EffectiveCount(project, service string, configDefault int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if svc := s.Projects[project][service]; svc != nil && svc.Count > 0 {
		return svc.Count
	}
	return configDefault
}

// SetCount persists the desired instance count and drops state of slots beyond
// it, so a later scale-up starts those slots from a clean blue/green.
func (s *State) SetCount(project, service string, n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	svc := s.service(project, service)
	svc.Count = n
	if len(svc.Instances) > n {
		svc.Instances = svc.Instances[:n]
	}
	return s.save()
}

// Snapshot returns a copy to serve the /status endpoint, avoiding a race with a
// deploy writing at the same time.
func (s *State) Snapshot() map[string]map[string]*ServiceState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]map[string]*ServiceState{}
	for project, svcs := range s.Projects {
		out[project] = map[string]*ServiceState{}
		for name, svc := range svcs {
			out[project][name] = &ServiceState{Instances: append([]InstanceState{}, svc.Instances...), Count: svc.Count}
		}
	}
	return out
}

// Images lists every image recorded for any service, to keep cleanup away from them.
func (s *State) Images() map[string]bool {
	snapshot := s.Snapshot()
	images := map[string]bool{}
	for _, svcs := range snapshot {
		for _, svc := range svcs {
			for _, inst := range svc.Instances {
				images[inst.Image] = true
			}
		}
	}
	return images
}
