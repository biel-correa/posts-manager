package scheduler

import (
	"testing"
	"time"

	"linkedin-post-manager/internal/domain"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestCurrentWeekKey_SundayAnchored(t *testing.T) {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}

	// Nov 1, 2026 is a Sunday; Nov 7, 2026 is Saturday.
	cases := []struct {
		name string
		ts   time.Time
		want string
	}{
		{
			name: "Sunday morning",
			ts:   time.Date(2026, 11, 1, 8, 0, 0, 0, loc),
			want: "2026-11-01",
		},
		{
			name: "Wednesday mid-week catch-up",
			ts:   time.Date(2026, 11, 4, 10, 31, 0, 0, loc),
			want: "2026-11-01",
		},
		{
			name: "Saturday late night",
			ts:   time.Date(2026, 11, 7, 23, 59, 59, 0, loc),
			want: "2026-11-01",
		},
		{
			name: "Next Sunday starts new week",
			ts:   time.Date(2026, 11, 8, 0, 0, 1, 0, loc),
			want: "2026-11-08",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CurrentWeekKey(tc.ts, loc)
			if got != tc.want {
				t.Errorf("CurrentWeekKey(%v) = %q, want %q", tc.ts, got, tc.want)
			}
		})
	}
}

func TestSelectNextSubject_WeightedPriorityDistribution(t *testing.T) {
	// User's exact scenario: .NET = 10, Angular = 10, Golang = 4 (total = 24)
	subjects := []domain.Subject{
		{ID: bson.NewObjectID(), Name: ".NET", Priority: 10, Active: true},
		{ID: bson.NewObjectID(), Name: "Angular", Priority: 10, Active: true},
		{ID: bson.NewObjectID(), Name: "Golang", Priority: 4, Active: true},
	}

	counts := map[string]int{}
	state := subjects

	// Simulate 8 Sundays * 3 posts/week = 24 posts
	for i := 0; i < 24; i++ {
		chosen, nextState, ok := SelectNextSubject(state)
		if !ok {
			t.Fatalf("expected subject on iteration %d", i)
		}
		counts[chosen.Name]++
		now := time.Now().Add(time.Duration(i) * time.Minute)
		for j := range nextState {
			if nextState[j].ID == chosen.ID {
				nextState[j].UsageCount++
				nextState[j].LastUsedAt = &now
			}
		}
		state = nextState
	}

	if counts[".NET"] != 10 || counts["Angular"] != 10 || counts["Golang"] != 4 {
		t.Fatalf("unexpected priority distribution over 24 posts: got %+v, want .NET=10 Angular=10 Golang=4", counts)
	}
}
