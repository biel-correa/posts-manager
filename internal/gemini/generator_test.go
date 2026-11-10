package gemini

import (
	"errors"
	"testing"
)

func TestIsHighDemandError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("some regular error"), false},
		{errors.New(`{"error":{"code":"service_unavailable","message":"gemini-3.5-flash-lite is currently experiencing high demand, spikes in demand are usually temporary. Please try again later."}}`), true},
		{errors.New("503 Service Unavailable"), true},
		{errors.New("model is overloaded, try again later"), true},
		{errors.New("temporarily unavailable"), true},
	}

	for _, tc := range cases {
		got := IsHighDemandError(tc.err)
		if got != tc.want {
			t.Errorf("IsHighDemandError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestIsRetryableGeminiError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("regular bad request 400"), false},
		{errors.New(`{"error":{"code":"service_unavailable","message":"high demand"}}`), true},
		{errors.New(`{"error":{"code":"too_many_requests","message":"Rate limit exceeded"}}`), true},
		{errors.New("429 resource_exhausted"), true},
		{errors.New("504 deadline_exceeded"), true},
		// Plan quota errors must NOT be considered transiently retryable
		{errors.New("You exceeded your current quota, please check your plan and billing details"), false},
		{errors.New("Rate limit exceeded for model gemini-nano-banana-2.1 (limit: 0 requests per day on Free Tier). Please upgrade your tier..."), false},
	}

	for _, tc := range cases {
		got := isRetryableGeminiError(tc.err)
		if got != tc.want {
			t.Errorf("isRetryableGeminiError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestIsPlanQuotaError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("regular 500 error"), false},
		{errors.New("service_unavailable high demand"), false},
		{errors.New("You exceeded your current quota, please check your plan and billing details"), true},
		{errors.New("billing details required"), true},
		{errors.New("quota exceeded for your plan"), true},
		{errors.New("Rate limit exceeded for model gemini-nano-banana-2.1 (limit: 0 requests per day on Free Tier). Please upgrade your tier..."), true},
		{errors.New("limit: 0"), true},
	}

	for _, tc := range cases {
		got := IsPlanQuotaError(tc.err)
		if got != tc.want {
			t.Errorf("IsPlanQuotaError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestIsImageDisabledOrQuotaMessage(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"", false},
		{"regular network error", false},
		{"Image generation deferred: Gemini is experiencing temporary high demand.", false},
		{"Image generation quota exceeded: Rate limit exceeded for model gemini-nano-banana-2.1 (limit: 0 requests per day on Free Tier)", true},
		{"image generation is disabled (GEMINI_IMAGE_GENERATION=false)", true},
	}

	for _, tc := range cases {
		got := IsImageDisabledOrQuotaMessage(tc.msg)
		if got != tc.want {
			t.Errorf("IsImageDisabledOrQuotaMessage(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

func TestImageDisabledReason(t *testing.T) {
	gen := NewGenerator("key", "gemini-3.5-flash-lite", "gemini-nano-banana-2.1", false, false)
	if gen.ImageGenerationEnabled() {
		t.Fatalf("expected ImageGenerationEnabled() to be false")
	}
	if gen.ImageDisabledReason() == "" {
		t.Fatalf("expected non-empty ImageDisabledReason() when image generation is disabled")
	}

	genAuto := NewGenerator("key", "gemini-3.5-flash-lite", "gemini-nano-banana-2.1", true, true)
	genAuto.disableImageGenerationOnQuota()
	if genAuto.ImageGenerationEnabled() {
		t.Fatalf("expected ImageGenerationEnabled() to be false after quota disable")
	}
	if !genAuto.ImageAutoDisabled() {
		t.Fatalf("expected ImageAutoDisabled() to be true after quota disable")
	}
}

