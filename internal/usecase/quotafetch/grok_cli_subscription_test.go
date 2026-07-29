package quotafetch

// grok_cli_subscription_test.go pins the upstream 59b78282 subscriptionAccess +
// plan-resolution logic: a non-empty tier that is not free/none/null grants
// subscription access even without hasGrokCodeAccess, and the tier is
// title-cased for display ("grok_build" → "Grok Build").

import "testing"

func TestGrokCliSubscriptionAccess_TierGrantsAccess(t *testing.T) {
	cases := []struct {
		name string
		user map[string]any
		want bool
	}{
		{"hasGrokCodeAccess", map[string]any{"hasGrokCodeAccess": true}, true},
		{"unified billing via config", nil, true}, // config set below
		{"pro tier grants access", map[string]any{"subscriptionTier": "pro"}, true},
		{"grok_build tier grants access", map[string]any{"subscription_tier": "grok_build"}, true},
		{"free tier no access", map[string]any{"subscriptionTier": "free"}, false},
		{"none tier no access", map[string]any{"subscriptionTier": "none"}, false},
		{"null tier no access", map[string]any{"subscriptionTier": "null"}, false},
		{"empty user no access", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			config := map[string]any{}
			if c.name == "unified billing via config" {
				config["isUnifiedBillingUser"] = true
			}
			if got := grokCliSubscriptionAccess(c.user, config); got != c.want {
				t.Errorf("grokCliSubscriptionAccess(%v) = %v, want %v", c.user, got, c.want)
			}
		})
	}
}

func TestGrokCliResolvePlan_TitleCase(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"grok_build", "Grok Build"},
		{"grok-build", "Grok Build"},
		{"GROK BUILD", "Grok Build"},
		{"pro", "Pro"},
		{"super_pro_tier", "Super Pro Tier"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			user := map[string]any{"subscriptionTier": c.in}
			if got := grokCliResolvePlan(user, nil); got != c.want {
				t.Errorf("grokCliResolvePlan(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
	// hasGrokCodeAccess path still wins the Grok Code label.
	if got := grokCliResolvePlan(map[string]any{"hasGrokCodeAccess": true}, nil); got != "Grok Code" {
		t.Errorf("hasGrokCodeAccess plan = %q, want Grok Code", got)
	}
}

func TestGrokCliTitleCaseTier(t *testing.T) {
	if got := grokCliTitleCaseTier("grok_build"); got != "Grok Build" {
		t.Errorf("grok_build → %q, want Grok Build", got)
	}
	if got := grokCliTitleCaseTier("free"); got != "Free" {
		t.Errorf("free → %q, want Free", got)
	}
}
