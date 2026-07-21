// Package cursor implements the cursor format translator.
//
// The cursor request/response translation is handled by the openai-format path
// (cursor uses the OpenAI wire shape); no dedicated translator is registered
// here. The previous stubTranslator placeholder was removed as dead code.
package cursor