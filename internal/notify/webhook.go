package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// SendReadyWebhook posts a summary message to Discord, Slack, or any generic JSON webhook URL.
func SendReadyWebhook(ctx context.Context, webhookURL, weekLabel string, generatedCount int) error {
	webhookURL = strings.TrimSpace(webhookURL)
	if webhookURL == "" {
		return nil
	}

	msg := fmt.Sprintf("LinkedIn Post Manager: %d post(s) ready for the week of %s!", generatedCount, weekLabel)

	// Compatible simultaneously with Discord (`content`), Slack (`text`), and generic consumers.
	payload := map[string]any{
		"content":         msg,
		"text":            msg,
		"week":            weekLabel,
		"generated_count": generatedCount,
		"timestamp":       time.Now().UTC().Format(time.RFC3339),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}
