package router

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestCapabilityIndex_MergeOverrideWinsAndDeny(t *testing.T) {
	idx := newCapabilityIndex(map[string][]string{
		"m-override-adds": {"tools"},
		"m-deny":          {"!vision"}, // remove a wrongly-auto-reported cap
	})
	// Auto-derived data: m-deny has vision (wrong), m-auto has tools.
	idx.setAuto(map[string]map[string]bool{
		"m-deny": {"vision": true, "tools": true},
		"m-auto": {"tools": true},
	})

	if !idx.satisfies("m-override-adds", []string{"tools"}) {
		t.Error("override should add tools")
	}
	if idx.satisfies("m-deny", []string{"vision"}) {
		t.Error("!vision override should remove the auto-reported vision cap")
	}
	if !idx.satisfies("m-deny", []string{"tools"}) {
		t.Error("m-deny should keep tools (only vision denied)")
	}
	if !idx.satisfies("m-auto", []string{"tools"}) {
		t.Error("auto tools should apply")
	}
	// Unknown model → fail-closed.
	if idx.satisfies("m-unknown", []string{"tools"}) {
		t.Error("unknown model must not satisfy (fail-closed)")
	}
	if idx.known("m-unknown") {
		t.Error("m-unknown should be unknown")
	}
	// Empty required → always satisfied.
	if !idx.satisfies("m-unknown", nil) {
		t.Error("no required caps → satisfied")
	}
}

func TestCapabilityIndex_CapableModelsSorted(t *testing.T) {
	idx := newCapabilityIndex(map[string][]string{
		"zeta":  {"vision", "tools"},
		"alpha": {"vision", "tools"},
		"beta":  {"tools"}, // no vision
	})
	got := idx.capableModels([]string{"vision", "tools"})
	want := []string{"alpha", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("capableModels(vision,tools) = %v; want %v", got, want)
	}
}

func TestCapsRequired(t *testing.T) {
	if got := capsRequired(false, false); len(got) != 0 {
		t.Errorf("none: %v", got)
	}
	if got := capsRequired(true, false); !reflect.DeepEqual(got, []string{capVision}) {
		t.Errorf("vision only: %v", got)
	}
	if got := capsRequired(false, true); !reflect.DeepEqual(got, []string{capTools}) {
		t.Errorf("tools only: %v", got)
	}
	if got := capsRequired(true, true); !reflect.DeepEqual(got, []string{capVision, capTools}) {
		t.Errorf("both: %v", got)
	}
}

func TestFetchOpenRouterCapabilities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[
			{"id":"vendor/vis-tools:free","architecture":{"input_modalities":["text","image"]},"supported_parameters":["tools","temperature"]},
			{"id":"vendor/text-only:free","architecture":{"input_modalities":["text"]},"supported_parameters":["temperature"]},
			{"id":"vendor/tools-only:free","architecture":{"input_modalities":["text"]},"supported_parameters":["tools"]}
		]}`)
	}))
	defer srv.Close()
	t.Setenv("PROXY_OPENROUTER_MODELS_URL", srv.URL)

	auto, err := fetchOpenRouterCapabilities(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !auto["vendor/vis-tools:free"]["vision"] || !auto["vendor/vis-tools:free"]["tools"] {
		t.Errorf("vis-tools caps wrong: %v", auto["vendor/vis-tools:free"])
	}
	if len(auto["vendor/text-only:free"]) != 0 {
		t.Errorf("text-only should have no caps: %v", auto["vendor/text-only:free"])
	}
	if !auto["vendor/tools-only:free"]["tools"] || auto["vendor/tools-only:free"]["vision"] {
		t.Errorf("tools-only caps wrong: %v", auto["vendor/tools-only:free"])
	}
}

func TestApplyCapabilityFilterAndRescue(t *testing.T) {
	s := &RouterService{
		config:        breakerConfig{FailureThreshold: 3},
		modelBreakers: map[string]*modelBreakerState{},
		// Rescue draws only from the configured profiles' models (not the whole
		// capability catalog), so the pool must live in a profile here.
		policy: policyConfig{Profiles: map[string][]string{"p": {"vis-a", "vis-b", "text-x", "tools-y"}}},
		capabilities: newCapabilityIndex(map[string][]string{
			"vis-a":   {"vision"},
			"vis-b":   {"vision"},
			"text-x":  {},
			"tools-y": {"tools"},
		}),
	}
	// Chain has one vision model + one text-only. Vision required → keep vis-a only.
	chain := []modelAttempt{{Profile: "p", Model: "vis-a"}, {Profile: "p", Model: "text-x"}}
	got := s.applyCapabilityFilter(chain, "p", "", []string{capVision})
	if len(got) != 1 || got[0].Model != "vis-a" {
		t.Fatalf("filter should keep only vis-a, got %v", got)
	}

	// Chain has NO tool model → rescue to all known tool-capable models.
	chain2 := []modelAttempt{{Profile: "p", Model: "vis-a"}, {Profile: "p", Model: "text-x"}}
	got2 := s.applyCapabilityFilter(chain2, "p", "", []string{capTools})
	if len(got2) != 1 || got2[0].Model != "tools-y" {
		t.Fatalf("rescue should surface tools-y, got %v", got2)
	}
	if got2[0].Profile != "p" {
		t.Errorf("rescue must stamp the request profile, got %q", got2[0].Profile)
	}

	// No known-capable model at all → nil (caller turns into a clear 503).
	got3 := s.applyCapabilityFilter(chain2, "p", "", []string{"audio"})
	if got3 != nil {
		t.Errorf("no capable model → nil, got %v", got3)
	}
}

// TestCapabilityRescue_StaysWithinProfiles guards High#1: rescue must draw only
// from the configured profiles' models, never the full (auto-derived) catalog,
// so it can't escape to paid/unvetted model ids.
func TestCapabilityRescue_StaysWithinProfiles(t *testing.T) {
	s := &RouterService{
		config:        breakerConfig{FailureThreshold: 3},
		modelBreakers: map[string]*modelBreakerState{},
		policy:        policyConfig{Profiles: map[string][]string{"p": {"in-profile"}}},
		capabilities: newCapabilityIndex(map[string][]string{
			"in-profile":          {"tools"},
			"off-profile-catalog": {"tools"}, // known-capable but NOT in any profile
		}),
	}
	got := s.capabilityRescue("p", "", []string{capTools})
	if len(got) != 1 || got[0].Model != "in-profile" {
		t.Fatalf("rescue must stay within profiles, got %v", got)
	}
}

func TestCapabilityRescuePreservesExplicitProviderModelPairs(t *testing.T) {
	s := &RouterService{
		config:        breakerConfig{FailureThreshold: 3},
		modelBreakers: map[string]*modelBreakerState{},
		providerProfiles: providerProfiles{
			"coding": {
				{Provider: providerOpenRouter, Model: "shared-model"},
				{Provider: providerCerebras, Model: "shared-model"},
				{Provider: providerGroq, Model: "groq-tools"},
			},
		},
		capabilities: newCapabilityIndex(map[string][]string{
			"shared-model": {"tools"},
			"groq-tools":   {"tools"},
		}),
	}
	got := s.capabilityRescue("reasoning", "/chat/completions", []string{capTools})
	if len(got) != 3 {
		t.Fatalf("expected every explicit provider-model pair, got %v", got)
	}
	if got[0].Provider != providerOpenRouter || got[1].Provider != providerCerebras || got[2].Provider != providerGroq {
		t.Fatalf("provider priority/pairs were lost: %v", got)
	}
	for _, attempt := range got {
		if attempt.Profile != "reasoning" || attempt.Path != "/chat/completions" {
			t.Fatalf("rescue should stamp request metadata: %v", got)
		}
	}
}

func TestCapabilityIndex_ConcurrentRefreshNoRace(t *testing.T) {
	idx := newCapabilityIndex(map[string][]string{"m": {"tools"}})
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			idx.setAuto(map[string]map[string]bool{"m": {"vision": true}, "n": {"tools": true}})
		}
		close(done)
	}()
	for i := 0; i < 1000; i++ {
		_ = idx.satisfies("m", []string{"tools"})
		_ = idx.capableModels([]string{"tools"})
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh/read deadlock")
	}
}
