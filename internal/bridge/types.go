package bridge

type ProxyState struct {
	Running       bool   `json:"running"`
	ListenAddr    string `json:"listenAddr"`
	BaseURL       string `json:"baseURL"`
	StartedAt     int64  `json:"startedAt"`
	CAFingerprint string `json:"caFingerprint"`
	CAPath        string `json:"caPath"`
	CAInstalled   bool   `json:"caInstalled"`
	CAInstallMode string `json:"caInstallMode,omitempty"`
	CAWarning     string `json:"caWarning,omitempty"`
	LastError     string `json:"lastError,omitempty"`
}

type ModelAdapterConfig struct {
	DisplayName     string `json:"displayName"`
	Type            string `json:"type"`
	BaseURL         string `json:"baseURL"`
	APIKey          string `json:"apiKey"`
	ModelID         string `json:"modelID"`
	ContextWindow   string `json:"contextWindow,omitempty"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	ServiceTier     string `json:"serviceTier,omitempty"`
	MaxOutputTokens string `json:"maxOutputTokens,omitempty"`
	ThinkingBudget  string `json:"thinkingBudget,omitempty"`
	Notes           string `json:"notes,omitempty"`
	LastTestResult  string `json:"lastTestResult,omitempty"`
	LastTestedAt    int64  `json:"lastTestedAt,omitempty"`
}

type UserConfig struct {
	BaseURL       string               `json:"baseURL"`
	ModelAdapters []ModelAdapterConfig `json:"modelAdapters"`
	// CloseAction remembers what the user picked in the "Quit or minimize
	// to tray?" dialog. Empty = never answered, show the dialog on close.
	// "quit" = shut the proxy down and exit. "tray" = hide to system tray.
	CloseAction string `json:"closeAction,omitempty"`
}

// UsageStats is the aggregate token-use snapshot the Stats tab in the Wails
// frontend displays. It's computed on demand from the on-disk history root
// (%APPDATA%/CursorForge/history/<conversationID>/turns/*/summary.json),
// so the values always reflect the complete recorded conversation corpus —
// not just the live in-memory session window.
type UsageStats struct {
	TotalPromptTokens     int64             `json:"totalPromptTokens"`
	TotalCompletionTokens int64             `json:"totalCompletionTokens"`
	TotalTokens           int64             `json:"totalTokens"`
	ConversationCount     int               `json:"conversationCount"`
	TurnCount             int               `json:"turnCount"`
	PerModel              []ModelUsageEntry `json:"perModel"`
	Last7Days             []DailyUsageEntry `json:"last7Days"`
}

type ModelUsageEntry struct {
	Model            string `json:"model"`
	Provider         string `json:"provider"`
	PromptTokens     int64  `json:"promptTokens"`
	CompletionTokens int64  `json:"completionTokens"`
	TurnCount        int    `json:"turnCount"`
}

type DailyUsageEntry struct {
	Date             string `json:"date"` // YYYY-MM-DD in local time
	PromptTokens     int64  `json:"promptTokens"`
	CompletionTokens int64  `json:"completionTokens"`
}

type CursorSettingsStatus struct {
	Path            string `json:"path"`
	Found           bool   `json:"found"`
	Error           string `json:"error,omitempty"`
	ProxySet        bool   `json:"proxySet"`
	ProxyValue      string `json:"proxyValue,omitempty"`
	StrictSSLOff    bool   `json:"strictSSLOff"`
	ProxySupportOn  bool   `json:"proxySupportOn"`
	SystemCertsV2On bool   `json:"systemCertsV2On"`
	UseHTTP1        bool   `json:"useHttp1"`
	DisableHTTP2    bool   `json:"disableHttp2"`
	ProxyKerberos   bool   `json:"proxyKerberos"`
}

