// Package shard puts a shard number inside UUIDv7 ids so any id routes
// itself (A§9.1). The number lives in the top 4 bits of rand_a, i.e. the low
// nibble of byte 6 just after the version nibble. The 48-bit timestamp is
// untouched, so ids still sort by creation time. Up to 16 shards.
package shard

import (
	"fmt"

	"github.com/google/uuid"
)

// Max is the number of shards the id format can address.
const Max = 16

// NewID mints a UUIDv7 that belongs to shard s.
func NewID(s int) (uuid.UUID, error) {
	if s < 0 || s >= Max {
		return uuid.Nil, fmt.Errorf("shard %d out of range [0, %d)", s, Max)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, err
	}
	id[6] = id[6]&0xF0 | byte(s)
	return id, nil
}

// Like mints a UUIDv7 on the same shard as ref. Transfers and Holds follow
// their source Account, Entries their Account, and a Receivable its debtor.
func Like(ref uuid.UUID) (uuid.UUID, error) {
	return NewID(Of(ref))
}

// Of is the shard number an id carries. Ids minted before sharding carry
// random bits here; Route maps any value onto the configured shards.
func Of(id uuid.UUID) int {
	return int(id[6] & 0x0F)
}

// Route is the index of the shard that stores id when there are n shards.
func Route(id uuid.UUID, n int) int {
	return Of(id) % n
}
