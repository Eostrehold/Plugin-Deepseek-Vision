package downstream

// Protocol identifies a supported downstream request protocol.
type Protocol string

const (
	ProtocolResponses Protocol = "responses"
	ProtocolChat      Protocol = "chat"
	ProtocolClaude    Protocol = "claude"
)

// Plan is an immutable, protocol-specific image discovery result.
type Plan interface {
	Protocol() Protocol
	InputItemCount() int
	ImageCountDetails() ImageCountDetails
	Images() []Image
	Groups() []PromptGroup
	HasImages() bool
	RewriteGroupsText([]string) ([]byte, error)
}

// Adapter discovers and rewrites one exact downstream wire protocol.
type Adapter interface {
	Protocol() Protocol
	Discover(body []byte, options ...Options) (Plan, error)
}

type route struct {
	sourceFormat string
	path         string
}

type responsesAdapter struct{}

func (responsesAdapter) Protocol() Protocol { return ProtocolResponses }

func (responsesAdapter) Discover(body []byte, options ...Options) (Plan, error) {
	return Discover(body, options...)
}

var adapters = map[route]Adapter{
	{sourceFormat: "openai-response", path: "/v1/responses"}: responsesAdapter{},
	{sourceFormat: "openai", path: "/v1/chat/completions"}:   chatAdapter{},
}

// Match returns the adapter for an exact host source-format and request-path
// pair. Near matches intentionally remain outside the plugin boundary.
func Match(sourceFormat, path string) (Adapter, bool) {
	adapter, ok := adapters[route{sourceFormat: sourceFormat, path: path}]
	return adapter, ok
}
