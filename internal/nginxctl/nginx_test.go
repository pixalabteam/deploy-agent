package nginxctl

import (
	"strings"
	"testing"
)

func TestRenderUsesNameAndOptionalIPHash(t *testing.T) {
	sticky := Render("shop_web", true, []int{4001, 4011})
	for _, want := range []string{"upstream shop_web {", "ip_hash;", "server 127.0.0.1:4001;", "server 127.0.0.1:4011;"} {
		if !strings.Contains(sticky, want) {
			t.Errorf("missing %q in:\n%s", want, sticky)
		}
	}
	if strings.Contains(Render("shop_web", false, []int{4001}), "ip_hash") {
		t.Error("ip_hash must be opt-in")
	}
}
