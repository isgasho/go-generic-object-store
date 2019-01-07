package gos

import (
	"fmt"
	"reflect"
	"sort"
	"sync"
	"syscall"
	"unsafe"
)

// slabPool is a struct that contains and manages multiple slabs of data
// all objects in all the slabs must have the same size
type slabPool struct {
	slabs       []*slab
	objSize     uint8
	objsPerSlab uint
}

// NewSlabPool initializes a new slab pool and returns a pointer to it
func NewSlabPool(objSize uint8, objsPerSlab uint) *slabPool {
	return &slabPool{
		objSize:     objSize,
		objsPerSlab: objsPerSlab,
	}
}

// add adds an object to the pool
// It will try to find a slab that has a free object slot to avoid
// unnecessary allocations. If it can't find a free slot, it will add a
// slab and then use that one
// The first return value is the ObjAddr of the added object
// The second value is the slab address if the call created a new slab
// If no new slab has been created, then the second value is 0
// The third value is nil if there was no error, otherwise it is the error
func (s *slabPool) add(obj []byte) (ObjAddr, SlabAddr, error) {
	var success bool
	var objAddr ObjAddr
	var currentSlab *sla

	// find a slab where the addObj call succeeds
	// on full slabs the returned success value is false
	for _, currentSlab = range s.slabs {
		objAddr, success = currentSlab.addObj(obj)
		if success {
			// the object has been added
			return objAddr, 0, nil
		}
	}

	// the previous loop has not found a slab with free space,
	// so we add a new one
	var err error
	currentSlab, err = s.addSlab()
	if err != nil {
		return 0, 0, err
	}

	// add the object to the new slab
	objAddr, success = currentSlab.addObj(obj)
	if !success {
		return 0, 0, fmt.Errorf("Add: Failed adding object to new slab")
	}

	// a new slab has been created, so its address is returned as
	// the second return value
	return objAddr, currentSlab.addr(), nil
}

// findSlabByObjAddr takes an object address or slab address and then
// finds the slab where this object exists by looking it up from
// its slab list.
// It returns the slab index if the correct slab was found, otherwise
// the return value is the number of known slabs.
// For the lookup to succeed it relies on s.slabs to be sorted in descending order
func (s *slabPool) findSlabByAddr(obj uintptr) int {
	return sort.Search(len(s.slabs), func(i int) bool { return s.slabs[i].addr() <= obj })
}

// addSlab adds another slab to the pool and initalizes the related structs
// on success the first returned value is a pointer to the new slab
// on failure the second returned value is the error message
func (s *slabPool) addSlab() (*slab, error) {
	addedSlab, err := newSlab(s.objSize, s.objsPerSlab)
	if err != nil {
		return nil, err
	}

	newSlabAddr := addedSlab.addr()

	// find the right location to insert the new slab
	// note that s.slabs must remain sorted
	insertAt := sort.Search(len(s.slabs), func(i int) bool { return s.slabs[i].addr() < newSlabAddr })
	s.slabs = append(s.slabs, &slab{})
	copy(s.slabs[insertAt+1:], s.slabs[insertAt:])
	s.slabs[insertAt] = addedSlab

	return addedSlab, nil
}

// search searches for a byte slice with the length of
// this slab's objectSize.
// When found it returns the object address and true,
// otherwise the second returned value is false
func (s *slabPool) search(searching []byte) (ObjAddr, bool) {
	if len(searching) != int(s.objSize) {
		// if the size of the searched object does not match
		// the object size of this slab, then give up
		return 0, false
	}

	for _, currentSlab := range s.slabs {
		objSize := int(s.objSize)

	OBJECT:
		for i := uint(0); i < s.objsPerSlab; i++ {
			if currentSlab.bitSet().Test(i) {
				obj := currentSlab.getObjByIdx(i)
				for j := 0; j < objSize; j++ {
					if obj[j] != searching[j] {
						continue OBJECT
					}
				}
				return ObjAddr(unsafe.Pointer(&obj[0])), true
			}
		}
	}

	return 0, false
}

// searchBatched searches for a batch of search objects.
// It is similar to the search method, but it can do many searches at once.
// The returned value is a slice of ObjAddr which always has the same length
// as the slice of searched objects.
// If a searched object has been found then its address is at the same index
// in the returned slice as it was in the search slice.
// If a searched object has not been found, then the value in the returned
// slice is 0 at the index of the searched object.
func (s *slabPool) searchBatched(searching [][]byte) []ObjAddr {
	wg := sync.WaitGroup{}

	// preallocate the result set that will be returned
	resultSet := make([]ObjAddr, len(searching))

	// create a channel of result structs to push the search results through
	type result struct {
		idx  uint
		addr ObjAddr
	}
	resChan := make(chan result)
	objSize := int(s.objSize)

	wg.Add(len(s.slabs))
	for i := range s.slabs {

		// every slab gets a go routine which searches for all searched objects
		go func(currentSlab *slab) {
			defer wg.Done()

			// iterate over objects in slab
			for j := uint(0); j < s.objsPerSlab; j++ {

				// if the current object slot is in use, then we compare its
				// value to the searched objects
				if currentSlab.bitSet().Test(j) {
					storedObj := currentSlab.getObjByIdx(j)

					// compare all searched objects to the stored object
				SEARCH:
					for k, searchedObj := range searching {
						for l := 0; l < objSize; l++ {
							if storedObj[l] != searchedObj[l] {
								continue SEARCH
							}

						}

						// there was a match between a searched object and a stored object
						// so we push it back through the result channel
						resChan <- result{
							idx:  uint(k),
							addr: objAddrFromObj(storedObj),
						}
					}
				}
			}
		}(s.slabs[i])
	}

	// wait for all search routines to finish, then close the result channel
	go func() {
		wg.Wait()
		close(resChan)
	}()

	// read the result channel and feed the results into the result set
	for res := range resChan {
		resultSet[res.idx] = res.addr
	}

	return resultSet
}

// get returns an object of the given object address as a byte slice
func (s *slabPool) get(obj ObjAddr) []byte {
	return objFromObjAddr(obj, s.objSize)
}

// deleteSlab deletes the slab at the given slab index
// on success it returns nil, otherwise it returns an error
func (s *slabPool) deleteSlab(slabAddr SlabAddr) error {
	slabIdx := s.findSlabByAddr(uintptr(slabAddr))

	currentSlab := s.slabs[slabIdx]

	// delete slab id from slab slice
	copy(s.slabs[slabIdx:], s.slabs[slabIdx+1:])
	s.slabs[len(s.slabs)-1] = &slab{}
	s.slabs = s.slabs[:len(s.slabs)-1]

	totalLen := int(currentSlab.getTotalLength())

	// unmap the slab's memory
	// to do so we need to built a byte slice that refers to the whole
	// slab as its underlying memory area
	var toDelete []byte
	sliceHeader := (*reflect.SliceHeader)(unsafe.Pointer(&toDelete))
	sliceHeader.Data = uintptr(unsafe.Pointer(currentSlab))
	sliceHeader.Len = totalLen
	sliceHeader.Cap = sliceHeader.Len

	err := syscall.Munmap(toDelete)
	if err != nil {
		return err
	}

	return nil
}
