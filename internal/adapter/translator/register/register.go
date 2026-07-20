// Package register side-effect imports every translator subpackage so their
// init() registrations (translator.RegisterRequest / RegisterResponse) run in
// the final binary. Without this, the Go linker drops subpackages that no
// production package imports directly, and the package-level registry stays
// empty at runtime — TranslateResponse falls back to returning the raw
// upstream chunk untranslated. Tests pass because the _test.go files live in
// the subpackages and trigger their own init(), masking the production gap.
//
// Import this package from the composition root (internal/app) so the
// registrations are linked into cmd/9gouter.
package register

import (
	// Register every translator pair. Blank imports are intentional.
	_ "github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/claude"
	_ "github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/commandcode"
	_ "github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/cursor"
	_ "github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/codex"
	_ "github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/gemini"
	_ "github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/kiro"
	_ "github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/ollama"
	_ "github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/openai"
)