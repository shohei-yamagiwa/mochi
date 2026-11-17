package configs

import "embed"

// PromptsFS embeds the standard prompt templates shipped with Mochi.
//
//go:embed prompts/*.md
var PromptsFS embed.FS
