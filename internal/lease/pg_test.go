package lease

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

func pgStore(t *testing.T) *Postgres {
	t.Helper()
	addr := os.Getenv("HOTLOOP_FLOW_TEST_PG")
	if addr == "" {
		t.Skip("set HOTLOOP_FLOW_TEST_PG=host:port to run this against a real PostgreSQL (user flow, password flowtest, database flowtest)")
	}
	p, err := OpenPostgres(context.Background(), "postgres://flow:flowtest@"+addr+"/flowtest?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// The row is the lease: one holder at a time, the number up when the holder
// changes, a holder that keeps asking keeps it, and a release hands it on.
func TestThePostgresLeaseIsOneHolderAtATime(t *testing.T) {
	p := pgStore(t)
	ctx := context.Background()
	name := "t-" + strconv.FormatInt(time.Now().UnixNano(), 36)

	a, err := p.Acquire(ctx, name, "a", time.Second)
	if err != nil || !a.Held || a.Epoch != 1 {
		t.Fatalf("first: %+v %v", a, err)
	}
	b, err := p.Acquire(ctx, name, "b", time.Second)
	if err != nil || b.Held || b.Holder != "a" || b.Epoch != 1 {
		t.Fatalf("a second holder while the first has it: %+v %v", b, err)
	}
	again, err := p.Acquire(ctx, name, "a", time.Second)
	if err != nil || !again.Held || again.Epoch != 1 {
		t.Fatalf("the holder renewing: %+v %v", again, err)
	}
	time.Sleep(1200 * time.Millisecond)
	b, err = p.Acquire(ctx, name, "b", time.Second)
	if err != nil || !b.Held || b.Epoch != 2 {
		t.Fatalf("after it ran out: %+v %v", b, err)
	}
	if a, _ := p.Acquire(ctx, name, "a", time.Second); a.Held {
		t.Fatal("the old holder took it back from the live one")
	}
	if err := p.Release(ctx, name, "a"); err != nil { // not the holder: does nothing
		t.Fatal(err)
	}
	if a, _ := p.Acquire(ctx, name, "a", time.Second); a.Held {
		t.Fatal("a release by a holder that does not hold it gave the lease away")
	}
	if err := p.Release(ctx, name, "b"); err != nil {
		t.Fatal(err)
	}
	a, err = p.Acquire(ctx, name, "a", time.Second)
	if err != nil || !a.Held || a.Epoch != 3 {
		t.Fatalf("after a release: %+v %v", a, err)
	}
}

// Many ask at the same instant for a lease nobody has: exactly one gets it.
func TestOnlyOneOfManyGetsTheLease(t *testing.T) {
	p := pgStore(t)
	name := "race-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			g, err := p.Acquire(context.Background(), name, fmt.Sprintf("h%d", i), 5*time.Second)
			if err != nil {
				t.Error(err)
				return
			}
			if g.Held {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d holders got the lease, want exactly 1", won)
	}
}
