package realtimevoice

const (
	ProviderQwen   = "qwen"
	ProviderSpeko  = "speko"
	ProviderOpenAI = "openai"
	ProviderGemini = "gemini"
	ProviderXAI    = "xai"
	ProviderMoss   = "moss"
)

type ProviderDescriptor struct {
	Name             string
	Label            string
	ModelPlaceholder string
	VoicePlaceholder string
}

var providerDescriptors = []ProviderDescriptor{
	{Name: ProviderQwen, Label: "Qwen", ModelPlaceholder: DefaultQwenRealtimeModel, VoicePlaceholder: DefaultQwenRealtimeVoice},
	{Name: ProviderSpeko, Label: "Speko S2S", VoicePlaceholder: "auto"},
	{Name: ProviderOpenAI, Label: "OpenAI Realtime", ModelPlaceholder: DefaultOpenAIRealtimeModel, VoicePlaceholder: "alloy"},
	{Name: ProviderGemini, Label: "Google Gemini Live", ModelPlaceholder: DefaultGeminiLiveModel, VoicePlaceholder: "Puck"},
	{Name: ProviderXAI, Label: "xAI Grok Voice", ModelPlaceholder: DefaultXAIRealtimeModel, VoicePlaceholder: "eve"},
	{Name: ProviderMoss, Label: "MOSS Live", ModelPlaceholder: DefaultMossLiveModel},
}

func ProviderDescriptors() []ProviderDescriptor {
	return append([]ProviderDescriptor(nil), providerDescriptors...)
}

func LookupProviderDescriptor(name string) (ProviderDescriptor, bool) {
	name = normalizeProviderName(name)
	for _, descriptor := range providerDescriptors {
		if descriptor.Name == name {
			return descriptor, true
		}
	}
	return ProviderDescriptor{}, false
}

func IsProvider(name string) bool {
	_, ok := LookupProviderDescriptor(name)
	return ok
}
