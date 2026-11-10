package domain

import (
	"strings"
	"time"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
)

type PostStatus string

const (
	StatusGenerating PostStatus = "generating"
	StatusDraft      PostStatus = "draft"
	StatusScheduled  PostStatus = "scheduled"
	StatusArchived   PostStatus = "archived"
	StatusFailed     PostStatus = "failed"
)

// Subject represents a recurring content pillar with a priority weight (1-10).
type Subject struct {
	ID            bson.ObjectID `bson:"_id,omitempty"`
	Name          string        `bson:"name"`
	Description   string        `bson:"description"`
	Priority      int           `bson:"priority"`       // 1..10 (higher = selected proportionally more often)
	CurrentWeight int           `bson:"current_weight"` // Smooth Weighted Round-Robin accumulator
	Active        bool          `bson:"active"`
	UsageCount    int           `bson:"usage_count"`
	LastUsedAt    *time.Time    `bson:"last_used_at,omitempty"`
	CreatedAt     time.Time     `bson:"created_at"`
	UpdatedAt     time.Time     `bson:"updated_at"`
}

// Idea represents a one-off spark, article link, or bug story queued for the next generation slot.
type Idea struct {
	ID          bson.ObjectID `bson:"_id,omitempty"`
	Content     string        `bson:"content"`
	SubjectName string        `bson:"subject_name"`
	Used        bool          `bson:"used"`
	UsedPostID  string        `bson:"used_post_id,omitempty"`
	UsedAt      *time.Time    `bson:"used_at,omitempty"`
	CreatedAt   time.Time     `bson:"created_at"`
}

// Post represents a generated LinkedIn post + custom image for a Sunday-anchored week.
type Post struct {
	ID           bson.ObjectID `bson:"_id,omitempty"`
	WeekKey      string        `bson:"week_key"` // YYYY-MM-DD of the week's Sunday in local TZ
	SlotIndex    int           `bson:"slot_index"`
	SuggestedDay string        `bson:"suggested_day"`
	SubjectID    string        `bson:"subject_id,omitempty"`
	SubjectName  string        `bson:"subject_name"`
	IdeaID       string        `bson:"idea_id,omitempty"`
	Status       PostStatus    `bson:"status"`
	ErrorMessage string        `bson:"error_message,omitempty"`

	// Grounded Web Search discovery (recent blog post / release)
	SourceTitle   string `bson:"source_title"`
	SourceURL     string `bson:"source_url"`
	SourceSummary string `bson:"source_summary"`

	// linkedin-post skill outputs
	Content            string   `bson:"content"`
	StrategyArea       string   `bson:"strategy_area"`
	StrategyHook       string   `bson:"strategy_hook"`
	StrategyAngle      string   `bson:"strategy_angle"`
	StrategyDiscussion string   `bson:"strategy_discussion"`
	AlternativeHooks   []string `bson:"alternative_hooks"`

	// linkedin-post-image skill outputs
	ImageFileID        bson.ObjectID `bson:"image_file_id,omitempty"`
	ImageMimeType      string        `bson:"image_mime_type"`
	ImagePrompt        string        `bson:"image_prompt"`
	ImageStyle         string        `bson:"image_style"`
	ImageAspectRatio   string        `bson:"image_aspect_ratio"`
	AltText            string        `bson:"alt_text"`
	AlternativeVisuals []string      `bson:"alternative_visuals"`
	ImageVersion       int           `bson:"image_version"`

	// Voice calibration ("Golden Posts")
	IsGolden bool `bson:"is_golden"`

	CreatedAt time.Time `bson:"created_at"`
	UpdatedAt time.Time `bson:"updated_at"`
}

// FoldPreview splits the post content around LinkedIn's mobile/desktop "...more" fold (~210 chars or 3 lines).
func (p Post) FoldPreview() (beforeFold string, afterFold string, hasFold bool) {
	text := strings.TrimSpace(p.Content)
	if text == "" {
		return "", "", false
	}

	lines := strings.Split(text, "\n")
	// LinkedIn typically truncates after ~3 non-empty lines or ~210 characters, whichever comes first.
	const maxChars = 210
	const maxLines = 4

	if len(lines) > maxLines {
		prefix := strings.Join(lines[:maxLines], "\n")
		if utf8.RuneCountInString(prefix) <= maxChars {
			rest := strings.TrimLeft(strings.Join(lines[maxLines:], "\n"), "\n")
			if rest != "" {
				return prefix, rest, true
			}
		}
	}

	runes := []rune(text)
	if len(runes) <= maxChars {
		return text, "", false
	}

	// Cut cleanly on a space or newline near maxChars
	cut := maxChars
	for i := maxChars; i > maxChars-50 && i > 0; i-- {
		if runes[i] == ' ' || runes[i] == '\n' {
			cut = i
			break
		}
	}
	return strings.TrimRight(string(runes[:cut]), " "), strings.TrimLeft(string(runes[cut:]), " \n"), true
}

func (p Post) WordCount() int {
	return len(strings.Fields(p.Content))
}

func (p Post) CharCount() int {
	return utf8.RuneCountInString(p.Content)
}

func (p Post) HasImage() bool {
	return !p.ImageFileID.IsZero()
}

// Settings holds editable skill prompts, posting days, persona, and webhook config.
type Settings struct {
	ID                 string    `bson:"_id"`
	PostSkillPrompt    string    `bson:"post_skill_prompt"`
	ImageSkillPrompt   string    `bson:"image_skill_prompt"`
	AuthorPersona      string    `bson:"author_persona"`
	PreferredDays      []string  `bson:"preferred_days"`
	DefaultAspectRatio string    `bson:"default_aspect_ratio"`
	WebhookURL         string    `bson:"webhook_url"`
	UpdatedAt          time.Time `bson:"updated_at"`
}
