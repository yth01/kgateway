package policy

import (
	"errors"
	"maps"
	"reflect"
	"slices"

	apiannotations "github.com/kgateway-dev/kgateway/v2/api/annotations"
	"github.com/kgateway-dev/kgateway/v2/pkg/pluginsdk/ir"
)

var ErrUnsupportedMergeStrategy = errors.New("unsupported merge strategy")

// MergeStrategy defines how two policies should be merged
type MergeStrategy string

const (
	// AugmentedShallowMerge implies that Merge(p1,p2) will produce a policy composition of p1 and p2 such
	// that the policy contains all set fields in p1 and set fields in p2 that are not set in p1,
	// i.e., p1 is augmented by p2.
	AugmentedShallowMerge MergeStrategy = "AugmentedShallow"

	// AugmentedDeepMerge implies that Merge(p1,p2) will produce a policy composition of p1 and p2 such
	// that the policy contains all set fields in p1 and deep merge fields in p2 while giving priority to p1,
	// i.e., p1 is augmented by deep merging p2.
	AugmentedDeepMerge MergeStrategy = "AugmentedDeep"

	// OverridableShallowMerge implies that Merge(p1,p2) will produce a policy composition of p1 and p2 such
	// that the policy contains all set fields in p2 and set fields in p1 that are not set in p2,
	// i.e., p2 overrides p1.
	OverridableShallowMerge MergeStrategy = "OverridableShallow"

	// OverridableDeepMerge implies that Merge(p1,p2) will produce a policy composition of p1 and p2 such
	// that the policy contains all set fields in p2 and deep merge fields in p1 while giving priority to p2,
	// i.e., p1 is overridden by deep merging p2.
	OverridableDeepMerge MergeStrategy = "OverridableDeep"
)

type MergeOptions struct {
	// Merge strategy to use
	// Defaults to AugmentedMerge
	Strategy MergeStrategy
}

// IsMergeable returns a boolean indicating whether p2 can be merged into p1 for the given merge options
func IsMergeable(p1, p2 any, opts MergeOptions) bool {
	switch opts.Strategy {
	case AugmentedShallowMerge:
		return isNil(p1) && !isNil(p2)

	case OverridableShallowMerge, AugmentedDeepMerge, OverridableDeepMerge:
		return !isNil(p2)

	default:
		return false
	}
}

// IsSettable returns a boolean indicating whether p1 can be set for the given merge options
func IsSettable(p1 any, opts MergeOptions) bool {
	// Pass non-empty string value to treat p2 as a non-nil value while short-circuiting to IsMergeable
	return IsMergeable(p1, "not-nil", opts)
}

func GetMergeStrategy(
	priority apiannotations.InheritedPolicyPriorityValue,
	sameHierarchy bool,
) MergeStrategy {
	if sameHierarchy {
		return AugmentedShallowMerge
	}

	switch priority {
	case apiannotations.ShallowMergePreferParent:
		return OverridableShallowMerge

	case apiannotations.ShallowMergePreferChild:
		return AugmentedShallowMerge

	case apiannotations.DeepMergePreferParent:
		return OverridableDeepMerge

	case apiannotations.DeepMergePreferChild:
		return AugmentedDeepMerge

	default:
		return AugmentedShallowMerge
	}
}

func isNil(arg any) bool {
	if v := reflect.ValueOf(arg); !v.IsValid() ||
		((v.Kind() == reflect.Ptr ||
			v.Kind() == reflect.Interface ||
			v.Kind() == reflect.Slice ||
			v.Kind() == reflect.Map ||
			v.Kind() == reflect.Chan ||
			v.Kind() == reflect.Func) && v.IsNil()) ||
		(v.Kind() == reflect.String && v.Len() == 0) {
		return true
	}
	return false
}

func groupPoliciesByHierarchicalPriority(policies []ir.PolicyAtt) map[int][]ir.PolicyAtt {
	groups := make(map[int][]ir.PolicyAtt)
	for _, policy := range policies {
		priority := policy.HierarchicalPriority
		groups[priority] = append(groups[priority], policy)
	}
	return groups
}

// mergePolicies merges the given policy ordered from high to low priority (both hierarchically
// and within the same hierarchy) based on the constraints defined per PolicyAtt.
//
// It first merges policies that belong to the same hierarchy in the config tree, and then
// merges the result of the merged policy per hierarchy into a single policy.
func MergePolicies[T comparable](
	policies []ir.PolicyAtt,
	mergeFn func(*T, *T, *ir.AttachedPolicyRef, MergeOptions, ir.MergeOrigins),
) ir.PolicyAtt {
	var out ir.PolicyAtt
	if len(policies) == 0 {
		return out
	}
	_, ok := any(policies[0].PolicyIr).(*T)
	// ignore unknown types
	if !ok {
		return out
	}

	policiesByHierarchy := groupPoliciesByHierarchicalPriority(policies)
	if len(policiesByHierarchy) == 0 {
		return out
	}

	out.MergeOrigins = make(ir.MergeOrigins)
	mergedByHierarchy := make([]ir.PolicyAtt, 0, len(policiesByHierarchy))
	for _, hierarchicalPriority := range slices.Backward(slices.Sorted(maps.Keys(policiesByHierarchy))) {
		tmp := merge(policiesByHierarchy[hierarchicalPriority], true, out.MergeOrigins, mergeFn)
		mergedByHierarchy = append(mergedByHierarchy, tmp)
	}
	out = merge(mergedByHierarchy, false, out.MergeOrigins, mergeFn)

	return out
}

func merge[T comparable](
	policies []ir.PolicyAtt,
	sameHierarchy bool,
	mergeOrigins ir.MergeOrigins,
	mergeFn func(*T, *T, *ir.AttachedPolicyRef, MergeOptions, ir.MergeOrigins),
) ir.PolicyAtt {
	// base policy to merge into has an empty PolicyIr so it can always be merged into
	var pol T
	out := ir.PolicyAtt{
		GroupKind: policies[0].GroupKind,
		PolicyRef: policies[0].PolicyRef,
		PolicyIr:  any(&pol).(ir.PolicyIR),
	}
	merged := any(out.PolicyIr).(*T)

	for i := range policies {
		p2 := any(policies[i].PolicyIr).(*T)
		p2Ref := policies[i].PolicyRef

		mergeOpts := MergeOptions{
			Strategy: GetMergeStrategy(policies[i].InheritedPolicyPriority, sameHierarchy),
		}

		mergeFn(merged, p2, p2Ref, mergeOpts, mergeOrigins)
		out.Errors = append(out.Errors, policies[i].Errors...)
		if sameHierarchy {
			out.InheritedPolicyPriority = policies[i].InheritedPolicyPriority
		}
	}
	out.MergeOrigins = mergeOrigins

	return out
}
