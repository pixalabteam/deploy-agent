package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestJobsRunOneAtATimeInSubmissionOrder(t *testing.T) {
	srv, _ := newTestServer(t, 40000)
	release := make(chan struct{})
	var order []string
	record := func(name string, block bool) job {
		return job{
			desc: deployResponse{Project: testProject, Service: name},
			fn: func(context.Context) error {
				order = append(order, name)
				if block {
					<-release
				}
				return nil
			},
		}
	}

	first, _ := srv.startJob(record("a", true))
	second, _ := srv.startJob(record("b", false))
	third, _ := srv.startJob(record("c", false))
	if first.Status != "running" || second.Status != "queued" || second.QueuePosition != 1 || third.QueuePosition != 2 {
		t.Fatalf("unexpected descriptors: %+v %+v %+v", first, second, third)
	}
	if got := srv.deploymentFor("").Service; got != "a" {
		t.Errorf("status should keep showing the running job, got %q", got)
	}

	close(release)
	waitFor(t, "queue to drain", func() bool {
		srv.queueMu.Lock()
		defer srv.queueMu.Unlock()
		return !srv.workerRunning
	})
	if len(order) != 3 || order[0] != "a" || order[1] != "b" || order[2] != "c" {
		t.Errorf("order = %v", order)
	}
}

func TestQueueRejectsWhenFull(t *testing.T) {
	srv, _ := newTestServer(t, 40000)
	block := make(chan struct{})
	defer close(block)
	blocked := job{fn: func(context.Context) error { <-block; return nil }}
	for i := 0; i < maxQueuedJobs; i++ {
		if _, ok := srv.startJob(blocked); !ok {
			t.Fatalf("job %d rejected too early", i)
		}
	}
	if _, ok := srv.startJob(blocked); ok {
		t.Error("queue over capacity must be rejected")
	}
}

func TestQueueIsPersistedAndRestored(t *testing.T) {
	srv, _ := newTestServer(t, 40000)
	srv.cfg.QueueFile = filepath.Join(t.TempDir(), "queue.json")

	release := make(chan struct{})
	srv.startJob(job{fn: func(context.Context) error { <-release; return nil }, spec: &jobSpec{Kind: jobDeploy, Project: testProject, Service: "api", Image: "img:1"}})
	srv.startJob(job{fn: func(context.Context) error { return nil }, spec: &jobSpec{Kind: jobScale, Project: testProject, Service: "api", Count: 1}})
	srv.startJob(job{fn: func(context.Context) error { return nil }}) // self-update style: never persisted

	var saved []jobSpec
	data, err := os.ReadFile(srv.cfg.QueueFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 2 || saved[0].Image != "img:1" || saved[1].Kind != jobScale {
		t.Fatalf("persisted = %+v (running job must stay in the file, update job must not)", saved)
	}

	// A fresh process finds the file and runs what was left, even a job that was running.
	restored, _ := newTestServer(t, 40001)
	restored.cfg.QueueFile = filepath.Join(t.TempDir(), "queue.json")
	os.WriteFile(restored.cfg.QueueFile, []byte(`[
	  {"kind":"scale","project":"app","service":"api","count":1},
	  {"kind":"deploy","project":"gone","service":"api","image":"x"}]`), 0o600)
	if err := restored.RestoreQueue(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "restored job to finish", func() bool { return restored.deploymentFor(testProject).Status == "ok" })
	close(release)
	for _, s := range []*Server{srv, restored} {
		s := s
		waitFor(t, "queue to drain", func() bool {
			s.queueMu.Lock()
			defer s.queueMu.Unlock()
			return !s.workerRunning
		})
	}
	if b, _ := os.ReadFile(restored.cfg.QueueFile); string(b) != "[]" {
		t.Errorf("queue file should be empty once jobs finish, got %s", b)
	}
}
