package quota

import (
	"sort"
	"strings"
)

// SchedulerCandidate is the non-sensitive CPA identity supplied for one
// request. Candidate selection remains owned by CPA.
type SchedulerCandidate struct {
	AuthID   string
	Priority int
	Status   string
}

// selectCPACandidates keeps CPA's priority order and uses the account ID only
// as a deterministic tie-breaker. Dollar limits belong exclusively to the Key.
func (engine *Engine) selectCPACandidates(candidates []SchedulerCandidate) ([]string, string) {
	_ = engine
	choices := make([]SchedulerCandidate, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		candidate.AuthID = strings.TrimSpace(candidate.AuthID)
		if candidate.AuthID == "" {
			continue
		}
		if _, exists := seen[candidate.AuthID]; exists {
			continue
		}
		seen[candidate.AuthID] = struct{}{}
		choices = append(choices, candidate)
	}
	if len(choices) == 0 {
		return nil, "quota_scheduler_candidates_required"
	}
	sort.SliceStable(choices, func(left, right int) bool {
		if choices[left].Priority != choices[right].Priority {
			return choices[left].Priority < choices[right].Priority
		}
		return choices[left].AuthID < choices[right].AuthID
	})
	result := make([]string, 0, len(choices))
	for _, choice := range choices {
		result = append(result, choice.AuthID)
	}
	return result, ""
}
