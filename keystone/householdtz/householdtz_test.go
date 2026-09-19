package householdtz

import (
	"sync"
	"testing"
	"time"
)

func TestAZoneChangesForEveryoneHoldingIt(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	z := New(nil)
	if z.Location() != time.UTC {
		t.Fatalf("a new zone with nothing = %v, want UTC", z.Location())
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = z.Location().String() }()
	}
	z.Set(chicago)
	wg.Wait()
	if z.Location() != chicago {
		t.Errorf("after Set = %v", z.Location())
	}
	var none *Zone
	if none.Or(chicago) != chicago || none.Location() != nil {
		t.Error("a nil Zone does not fall back")
	}
}
