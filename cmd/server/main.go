package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"linkedin-post-manager/internal/config"
	"linkedin-post-manager/internal/gemini"
	"linkedin-post-manager/internal/repository"
	"linkedin-post-manager/internal/scheduler"
	"linkedin-post-manager/internal/web"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg := config.Load()

	appCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := repository.Connect(appCtx, cfg.MongoURI, cfg.MongoDBName, cfg.WebhookURL)
	if err != nil {
		slog.Error("failed connecting to MongoDB", "error", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = store.Close(shutdownCtx)
	}()

	gen := gemini.NewGenerator(cfg.GeminiAPIKey, cfg.GeminiTextModel, cfg.GeminiImageModel, cfg.GeminiSearchGrounding, cfg.GeminiImageGeneration)
	sched := scheduler.NewService(cfg, store, gen)
	if err := sched.Start(appCtx); err != nil {
		slog.Error("failed starting weekly scheduler", "error", err)
		os.Exit(1)
	}

	srvHandler, err := web.NewServer(appCtx, cfg, store, gen, sched)
	if err != nil {
		slog.Error("failed initializing web server", "error", err)
		os.Exit(1)
	}

	httpSrv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srvHandler.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("LinkedIn Post Manager listening",
			"port", cfg.Port,
			"timezone", cfg.Timezone,
			"sunday_cron", cfg.SundayCron,
			"posts_per_week", cfg.PostsPerWeek,
			"text_model", cfg.GeminiTextModel,
			"image_model", cfg.GeminiImageModel,
			"search_grounding", cfg.GeminiSearchGrounding,
			"image_generation", cfg.GeminiImageGeneration,
			"gemini_key_set", gen.Configured(),
		)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server error", "error", err)
			os.Exit(1)
		}
	}()

	<-appCtx.Done()
	slog.Info("shutting down gracefully...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}
