package runtime

import (
	"testing"
	"unsafe"
)

// Every node's inbox is allocated at its full capacity when the node starts, so
// a flow of 12,000 nodes holds 12,000 x 1,024 deliveries before one message
// moves. Growing this struct by a word costs that flow about 100 MB of heap.
func TestADeliveryStaysThreeWords(t *testing.T) {
	if n := unsafe.Sizeof(delivery{}); n > 24 {
		t.Fatalf("a delivery is %d bytes; it must stay at 24 or less because every node's inbox holds 1,024 of them", n)
	}
}
