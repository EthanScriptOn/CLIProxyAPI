package translator

import (
	_ "proxycore/api/v6/internal/translator/claude/gemini"
	_ "proxycore/api/v6/internal/translator/claude/gemini-cli"
	_ "proxycore/api/v6/internal/translator/claude/openai/chat-completions"
	_ "proxycore/api/v6/internal/translator/claude/openai/responses"

	_ "proxycore/api/v6/internal/translator/codex/claude"
	_ "proxycore/api/v6/internal/translator/codex/gemini"
	_ "proxycore/api/v6/internal/translator/codex/gemini-cli"
	_ "proxycore/api/v6/internal/translator/codex/openai/chat-completions"
	_ "proxycore/api/v6/internal/translator/codex/openai/responses"

	_ "proxycore/api/v6/internal/translator/gemini-cli/claude"
	_ "proxycore/api/v6/internal/translator/gemini-cli/gemini"
	_ "proxycore/api/v6/internal/translator/gemini-cli/openai/chat-completions"
	_ "proxycore/api/v6/internal/translator/gemini-cli/openai/responses"

	_ "proxycore/api/v6/internal/translator/gemini/claude"
	_ "proxycore/api/v6/internal/translator/gemini/gemini"
	_ "proxycore/api/v6/internal/translator/gemini/gemini-cli"
	_ "proxycore/api/v6/internal/translator/gemini/openai/chat-completions"
	_ "proxycore/api/v6/internal/translator/gemini/openai/responses"

	_ "proxycore/api/v6/internal/translator/openai/claude"
	_ "proxycore/api/v6/internal/translator/openai/gemini"
	_ "proxycore/api/v6/internal/translator/openai/gemini-cli"
	_ "proxycore/api/v6/internal/translator/openai/openai/chat-completions"
	_ "proxycore/api/v6/internal/translator/openai/openai/responses"

	_ "proxycore/api/v6/internal/translator/antigravity/claude"
	_ "proxycore/api/v6/internal/translator/antigravity/gemini"
	_ "proxycore/api/v6/internal/translator/antigravity/openai/chat-completions"
	_ "proxycore/api/v6/internal/translator/antigravity/openai/responses"
)
