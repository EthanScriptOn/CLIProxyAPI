package gemini

import (
	. "proxycore/api/v6/internal/constant"
	"proxycore/api/v6/internal/interfaces"
	"proxycore/api/v6/internal/translator/translator"
)

func init() {
	translator.Register(
		Gemini,
		Codex,
		ConvertGeminiRequestToCodex,
		interfaces.TranslateResponse{
			Stream:     ConvertCodexResponseToGemini,
			NonStream:  ConvertCodexResponseToGeminiNonStream,
			TokenCount: GeminiTokenCount,
		},
	)
}
