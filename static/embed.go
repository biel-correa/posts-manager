package static

import "embed"

//go:embed styles.css app.js
var Assets embed.FS
