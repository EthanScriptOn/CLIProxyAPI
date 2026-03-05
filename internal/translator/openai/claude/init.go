package claude

import (
	. "proxycore/api/v6/internal/constant"
	"proxycore/api/v6/internal/interfaces"
	"proxycore/api/v6/internal/translator/translator"
)

func init() {
	translator.Register(
		Claude,
		OpenAI,
		ConvertClaudeRequestToOpenAI,
		interfaces.TranslateResponse{
			Stream:     ConvertOpenAIResponseToClaude,
			NonStream:  ConvertOpenAIResponseToClaudeNonStream,
			TokenCount: ClaudeTokenCount,
		},
	)
}
