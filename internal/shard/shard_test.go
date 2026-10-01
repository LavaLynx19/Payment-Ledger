package shard

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNewIDCarriesShard(t *testing.T) {
	for s := range Max {
		id, err := NewID(s)
		if err != nil {
			t.Fatal(err)
		}
		if Of(id) != s {
			t.Errorf("Of(NewID(%d)) = %d", s, Of(id))
		}
		if id.Version() != 7 || id.Variant() != uuid.RFC4122 {
			t.Errorf("NewID(%d) = %s: version %d variant %v, want a valid v7", s, id, id.Version(), id.Variant())
		}
		like, err := Like(id)
		if err != nil {
			t.Fatal(err)
		}
		if Of(like) != s {
			t.Errorf("Like keeps shard %d, got %d", s, Of(like))
		}
	}
	if _, err := NewID(Max); err == nil {
		t.Error("NewID(Max) should fail")
	}
}

func TestRoute(t *testing.T) {
	id, _ := NewID(5)
	for n, want := range map[int]int{1: 0, 2: 1, 4: 1, 16: 5} {
		if got := Route(id, n); got != want {
			t.Errorf("Route(shard 5, n=%d) = %d, want %d", n, got, want)
		}
	}
}

// Shard bits sit below the timestamp, so ids still sort by creation time.
func TestTimeOrderPreserved(t *testing.T) {
	early, _ := NewID(15)
	time.Sleep(2 * time.Millisecond)
	late, _ := NewID(0)
	if bytes.Compare(early[:], late[:]) >= 0 {
		t.Errorf("earlier id %s does not sort before later id %s", early, late)
	}
}
