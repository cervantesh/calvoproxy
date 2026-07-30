package cervomodelpolicy

import (
	"fmt"
	"strings"
)

const defaultMaxModelChainLength = 8

// ValidationCode identifies a specific configuration issue.
type ValidationCode string

const (
	ValidationDefaultProfileMissing ValidationCode = "default_profile_missing"
	ValidationProfileNameEmpty      ValidationCode = "profile_name_empty"
	ValidationProfileChainEmpty     ValidationCode = "profile_chain_empty"
	ValidationModelNameEmpty        ValidationCode = "model_name_empty"
	ValidationModelDuplicate        ValidationCode = "model_duplicate"
	ValidationModelChainTooLong     ValidationCode = "model_chain_too_long"
	ValidationAliasNameEmpty        ValidationCode = "alias_name_empty"
	ValidationAliasTargetMissing    ValidationCode = "alias_target_missing"
)

// ValidationIssue describes a non-fatal model policy configuration problem.
type ValidationIssue struct {
	Code    ValidationCode
	Field   string
	Message string
}

// ValidateConfig reports actionable issues in a model policy config without
// normalizing or mutating it.
func ValidateConfig(cfg Config) []ValidationIssue {
	var issues []ValidationIssue

	defaultProfile := strings.ToLower(strings.TrimSpace(cfg.DefaultProfile))
	if defaultProfile == "" {
		issues = append(issues, ValidationIssue{
			Code:    ValidationDefaultProfileMissing,
			Field:   "DefaultProfile",
			Message: "default profile is empty",
		})
	}

	knownProfiles := map[string]struct{}{}
	for profile, chain := range cfg.Profiles {
		trimmedProfile := strings.ToLower(strings.TrimSpace(profile))
		if trimmedProfile == "" {
			issues = append(issues, ValidationIssue{
				Code:    ValidationProfileNameEmpty,
				Field:   "Profiles",
				Message: "profile name is empty",
			})
			continue
		}
		knownProfiles[trimmedProfile] = struct{}{}
		if len(chain) == 0 {
			issues = append(issues, ValidationIssue{
				Code:    ValidationProfileChainEmpty,
				Field:   fmt.Sprintf("Profiles[%q]", profile),
				Message: "profile model chain is empty",
			})
			continue
		}
		validateModelChain(profile, chain, &issues)
	}

	if defaultProfile != "" {
		if _, ok := knownProfiles[defaultProfile]; !ok {
			issues = append(issues, ValidationIssue{
				Code:    ValidationDefaultProfileMissing,
				Field:   "DefaultProfile",
				Message: fmt.Sprintf("default profile %q is not defined", cfg.DefaultProfile),
			})
		}
	}

	for alias, profile := range cfg.Aliases {
		trimmedAlias := strings.ToLower(strings.TrimSpace(alias))
		if trimmedAlias == "" {
			issues = append(issues, ValidationIssue{
				Code:    ValidationAliasNameEmpty,
				Field:   "Aliases",
				Message: "alias name is empty",
			})
			continue
		}
		trimmedProfile := strings.ToLower(strings.TrimSpace(profile))
		if _, ok := knownProfiles[trimmedProfile]; !ok {
			issues = append(issues, ValidationIssue{
				Code:    ValidationAliasTargetMissing,
				Field:   fmt.Sprintf("Aliases[%q]", alias),
				Message: fmt.Sprintf("alias %q points to missing profile %q", alias, profile),
			})
		}
	}

	return issues
}

func validateModelChain(profile string, chain []string, issues *[]ValidationIssue) {
	seen := map[string]struct{}{}
	validModels := 0
	for i, model := range chain {
		trimmedModel := strings.TrimSpace(model)
		field := fmt.Sprintf("Profiles[%q][%d]", profile, i)
		if trimmedModel == "" {
			*issues = append(*issues, ValidationIssue{
				Code:    ValidationModelNameEmpty,
				Field:   field,
				Message: "model name is empty",
			})
			continue
		}
		validModels++
		if _, ok := seen[trimmedModel]; ok {
			*issues = append(*issues, ValidationIssue{
				Code:    ValidationModelDuplicate,
				Field:   field,
				Message: fmt.Sprintf("model %q appears more than once in profile %q", trimmedModel, profile),
			})
			continue
		}
		seen[trimmedModel] = struct{}{}
	}
	if validModels == 0 {
		*issues = append(*issues, ValidationIssue{
			Code:    ValidationProfileChainEmpty,
			Field:   fmt.Sprintf("Profiles[%q]", profile),
			Message: "profile model chain has no usable models",
		})
	}
	if validModels > defaultMaxModelChainLength {
		*issues = append(*issues, ValidationIssue{
			Code:    ValidationModelChainTooLong,
			Field:   fmt.Sprintf("Profiles[%q]", profile),
			Message: fmt.Sprintf("profile model chain has %d models; recommended maximum is %d", validModels, defaultMaxModelChainLength),
		})
	}
}
