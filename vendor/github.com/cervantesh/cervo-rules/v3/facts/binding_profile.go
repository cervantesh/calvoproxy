package facts

import (
	"sort"
	"strconv"
)

// BindingLayout describes a compact slot layout for one rule.
type BindingLayout struct {
	Rule      string        `json:"rule"`
	Variables []BindingSlot `json:"variables,omitempty"`
}

// BindingSlot assigns one variable name to a stable slot index.
type BindingSlot struct {
	Name  string `json:"name"`
	Index int    `json:"index"`
}

// NewBindingLayout builds a stable first-seen variable layout for a rule.
func NewBindingLayout(rule string, patterns ...Pattern) BindingLayout {
	seen := map[string]int{}
	var slots []BindingSlot
	for _, pattern := range patterns {
		for _, term := range pattern.Terms {
			if term.Kind != TermVar || term.Value == "" {
				continue
			}
			if _, ok := seen[term.Value]; ok {
				continue
			}
			index := len(slots)
			seen[term.Value] = index
			slots = append(slots, BindingSlot{Name: term.Value, Index: index})
		}
	}
	return BindingLayout{Rule: rule, Variables: slots}
}

// SlotFor returns the compact slot index for a variable.
func (l BindingLayout) SlotFor(variable string) (int, bool) {
	for _, slot := range l.Variables {
		if slot.Name == variable {
			return slot.Index, true
		}
	}
	return 0, false
}

// BindingFrame stores variable values by compact slot index.
type BindingFrame struct {
	layout BindingLayout
	values []Term
	set    []bool
}

// NewBindingFrame creates a mutable compact binding frame for a layout.
func NewBindingFrame(layout BindingLayout) BindingFrame {
	return BindingFrame{
		layout: layout,
		values: make([]Term, len(layout.Variables)),
		set:    make([]bool, len(layout.Variables)),
	}
}

// Bind sets a variable value. It returns false if the variable is not in the layout.
func (f *BindingFrame) Bind(variable string, value Term) bool {
	slot, ok := f.layout.SlotFor(variable)
	if !ok {
		return false
	}
	f.values[slot] = value
	f.set[slot] = true
	return true
}

// Lookup returns the value bound to a variable.
func (f BindingFrame) Lookup(variable string) (Term, bool) {
	slot, ok := f.layout.SlotFor(variable)
	if !ok || !f.set[slot] {
		return Term{}, false
	}
	return f.values[slot], true
}

// Binding is the public debug/API shape for variable bindings.
type Binding map[string]Term

// Binding materializes the compact frame into a public map.
func (f BindingFrame) Binding() Binding {
	out := Binding{}
	for _, slot := range f.layout.Variables {
		if f.set[slot.Index] {
			out[slot.Name] = f.values[slot.Index]
		}
	}
	return out
}

// CompoundIndexOptions controls index recommendation sensitivity.
type CompoundIndexOptions struct {
	MinCandidateCount int `json:"min_candidate_count,omitempty"`
	MinBindings       int `json:"min_bindings,omitempty"`
}

// Normalize applies conservative defaults for index recommendations.
func (o CompoundIndexOptions) Normalize() CompoundIndexOptions {
	if o.MinCandidateCount <= 0 {
		o.MinCandidateCount = 128
	}
	if o.MinBindings <= 0 {
		o.MinBindings = 512
	}
	return o
}

// CompoundIndexRecommendation is a machine-readable suggestion for an index.
type CompoundIndexRecommendation struct {
	Rule              string `json:"rule"`
	Predicate         string `json:"predicate"`
	DeclaredIndex     int    `json:"declared_index"`
	ConstantPositions []int  `json:"constant_positions,omitempty"`
	CandidateCount    int    `json:"candidate_count,omitempty"`
	MaxBindings       int    `json:"max_bindings,omitempty"`
	Reason            string `json:"reason"`
}

// RecommendCompoundIndexes suggests narrow indexes from plan and cost evidence.
func RecommendCompoundIndexes(plan EvaluationPlan, stats EvaluationStats, options CompoundIndexOptions) []CompoundIndexRecommendation {
	options = options.Normalize()
	bindingsByRule := map[string]int{}
	for _, stat := range stats.Rules {
		bindingsByRule[stat.Name] = stat.MaxBindings
	}
	seen := map[string]struct{}{}
	var out []CompoundIndexRecommendation
	for _, rule := range plan.Rules {
		maxBindings := bindingsByRule[rule.Name]
		for _, pattern := range rule.Patterns {
			if pattern.Negated || pattern.Predicate == "" {
				continue
			}
			if pattern.CandidateCount < options.MinCandidateCount && maxBindings < options.MinBindings {
				continue
			}
			key := recommendationKey(rule.Name, pattern)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, CompoundIndexRecommendation{
				Rule:              rule.Name,
				Predicate:         pattern.Predicate,
				DeclaredIndex:     pattern.DeclaredIndex,
				ConstantPositions: append([]int(nil), pattern.ConstantPositions...),
				CandidateCount:    pattern.CandidateCount,
				MaxBindings:       maxBindings,
				Reason:            indexRecommendationReason(pattern.CandidateCount, maxBindings, options),
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].MaxBindings != out[j].MaxBindings {
			return out[i].MaxBindings > out[j].MaxBindings
		}
		if out[i].CandidateCount != out[j].CandidateCount {
			return out[i].CandidateCount > out[j].CandidateCount
		}
		if out[i].Rule != out[j].Rule {
			return out[i].Rule < out[j].Rule
		}
		return out[i].DeclaredIndex < out[j].DeclaredIndex
	})
	return out
}

func recommendationKey(rule string, pattern PatternPlan) string {
	return rule + "\x00" + pattern.Predicate + "\x00" + strconv.Itoa(pattern.DeclaredIndex)
}

func indexRecommendationReason(candidateCount, maxBindings int, options CompoundIndexOptions) string {
	switch {
	case candidateCount >= options.MinCandidateCount && maxBindings >= options.MinBindings:
		return "high candidate count and binding fanout"
	case candidateCount >= options.MinCandidateCount:
		return "high candidate count"
	default:
		return "high binding fanout"
	}
}
