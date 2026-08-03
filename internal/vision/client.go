package vision

import (
	"context"
)

// Analyzer is the small interface consumed by the preprocessing service.
// Implementations must perform one VLM request for one image reference.
type Analyzer interface {
	Analyze(ctx context.Context, imageReference, focusHint string) (string, error)
}
