package chat_completions

import (
	. "proxycore/api/v6/internal/constant"
	"proxycore/api/v6/internal/interfaces"
	"proxycore/api/v6/internal/translator/translator"
)

func init() {
	translator.Register(
		OpenAI,
		Claude,
		ConvertOpenAIRequestToClaude,
		interfaces.TranslateResponse{
			Stream:    ConvertClaudeResponseToOpenAI,
			NonStream: ConvertClaudeResponseToOpenAINonStream,
		},
	)
}
