package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"linkedin-post-manager/internal/domain"

	"google.golang.org/genai"
	"google.golang.org/genai/interactions/models/interactions"
	"google.golang.org/genai/interactions/models/operations"
)

type Generator struct {
	apiKey            string
	textModel         string
	imageModel        string
	searchGrounding   bool
	imageGeneration   bool
	imageAutoDisabled bool
	stateMu           sync.RWMutex
	textRateMutex     sync.Mutex
	imageRateMutex    sync.Mutex
}

func NewGenerator(apiKey, textModel, imageModel string, searchGrounding, imageGeneration bool) *Generator {
	return &Generator{
		apiKey:          strings.TrimSpace(apiKey),
		textModel:       textModel,
		imageModel:      imageModel,
		searchGrounding: searchGrounding,
		imageGeneration: imageGeneration,
	}
}

func (g *Generator) Configured() bool {
	return g.apiKey != ""
}

func (g *Generator) TextModel() string {
	g.stateMu.RLock()
	defer g.stateMu.RUnlock()
	return g.textModel
}

func (g *Generator) ImageModel() string {
	return g.imageModel
}

func (g *Generator) SearchGrounding() bool {
	g.stateMu.RLock()
	defer g.stateMu.RUnlock()
	return g.searchGrounding
}

func (g *Generator) ImageGenerationEnabled() bool {
	g.stateMu.RLock()
	defer g.stateMu.RUnlock()
	return g.imageGeneration
}

func (g *Generator) ImageAutoDisabled() bool {
	g.stateMu.RLock()
	defer g.stateMu.RUnlock()
	return g.imageAutoDisabled
}

func (g *Generator) ImageDisabledReason() string {
	g.stateMu.RLock()
	defer g.stateMu.RUnlock()
	if g.imageGeneration {
		return ""
	}
	if g.imageAutoDisabled {
		return "Image generation was automatically disabled because the active Gemini API key is on the Free Tier (0 image requests/day). Missing an image is not an error — posts are ready to publish as text-only."
	}
	return "Image generation is disabled (GEMINI_IMAGE_GENERATION=false). Missing an image is not an error — posts are ready to publish as text-only."
}

func (g *Generator) disableSearchGroundingOnQuota() {
	g.stateMu.Lock()
	defer g.stateMu.Unlock()
	g.searchGrounding = false
}

func (g *Generator) fallbackTextModelOnQuota(fallback string) bool {
	g.stateMu.Lock()
	defer g.stateMu.Unlock()
	if g.textModel == fallback {
		return false
	}
	g.textModel = fallback
	return true
}

func (g *Generator) disableImageGenerationOnQuota() {
	g.stateMu.Lock()
	defer g.stateMu.Unlock()
	g.imageGeneration = false
	g.imageAutoDisabled = true
}

func (g *Generator) newClient(ctx context.Context) (*genai.Client, error) {
	if !g.Configured() {
		return nil, errors.New("GEMINI_API_KEY is not configured; set it in .env and restart the container")
	}
	return genai.NewClient(ctx, &genai.ClientConfig{
		APIKey: g.apiKey,
	})
}

// IsPlanQuotaError detects whether an error is a permanent plan/tier quota restriction
// (e.g. Free Tier limit of 0 RPD, billing not configured, plan quota exceeded)
// where retries will never succeed without changing plans or upgrading tiers.
func IsPlanQuotaError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "check your plan and billing details") ||
		strings.Contains(msg, "billing") ||
		(strings.Contains(msg, "quota") && strings.Contains(msg, "plan")) ||
		strings.Contains(msg, "free tier") ||
		strings.Contains(msg, "upgrade your tier") ||
		strings.Contains(msg, "limit: 0") ||
		strings.Contains(msg, "limit of 0") ||
		strings.Contains(msg, "0 requests per day") ||
		strings.Contains(msg, "ai.dev/rate-limit")
}

// IsHighDemandError detects Google Gemini 503 / Service Unavailable / temporary high demand spikes.
func IsHighDemandError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "high demand") ||
		strings.Contains(msg, "spikes in demand") ||
		strings.Contains(msg, "service_unavailable") ||
		strings.Contains(msg, "503") ||
		strings.Contains(msg, "temporarily unavailable") ||
		strings.Contains(msg, "try again later") ||
		strings.Contains(msg, "overloaded")
}

func isRetryableGeminiError(err error) bool {
	if err == nil {
		return false
	}
	if IsPlanQuotaError(err) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "too_many_requests") ||
		strings.Contains(msg, "resource_exhausted") ||
		strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "429") ||
		strings.Contains(msg, "service_unavailable") ||
		strings.Contains(msg, "503") ||
		strings.Contains(msg, "high demand") ||
		strings.Contains(msg, "spikes in demand") ||
		strings.Contains(msg, "temporarily unavailable") ||
		strings.Contains(msg, "try again later") ||
		strings.Contains(msg, "overloaded") ||
		strings.Contains(msg, "deadline_exceeded") ||
		strings.Contains(msg, "504") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "api_error")
}

func (g *Generator) executeWithRetry(parentCtx context.Context, mu *sync.Mutex, backoffs []time.Duration, timeout time.Duration, label string, fn func(ctx context.Context) error) error {
	var lastErr error

	for i, wait := range backoffs {
		if wait > 0 {
			if IsHighDemandError(lastErr) {
				slog.Warn("retrying gemini request after high demand backoff", "operation", label, "attempt", i+1, "wait", wait)
			} else {
				slog.Warn("retrying gemini request after backoff", "operation", label, "attempt", i+1, "wait", wait)
			}
			select {
			case <-parentCtx.Done():
				return parentCtx.Err()
			case <-time.After(wait):
			}
		}

		var err error
		func() {
			mu.Lock()
			defer mu.Unlock()

			attemptCtx, cancelAttempt := context.WithTimeout(parentCtx, timeout)
			defer cancelAttempt()

			err = fn(attemptCtx)
			if err == nil {
				// Mandatory 3-second breathing room after every successful call to stay comfortably under RPM limits
				time.Sleep(3 * time.Second)
			}
		}()

		if err == nil {
			return nil
		}

		lastErr = err
		if IsPlanQuotaError(err) {
			// Plan-level quota error: retrying cannot succeed without upgrading the plan.
			return err
		}
		if !isRetryableGeminiError(err) {
			return err
		}
	}
	return fmt.Errorf("%s exceeded retries: %w", label, lastErr)
}

type PostDraftRequest struct {
	SubjectName         string
	SubjectDescription  string
	IdeaSeed            string
	SteeringInstruction string
	PreviousDraft       string
	RecentPosts         []domain.Post
	GoldenPosts         []domain.Post
	SkillSystemPrompt   string
	AuthorPersona       string
	Now                 time.Time
}

type PostDraftResult struct {
	SourceTitle        string   `json:"source_title"`
	SourceURL          string   `json:"source_url"`
	SourceSummary      string   `json:"source_summary"`
	PostContent        string   `json:"post_content"`
	StrategyArea       string   `json:"strategy_area"`
	StrategyHook       string   `json:"strategy_hook"`
	StrategyAngle      string   `json:"strategy_angle"`
	StrategyDiscussion string   `json:"strategy_discussion"`
	AlternativeHooks   []string `json:"alternative_hooks"`
}

var postDraftSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"source_title": map[string]any{
			"type":        "string",
			"description": "Title of the recent blog post, release announcement, or engineering article discovered via Google Search.",
		},
		"source_url": map[string]any{
			"type":        "string",
			"description": "Direct URL of the recent blog post or engineering article discovered via Google Search.",
		},
		"source_summary": map[string]any{
			"type":        "string",
			"description": "1-2 sentence summary of the fresh insight, feature, or takeaway from that article.",
		},
		"post_content": map[string]any{
			"type":        "string",
			"description": "Complete, ready-to-copy LinkedIn post in plain text (no Markdown bold asterisks, clean blank-line spacing, ending with a discussion question and 3 hashtags).",
		},
		"strategy_area": map[string]any{
			"type":        "string",
			"description": "The Area pillar summary.",
		},
		"strategy_hook": map[string]any{
			"type":        "string",
			"description": "The opening Hook strategy used.",
		},
		"strategy_angle": map[string]any{
			"type":        "string",
			"description": "The Angle/perspective chosen (Contrarian, Tactical, Story, or Trend Analysis).",
		},
		"strategy_discussion": map[string]any{
			"type":        "string",
			"description": "Core discussion meat and closing question intent.",
		},
		"alternative_hooks": map[string]any{
			"type":        "array",
			"description": "Two alternative opening hooks (2 lines each) the author can swap in.",
			"items": map[string]any{
				"type": "string",
			},
		},
	},
	"required": []string{
		"source_title",
		"source_url",
		"source_summary",
		"post_content",
		"strategy_area",
		"strategy_hook",
		"strategy_angle",
		"strategy_discussion",
		"alternative_hooks",
	},
}

func (g *Generator) GeneratePostDraft(ctx context.Context, req PostDraftRequest) (*PostDraftResult, error) {
	client, err := g.newClient(ctx)
	if err != nil {
		return nil, err
	}

	var sys strings.Builder
	sys.WriteString(strings.TrimSpace(req.SkillSystemPrompt))
	if strings.TrimSpace(req.AuthorPersona) != "" {
		sys.WriteString("\n\n---\n## Author Persona & Voice\n")
		sys.WriteString(strings.TrimSpace(req.AuthorPersona))
	}
	if len(req.GoldenPosts) > 0 {
		sys.WriteString("\n\n---\n## Golden Voice Reference Examples (Match This Authentic Tone & Rhythm)\n")
		for i, gp := range req.GoldenPosts {
			sys.WriteString(fmt.Sprintf("\n### Voice Example %d:\n%s\n", i+1, strings.TrimSpace(gp.Content)))
		}
	}
	sysStr := sys.String()

	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}

	format := interactions.NewCreateModelInteractionResponseFormat(
		interactions.NewResponseFormat(interactions.TextResponseFormat{
			MimeType: interactions.TextResponseFormatMimeTypeApplicationJSON.ToPointer(),
			Schema:   postDraftSchema,
		}),
	)

	runAttempt := func(withSearch bool) (*PostDraftResult, error) {
		var prompt strings.Builder
		prompt.WriteString(fmt.Sprintf("Current Date: %s\n", now.Format("2006-01-02")))
		prompt.WriteString(fmt.Sprintf("Target Subject / Area: %s\n", req.SubjectName))
		if strings.TrimSpace(req.SubjectDescription) != "" {
			prompt.WriteString(fmt.Sprintf("Subject Guidelines & Focus: %s\n", strings.TrimSpace(req.SubjectDescription)))
		}

		if strings.TrimSpace(req.IdeaSeed) != "" {
			if withSearch {
				prompt.WriteString(fmt.Sprintf("\nPriority Author Spark / Seed Note (center the post around this and enrich with recent web search context): %s\n", strings.TrimSpace(req.IdeaSeed)))
			} else {
				prompt.WriteString(fmt.Sprintf("\nPriority Author Spark / Seed Note (center the post around this): %s\n", strings.TrimSpace(req.IdeaSeed)))
			}
		} else if withSearch {
			prompt.WriteString(fmt.Sprintf("\nTask: Use Google Search right now to find a specific, recent technical blog post, official release post, engineering teardown, or community article about %s published recently (prioritize the last 30 days around %s). Synthesize a fresh, specific engineering insight from it and write a high-impact LinkedIn post.\n", req.SubjectName, now.Format("January 2006")))
		} else {
			prompt.WriteString(fmt.Sprintf("\nTask: Synthesize a fresh, specific engineering insight about %s and write a high-impact LinkedIn post. If referencing a specific engineering topic, library, or architecture pattern, provide its topic title, a concise summary, and a relevant reference URL (or leave empty if general).\n", req.SubjectName))
		}

		if len(req.RecentPosts) > 0 {
			prompt.WriteString("\nPreviously Covered Articles & Hooks (DO NOT reuse these URLs, articles, or angles — pick something completely different):\n")
			for _, rp := range req.RecentPosts {
				firstLine := firstNonEmptyLine(rp.Content)
				if rp.SourceURL != "" {
					prompt.WriteString(fmt.Sprintf("- URL: %s | Title: %s | Hook: %s\n", rp.SourceURL, rp.SourceTitle, firstLine))
				} else if firstLine != "" {
					prompt.WriteString(fmt.Sprintf("- Hook: %s\n", firstLine))
				}
			}
		}

		if strings.TrimSpace(req.PreviousDraft) != "" {
			prompt.WriteString("\nPrevious Draft Being Revised:\n---\n")
			prompt.WriteString(strings.TrimSpace(req.PreviousDraft))
			prompt.WriteString("\n---\n")
		}

		if strings.TrimSpace(req.SteeringInstruction) != "" {
			prompt.WriteString(fmt.Sprintf("\nExplicit Revision / Steering Direction from Author (MUST FOLLOW): %s\n", strings.TrimSpace(req.SteeringInstruction)))
		}

		activeTextModel := g.TextModel()
		input := interactions.NewInteractionsInput(prompt.String())
		var tools []interactions.Tool
		opLabel := fmt.Sprintf("post generation ungrounded (%s)", activeTextModel)
		if withSearch {
			tools = []interactions.Tool{
				interactions.NewTool(interactions.GoogleSearch{}),
				interactions.NewTool(interactions.URLContext{}),
			}
			opLabel = fmt.Sprintf("post generation (%s)", activeTextModel)
		}

		var resp *operations.CreateInteractionResponse
		textBackoffs := []time.Duration{0, 4 * time.Second, 8 * time.Second}
		err := g.executeWithRetry(ctx, &g.textRateMutex, textBackoffs, 60*time.Second, opLabel, func(attemptCtx context.Context) error {
			var callErr error
			resp, callErr = client.Interactions.Create(attemptCtx, operations.CreateInteractionRequest{
				Body: operations.NewCreateInteractionRequestBody(interactions.CreateModelInteraction{
					Model:             interactions.Model(activeTextModel),
					SystemInstruction: &sysStr,
					Input:             &input,
					Tools:             tools,
					ResponseFormat:    &format,
				}),
			})
			return callErr
		})
		if err != nil {
			return nil, err
		}
		if resp.Interaction == nil || resp.Interaction.GetOutputText() == nil {
			return nil, errors.New("gemini returned empty text output")
		}

		raw := cleanJSONFence(*resp.Interaction.GetOutputText())
		var out PostDraftResult
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			return nil, fmt.Errorf("parse post JSON output: %w", err)
		}

		out.PostContent = sanitizeLinkedInPlainText(out.PostContent)
		for i := range out.AlternativeHooks {
			out.AlternativeHooks[i] = sanitizeLinkedInPlainText(out.AlternativeHooks[i])
		}
		return &out, nil
	}

	if g.SearchGrounding() {
		res, err := runAttempt(true)
		if err == nil {
			return res, nil
		}
		if IsPlanQuotaError(err) || isRetryableGeminiError(err) {
			if IsPlanQuotaError(err) {
				slog.Warn("gemini search grounding requires a paid Gemini API plan/billing; automatically disabling search grounding for subsequent posts", "model", g.TextModel())
				g.disableSearchGroundingOnQuota()
			} else {
				slog.Warn("gemini search grounding failed due to quota/plan limits or service unavailability; falling back to ungrounded post generation", "model", g.TextModel(), "error", err)
			}
		} else {
			return nil, fmt.Errorf("gemini post generation (%s): %w", g.TextModel(), err)
		}
	}

	res, err := runAttempt(false)
	if err == nil {
		return res, nil
	}
	if IsPlanQuotaError(err) && g.fallbackTextModelOnQuota("gemini-3.5-flash-lite") {
		slog.Warn("configured text model requires a paid Gemini API tier; automatically falling back to gemini-3.5-flash-lite", "fallback_model", "gemini-3.5-flash-lite")
		res, err = runAttempt(false)
		if err == nil {
			return res, nil
		}
	}
	return nil, fmt.Errorf("gemini post generation (%s): %w", g.TextModel(), err)
}

type PostImageRequest struct {
	PostContent         string
	StrategyHook        string
	StrategyAngle       string
	AspectRatio         string
	SteeringInstruction string
	ImageSkillPrompt    string
}

type PostImageResult struct {
	ImageBytes         []byte
	MimeType           string
	AspectRatio        string
	VisualStyle        string
	ImagePrompt        string
	AltText            string
	AlternativeVisuals []string
}

type imageBriefJSON struct {
	VisualStyle        string   `json:"visual_style"`
	ImagePrompt        string   `json:"image_prompt"`
	AltText            string   `json:"alt_text"`
	AlternativeVisuals []string `json:"alternative_visuals"`
}

var imageBriefSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"visual_style": map[string]any{
			"type":        "string",
			"description": "Chosen visual format name (e.g. Editorial Concept Illustration, Split-Screen Contrast, Minimalist Whiteboard Sketch, or Cinematic Editorial Photography).",
		},
		"image_prompt": map[string]any{
			"type":        "string",
			"description": "Detailed, self-contained image generation prompt specifying concrete visual metaphor, composition, color palette, lighting/texture, and explicit zero-text / anti-cliche constraints.",
		},
		"alt_text": map[string]any{
			"type":        "string",
			"description": "Concise 1-2 sentence accessible LinkedIn Alt Text describing the image.",
		},
		"alternative_visuals": map[string]any{
			"type":        "array",
			"description": "Two concise alternative visual concept descriptions the author can click to try a different style.",
			"items": map[string]any{
				"type": "string",
			},
		},
	},
	"required": []string{"visual_style", "image_prompt", "alt_text", "alternative_visuals"},
}

func IsImageDisabledOrQuotaMessage(msg string) bool {
	trimmed := strings.TrimSpace(msg)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "image generation is disabled") ||
		strings.Contains(lower, "image generation quota exceeded") {
		return true
	}
	return strings.Contains(lower, "image") && IsPlanQuotaError(errors.New(trimmed))
}

func (g *Generator) GeneratePostImage(ctx context.Context, req PostImageRequest) (*PostImageResult, error) {
	if !g.ImageGenerationEnabled() {
		return nil, errors.New(g.ImageDisabledReason())
	}
	client, err := g.newClient(ctx)
	if err != nil {
		return nil, err
	}

	aspect := strings.TrimSpace(req.AspectRatio)
	if aspect == "" {
		aspect = "4:5"
	}

	var briefInput strings.Builder
	briefInput.WriteString(fmt.Sprintf("Target Aspect Ratio: %s\n", aspect))
	if req.StrategyHook != "" {
		briefInput.WriteString(fmt.Sprintf("Post Hook: %s\n", req.StrategyHook))
	}
	if req.StrategyAngle != "" {
		briefInput.WriteString(fmt.Sprintf("Post Angle: %s\n", req.StrategyAngle))
	}
	briefInput.WriteString("\nLinkedIn Post Copy:\n---\n")
	briefInput.WriteString(strings.TrimSpace(req.PostContent))
	briefInput.WriteString("\n---\n")
	if strings.TrimSpace(req.SteeringInstruction) != "" {
		briefInput.WriteString(fmt.Sprintf("\nAuthor Visual Steering / Override (MUST FOLLOW): %s\n", strings.TrimSpace(req.SteeringInstruction)))
	}

	briefFormat := interactions.NewCreateModelInteractionResponseFormat(
		interactions.NewResponseFormat(interactions.TextResponseFormat{
			MimeType: interactions.TextResponseFormatMimeTypeApplicationJSON.ToPointer(),
			Schema:   imageBriefSchema,
		}),
	)

	sysPrompt := strings.TrimSpace(req.ImageSkillPrompt)
	briefIn := interactions.NewInteractionsInput(briefInput.String())

	var briefResp *operations.CreateInteractionResponse
	imageBriefBackoffs := []time.Duration{0, 3 * time.Second}
	err = g.executeWithRetry(ctx, &g.imageRateMutex, imageBriefBackoffs, 25*time.Second, fmt.Sprintf("image brief (%s)", g.textModel), func(attemptCtx context.Context) error {
		var callErr error
		briefResp, callErr = client.Interactions.Create(attemptCtx, operations.CreateInteractionRequest{
			Body: operations.NewCreateInteractionRequestBody(interactions.CreateModelInteraction{
				Model:             interactions.Model(g.textModel),
				SystemInstruction: &sysPrompt,
				Input:             &briefIn,
				ResponseFormat:    &briefFormat,
			}),
		})
		return callErr
	})
	if err != nil {
		if IsPlanQuotaError(err) {
			slog.Warn("gemini image brief encountered plan quota restriction; automatically disabling image generation for subsequent operations", "model", g.textModel)
			g.disableImageGenerationOnQuota()
		}
		return nil, fmt.Errorf("gemini image brief (%s): %w", g.textModel, err)
	}
	if briefResp.Interaction == nil || briefResp.Interaction.GetOutputText() == nil {
		return nil, errors.New("gemini returned empty image brief")
	}

	var brief imageBriefJSON
	if err := json.Unmarshal([]byte(cleanJSONFence(*briefResp.Interaction.GetOutputText())), &brief); err != nil {
		return nil, fmt.Errorf("parse image brief JSON: %w", err)
	}

	imgFormat := interactions.NewCreateModelInteractionResponseFormat(
		interactions.NewResponseFormat(interactions.ImageResponseFormat{
			AspectRatio: interactions.ImageResponseFormatAspectRatio(aspect).ToPointer(),
			ImageSize:   interactions.ImageResponseFormatImageSize("1K").ToPointer(),
		}),
	)

	imgIn := interactions.NewInteractionsInput(brief.ImagePrompt)

	var imgResp *operations.CreateInteractionResponse
	imageRenderBackoffs := []time.Duration{0, 3 * time.Second}
	err = g.executeWithRetry(ctx, &g.imageRateMutex, imageRenderBackoffs, 40*time.Second, fmt.Sprintf("image render (%s)", g.imageModel), func(attemptCtx context.Context) error {
		var callErr error
		imgResp, callErr = client.Interactions.Create(attemptCtx, operations.CreateInteractionRequest{
			Body: operations.NewCreateInteractionRequestBody(interactions.CreateModelInteraction{
				Model:          interactions.Model(g.imageModel),
				Input:          &imgIn,
				ResponseFormat: &imgFormat,
			}),
		})
		return callErr
	})
	if err != nil {
		if IsPlanQuotaError(err) {
			slog.Warn("gemini image model requires a paid Gemini API tier/billing; automatically disabling image generation for subsequent operations", "model", g.imageModel)
			g.disableImageGenerationOnQuota()
		}
		return nil, fmt.Errorf("gemini image render (%s): %w", g.imageModel, err)
	}
	if imgResp.Interaction == nil || imgResp.Interaction.OutputImage == nil || imgResp.Interaction.OutputImage.Data == nil {
		return nil, errors.New("gemini image model returned no image bytes")
	}

	b64 := strings.TrimSpace(*imgResp.Interaction.OutputImage.Data)
	imgBytes, err := decodeBase64Image(b64)
	if err != nil {
		return nil, fmt.Errorf("decode image base64: %w", err)
	}

	mimeType := "image/png"
	if imgResp.Interaction.OutputImage.MimeType != nil && string(*imgResp.Interaction.OutputImage.MimeType) != "" {
		mimeType = string(*imgResp.Interaction.OutputImage.MimeType)
	}

	return &PostImageResult{
		ImageBytes:         imgBytes,
		MimeType:           mimeType,
		AspectRatio:        aspect,
		VisualStyle:        brief.VisualStyle,
		ImagePrompt:        brief.ImagePrompt,
		AltText:            brief.AltText,
		AlternativeVisuals: brief.AlternativeVisuals,
	}, nil
}

func decodeBase64Image(s string) ([]byte, error) {
	if idx := strings.Index(s, ","); idx != -1 && strings.Contains(s[:idx], "base64") {
		s = s[idx+1:]
	}
	if data, err := base64.StdEncoding.DecodeString(s); err == nil {
		return data, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

func cleanJSONFence(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

func sanitizeLinkedInPlainText(s string) string {
	s = strings.ReplaceAll(s, "**", "")
	s = strings.ReplaceAll(s, "__", "")
	return strings.TrimSpace(s)
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}
