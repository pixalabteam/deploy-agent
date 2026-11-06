package agent

import (
	"os"
	"strings"
	"testing"
)

func TestCleanupImagesKeepsNewestAndNeverPrunesVolumes(t *testing.T) {
	log := t.TempDir() + "/docker.log"
	script := t.TempDir() + "/docker"
	body := "#!/bin/sh\necho \"$@\" >> " + log + "\n" +
		"if [ \"$1 $2\" = \"image ls\" ]; then printf 'r:c\\nr:b\\nr:a\\nr:old\\n'; fi\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	state := &State{path: t.TempDir() + "/state.json"}
	if err := state.SetInstance("other", "worker", 0, InstanceState{Image: "r:a"}); err != nil {
		t.Fatal(err)
	}
	d := NewDeployer(Config{DockerBin: script, BuildCacheMaxAge: "72h"}, state)

	d.CleanupImages(Target{Project: ProjectConfig{ImageKeepCount: 2}}, "r:c")

	got, _ := os.ReadFile(log)
	calls := string(got)
	if strings.Contains(calls, "rmi r:c") || strings.Contains(calls, "rmi r:b") || strings.Contains(calls, "rmi r:a") {
		t.Errorf("removed a protected image:\n%s", calls)
	}
	for _, want := range []string{"rmi r:old", "image prune -f", "builder prune -af --filter until=72h"} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing call %q:\n%s", want, calls)
		}
	}
	if strings.Contains(calls, "volume") || strings.Contains(calls, "system prune") {
		t.Errorf("must never prune volumes:\n%s", calls)
	}
}
