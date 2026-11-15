package nodes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// ErrGateRefused marks a write HotLoop's gate said no to: writes are switched
// off, the tag is not armed, or the value is out of range. The error text is the
// gate's own reason.
var ErrGateRefused = errors.New("HotLoop's gate refused the write")

// GatedWrite is the way code that is not a message node, a FlowLab diagram,
// writes to equipment: through a running hotloop-flow-hotloop config node and
// HotLoop's write gate, as the tag device/tag, with a reason HotLoop records
// beside the write. It never writes to the equipment itself, and when HotLoop
// refuses or cannot be reached the write does not happen.
func GatedWrite(ctx context.Context, cfg node.Node, tag string, value any, reason string) error {
	t, ok := cfg.(hotloopTarget)
	if !ok {
		return fmt.Errorf("the config node is not a HotLoop connection")
	}
	if !strings.Contains(tag, "/") || strings.HasPrefix(tag, "/") || strings.HasSuffix(tag, "/") {
		return fmt.Errorf("HotLoop tag %q has to be written device/tag, like plc-1/Setpoint", tag)
	}
	v, err := hotloopScalar(value)
	if err != nil {
		return err
	}
	status, body, err := t.call(ctx, http.MethodPost, "/api/write", map[string]any{"tag": tag, "value": v, "reason": reason})
	if err != nil {
		return fmt.Errorf("writing %s through HotLoop: %w", tag, err)
	}
	if status == http.StatusOK {
		return nil
	}
	if status == http.StatusForbidden || status == http.StatusUnprocessableEntity {
		return fmt.Errorf("%w to %s: %s (HTTP %d)", ErrGateRefused, tag, hotloopErrorText(body), status)
	}
	return fmt.Errorf("HotLoop's gate failed the write to %s: %s (HTTP %d)", tag, hotloopErrorText(body), status)
}
