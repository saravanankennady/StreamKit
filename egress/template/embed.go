// Package template holds the page streamkit-egress opens in headless Chrome. Run "node build.mjs" here after changing
// src/ or the SDK.
package template

import "embed"

// Files holds the page and its bundled script.
//
//go:embed index.html assets/recorder.js
var Files embed.FS
