package scheduler

import (
	"linkedin-post-manager/internal/domain"
)

// SelectNextSubject implements Nginx's Smooth Weighted Round-Robin (SWRR) algorithm.
//
// Given active subjects with Priority weights (e.g. .NET=10, Angular=10, Golang=4):
//  1. Each active subject's CurrentWeight is incremented by its Priority (clamped 1..10).
//  2. The subject with the highest CurrentWeight is selected (ties broken by older/never LastUsedAt).
//  3. The sum of all active priorities (totalWeight) is subtracted from the winner's CurrentWeight.
//
// Over N selections, subjects receive posts strictly proportional to Priority (e.g. 10:10:4)
// while smoothly interleaving topics week-to-week instead of clumping.
func SelectNextSubject(subjects []domain.Subject) (chosen domain.Subject, updated []domain.Subject, ok bool) {
	if len(subjects) == 0 {
		return domain.Subject{}, nil, false
	}

	updated = make([]domain.Subject, len(subjects))
	copy(updated, subjects)

	totalWeight := 0
	bestIdx := -1

	for i := range updated {
		p := updated[i].Priority
		if p < 1 {
			p = 1
		}
		if p > 10 {
			p = 10
		}
		updated[i].Priority = p
		updated[i].CurrentWeight += p
		totalWeight += p

		if bestIdx == -1 || isBetterCandidate(updated[i], updated[bestIdx]) {
			bestIdx = i
		}
	}

	updated[bestIdx].CurrentWeight -= totalWeight
	return updated[bestIdx], updated, true
}

func isBetterCandidate(a, b domain.Subject) bool {
	if a.CurrentWeight != b.CurrentWeight {
		return a.CurrentWeight > b.CurrentWeight
	}
	// Tie-breaker: prefer never-used, then least-recently-used
	if a.LastUsedAt == nil && b.LastUsedAt != nil {
		return true
	}
	if a.LastUsedAt != nil && b.LastUsedAt == nil {
		return false
	}
	if a.LastUsedAt != nil && b.LastUsedAt != nil && !a.LastUsedAt.Equal(*b.LastUsedAt) {
		return a.LastUsedAt.Before(*b.LastUsedAt)
	}
	return a.UsageCount < b.UsageCount
}
