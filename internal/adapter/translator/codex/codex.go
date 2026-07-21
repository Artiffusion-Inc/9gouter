// Package codex implements the codex format translator.
//
// The codex request/response translation is handled by the openai-format path
// (codex uses the OpenAI wire shape); no dedicated translator is registered
// here. The previous stubTranslator placeholder was removed as dead code.
package codex