package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"linkedin-post-manager/internal/config"
	"linkedin-post-manager/internal/domain"
	"linkedin-post-manager/internal/gemini"
	"linkedin-post-manager/internal/notify"
	"linkedin-post-manager/internal/repository"

	"github.com/robfig/cron/v3"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type Service struct {
	cfg   *config.Config
	store *repository.Store
	gen   *gemini.Generator
	cron  *cron.Cron

	mu        sync.Mutex
	imageMu   sync.Mutex
	isRunning bool
	lastCheck time.Time
	lastError string
}

func NewService(cfg *config.Config, store *repository.Store, gen *gemini.Generator) *Service {
	c := cron.New(cron.WithLocation(cfg.Location))
	return &Service{
		cfg:   cfg,
		store: store,
		gen:   gen,
		cron:  c,
	}
}

// Start launches:
//  1. Immediate background startup check for missing posts in the CURRENT week only (max 1-week catch-up).
//  2. Configured Sunday cron job (e.g. "0 8 * * 0" in local TZ).
//  3. 30-minute lightweight reconciliation ticker (catches laptop sleep/wake across Sunday).
func (s *Service) Start(ctx context.Context) error {
	_, err := s.cron.AddFunc(s.cfg.SundayCron, func() {
		slog.Info("sunday cron triggered")
		if _, err := s.EnsureCurrentWeekPosts(ctx, "sunday-cron"); err != nil {
			slog.Error("sunday cron generation failed", "error", err)
		}
	})
	if err != nil {
		return fmt.Errorf("register sunday cron (%s): %w", s.cfg.SundayCron, err)
	}
	s.cron.Start()

	// Startup catch-up check (current week only)
	go func() {
		if _, err := s.EnsureCurrentWeekPosts(ctx, "startup-reconciler"); err != nil {
			slog.Warn("startup week reconciliation skipped or failed", "error", err)
		}
		if err := s.RetryMissingImages(ctx); err != nil {
			slog.Debug("startup missing image check completed", "error", err)
		}
	}()

	// Periodic image recovery ticker: checks for posts in the current week missing images
	// and automatically generates them once temporary service spikes or high demand subside.
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.RetryMissingImages(ctx); err != nil {
					slog.Debug("periodic missing image check completed", "error", err)
				}
			}
		}
	}()

	// Periodic wake check in case Docker host slept over Sunday cron time
	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				s.cron.Stop()
				return
			case <-ticker.C:
				if _, err := s.EnsureCurrentWeekPosts(ctx, "periodic-check"); err != nil {
					slog.Warn("periodic reconciliation failed", "error", err)
				}
			}
		}
	}()

	return nil
}

func (s *Service) Status() (running bool, lastCheck time.Time, lastError string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isRunning, s.lastCheck, s.lastError
}

// TriggerEnsureCurrentWeekAsync validates prerequisites synchronously, marks the service as running,
// and launches EnsureCurrentWeekPosts in the background so the HTMX board can poll progress live.
func (s *Service) TriggerEnsureCurrentWeekAsync(bgCtx context.Context) error {
	if !s.gen.Configured() {
		return fmt.Errorf("GEMINI_API_KEY is not configured. Set GEMINI_API_KEY in your .env file and restart.")
	}
	ideas, _ := s.store.ListPendingIdeas(bgCtx)
	subjects, err := s.store.ListActiveSubjects(bgCtx)
	if err != nil {
		return err
	}
	if len(ideas) == 0 && len(subjects) == 0 {
		return fmt.Errorf("add at least one active Subject (or queue a spark in the Idea Inbox) before generating posts")
	}

	s.setRunning(true, nil)
	go func() {
		_, err := s.EnsureCurrentWeekPosts(bgCtx, "manual-ui")
		if err != nil {
			slog.Error("manual EnsureCurrentWeekPosts failed", "error", err)
			s.setRunning(false, err)
		} else {
			s.setRunning(false, nil)
		}
	}()
	return nil
}

// TriggerGenerateOneAsync validates prerequisites and generates 1 additional post slot asynchronously.
func (s *Service) TriggerGenerateOneAsync(bgCtx context.Context, weekKey string, nextSlot int) error {
	if !s.gen.Configured() {
		return fmt.Errorf("GEMINI_API_KEY is not configured. Set GEMINI_API_KEY in your .env file and restart.")
	}
	ideas, _ := s.store.ListPendingIdeas(bgCtx)
	subjects, err := s.store.ListActiveSubjects(bgCtx)
	if err != nil {
		return err
	}
	if len(ideas) == 0 && len(subjects) == 0 {
		return fmt.Errorf("add at least one active Subject (or queue a spark in the Idea Inbox) before generating posts")
	}

	s.setRunning(true, nil)
	go func() {
		_, err := s.GenerateSingleSlot(bgCtx, weekKey, nextSlot)
		if err != nil {
			slog.Error("manual GenerateSingleSlot failed", "error", err)
			s.setRunning(false, err)
		} else {
			s.setRunning(false, nil)
		}
	}()
	return nil
}

func (s *Service) setRunning(running bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.isRunning = running
	s.lastCheck = time.Now().In(s.cfg.Location)
	if err != nil {
		s.lastError = err.Error()
	} else if !running {
		s.lastError = ""
	}
}

// EnsureCurrentWeekPosts checks ONLY the current Sunday-anchored week (never past weeks,
// enforcing at most 1 week of compensation) and generates any missing posts up to PostsPerWeek.
func (s *Service) EnsureCurrentWeekPosts(ctx context.Context, trigger string) (int, error) {
	weekKey := CurrentWeekKey(time.Now(), s.cfg.Location)

	// Clean up any stale placeholder posts stuck in 'generating' state (>2 minutes old)
	if cleaned, cErr := s.store.CleanupStuckGeneratingPosts(ctx, 2*time.Minute); cErr == nil && cleaned > 0 {
		slog.Info("cleaned up stale generating posts", "count", cleaned)
	}

	existing, err := s.store.CountWeekPosts(ctx, weekKey)
	if err != nil {
		return 0, err
	}

	missing := s.cfg.PostsPerWeek - int(existing)
	if missing <= 0 {
		s.mu.Lock()
		s.lastCheck = time.Now().In(s.cfg.Location)
		s.mu.Unlock()
		return 0, nil
	}

	if !s.gen.Configured() {
		err := fmt.Errorf("GEMINI_API_KEY not configured (%d posts missing for week %s)", missing, weekKey)
		s.setRunning(false, err)
		return 0, err
	}

	owner := fmt.Sprintf("%s-%d", trigger, time.Now().UnixNano())
	acquired, err := s.store.TryAcquireWeekLock(ctx, weekKey, owner, 15*time.Minute)
	if err != nil {
		return 0, err
	}
	if !acquired {
		slog.Info("week generation already in progress", "week_key", weekKey)
		return 0, nil
	}
	defer s.store.ReleaseWeekLock(ctx, weekKey, owner)

	// Re-count after acquiring lock
	existing, err = s.store.CountWeekPosts(ctx, weekKey)
	if err != nil {
		return 0, err
	}
	missing = s.cfg.PostsPerWeek - int(existing)
	if missing <= 0 {
		return 0, nil
	}

	// Check we have either pending ideas or active subjects before starting
	ideas, _ := s.store.ListPendingIdeas(ctx)
	subjects, err := s.store.ListActiveSubjects(ctx)
	if err != nil {
		return 0, err
	}
	if len(ideas) == 0 && len(subjects) == 0 {
		slog.Info("no active subjects or queued ideas yet; waiting for user setup", "week_key", weekKey)
		return 0, nil
	}

	s.setRunning(true, nil)
	var lastErr error
	generated := 0

	currentPosts, _ := s.store.ListPostsByWeek(ctx, weekKey)
	usedSlots := make(map[int]bool)
	for _, p := range currentPosts {
		if p.Status != domain.StatusArchived && p.Status != domain.StatusFailed {
			usedSlots[p.SlotIndex] = true
		}
	}

	for i := 0; i < missing; i++ {
		slotIndex := int(existing) + i + 1
		for slotNum := 1; slotNum <= s.cfg.PostsPerWeek; slotNum++ {
			if !usedSlots[slotNum] {
				slotIndex = slotNum
				break
			}
		}
		usedSlots[slotIndex] = true
		if i > 0 {
			// Throttle between consecutive post slots (4 seconds) so we stay well below RPM caps
			slog.Info("throttling between post generation slots", "slot", slotIndex, "pause", "4s")
			select {
			case <-ctx.Done():
				return generated, ctx.Err()
			case <-time.After(4 * time.Second):
			}
		}
		if _, err := s.GenerateSingleSlot(ctx, weekKey, slotIndex); err != nil {
			slog.Error("failed generating weekly post slot", "week_key", weekKey, "slot", slotIndex, "error", err)
			lastErr = err
			continue
		}
		generated++
	}

	s.setRunning(false, lastErr)

	if generated > 0 {
		settings, err := s.store.GetSettings(ctx)
		if err == nil && strings.TrimSpace(settings.WebhookURL) != "" {
			weekLabel := FormatWeekRange(weekKey, s.cfg.Location)
			if wErr := notify.SendReadyWebhook(ctx, settings.WebhookURL, weekLabel, generated); wErr != nil {
				slog.Warn("webhook notification failed", "error", wErr)
			}
		}
	}

	return generated, lastErr
}

// GenerateSingleSlot creates 1 full post (copy + image) for weekKey.
// Queued items in the Quick Idea Inbox take priority; otherwise Smooth Weighted Round-Robin
// picks an active Subject proportional to its 1-10 Priority level.
func (s *Service) GenerateSingleSlot(ctx context.Context, weekKey string, slotIndex int) (domain.Post, error) {
	settings, err := s.store.GetSettings(ctx)
	if err != nil {
		return domain.Post{}, err
	}

	var chosenSubject domain.Subject
	var chosenIdea *domain.Idea

	ideas, _ := s.store.ListPendingIdeas(ctx)
	if len(ideas) > 0 {
		idea := ideas[0]
		chosenIdea = &idea
		chosenSubject.Name = strings.TrimSpace(idea.SubjectName)
		if chosenSubject.Name == "" {
			chosenSubject.Name = "Software Engineering"
		}
	} else {
		subjects, err := s.store.ListActiveSubjects(ctx)
		if err != nil {
			return domain.Post{}, err
		}
		picked, updatedWeights, ok := SelectNextSubject(subjects)
		if !ok {
			return domain.Post{}, fmt.Errorf("add at least one active subject (or drop a note in the Idea Inbox) before generating posts")
		}
		chosenSubject = picked
		if err := s.store.SaveSubjectWeightsAndUsage(ctx, updatedWeights, picked.ID); err != nil {
			return domain.Post{}, err
		}
	}

	placeholder := domain.Post{
		ID:               bson.NewObjectID(),
		WeekKey:          weekKey,
		SlotIndex:        slotIndex,
		SuggestedDay:     SuggestedDayForSlot(slotIndex, settings.PreferredDays),
		SubjectName:      chosenSubject.Name,
		Status:           domain.StatusGenerating,
		ImageAspectRatio: settings.DefaultAspectRatio,
	}
	if !chosenSubject.ID.IsZero() {
		placeholder.SubjectID = chosenSubject.ID.Hex()
	}
	if chosenIdea != nil {
		placeholder.IdeaID = chosenIdea.ID.Hex()
	}

	post, err := s.store.InsertPost(ctx, placeholder)
	if err != nil {
		return domain.Post{}, err
	}

	recentPosts, _ := s.store.ListRecentPostsForSubject(ctx, chosenSubject.Name, 8)
	goldenPosts, _ := s.store.ListGoldenPosts(ctx, 3)

	var ideaSeed string
	if chosenIdea != nil {
		ideaSeed = chosenIdea.Content
	}

	draft, err := s.gen.GeneratePostDraft(ctx, gemini.PostDraftRequest{
		SubjectName:        chosenSubject.Name,
		SubjectDescription: chosenSubject.Description,
		IdeaSeed:           ideaSeed,
		RecentPosts:        recentPosts,
		GoldenPosts:        goldenPosts,
		SkillSystemPrompt:  settings.PostSkillPrompt,
		AuthorPersona:      settings.AuthorPersona,
		Now:                time.Now().In(s.cfg.Location),
	})
	if err != nil {
		post.Status = domain.StatusFailed
		post.ErrorMessage = err.Error()
		saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.store.SavePost(saveCtx, post)
		cancel()
		return post, err
	}

	post.SourceTitle = draft.SourceTitle
	post.SourceURL = draft.SourceURL
	post.SourceSummary = draft.SourceSummary
	post.Content = draft.PostContent
	post.StrategyArea = draft.StrategyArea
	post.StrategyHook = draft.StrategyHook
	post.StrategyAngle = draft.StrategyAngle
	post.StrategyDiscussion = draft.StrategyDiscussion
	post.AlternativeHooks = draft.AlternativeHooks
	post.Status = domain.StatusDraft
	post.ErrorMessage = ""

	if err := s.store.SavePost(ctx, post); err != nil {
		return post, err
	}

	if chosenIdea != nil {
		_ = s.store.MarkIdeaUsed(ctx, chosenIdea.ID, post.ID.Hex())
	}

	// Trigger image generation asynchronously in background so text draft creation never blocks next post slots
	if s.gen.ImageGenerationEnabled() {
		s.attachImageInBackground(post.ID, settings)
	}

	return post, nil
}

func (s *Service) attachImageInBackground(postID bson.ObjectID, settings domain.Settings) {
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
		defer cancel()

		post, err := s.store.GetPost(bgCtx, postID)
		if err != nil {
			return
		}

		aspectRatio := post.ImageAspectRatio
		if strings.TrimSpace(aspectRatio) == "" {
			aspectRatio = settings.DefaultAspectRatio
		}

		imgRes, imgErr := s.gen.GeneratePostImage(bgCtx, gemini.PostImageRequest{
			PostContent:      post.Content,
			StrategyHook:     post.StrategyHook,
			StrategyAngle:    post.StrategyAngle,
			AspectRatio:      aspectRatio,
			ImageSkillPrompt: settings.ImageSkillPrompt,
		})
		if imgErr != nil {
			slog.Warn("background image generation failed", "post_id", post.ID.Hex(), "error", imgErr)
			if gemini.IsPlanQuotaError(imgErr) || !s.gen.ImageGenerationEnabled() {
				if gemini.IsImageDisabledOrQuotaMessage(post.ErrorMessage) {
					post.ErrorMessage = ""
					_ = s.store.SavePost(bgCtx, post)
				}
				return
			}
			if gemini.IsHighDemandError(imgErr) {
				post.ErrorMessage = "Image generation deferred: Gemini is experiencing temporary high demand. Retrying in background..."
			} else {
				post.ErrorMessage = "Image generation warning: " + imgErr.Error()
			}
			_ = s.store.SavePost(bgCtx, post)
			return
		}

		fileID, upErr := s.store.UploadImage(bgCtx, fmt.Sprintf("post-%s.png", post.ID.Hex()), imgRes.ImageBytes, post.ImageFileID)
		if upErr != nil {
			slog.Error("failed uploading background generated image", "post_id", post.ID.Hex(), "error", upErr)
			return
		}

		post.ImageFileID = fileID
		post.ImageMimeType = imgRes.MimeType
		post.ImagePrompt = imgRes.ImagePrompt
		post.ImageStyle = imgRes.VisualStyle
		post.ImageAspectRatio = imgRes.AspectRatio
		post.AltText = imgRes.AltText
		post.AlternativeVisuals = imgRes.AlternativeVisuals
		post.ImageVersion = 1
		post.ErrorMessage = ""
		_ = s.store.SavePost(bgCtx, post)
		slog.Info("successfully generated and attached image in background", "post_id", post.ID.Hex(), "subject", post.SubjectName)
	}()
}

func (s *Service) RegeneratePostText(ctx context.Context, postID bson.ObjectID, steering string) (domain.Post, error) {
	post, err := s.store.GetPost(ctx, postID)
	if err != nil {
		return domain.Post{}, err
	}
	settings, err := s.store.GetSettings(ctx)
	if err != nil {
		return post, err
	}

	var subjectDesc string
	if post.SubjectID != "" {
		if subOID, err := bson.ObjectIDFromHex(post.SubjectID); err == nil {
			if sub, err := s.store.GetSubject(ctx, subOID); err == nil {
				subjectDesc = sub.Description
			}
		}
	}

	recentPosts, _ := s.store.ListRecentPostsForSubject(ctx, post.SubjectName, 8)
	goldenPosts, _ := s.store.ListGoldenPosts(ctx, 3)

	draft, err := s.gen.GeneratePostDraft(ctx, gemini.PostDraftRequest{
		SubjectName:         post.SubjectName,
		SubjectDescription:  subjectDesc,
		SteeringInstruction: steering,
		PreviousDraft:       post.Content,
		RecentPosts:         recentPosts,
		GoldenPosts:         goldenPosts,
		SkillSystemPrompt:   settings.PostSkillPrompt,
		AuthorPersona:       settings.AuthorPersona,
		Now:                 time.Now().In(s.cfg.Location),
	})
	if err != nil {
		return post, err
	}

	post.SourceTitle = draft.SourceTitle
	post.SourceURL = draft.SourceURL
	post.SourceSummary = draft.SourceSummary
	post.Content = draft.PostContent
	post.StrategyArea = draft.StrategyArea
	post.StrategyHook = draft.StrategyHook
	post.StrategyAngle = draft.StrategyAngle
	post.StrategyDiscussion = draft.StrategyDiscussion
	post.AlternativeHooks = draft.AlternativeHooks
	post.ErrorMessage = ""
	if post.Status == domain.StatusFailed || post.Status == domain.StatusGenerating {
		post.Status = domain.StatusDraft
	}

	err = s.store.SavePost(ctx, post)
	return post, err
}

func (s *Service) RegeneratePostImage(ctx context.Context, postID bson.ObjectID, steering, aspectRatio string) (domain.Post, error) {
	post, err := s.store.GetPost(ctx, postID)
	if err != nil {
		return domain.Post{}, err
	}
	settings, err := s.store.GetSettings(ctx)
	if err != nil {
		return post, err
	}

	if strings.TrimSpace(aspectRatio) == "" {
		aspectRatio = post.ImageAspectRatio
	}
	if strings.TrimSpace(aspectRatio) == "" {
		aspectRatio = settings.DefaultAspectRatio
	}

	if !s.gen.ImageGenerationEnabled() {
		if gemini.IsImageDisabledOrQuotaMessage(post.ErrorMessage) {
			post.ErrorMessage = ""
			_ = s.store.SavePost(ctx, post)
		}
		return post, nil
	}

	imgRes, err := s.gen.GeneratePostImage(ctx, gemini.PostImageRequest{
		PostContent:         post.Content,
		StrategyHook:        post.StrategyHook,
		StrategyAngle:       post.StrategyAngle,
		AspectRatio:         aspectRatio,
		SteeringInstruction: steering,
		ImageSkillPrompt:    settings.ImageSkillPrompt,
	})
	if err != nil {
		if gemini.IsPlanQuotaError(err) || !s.gen.ImageGenerationEnabled() {
			if gemini.IsImageDisabledOrQuotaMessage(post.ErrorMessage) {
				post.ErrorMessage = ""
				_ = s.store.SavePost(ctx, post)
			}
			return post, nil
		}
		return post, err
	}

	fileID, err := s.store.UploadImage(ctx, fmt.Sprintf("post-%s-v%d.png", post.ID.Hex(), post.ImageVersion+1), imgRes.ImageBytes, post.ImageFileID)
	if err != nil {
		return post, err
	}

	post.ImageFileID = fileID
	post.ImageMimeType = imgRes.MimeType
	post.ImagePrompt = imgRes.ImagePrompt
	post.ImageStyle = imgRes.VisualStyle
	post.ImageAspectRatio = imgRes.AspectRatio
	post.AltText = imgRes.AltText
	post.AlternativeVisuals = imgRes.AlternativeVisuals
	post.ImageVersion++
	post.ErrorMessage = ""

	err = s.store.SavePost(ctx, post)
	return post, err
}

func (s *Service) clearStaleImageDisabledErrors(ctx context.Context, weekKey string) {
	posts, err := s.store.ListPostsMissingImages(ctx, weekKey)
	if err != nil {
		return
	}
	for _, post := range posts {
		if gemini.IsImageDisabledOrQuotaMessage(post.ErrorMessage) {
			post.ErrorMessage = ""
			_ = s.store.SavePost(ctx, post)
		}
	}
}

func (s *Service) RetryMissingImages(ctx context.Context) error {
	weekKey := CurrentWeekKey(time.Now(), s.cfg.Location)
	if !s.gen.ImageGenerationEnabled() {
		s.clearStaleImageDisabledErrors(ctx, weekKey)
		return nil
	}
	if !s.gen.Configured() {
		return nil
	}

	s.mu.Lock()
	running := s.isRunning
	s.mu.Unlock()
	if running {
		slog.Debug("skipping background image recovery while post generation is running")
		return nil
	}

	if !s.imageMu.TryLock() {
		return nil
	}
	defer s.imageMu.Unlock()

	posts, err := s.store.ListPostsMissingImages(ctx, weekKey)
	if err != nil {
		return err
	}
	if len(posts) == 0 {
		return nil
	}

	settings, err := s.store.GetSettings(ctx)
	if err != nil {
		return err
	}

	for _, post := range posts {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		s.mu.Lock()
		if s.isRunning {
			s.mu.Unlock()
			slog.Debug("stopping background image recovery because post generation is running")
			return nil
		}
		s.mu.Unlock()

		if gemini.IsPlanQuotaError(errors.New(post.ErrorMessage)) ||
			strings.Contains(strings.ToLower(post.ErrorMessage), "quota exceeded") ||
			strings.Contains(strings.ToLower(post.ErrorMessage), "free tier") ||
			strings.Contains(strings.ToLower(post.ErrorMessage), "limit: 0") ||
			strings.Contains(strings.ToLower(post.ErrorMessage), "billing") {
			continue
		}

		slog.Info("attempting deferred image generation for post", "post_id", post.ID.Hex(), "subject", post.SubjectName)

		aspectRatio := post.ImageAspectRatio
		if strings.TrimSpace(aspectRatio) == "" {
			aspectRatio = settings.DefaultAspectRatio
		}

		postCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		imgRes, err := s.gen.GeneratePostImage(postCtx, gemini.PostImageRequest{
			PostContent:      post.Content,
			StrategyHook:     post.StrategyHook,
			StrategyAngle:    post.StrategyAngle,
			AspectRatio:      aspectRatio,
			ImageSkillPrompt: settings.ImageSkillPrompt,
		})
		cancel()
		if err != nil {
			slog.Warn("deferred image generation failed, will retry on next check", "post_id", post.ID.Hex(), "error", err)
			if gemini.IsPlanQuotaError(err) || !s.gen.ImageGenerationEnabled() {
				s.clearStaleImageDisabledErrors(ctx, weekKey)
				return nil
			}
			if gemini.IsHighDemandError(err) {
				post.ErrorMessage = "Image generation deferred: Gemini is experiencing temporary high demand. Retrying in background..."
				_ = s.store.SavePost(ctx, post)
				return nil
			}
			post.ErrorMessage = "Image generation warning: " + err.Error()
			_ = s.store.SavePost(ctx, post)
			continue
		}

		fileID, upErr := s.store.UploadImage(ctx, fmt.Sprintf("post-%s.png", post.ID.Hex()), imgRes.ImageBytes, post.ImageFileID)
		if upErr != nil {
			slog.Error("failed uploading retried post image", "post_id", post.ID.Hex(), "error", upErr)
			continue
		}

		post.ImageFileID = fileID
		post.ImageMimeType = imgRes.MimeType
		post.ImagePrompt = imgRes.ImagePrompt
		post.ImageStyle = imgRes.VisualStyle
		post.ImageAspectRatio = imgRes.AspectRatio
		post.AltText = imgRes.AltText
		post.AlternativeVisuals = imgRes.AlternativeVisuals
		post.ImageVersion = 1
		post.ErrorMessage = ""

		if err := s.store.SavePost(ctx, post); err != nil {
			slog.Error("failed saving post after deferred image generation", "post_id", post.ID.Hex(), "error", err)
		} else {
			slog.Info("successfully generated and attached deferred image", "post_id", post.ID.Hex(), "subject", post.SubjectName)
		}

		time.Sleep(3 * time.Second)
	}

	return nil
}
