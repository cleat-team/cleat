package providers

// ModelPrice is a model's rate in dollars per million tokens.
type ModelPrice struct {
	PromptPerMillion     float64
	CompletionPerMillion float64
}

// prices is the single source of truth for what cleat charges a spend
// ceiling for an LLM call. Before cleat#2572, each provider file carried its
// own switch (and a `default:` that silently priced an unrecognised model as
// one of its mid-range ones), and host_functions.go's listModels carried a
// second, differently-shaped table (one blended $/1k figure, versus these
// split prompt/completion $/M rates) that was only consistent with this one
// at a 1:1 prompt:completion ratio. Both defects are closed by having exactly
// one table: listModels derives its display figures from this map, and every
// ChatXXX function prices through CostFor rather than its own switch.
//
// Groq's entries are its own real rates from Groq's pricing, not borrowed
// from OpenAI's table -- see cleat#2572: GroqChat delegates to OpenAIChat for
// everything else, and OpenAIChat's old cost switch matched none of Groq's
// model names, so it priced 100% of Groq calls at OpenAI's default rate.
var prices = map[string]map[string]ModelPrice{
	"openai": {
		"gpt-4o":      {PromptPerMillion: 2.50, CompletionPerMillion: 10.0},
		"gpt-4o-mini": {PromptPerMillion: 0.15, CompletionPerMillion: 0.60},
		"gpt-4-turbo": {PromptPerMillion: 10.0, CompletionPerMillion: 30.0},
	},
	"anthropic": {
		"claude-opus-4-7":   {PromptPerMillion: 15.0, CompletionPerMillion: 75.0},
		"claude-sonnet-4-6": {PromptPerMillion: 3.0, CompletionPerMillion: 15.0},
		"claude-haiku-4-5":  {PromptPerMillion: 0.80, CompletionPerMillion: 4.0},
	},
	"groq": {
		"llama-3.3-70b": {PromptPerMillion: 0.59, CompletionPerMillion: 0.79},
		"mixtral-8x7b":  {PromptPerMillion: 0.24, CompletionPerMillion: 0.24},
	},
	"gemini": {
		"gemini-2.5-flash":      {PromptPerMillion: 0.15, CompletionPerMillion: 0.60},
		"gemini-2.5-pro":        {PromptPerMillion: 1.25, CompletionPerMillion: 5.00},
		"gemini-2.0-flash":      {PromptPerMillion: 0.10, CompletionPerMillion: 0.40},
		"gemini-2.0-flash-lite": {PromptPerMillion: 0.075, CompletionPerMillion: 0.30},
	},
	"mistral": {
		"mistral-large-latest":  {PromptPerMillion: 2.0, CompletionPerMillion: 6.0},
		"mistral-medium-latest": {PromptPerMillion: 2.5, CompletionPerMillion: 2.5},
		"mistral-small-latest":  {PromptPerMillion: 1.0, CompletionPerMillion: 3.0},
		"open-mistral-nemo":     {PromptPerMillion: 0.3, CompletionPerMillion: 0.3},
	},
}

// ProviderModels returns the sorted-by-caller-insertion model names cleat has
// a rate for under provider, for listModels. Returns nil for an unknown
// provider.
func ProviderModels(provider string) map[string]ModelPrice {
	return prices[provider]
}

// PriceFor returns the per-token rate cleat has on file for provider/model,
// and whether the table actually carries that model. false means CostFor
// will price conservatively rather than by a known rate -- see its doc
// comment.
func PriceFor(provider, model string) (ModelPrice, bool) {
	table, ok := prices[provider]
	if !ok {
		return ModelPrice{}, false
	}
	p, ok := table[model]
	return p, ok
}

// highestKnownRate returns the most expensive rate cleat knows for provider,
// for pricing a model the table does not carry. Conservative is the safe
// direction for a spend ceiling (cleat#2572): pricing high fails toward
// tripping the ceiling early, pricing low (or mid-range, the old default)
// fails toward permitting undetected overspend.
func highestKnownRate(provider string) ModelPrice {
	var worst ModelPrice
	for _, p := range prices[provider] {
		if p.PromptPerMillion+p.CompletionPerMillion > worst.PromptPerMillion+worst.CompletionPerMillion {
			worst = p
		}
	}
	return worst
}

// CostFor prices usage for provider/model. known reports whether the price
// came from the table; when false, cost was computed at the provider's
// highest known rate as a conservative estimate, and the caller must not
// present it as an exact figure the way the old per-provider `default:`
// branches did silently. A provider absent from the table entirely (there is
// none today, but a future provider could be added to the switch in
// host_functions.go before it has a price row) also reports known=false,
// pricing at zero -- there is nothing to be conservative relative to.
func CostFor(provider, model string, usage Usage) (cost float64, known bool) {
	p, known := PriceFor(provider, model)
	if !known {
		p = highestKnownRate(provider)
	}
	cost = float64(usage.PromptTokens)*p.PromptPerMillion/1_000_000 +
		float64(usage.CompletionTokens)*p.CompletionPerMillion/1_000_000
	return cost, known
}
