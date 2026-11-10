package web

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"linkedin-post-manager/internal/config"
	"linkedin-post-manager/internal/domain"
	"linkedin-post-manager/internal/gemini"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestTemplatesParseAndRenderCleanly(t *testing.T) {
	cfg := config.Load()
	gen := gemini.NewGenerator("test-key", cfg.GeminiTextModel, cfg.GeminiImageModel, cfg.GeminiSearchGrounding, cfg.GeminiImageGeneration)

	srv, err := NewServer(context.Background(), cfg, nil, gen, nil)
	if err != nil {
		t.Fatalf("NewServer template parse failed: %v", err)
	}

	now := time.Now()
	samplePost := domain.Post{
		ID:                 bson.NewObjectID(),
		WeekKey:            "2026-11-01",
		SlotIndex:          1,
		SuggestedDay:       "Tuesday",
		SubjectName:        ".NET",
		Status:             domain.StatusDraft,
		SourceTitle:        "Performance Improvements in .NET",
		SourceURL:          "https://devblogs.microsoft.com/dotnet/",
		SourceSummary:      "Deep dive into Span<T> and zero-allocation HTTP routing.",
		Content:            "Most .NET APIs don't have a CPU problem.\n\nThey have a garbage collection problem.\n\nWhen throughput drops under load, teams immediately reach for horizontal scaling.\n\nInstead, three small memory shifts cut our p99 latency in half:\n\n1. Pooling buffers on hot serialization paths\n2. Avoiding closure allocations in middleware\n3. Streaming responses instead of buffering JSON\n\nMeasure allocations before adding nodes.\n\nWhere have you found the biggest hidden allocation wins in C#?\n\n#DotNet #CSharp #BackendEngineering",
		StrategyArea:       ".NET Backend Performance",
		StrategyHook:       "Contrarian GC vs CPU opener",
		StrategyAngle:      "Tactical 3-point breakdown",
		StrategyDiscussion: "Allocation profiling wins",
		AlternativeHooks:   []string{"Hook option A", "Hook option B"},
		ImageFileID:        bson.NewObjectID(),
		ImageStyle:         "Editorial Concept Illustration",
		ImageAspectRatio:   "4:5",
		ImageVersion:       1,
		AltText:            "Editorial illustration of streamlined data lanes bypassing a bottleneck.",
		AlternativeVisuals: []string{"Split screen before/after GC pauses", "Minimalist system sketch"},
	}

	var buf bytes.Buffer
	if err := srv.tmpl.ExecuteTemplate(&buf, "page_board", boardViewData{
		GeminiConfigured:   true,
		ActiveSubjectCount: 3,
		SelectedWeek:       "2026-11-01",
		SelectedWeekLabel:  "Nov 1 – Nov 7, 2026",
		IsCurrentWeek:      true,
		Weeks:              []weekOption{{Key: "2026-11-01", Label: "Nov 1 – Nov 7, 2026", IsCurrent: true}},
		Posts:              []domain.Post{samplePost},
		Ideas:              []domain.Idea{{ID: bson.NewObjectID(), Content: "Write about Angular signals", SubjectName: "Angular"}},
		TargetPerWeek:      3,
	}); err != nil {
		t.Fatalf("render page_board failed: %v", err)
	}
	if !strings.Contains(buf.String(), "LinkedIn \"...more\" fold") {
		t.Errorf("expected fold divider in rendered post card")
	}

	buf.Reset()
	if err := srv.tmpl.ExecuteTemplate(&buf, "page_subjects", subjectsViewData{
		Subjects: []domain.Subject{
			{ID: bson.NewObjectID(), Name: ".NET", Priority: 10, Active: true, LastUsedAt: &now},
			{ID: bson.NewObjectID(), Name: "Angular", Priority: 10, Active: true},
			{ID: bson.NewObjectID(), Name: "Golang", Priority: 4, Active: true},
		},
		TotalActivePriority: 24,
	}); err != nil {
		t.Fatalf("render page_subjects failed: %v", err)
	}
	if !strings.Contains(buf.String(), "42% share") || !strings.Contains(buf.String(), "17% share") {
		t.Errorf("expected weighted share percentages in subjects table, got:\n%s", buf.String())
	}

	buf.Reset()
	if err := srv.tmpl.ExecuteTemplate(&buf, "page_settings", settingsViewData{
		Settings: domain.Settings{
			PostSkillPrompt:    "post prompt",
			ImageSkillPrompt:   "image prompt",
			PreferredDays:      []string{"Tuesday", "Wednesday", "Thursday"},
			DefaultAspectRatio: "4:5",
		},
		GeminiConfigured: true,
		TextModel:        "gemini-3.8-flash",
		ImageModel:       "gemini-nano-banana-2.1",
		Timezone:         "America/Sao_Paulo",
		SundayCron:       "0 8 * * 0",
		PostsPerWeek:     3,
	}); err != nil {
		t.Fatalf("render page_settings failed: %v", err)
	}
}

func TestBoardRendersNonErrorNoticeWhenImageGenerationDisabled(t *testing.T) {
	cfg := config.Load()
	gen := gemini.NewGenerator("test-key", cfg.GeminiTextModel, cfg.GeminiImageModel, false, false)

	srv, err := NewServer(context.Background(), cfg, nil, gen, nil)
	if err != nil {
		t.Fatalf("NewServer template parse failed: %v", err)
	}

	textOnlyPost := domain.Post{
		ID:           bson.NewObjectID(),
		WeekKey:      "2026-11-08",
		SlotIndex:    1,
		SuggestedDay: "Tuesday",
		SubjectName:  "Golang",
		Status:       domain.StatusDraft,
		Content:      "Go 1.24 makes generic type aliases first-class.\n\n#Golang #Backend #SoftwareEngineering",
		ErrorMessage: "Image generation quota exceeded: Rate limit exceeded (limit: 0 requests per day on Free Tier)",
	}

	cleaned := srv.sanitizePostForView(context.Background(), textOnlyPost)
	if cleaned.ErrorMessage != "" {
		t.Fatalf("expected stale image quota error to be cleared when image generation is disabled, got %q", cleaned.ErrorMessage)
	}

	var buf bytes.Buffer
	if err := srv.tmpl.ExecuteTemplate(&buf, "page_board", boardViewData{
		GeminiConfigured:       true,
		ImageGenerationEnabled: false,
		ImageDisabledReason:    gen.ImageDisabledReason(),
		ActiveSubjectCount:     3,
		SelectedWeek:           "2026-11-08",
		SelectedWeekLabel:      "Nov 8 – Nov 14, 2026",
		IsCurrentWeek:          true,
		Weeks:                  []weekOption{{Key: "2026-11-08", Label: "Nov 8 – Nov 14, 2026", IsCurrent: true}},
		Posts:                  []domain.Post{cleaned},
		TargetPerWeek:          3,
	}); err != nil {
		t.Fatalf("render page_board with disabled image generation failed: %v", err)
	}

	rendered := buf.String()
	if !strings.Contains(rendered, "Not having an image is not an error") {
		t.Errorf("expected post card to explain that missing image is not an error when image generation is disabled")
	}
	if strings.Contains(rendered, "No image generated yet") {
		t.Errorf("did not expect 'No image generated yet' placeholder when image generation is disabled")
	}
}

