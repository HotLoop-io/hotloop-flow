package nodes

import (
	"fmt"

	"github.com/HotLoop-io/hotloop-flow/internal/fence"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// fenceWrite is the question every node that commands equipment asks just before
// it sends: may this instance write right now? With failover off it always may. On
// a standby, or on an active instance whose lease is about to run out, it may not,
// and the node reports that as its error, to a Catch, like any refused write. A
// flow test stands in for the outside world and writes nothing, so it is never
// held back.
func fenceWrite(svc node.Services, op string) error {
	if node.StandInOf(svc) != nil {
		return nil
	}
	if err := fence.Check(op); err != nil {
		return fmt.Errorf("not written, %s: %w", op, err)
	}
	return nil
}
