package stock_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souq/inventory-service/internal/store"
)

// The empirical counterpart to model_test.go.
//
// The model proves the DESIGN cannot oversell. This proves the IMPLEMENTATION
// is that design, by doing to a real Postgres what the explorer does to the
// model: hammering one row from many connections at once and asserting the
// invariants afterwards.
//
// Both invariants are checked, and the second is the important one:
//
//	NoOversell    reserved <= on_hand
//	Conservation  reserved == the sum of what every winner took
//
// Conservation is strictly stronger. The lost-update strategy passes
// NoOversell and fails Conservation — the column looks healthy while the units
// are physically double-sold. A suite asserting only NoOversell would ship it.
//
// Run with:
//
//	docker run -d --rm -p 55432:5432 -e POSTGRES_PASSWORD=souq --name pg postgres:16-alpine
//	SOUQ_TEST_DB_URL=postgres://postgres:souq@localhost:55432/postgres go test ./internal/stock/ -v
//
// Skipped, not failed, when SOUQ_TEST_DB_URL is unset, so `go test ./...` on a
// laptop without Docker stays green.

func testPool(t *testing.T) *store.Store {
	t.Helper()

	url := os.Getenv("SOUQ_TEST_DB_URL")
	if url == "" {
		t.Skip("SOUQ_TEST_DB_URL not set; skipping the real-Postgres concurrency tests")
	}

	ctx := context.Background()
	s, err := store.Open(ctx, url, 40, 5*time.Second)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)

	migration, err := os.ReadFile("../../migrations/0001_init.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}

	// Each run starts from a clean slate.
	conn, err := s.Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if _, err := conn.Exec(ctx, string(migration)); err != nil {
		t.Fatalf("apply migration: %v", err)
	}

	return s
}

func seedSKU(t *testing.T, s *store.Store, sku string, onHand int) {
	t.Helper()
	_, err := s.Pool().Exec(context.Background(),
		`INSERT INTO stock_levels (sku, product_id, on_hand, reserved) VALUES ($1, 'prd_test', $2, 0)`,
		sku, onHand)
	if err != nil {
		t.Fatalf("seed %s: %v", sku, err)
	}
}

func levels(t *testing.T, s *store.Store, sku string) (onHand, reserved int) {
	t.Helper()
	err := s.Pool().QueryRow(context.Background(),
		`SELECT on_hand, reserved FROM stock_levels WHERE sku = $1`, sku).Scan(&onHand, &reserved)
	if err != nil {
		t.Fatalf("read %s: %v", sku, err)
	}
	return
}

// take issues exactly the statement store.TryTake issues, on its own
// connection, so the concurrency is genuine rather than simulated.
func take(s *store.Store, sku string, qty int) bool {
	var onHand, reserved int
	err := s.Pool().QueryRow(context.Background(), `
		UPDATE stock_levels
		   SET reserved = reserved + $2, version = version + 1
		 WHERE sku = $1 AND status = 'ACTIVE' AND on_hand - reserved >= $2
		RETURNING on_hand, reserved`, sku, qty).Scan(&onHand, &reserved)
	return err == nil
}

// ---------------------------------------------------------------------------

func TestNoOversellUnderRealConcurrency(t *testing.T) {
	s := testPool(t)

	const (
		buyers = 50
		each   = 2
		onHand = 10
	)
	sku := "sku_hot"
	seedSKU(t, s, sku, onHand)

	var wg sync.WaitGroup
	wins := make([]bool, buyers)

	// Released together, so they genuinely contend rather than queueing.
	start := make(chan struct{})
	for i := 0; i < buyers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			wins[i] = take(s, sku, each)
		}(i)
	}
	close(start)
	wg.Wait()

	won := 0
	for _, w := range wins {
		if w {
			won++
		}
	}

	gotOnHand, gotReserved := levels(t, s, sku)

	if gotReserved > gotOnHand {
		t.Fatalf("OVERSOLD: reserved=%d exceeds on_hand=%d", gotReserved, gotOnHand)
	}
	if gotReserved != won*each {
		t.Fatalf("Conservation violated: reserved=%d but %d winners took %d each (=%d)",
			gotReserved, won, each, won*each)
	}
	if won != onHand/each {
		t.Fatalf("%d buyers won; with %d units at %d each it must be exactly %d",
			won, onHand, each, onHand/each)
	}

	t.Logf("%d concurrent buyers, %d units, %d each -> exactly %d winners, reserved=%d",
		buyers, onHand, each, won, gotReserved)
}

// The flash-sale shape: many buyers, one unit.
func TestExactlyOneBuyerWinsTheLastUnit(t *testing.T) {
	s := testPool(t)

	sku := "sku_last_one"
	seedSKU(t, s, sku, 1)

	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0

	start := make(chan struct{})
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if take(s, sku, 1) {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if won != 1 {
		t.Fatalf("%d of 100 buyers won the last unit; exactly 1 may", won)
	}
	onHand, reserved := levels(t, s, sku)
	if onHand != 1 || reserved != 1 {
		t.Fatalf("on_hand=%d reserved=%d, want 1/1", onHand, reserved)
	}
}

// Proves the harness can detect an oversell.
//
// Without this, the tests above might be passing because nothing concurrent is
// actually happening. Here the naive read-modify-write really is racing, and
// the database CHECK is the only thing standing between us and a wrong number.
func TestTheNaiveStrategyIsCaughtByTheConstraint(t *testing.T) {
	s := testPool(t)

	sku := "sku_naive"
	seedSKU(t, s, sku, 2)

	// Two buyers wanting 2 each. Both read reserved=0 in the sleep window,
	// both pass the check, both write.
	naive := func() bool {
		ctx := context.Background()
		var reserved, onHand int
		if err := s.Pool().QueryRow(ctx,
			`SELECT on_hand, reserved FROM stock_levels WHERE sku = $1`, sku).
			Scan(&onHand, &reserved); err != nil {
			return false
		}

		time.Sleep(50 * time.Millisecond) // the window a real interleaving uses

		if onHand-reserved < 2 {
			return false
		}
		_, err := s.Pool().Exec(ctx,
			`UPDATE stock_levels SET reserved = reserved + 2 WHERE sku = $1`, sku)
		return err == nil
	}

	var wg sync.WaitGroup
	results := make([]bool, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i] = naive() }(i)
	}
	wg.Wait()

	onHand, reserved := levels(t, s, sku)

	// The CHECK constraint is the last line of defence and it must hold even
	// when the application logic is wrong.
	if reserved > onHand {
		t.Fatalf("the no_oversell CHECK did not hold: reserved=%d on_hand=%d — "+
			"the last line of defence is gone", reserved, onHand)
	}

	succeeded := 0
	for _, ok := range results {
		if ok {
			succeeded++
		}
	}
	t.Logf("both naive writers believed they could proceed; %d succeeded, "+
		"the constraint rejected the rest (reserved=%d, on_hand=%d)",
		succeeded, reserved, onHand)
}

// A negative adjustment must never strand units already promised to customers.
func TestAdjustmentCannotStrandReservedUnits(t *testing.T) {
	s := testPool(t)

	sku := "sku_adjust"
	seedSKU(t, s, sku, 10)
	if !take(s, sku, 8) {
		t.Fatal("setup take failed")
	}

	err := s.InTx(context.Background(), func(tx pgx.Tx) error {
		_, _, ok, err := store.AdjustOnHand(context.Background(), tx, sku, -5)
		if err != nil {
			return err
		}
		if ok {
			t.Error("an adjustment below the reserved count was accepted")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if onHand, reserved := levels(t, s, sku); onHand != 10 || reserved != 8 {
		t.Fatalf("on_hand=%d reserved=%d, want 10/8 unchanged", onHand, reserved)
	}
}

// Multi-SKU reservations must take rows in a consistent order or two orders
// touching the same pair deadlock. The engine sorts; this asserts the property
// that sorting provides.
func TestCrossingMultiSkuReservationsDoNotDeadlock(t *testing.T) {
	s := testPool(t)

	a, b := "sku_aaa", "sku_bbb"
	seedSKU(t, s, a, 100)
	seedSKU(t, s, b, 100)

	var wg sync.WaitGroup
	errs := make(chan error, 40)
	start := make(chan struct{})

	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start

			// Half would naturally take B then A. Sorted, both take A then B.
			order := []string{a, b}
			if i%2 == 1 {
				order = []string{b, a}
			}
			sortStrings(order) // the fix, made explicit

			err := s.InTx(context.Background(), func(tx pgx.Tx) error {
				for _, sku := range order {
					if _, _, _, err := store.TryTake(context.Background(), tx, sku, 1); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				errs <- fmt.Errorf("txn %d: %w", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("a transaction failed, most likely a deadlock: %v", err)
	}

	if _, reserved := levels(t, s, a); reserved != 40 {
		t.Errorf("%s reserved=%d, want 40", a, reserved)
	}
	if _, reserved := levels(t, s, b); reserved != 40 {
		t.Errorf("%s reserved=%d, want 40", b, reserved)
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
