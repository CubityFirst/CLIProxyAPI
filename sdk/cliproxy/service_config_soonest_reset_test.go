package cliproxy

import (
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestSoonestResetRoutingSelector(t *testing.T) {
	for _, name := range []string{"soonest-reset", "SoonestReset", "sr"} {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{
			Routing: internalconfig.RoutingConfig{Strategy: name},
		})
		if state.strategy != "soonest-reset" {
			t.Fatalf("strategy for %q = %q, want soonest-reset", name, state.strategy)
		}
		if _, ok := newRoutingSelector(state).(*coreauth.SoonestResetSelector); !ok {
			t.Fatalf("selector type = %T, want *auth.SoonestResetSelector", newRoutingSelector(state))
		}
	}
}

func TestSessionAffinityExpiringWindowRoutingState(t *testing.T) {
	cases := []struct {
		affinity bool
		raw      string
		want     time.Duration
	}{
		{affinity: true, raw: "24h", want: 24 * time.Hour},
		{affinity: true, raw: " 6h ", want: 6 * time.Hour},
		{affinity: true, raw: "", want: 0},
		{affinity: true, raw: "0", want: 0},
		{affinity: true, raw: "soon", want: 0},
		{affinity: false, raw: "24h", want: 0},
	}
	for _, tc := range cases {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{
			Routing: internalconfig.RoutingConfig{
				Strategy:                      "soonest-reset",
				SessionAffinity:               tc.affinity,
				SessionAffinityExpiringWindow: tc.raw,
			},
		})
		if state.sessionAffinityExpiring != tc.want {
			t.Fatalf("affinity=%v window=%q: got %v, want %v", tc.affinity, tc.raw, state.sessionAffinityExpiring, tc.want)
		}
	}
}
