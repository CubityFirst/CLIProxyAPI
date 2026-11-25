package cliproxy

import (
	"testing"

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
