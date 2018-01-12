package dht

import (
	"sort"
	"sync"
	"time"

	"github.com/republicprotocol/go-identity"
)

// Constants for use in the DHT.
const (
	IDLengthInBits = identity.IDLength * 8
	MaxBucketSize  = 100
	MaxDHTSize     = IDLengthInBits * MaxBucketSize
)

// A DHT is a Distributed Hash Table. Each instance has an identity.Address and
// several Buckets of identity.MultiAddresses that are directly connected to
// that identity.Address. It uses a modified Kademlia approach to storing
// identity.MultiAddresses in each Bucket, favoring old connections over new
// connections. It is safe to use concurrently.
type DHT struct {
	μ       *sync.RWMutex
	Address identity.Address
	Buckets [IDLengthInBits]Bucket
}

// NewDHT returns a new DHT with the given Address, and empty Buckets.
func NewDHT(address identity.Address) *DHT {
	return &DHT{
		μ:       new(sync.RWMutex),
		Address: address,
		Buckets: [IDLengthInBits]Bucket{},
	}
}

// Update an identity.MultiAddress by adding it to its respective Bucket.
// Returns an error if the Bucket is full, or any error that happens while
// finding the required Bucket.
func (dht *DHT) Update(multi identity.MultiAddress) error {
	dht.μ.Lock()
	defer dht.μ.Unlock()
	return dht.update(multi)
}

// Remove an identity.MultiAddress by removing it from its respective Bucket.
// Nothing happens if the identity.MultiAddress is not in the DHT. Returns any
// error that happens while finding the required Bucket.
func (dht *DHT) Remove(multi identity.MultiAddress) error {
	dht.μ.Lock()
	defer dht.μ.Unlock()
	return dht.remove(multi)
}

// FindMultiAddress finds the identity.MultiAddress associated with the target
// identity.Address. Returns nil if the target is not in the DHT, or an error.
func (dht *DHT) FindMultiAddress(target identity.Address) (*identity.MultiAddress, error) {
	dht.μ.RLock()
	defer dht.μ.RUnlock()
	return dht.findMultiAddress(target)
}

// FindBucket uses the target identity.Address and returns the respective
// Bucket. The target does not have to be in the DHT. Returns the Bucket, or an
// error.
func (dht *DHT) FindBucket(target identity.Address) (*Bucket, error) {
	dht.μ.RLock()
	defer dht.μ.RUnlock()
	return dht.findBucket(target)
}

// FindNeighborhoodBuckets uses the target identity.Address to find Buckets
// within a given neighborhood of the target Bucket. The target does not have
// to be in the DHT. Returns the Buckets, or an error.
func (dht *DHT) FindNeighborhoodBuckets(target identity.Address, neighborhood uint) (Buckets, error) {
	dht.μ.RLock()
	defer dht.μ.RUnlock()
	return dht.findNeighborhoodBuckets(target, neighborhood)
}

// Neighborhood returns the start and end indices of a neighborhood around the
// Bucket associated with the target identity.Address.
func (dht *DHT) Neighborhood(target identity.Address, neighborhood uint) (int, int, error) {
	dht.μ.RLock()
	defer dht.μ.RUnlock()
	return dht.neighborhood(target, neighborhood)
}

// MultiAddresses returns all identity.MultiAddresses in all Buckets.
func (dht *DHT) MultiAddresses() identity.MultiAddresses {
	dht.μ.RLock()
	defer dht.μ.RUnlock()
	return dht.multiAddresses()
}

func (dht *DHT) update(multi identity.MultiAddress) error {
	target, err := multi.Address()
	if err != nil {
		return err
	}
	bucket, err := dht.findBucket(target)
	if err != nil {
		return err
	}

	// Remove the target if it is already in the Bucket.
	exists := bucket.FindMultiAddress(target)
	if exists != nil {
		for i, entry := range *bucket {
			address, err := entry.MultiAddress.Address()
			if err != nil {
				return err
			}
			if address == target {
				// We do not update the time otherwise the sorting method does
				// not make sense.
				(*bucket)[i].MultiAddress = multi
				return nil
			}
		}
	}

	if bucket.IsFull() {
		return ErrFullBucket
	}
	*bucket = append(*bucket, Entry{multi, time.Now()})
	return nil
}

func (dht *DHT) remove(multi identity.MultiAddress) error {
	target, err := multi.Address()
	if err != nil {
		return err
	}
	bucket, err := dht.findBucket(target)
	if err != nil {
		return err
	}
	removeIndex := -1
	for i, entry := range *bucket {
		address, err := entry.MultiAddress.Address()
		if err != nil {
			return err
		}
		if address == target {
			removeIndex = i
			break
		}
	}
	if removeIndex >= 0 {
		if removeIndex == len(*bucket)-1 {
			*bucket = (*bucket)[:removeIndex]
		} else {
			*bucket = append((*bucket)[:removeIndex], (*bucket)[removeIndex+1:]...)
		}
	}
	return nil
}

func (dht *DHT) findMultiAddress(target identity.Address) (*identity.MultiAddress, error) {
	bucket, err := dht.findBucket(target)
	if err != nil {
		return nil, err
	}
	return bucket.FindMultiAddress(target), nil
}

func (dht *DHT) findBucket(target identity.Address) (*Bucket, error) {
	same, err := dht.Address.SamePrefixLength(target)
	if err != nil {
		return nil, err
	}
	if same == IDLengthInBits {
		return nil, ErrDHTAddress
	}
	index := len(dht.Buckets) - same - 1
	if index < 0 || index > len(dht.Buckets)-1 {
		panic("runtime error: index out of range")
	}
	return &dht.Buckets[index], nil
}

func (dht *DHT) findNeighborhoodBuckets(target identity.Address, neighborhood uint) (Buckets, error) {
	// Find the index range of the neighborhood.
	start, end, err := dht.neighborhood(target, neighborhood)
	if err != nil {
		return nil, err
	}
	return dht.Buckets[start:end], nil
}

func (dht *DHT) neighborhood(target identity.Address, neighborhood uint) (int, int, error) {
	// Find the index range of the neighborhood.
	same, err := dht.Address.SamePrefixLength(target)
	if err != nil {
		return -1, -1, err
	}
	if same == IDLengthInBits {
		return -1, -1, ErrDHTAddress
	}
	index := len(dht.Buckets) - same - 1
	if index < 0 || index > len(dht.Buckets)-1 {
		panic("runtime error: index out of range")
	}
	start := index - int(neighborhood)
	if start < 0 {
		start = 0
	}
	end := index + int(neighborhood)
	if end > len(dht.Buckets) {
		end = len(dht.Buckets)
	}
	return start, end, nil
}

func (dht *DHT) multiAddresses() identity.MultiAddresses {
	numMultis := 0
	for _, bucket := range dht.Buckets {
		numMultis += len(bucket)
	}
	i := 0
	multis := make(identity.MultiAddresses, numMultis)
	for _, bucket := range dht.Buckets {
		for _, entry := range bucket {
			multis[i] = entry.MultiAddress
			i++
		}
	}
	return multis
}

// Bucket is a mapping of Addresses to Entries. In standard Kademlia, a list is
// used because Buckets need to be sorted.
type Bucket []Entry

// FindMultiAddress finds the identity.MultiAddress associated with a target
// identity.Address in the Bucket. Returns nil if the target identity.Address
// cannot be found.
func (bucket Bucket) FindMultiAddress(target identity.Address) *identity.MultiAddress {
	for _, entry := range bucket {
		address, err := entry.MultiAddress.Address()
		if err == nil && address == target {
			return &entry.MultiAddress
		}
	}
	return nil
}

// MultiAddresses returns all MultiAddresses in the Bucket.
func (bucket Bucket) MultiAddresses() identity.MultiAddresses {
	multis := make(identity.MultiAddresses, len(bucket))
	for i, entry := range bucket {
		multis[i] = entry.MultiAddress
	}
	return multis
}

// Sort the Bucket by the time at which Entries were added.
func (bucket Bucket) Sort() {
	sort.Slice(bucket, func(i, j int) bool {
		return bucket[i].Time.Before(bucket[j].Time)
	})
}

// NewestMultiAddress returns the most recently added identity.MultiAddress in
// the Bucket. Returns nil if there are no Entries in the Bucket.
func (bucket Bucket) NewestMultiAddress() *identity.MultiAddress {
	if len(bucket) == 0 {
		return nil
	}
	return &bucket[len(bucket)-1].MultiAddress
}

// OldestMultiAddress returns the least recently added identity.MultiAddress in
// the Bucket. Returns nil if there are no Entries in the Bucket.
func (bucket Bucket) OldestMultiAddress() *identity.MultiAddress {
	if len(bucket) == 0 {
		return nil
	}
	return &bucket[0].MultiAddress
}

// IsFull returns true if, and only if, the number of Entries in the Bucket is
// equal to the maximum number of Entries allowed.
func (bucket Bucket) IsFull() bool {
	return len(bucket) == MaxBucketSize
}

// Buckets is an alias.
type Buckets []Bucket

// MultiAddresses returns all MultiAddresses from all Buckets.
func (buckets Buckets) MultiAddresses() identity.MultiAddresses {
	numMultis := 0
	for _, bucket := range buckets {
		numMultis += len(bucket)
	}
	i := 0
	multis := make(identity.MultiAddresses, numMultis)
	for _, bucket := range buckets {
		for _, entry := range bucket {
			multis[i] = entry.MultiAddress
			i++
		}
	}
	return multis
}

// An Entry in a Bucket. It holds an identity.MultiAddress, and a timestamp for
// when it was added to the Bucket.
type Entry struct {
	identity.MultiAddress
	time.Time
}