package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

const (
	jobDeploy = "deploy"
	jobScale  = "scale"

	// maxQueuedJobs caps running + waiting jobs.
	maxQueuedJobs = 20
)

// jobSpec is the persistable description of a job: enough to rebuild its closure
// after the agent restarts.
type jobSpec struct {
	Kind    string `json:"kind"`
	Project string `json:"project"`
	Service string `json:"service"`
	Image   string `json:"image,omitempty"`
	Count   int    `json:"count,omitempty"`
}

type job struct {
	desc         deployResponse
	fn           func(ctx context.Context) error
	afterSuccess func()
	// spec is nil for jobs that must not survive a restart (self-update).
	spec *jobSpec
}

func (s *Server) buildJob(spec jobSpec) (job, error) {
	target, err := s.cfg.Target(spec.Project, spec.Service)
	if err != nil {
		return job{}, err
	}
	switch spec.Kind {
	case jobDeploy:
		return job{
			desc:         deployResponse{Project: spec.Project, Service: spec.Service, Image: spec.Image},
			fn:           func(ctx context.Context) error { return s.deployer.Deploy(ctx, target, spec.Image) },
			afterSuccess: func() { s.deployer.CleanupImages(target, spec.Image) },
			spec:         &spec,
		}, nil
	case jobScale:
		return job{
			desc: deployResponse{Project: spec.Project, Service: spec.Service, Image: fmt.Sprintf("scale to %d instances", spec.Count)},
			fn:   func(ctx context.Context) error { return s.deployer.Scale(ctx, target, spec.Count) },
			spec: &spec,
		}, nil
	}
	return job{}, fmt.Errorf("unknown job kind %q", spec.Kind)
}

// enqueue queues a validated spec; false means the queue is full.
func (s *Server) enqueue(spec jobSpec) (deployResponse, bool) {
	j, err := s.buildJob(spec)
	if err != nil { // callers validate the target first, so this is a programming error
		return deployResponse{Status: "error", Error: err.Error()}, false
	}
	return s.startJob(j)
}

// startJob appends the job to the FIFO queue and returns its descriptor, with
// status "running" when it starts right away or "queued" plus its position. It
// returns false when the queue is full. afterSuccess runs once the status already
// reads "ok", still inside the worker, so the next job never overlaps it.
func (s *Server) startJob(j job) (deployResponse, bool) {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	pending := len(s.queue)
	if s.current != nil {
		pending++
	}
	if pending >= maxQueuedJobs {
		return j.desc, false
	}
	j.desc.QueuePosition = pending
	j.desc.Status = "running"
	if j.desc.QueuePosition > 0 {
		j.desc.Status = "queued"
	}
	s.queue = append(s.queue, j)
	s.persistQueueLocked()
	s.setDeployment(j.desc)
	if !s.workerRunning {
		s.workerRunning = true
		go s.runQueue()
	}
	return j.desc, true
}

func (s *Server) runQueue() {
	for {
		s.queueMu.Lock()
		if len(s.queue) == 0 {
			s.workerRunning = false
			s.queueMu.Unlock()
			return
		}
		j := s.queue[0]
		s.queue = s.queue[1:]
		// The running job stays in the queue file until it finishes, so a restart
		// in the middle of it re-runs it (deploys and scaling are idempotent).
		s.current = &j
		s.persistQueueLocked()
		s.queueMu.Unlock()

		succeeded := s.runJob(j)

		s.queueMu.Lock()
		s.current = nil
		s.persistQueueLocked()
		s.queueMu.Unlock()

		if succeeded && j.afterSuccess != nil {
			j.afterSuccess()
		}
	}
}

func (s *Server) runJob(j job) (succeeded bool) {
	desc := j.desc
	desc.Status, desc.QueuePosition = "running", 0
	s.setDeployment(desc)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	log.Printf("starting job project=%s service=%s image=%s", desc.Project, desc.Service, desc.Image)
	if err := j.fn(ctx); err != nil {
		log.Printf("job project=%s service=%s image=%s failed: %v", desc.Project, desc.Service, desc.Image, err)
		desc.Status, desc.Error = "error", err.Error()
		s.setDeployment(desc)
		return false
	}
	log.Printf("job project=%s service=%s image=%s succeeded", desc.Project, desc.Service, desc.Image)
	desc.Status = "ok"
	s.setDeployment(desc)
	return true
}

// persistQueueLocked writes the running job plus the waiting ones to the queue
// file (tmp file + rename, like the state file). Callers hold queueMu. A failed
// write is only logged: refusing to deploy over a disk hiccup would be worse
// than losing restart-safety for that moment.
func (s *Server) persistQueueLocked() {
	if s.cfg.QueueFile == "" {
		return
	}
	specs := []jobSpec{}
	if s.current != nil && s.current.spec != nil {
		specs = append(specs, *s.current.spec)
	}
	for _, j := range s.queue {
		if j.spec != nil {
			specs = append(specs, *j.spec)
		}
	}
	if err := writeJSONFile(s.cfg.QueueFile, specs); err != nil {
		log.Printf("warning: persist queue: %v", err)
	}
}

func writeJSONFile(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RestoreQueue re-enqueues the jobs a previous agent process left in the queue
// file, in their original order. Specs naming a project or service that no longer
// exists in the config are skipped with a log line.
func (s *Server) RestoreQueue() error {
	if s.cfg.QueueFile == "" {
		return nil
	}
	data, err := os.ReadFile(s.cfg.QueueFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read queue file: %w", err)
	}
	var specs []jobSpec
	if err := json.Unmarshal(data, &specs); err != nil {
		return fmt.Errorf("parse queue file: %w", err)
	}
	for _, spec := range specs {
		j, err := s.buildJob(spec)
		if err != nil {
			log.Printf("warning: dropping persisted job %+v: %v", spec, err)
			continue
		}
		log.Printf("restored job project=%s service=%s image=%s count=%d", spec.Project, spec.Service, spec.Image, spec.Count)
		if _, ok := s.startJob(j); !ok {
			return fmt.Errorf("queue file holds more than %d jobs", maxQueuedJobs)
		}
	}
	return nil
}
