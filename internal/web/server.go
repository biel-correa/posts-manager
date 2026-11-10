package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"linkedin-post-manager/internal/config"
	"linkedin-post-manager/internal/domain"
	"linkedin-post-manager/internal/gemini"
	"linkedin-post-manager/internal/notify"
	"linkedin-post-manager/internal/repository"
	"linkedin-post-manager/internal/scheduler"
	"linkedin-post-manager/prompts"
	"linkedin-post-manager/static"

	"go.mongodb.org/mongo-driver/v2/bson"
)

//go:embed templates/*.html
var templatesFS embed.FS

type Server struct {
	appCtx context.Context
	cfg    *config.Config
	store  *repository.Store
	gen    *gemini.Generator
	sched  *scheduler.Service
	tmpl   *template.Template
}

type foldResult struct {
	Before  string
	After   string
	HasFold bool
}

type subjectRowData struct {
	Subject             domain.Subject
	TotalActivePriority int
}

type weekOption struct {
	Key       string
	Label     string
	IsCurrent bool
}

type boardViewData struct {
	GeminiConfigured       bool
	ImageGenerationEnabled bool
	ImageAutoDisabled      bool
	ImageDisabledReason    string
	SearchGroundingEnabled bool
	ActiveSubjectCount     int
	SelectedWeek           string
	SelectedWeekLabel      string
	IsCurrentWeek          bool
	Weeks                  []weekOption
	Posts                  []domain.Post
	Ideas                  []domain.Idea
	ScheduledCount         int
	TargetPerWeek          int
	IsGenerating           bool
	HasPendingImages       bool
	FlashError             string
}

type subjectsViewData struct {
	Subjects            []domain.Subject
	TotalActivePriority int
}

type settingsViewData struct {
	Settings            domain.Settings
	GeminiConfigured    bool
	TextModel           string
	ImageModel          string
	SearchGrounding     bool
	ImageGeneration     bool
	ImageAutoDisabled   bool
	ImageDisabledReason string
	Timezone            string
	SundayCron          string
	PostsPerWeek        int
	SavedMessage        string
	ErrorMessage        string
}

func NewServer(appCtx context.Context, cfg *config.Config, store *repository.Store, gen *gemini.Generator, sched *scheduler.Service) (*Server, error) {
	s := &Server{
		appCtx: appCtx,
		cfg:    cfg,
		store:  store,
		gen:    gen,
		sched:  sched,
	}

	funcs := template.FuncMap{
		"hex":      func(id bson.ObjectID) string { return id.Hex() },
		"contains": strings.Contains,
		"add1": func(i int) int {
			return i + 1
		},
		"sweetSpot": func(words int) bool {
			return words >= 120 && words <= 220
		},
		"fold": func(p domain.Post) foldResult {
			b, a, has := p.FoldPreview()
			return foldResult{Before: b, After: a, HasFold: has}
		},
		"barPct": func(priority int) int {
			if priority < 1 {
				return 10
			}
			if priority > 10 {
				return 100
			}
			return priority * 10
		},
		"pct": func(priority, total int) string {
			if total <= 0 {
				return "0%"
			}
			share := int(math.Round(float64(priority) / float64(total) * 100.0))
			return fmt.Sprintf("%d%% share", share)
		},
		"subjectRowCtx": func(sub domain.Subject, total int) subjectRowData {
			return subjectRowData{Subject: sub, TotalActivePriority: total}
		},
		"formatTime": func(t *time.Time) string {
			if t == nil || t.IsZero() {
				return "Never"
			}
			return t.In(cfg.Location).Format("Jan 2, 15:04")
		},
		"joinComma": func(items []string) string {
			return strings.Join(items, ", ")
		},
		"imageGenEnabled": func() bool {
			return s.gen.ImageGenerationEnabled()
		},
		"imageAutoDisabled": func() bool {
			return s.gen.ImageAutoDisabled()
		},
		"imageDisabledReason": func() string {
			return s.gen.ImageDisabledReason()
		},
		"searchGroundingEnabled": func() bool {
			return s.gen.SearchGrounding()
		},
	}

	tmpl, err := template.New("root").Funcs(funcs).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	s.tmpl = tmpl
	return s, nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static.Assets)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// Board & Weekly Generation
	mux.HandleFunc("GET /{$}", s.handleBoardPage)
	mux.HandleFunc("GET /partials/board", s.handleBoardPartial)
	mux.HandleFunc("POST /weeks/generate-missing", s.handleGenerateMissing)
	mux.HandleFunc("POST /weeks/generate-one", s.handleGenerateOne)

	// Quick Idea Inbox
	mux.HandleFunc("POST /ideas", s.handleCreateIdea)
	mux.HandleFunc("DELETE /ideas/{id}", s.handleDeleteIdea)

	// Posts & GridFS Images
	mux.HandleFunc("GET /posts/{id}/image", s.handlePostImage)
	mux.HandleFunc("GET /posts/{id}/card", s.handlePostCard)
	mux.HandleFunc("GET /posts/{id}/edit", s.handlePostEdit)
	mux.HandleFunc("POST /posts/{id}/content", s.handleUpdatePostContent)
	mux.HandleFunc("POST /posts/{id}/status", s.handleUpdatePostStatus)
	mux.HandleFunc("POST /posts/{id}/toggle-golden", s.handleToggleGolden)
	mux.HandleFunc("POST /posts/{id}/regenerate-text", s.handleRegenerateText)
	mux.HandleFunc("POST /posts/{id}/regenerate-image", s.handleRegenerateImage)
	mux.HandleFunc("DELETE /posts/{id}", s.handleDeletePost)

	// Subjects & Priority Weights
	mux.HandleFunc("GET /subjects", s.handleSubjectsPage)
	mux.HandleFunc("GET /subjects/table", s.handleSubjectsTable)
	mux.HandleFunc("POST /subjects", s.handleCreateSubject)
	mux.HandleFunc("GET /subjects/{id}/edit", s.handleSubjectEditRow)
	mux.HandleFunc("POST /subjects/{id}", s.handleUpdateSubject)
	mux.HandleFunc("POST /subjects/{id}/toggle", s.handleToggleSubject)
	mux.HandleFunc("DELETE /subjects/{id}", s.handleDeleteSubject)

	// Prompts & Settings
	mux.HandleFunc("GET /settings", s.handleSettingsPage)
	mux.HandleFunc("POST /settings", s.handleSaveSettings)
	mux.HandleFunc("POST /settings/test-webhook", s.handleTestWebhook)
	mux.HandleFunc("POST /settings/reset-prompts", s.handleResetPrompts)

	return s.loggingMiddleware(mux)
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if !strings.HasPrefix(r.URL.Path, "/static/") && r.URL.Path != "/healthz" {
			slog.Info("http request", "method", r.Method, "path", r.URL.Path, "duration_ms", time.Since(start).Milliseconds())
		}
	})
}

func (s *Server) render(w http.ResponseWriter, block string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, block, data); err != nil {
		slog.Error("template render failed", "block", block, "error", err)
		http.Error(w, "Template rendering error", http.StatusInternalServerError)
	}
}

// --- Board Handlers ---

func (s *Server) sanitizePostForView(ctx context.Context, post domain.Post) domain.Post {
	if !s.gen.ImageGenerationEnabled() && !post.HasImage() && gemini.IsImageDisabledOrQuotaMessage(post.ErrorMessage) {
		post.ErrorMessage = ""
		if s.store != nil {
			_ = s.store.SavePost(ctx, post)
		}
	}
	return post
}

func (s *Server) buildBoardData(ctx context.Context, requestedWeek, flashErr string) (boardViewData, error) {
	currentWeek := scheduler.CurrentWeekKey(time.Now(), s.cfg.Location)
	selectedWeek := strings.TrimSpace(requestedWeek)
	if selectedWeek == "" {
		selectedWeek = currentWeek
	}

	weeksRaw, _ := s.store.ListAvailableWeeks(ctx, currentWeek)
	weeks := make([]weekOption, 0, len(weeksRaw))
	for _, wk := range weeksRaw {
		weeks = append(weeks, weekOption{
			Key:       wk,
			Label:     scheduler.FormatWeekRange(wk, s.cfg.Location),
			IsCurrent: wk == currentWeek,
		})
	}

	posts, err := s.store.ListPostsByWeek(ctx, selectedWeek)
	if err != nil {
		return boardViewData{}, err
	}

	activeSubjects, _ := s.store.ListActiveSubjects(ctx)
	ideas, _ := s.store.ListPendingIdeas(ctx)

	scheduledCount := 0
	anyPostGenerating := false
	hasPendingImages := false
	for i, p := range posts {
		p = s.sanitizePostForView(ctx, p)
		posts[i] = p
		if p.Status == domain.StatusScheduled {
			scheduledCount++
		}
		if p.Status == domain.StatusGenerating {
			anyPostGenerating = true
		}
		if s.gen.ImageGenerationEnabled() && !p.HasImage() && (p.Status == domain.StatusDraft || p.Status == domain.StatusScheduled) && strings.Contains(p.ErrorMessage, "high demand") {
			hasPendingImages = true
		}
	}

	schedRunning, _, lastSchedErr := s.sched.Status()
	if flashErr == "" && lastSchedErr != "" && !schedRunning && len(posts) == 0 {
		flashErr = lastSchedErr
	}

	return boardViewData{
		GeminiConfigured:       s.gen.Configured(),
		ImageGenerationEnabled: s.gen.ImageGenerationEnabled(),
		ImageAutoDisabled:      s.gen.ImageAutoDisabled(),
		ImageDisabledReason:    s.gen.ImageDisabledReason(),
		SearchGroundingEnabled: s.gen.SearchGrounding(),
		ActiveSubjectCount:     len(activeSubjects),
		SelectedWeek:           selectedWeek,
		SelectedWeekLabel:      scheduler.FormatWeekRange(selectedWeek, s.cfg.Location),
		IsCurrentWeek:          selectedWeek == currentWeek,
		Weeks:                  weeks,
		Posts:                  posts,
		Ideas:                  ideas,
		ScheduledCount:         scheduledCount,
		TargetPerWeek:          s.cfg.PostsPerWeek,
		IsGenerating:           schedRunning || anyPostGenerating,
		HasPendingImages:       hasPendingImages,
		FlashError:             flashErr,
	}, nil
}

func (s *Server) handleBoardPage(w http.ResponseWriter, r *http.Request) {
	data, err := s.buildBoardData(r.Context(), r.URL.Query().Get("week"), "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "page_board", data)
}

func (s *Server) handleBoardPartial(w http.ResponseWriter, r *http.Request) {
	data, err := s.buildBoardData(r.Context(), r.URL.Query().Get("week"), "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "board_inner", data)
}

func (s *Server) handleGenerateMissing(w http.ResponseWriter, r *http.Request) {
	weekKey := r.URL.Query().Get("week")
	var flashErr string
	if err := s.sched.TriggerEnsureCurrentWeekAsync(s.appCtx); err != nil {
		flashErr = err.Error()
	}
	data, err := s.buildBoardData(r.Context(), weekKey, flashErr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "board_inner", data)
}

func (s *Server) handleGenerateOne(w http.ResponseWriter, r *http.Request) {
	weekKey := strings.TrimSpace(r.URL.Query().Get("week"))
	if weekKey == "" {
		weekKey = scheduler.CurrentWeekKey(time.Now(), s.cfg.Location)
	}
	posts, _ := s.store.ListPostsByWeek(r.Context(), weekKey)
	nextSlot := len(posts) + 1

	var flashErr string
	if err := s.sched.TriggerGenerateOneAsync(s.appCtx, weekKey, nextSlot); err != nil {
		flashErr = err.Error()
	}
	data, err := s.buildBoardData(r.Context(), weekKey, flashErr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "board_inner", data)
}

// --- Idea Inbox Handlers ---

func (s *Server) handleCreateIdea(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err == nil {
		content := strings.TrimSpace(r.FormValue("content"))
		subjectName := strings.TrimSpace(r.FormValue("subject_name"))
		if content != "" {
			_, _ = s.store.CreateIdea(r.Context(), content, subjectName)
		}
	}
	ideas, _ := s.store.ListPendingIdeas(r.Context())
	s.render(w, "ideas_panel", boardViewData{Ideas: ideas})
}

func (s *Server) handleDeleteIdea(w http.ResponseWriter, r *http.Request) {
	if oid, err := bson.ObjectIDFromHex(r.PathValue("id")); err == nil {
		_ = s.store.DeleteIdea(r.Context(), oid)
	}
	ideas, _ := s.store.ListPendingIdeas(r.Context())
	s.render(w, "ideas_panel", boardViewData{Ideas: ideas})
}

// --- Post & Image Handlers ---

func (s *Server) handlePostImage(w http.ResponseWriter, r *http.Request) {
	oid, err := bson.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	post, err := s.store.GetPost(r.Context(), oid)
	if err != nil || post.ImageFileID.IsZero() {
		http.NotFound(w, r)
		return
	}

	mimeType := post.ImageMimeType
	if mimeType == "" {
		mimeType = "image/png"
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if r.URL.Query().Get("download") == "1" {
		ext := ".png"
		if strings.Contains(mimeType, "jpeg") || strings.Contains(mimeType, "jpg") {
			ext = ".jpg"
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"linkedin-post-%s-slot%d%s\"", post.WeekKey, post.SlotIndex, ext))
	}

	if err := s.store.StreamImage(r.Context(), post.ImageFileID, w); err != nil {
		slog.Error("stream image failed", "post_id", oid.Hex(), "error", err)
	}
}

func (s *Server) handlePostCard(w http.ResponseWriter, r *http.Request) {
	oid, err := bson.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	post, err := s.store.GetPost(r.Context(), oid)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, "post_card", s.sanitizePostForView(r.Context(), post))
}

func (s *Server) handlePostEdit(w http.ResponseWriter, r *http.Request) {
	oid, err := bson.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	post, err := s.store.GetPost(r.Context(), oid)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, "post_edit", post)
}

func (s *Server) handleUpdatePostContent(w http.ResponseWriter, r *http.Request) {
	oid, err := bson.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	post, err := s.store.GetPost(r.Context(), oid)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	post.Content = strings.TrimSpace(r.FormValue("content"))
	post.SuggestedDay = strings.TrimSpace(r.FormValue("suggested_day"))
	post.AltText = strings.TrimSpace(r.FormValue("alt_text"))
	_ = s.store.SavePost(r.Context(), post)
	s.render(w, "post_card", s.sanitizePostForView(r.Context(), post))
}

func (s *Server) handleUpdatePostStatus(w http.ResponseWriter, r *http.Request) {
	oid, err := bson.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	post, err := s.store.GetPost(r.Context(), oid)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	target := domain.PostStatus(r.URL.Query().Get("status"))
	if target == domain.StatusScheduled || target == domain.StatusDraft || target == domain.StatusArchived {
		post.Status = target
		_ = s.store.SavePost(r.Context(), post)
	}
	s.render(w, "post_card", s.sanitizePostForView(r.Context(), post))
}

func (s *Server) handleToggleGolden(w http.ResponseWriter, r *http.Request) {
	oid, err := bson.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	post, err := s.store.GetPost(r.Context(), oid)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	post.IsGolden = !post.IsGolden
	_ = s.store.SavePost(r.Context(), post)
	s.render(w, "post_card", s.sanitizePostForView(r.Context(), post))
}

func (s *Server) handleRegenerateText(w http.ResponseWriter, r *http.Request) {
	oid, err := bson.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	steering := strings.TrimSpace(r.FormValue("steering"))

	post, err := s.sched.RegeneratePostText(r.Context(), oid, steering)
	if err != nil {
		post, _ = s.store.GetPost(r.Context(), oid)
		post.ErrorMessage = err.Error()
	}
	s.render(w, "post_card", s.sanitizePostForView(r.Context(), post))
}

func (s *Server) handleRegenerateImage(w http.ResponseWriter, r *http.Request) {
	oid, err := bson.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	steering := strings.TrimSpace(r.FormValue("steering"))
	aspect := strings.TrimSpace(r.FormValue("aspect_ratio"))

	post, err := s.sched.RegeneratePostImage(r.Context(), oid, steering, aspect)
	if err != nil {
		post, _ = s.store.GetPost(r.Context(), oid)
		if s.gen.ImageGenerationEnabled() {
			post.ErrorMessage = err.Error()
		}
	}
	s.render(w, "post_card", s.sanitizePostForView(r.Context(), post))
}

func (s *Server) handleDeletePost(w http.ResponseWriter, r *http.Request) {
	if oid, err := bson.ObjectIDFromHex(r.PathValue("id")); err == nil {
		_ = s.store.DeletePost(r.Context(), oid)
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) buildSubjectsData(ctx context.Context) (subjectsViewData, error) {
	list, err := s.store.ListSubjects(ctx)
	if err != nil {
		return subjectsViewData{}, err
	}
	total := 0
	for _, sub := range list {
		if sub.Active {
			total += sub.Priority
		}
	}
	return subjectsViewData{
		Subjects:            list,
		TotalActivePriority: total,
	}, nil
}

func (s *Server) handleSubjectsPage(w http.ResponseWriter, r *http.Request) {
	data, err := s.buildSubjectsData(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "page_subjects", data)
}

func (s *Server) handleSubjectsTable(w http.ResponseWriter, r *http.Request) {
	data, err := s.buildSubjectsData(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "subjects_table", data)
}

func (s *Server) handleCreateSubject(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err == nil {
		name := strings.TrimSpace(r.FormValue("name"))
		desc := strings.TrimSpace(r.FormValue("description"))
		prio, _ := strconv.Atoi(r.FormValue("priority"))
		if prio == 0 {
			prio = 5
		}
		if name != "" {
			_, _ = s.store.CreateSubject(r.Context(), name, desc, prio)
		}
	}
	s.handleSubjectsTable(w, r)
}

func (s *Server) handleSubjectEditRow(w http.ResponseWriter, r *http.Request) {
	oid, err := bson.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sub, err := s.store.GetSubject(r.Context(), oid)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, "subject_edit_row", sub)
}

func (s *Server) handleUpdateSubject(w http.ResponseWriter, r *http.Request) {
	oid, err := bson.ObjectIDFromHex(r.PathValue("id"))
	if err == nil && r.ParseForm() == nil {
		name := strings.TrimSpace(r.FormValue("name"))
		desc := strings.TrimSpace(r.FormValue("description"))
		prio, _ := strconv.Atoi(r.FormValue("priority"))
		active := r.FormValue("active") == "true"
		if name != "" {
			_ = s.store.UpdateSubject(r.Context(), oid, name, desc, prio, active)
		}
	}
	s.handleSubjectsTable(w, r)
}

func (s *Server) handleToggleSubject(w http.ResponseWriter, r *http.Request) {
	if oid, err := bson.ObjectIDFromHex(r.PathValue("id")); err == nil {
		_, _ = s.store.ToggleSubjectActive(r.Context(), oid)
	}
	s.handleSubjectsTable(w, r)
}

func (s *Server) handleDeleteSubject(w http.ResponseWriter, r *http.Request) {
	if oid, err := bson.ObjectIDFromHex(r.PathValue("id")); err == nil {
		_ = s.store.DeleteSubject(r.Context(), oid)
	}
	s.handleSubjectsTable(w, r)
}

func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.GetSettings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "page_settings", settingsViewData{
		Settings:            settings,
		GeminiConfigured:    s.gen.Configured(),
		TextModel:           s.gen.TextModel(),
		ImageModel:          s.gen.ImageModel(),
		SearchGrounding:     s.gen.SearchGrounding(),
		ImageGeneration:     s.gen.ImageGenerationEnabled(),
		ImageAutoDisabled:   s.gen.ImageAutoDisabled(),
		ImageDisabledReason: s.gen.ImageDisabledReason(),
		Timezone:            s.cfg.Timezone,
		SundayCron:          s.cfg.SundayCron,
		PostsPerWeek:        s.cfg.PostsPerWeek,
		SavedMessage:        r.URL.Query().Get("saved"),
		ErrorMessage:        r.URL.Query().Get("err"),
	})
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}

	var days []string
	for _, part := range strings.Split(r.FormValue("preferred_days"), ",") {
		if d := strings.TrimSpace(part); d != "" {
			days = append(days, d)
		}
	}
	if len(days) == 0 {
		days = []string{"Tuesday", "Wednesday", "Thursday"}
	}

	aspect := strings.TrimSpace(r.FormValue("default_aspect_ratio"))
	if aspect != "1:1" && aspect != "4:5" {
		aspect = "4:5"
	}

	cfg := domain.Settings{
		ID:                 "global",
		AuthorPersona:      strings.TrimSpace(r.FormValue("author_persona")),
		PreferredDays:      days,
		DefaultAspectRatio: aspect,
		WebhookURL:         strings.TrimSpace(r.FormValue("webhook_url")),
		PostSkillPrompt:    strings.TrimSpace(r.FormValue("post_skill_prompt")),
		ImageSkillPrompt:   strings.TrimSpace(r.FormValue("image_skill_prompt")),
	}
	if err := s.store.SaveSettings(r.Context(), cfg); err != nil {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings?saved="+url.QueryEscape("Settings and prompts saved!"), http.StatusSeeOther)
}

func (s *Server) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.GetSettings(r.Context())
	if err != nil {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	if strings.TrimSpace(settings.WebhookURL) == "" {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape("Save a Webhook URL above before sending a test notification."), http.StatusSeeOther)
		return
	}
	weekLabel := scheduler.FormatWeekRange(scheduler.CurrentWeekKey(time.Now(), s.cfg.Location), s.cfg.Location)
	if err := notify.SendReadyWebhook(r.Context(), settings.WebhookURL, weekLabel+" (Test)", s.cfg.PostsPerWeek); err != nil {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape("Webhook test failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings?saved="+url.QueryEscape("Test webhook notification delivered!"), http.StatusSeeOther)
}

func (s *Server) handleResetPrompts(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.GetSettings(r.Context())
	if err != nil {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	settings.PostSkillPrompt = prompts.DefaultPostSkillPrompt
	settings.ImageSkillPrompt = prompts.DefaultImageSkillPrompt
	if err := s.store.SaveSettings(r.Context(), settings); err != nil {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings?saved="+url.QueryEscape("Skill prompts reset to built-in SKILL.md defaults!"), http.StatusSeeOther)
}
