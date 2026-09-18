package modelsdev_test

import (
	"fmt"

	"github.com/fonlan/fobe/internal/server/modelsdev"
)

// ExampleReasoningLevels shows the two pure mappings the provider form needs.
// It lives in the _test package on purpose: it proves the exported surface is
// enough for a caller, without reaching into unexported fields.
func ExampleReasoningLevels() {
	// An effort-style model reached over the OpenAI Responses dialect.
	meta := modelsdev.ModelMeta{
		ID:               "gpt-5",
		Name:             "GPT-5",
		Reasoning:        true,
		ReasoningOptions: []string{modelsdev.ReasoningEffort},
	}

	fmt.Println(modelsdev.ProtocolForNPM("@ai-sdk/openai"))
	// OpenAI with effort only cannot honestly offer "off".
	fmt.Println(modelsdev.ReasoningLevels(meta, modelsdev.ProtocolOpenAIResponses))
	// The same model over Anthropic is opt-in, so "off" is always there.
	fmt.Println(modelsdev.ReasoningLevels(meta, modelsdev.ProtocolAnthropicMessages))
	// A vendor outside the three dialects: no protocol is guessed.
	fmt.Printf("%q\n", modelsdev.ProtocolForNPM("@ai-sdk/google-vertex"))

	// A gateway whose published effort values are ["none","low","high"]: the
	// vendor ships an off switch and only two graded names.
	gateway := modelsdev.ModelMeta{
		ID:               "deepseek/deepseek-v3.2",
		Reasoning:        true,
		ReasoningOptions: []string{modelsdev.ReasoningEffort},
		ReasoningValues:  []string{modelsdev.ReasoningValueNone, "low", "high"},
	}
	fmt.Println(modelsdev.ReasoningLevels(gateway, modelsdev.ProtocolOpenAICompletions))

	// Output:
	// openai-responses
	// [low medium high]
	// [off low medium high]
	// ""
	// [off low high]
}
