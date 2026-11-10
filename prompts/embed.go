package prompts

import _ "embed"

//go:embed linkedin_post.md
var DefaultPostSkillPrompt string

//go:embed linkedin_post_image.md
var DefaultImageSkillPrompt string
