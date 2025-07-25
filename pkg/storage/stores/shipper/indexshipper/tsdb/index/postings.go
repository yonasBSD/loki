// Copyright 2017 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package index

import (
	"container/heap"
	"encoding/binary"
	"runtime"
	"sort"
	"sync"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
)

var allPostingsKey = labels.Label{}

// AllPostingsKey returns the label key that is used to store the postings list of all existing IDs.
func AllPostingsKey() (name, value string) {
	return allPostingsKey.Name, allPostingsKey.Value
}

// ensureOrderBatchSize is the max number of postings passed to a worker in a single batch in MemPostings.EnsureOrder().
const ensureOrderBatchSize = 1024

// ensureOrderBatchPool is a pool used to recycle batches passed to workers in MemPostings.EnsureOrder().
var ensureOrderBatchPool = sync.Pool{
	New: func() interface{} {
		return make([][]storage.SeriesRef, 0, ensureOrderBatchSize)
	},
}

// MemPostings holds postings list for series ID per label pair. They may be written
// to out of order.
// EnsureOrder() must be called once before any reads are done. This allows for quick
// unordered batch fills on startup.
type MemPostings struct {
	mtx     sync.RWMutex
	m       map[string]map[string][]storage.SeriesRef
	ordered bool
}

// NewMemPostings returns a memPostings that's ready for reads and writes.
func NewMemPostings() *MemPostings {
	return &MemPostings{
		m:       make(map[string]map[string][]storage.SeriesRef, 512),
		ordered: true,
	}
}

// NewUnorderedMemPostings returns a memPostings that is not safe to be read from
// until EnsureOrder() was called once.
func NewUnorderedMemPostings() *MemPostings {
	return &MemPostings{
		m:       make(map[string]map[string][]storage.SeriesRef, 512),
		ordered: false,
	}
}

// Symbols returns an iterator over all unique name and value strings, in order.
func (p *MemPostings) Symbols() StringIter {
	p.mtx.RLock()

	// Add all the strings to a map to de-duplicate.
	symbols := make(map[string]struct{}, 512)
	for n, e := range p.m {
		symbols[n] = struct{}{}
		for v := range e {
			symbols[v] = struct{}{}
		}
	}
	p.mtx.RUnlock()

	res := make([]string, 0, len(symbols))
	for k := range symbols {
		res = append(res, k)
	}

	sort.Strings(res)
	return NewStringListIter(res)
}

// SortedKeys returns a list of sorted label keys of the postings.
func (p *MemPostings) SortedKeys() []labels.Label {
	p.mtx.RLock()
	keys := make([]labels.Label, 0, len(p.m))

	for n, e := range p.m {
		for v := range e {
			keys = append(keys, labels.Label{Name: n, Value: v})
		}
	}
	p.mtx.RUnlock()

	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Name != keys[j].Name {
			return keys[i].Name < keys[j].Name
		}
		return keys[i].Value < keys[j].Value
	})
	return keys
}

// LabelNames returns all the unique label names.
func (p *MemPostings) LabelNames() []string {
	p.mtx.RLock()
	defer p.mtx.RUnlock()
	n := len(p.m)
	if n == 0 {
		return nil
	}

	names := make([]string, 0, n-1)
	for name := range p.m {
		if name != allPostingsKey.Name {
			names = append(names, name)
		}
	}
	return names
}

// LabelValues returns label values for the given name.
func (p *MemPostings) LabelValues(name string) []string {
	p.mtx.RLock()
	defer p.mtx.RUnlock()

	values := make([]string, 0, len(p.m[name]))
	for v := range p.m[name] {
		values = append(values, v)
	}
	return values
}

// PostingsStats contains cardinality based statistics for postings.
type PostingsStats struct {
	CardinalityMetricsStats []Stat
	CardinalityLabelStats   []Stat
	LabelValueStats         []Stat
	LabelValuePairsStats    []Stat
	NumLabelPairs           int
}

// Stats calculates the cardinality statistics from postings.
func (p *MemPostings) Stats(label string) *PostingsStats {
	const maxNumOfRecords = 10
	var size uint64

	p.mtx.RLock()

	metrics := &maxHeap{}
	labels := &maxHeap{}
	labelValueLength := &maxHeap{}
	labelValuePairs := &maxHeap{}
	numLabelPairs := 0

	metrics.init(maxNumOfRecords)
	labels.init(maxNumOfRecords)
	labelValueLength.init(maxNumOfRecords)
	labelValuePairs.init(maxNumOfRecords)

	for n, e := range p.m {
		if n == "" {
			continue
		}
		labels.push(Stat{Name: n, Count: uint64(len(e))})
		numLabelPairs += len(e)
		size = 0
		for name, values := range e {
			if n == label {
				metrics.push(Stat{Name: name, Count: uint64(len(values))})
			}
			labelValuePairs.push(Stat{Name: n + "=" + name, Count: uint64(len(values))})
			size += uint64(len(name))
		}
		labelValueLength.push(Stat{Name: n, Count: size})
	}

	p.mtx.RUnlock()

	return &PostingsStats{
		CardinalityMetricsStats: metrics.get(),
		CardinalityLabelStats:   labels.get(),
		LabelValueStats:         labelValueLength.get(),
		LabelValuePairsStats:    labelValuePairs.get(),
		NumLabelPairs:           numLabelPairs,
	}
}

// Get returns a postings list for the given label pair.
func (p *MemPostings) Get(name, value string) Postings {
	var lp []storage.SeriesRef
	p.mtx.RLock()
	l := p.m[name]
	if l != nil {
		lp = l[value]
	}
	p.mtx.RUnlock()

	if lp == nil {
		return EmptyPostings()
	}
	return newListPostings(lp...)
}

// All returns a postings list over all documents ever added.
func (p *MemPostings) All() Postings {
	return p.Get(AllPostingsKey())
}

// EnsureOrder ensures that all postings lists are sorted. After it returns all further
// calls to add and addFor will insert new IDs in a sorted manner.
func (p *MemPostings) EnsureOrder() {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	if p.ordered {
		return
	}

	n := runtime.GOMAXPROCS(0)
	workc := make(chan [][]storage.SeriesRef)

	var wg sync.WaitGroup
	wg.Add(n)

	for i := 0; i < n; i++ {
		go func() {
			for job := range workc {
				for _, l := range job {
					sort.Sort(seriesRefSlice(l))
				}

				job = job[:0]
				ensureOrderBatchPool.Put(job) //nolint:staticcheck // Ignore SA6002 safe to ignore and actually fixing it has some performance penalty.
			}
			wg.Done()
		}()
	}

	nextJob := ensureOrderBatchPool.Get().([][]storage.SeriesRef)
	for _, e := range p.m {
		for _, l := range e {
			nextJob = append(nextJob, l)

			if len(nextJob) >= ensureOrderBatchSize {
				workc <- nextJob
				nextJob = ensureOrderBatchPool.Get().([][]storage.SeriesRef)
			}
		}
	}

	// If the last job was partially filled, we need to push it to workers too.
	if len(nextJob) > 0 {
		workc <- nextJob
	}

	close(workc)
	wg.Wait()

	p.ordered = true
}

// Delete removes all ids in the given map from the postings lists.
func (p *MemPostings) Delete(deleted map[storage.SeriesRef]struct{}) {
	var keys, vals []string

	// Collect all keys relevant for deletion once. New keys added afterwards
	// can by definition not be affected by any of the given deletes.
	p.mtx.RLock()
	for n := range p.m {
		keys = append(keys, n)
	}
	p.mtx.RUnlock()

	for _, n := range keys {
		p.mtx.RLock()
		vals = vals[:0]
		for v := range p.m[n] {
			vals = append(vals, v)
		}
		p.mtx.RUnlock()

		// For each posting we first analyse whether the postings list is affected by the deletes.
		// If yes, we actually reallocate a new postings list.
		for _, l := range vals {
			// Only lock for processing one postings list so we don't block reads for too long.
			p.mtx.Lock()

			found := false
			for _, id := range p.m[n][l] {
				if _, ok := deleted[id]; ok {
					found = true
					break
				}
			}
			if !found {
				p.mtx.Unlock()
				continue
			}
			repl := make([]storage.SeriesRef, 0, len(p.m[n][l]))

			for _, id := range p.m[n][l] {
				if _, ok := deleted[id]; !ok {
					repl = append(repl, id)
				}
			}
			if len(repl) > 0 {
				p.m[n][l] = repl
			} else {
				delete(p.m[n], l)
			}
			p.mtx.Unlock()
		}
		p.mtx.Lock()
		if len(p.m[n]) == 0 {
			delete(p.m, n)
		}
		p.mtx.Unlock()
	}
}

// Iter calls f for each postings list. It aborts if f returns an error and returns it.
func (p *MemPostings) Iter(f func(labels.Label, Postings) error) error {
	p.mtx.RLock()
	defer p.mtx.RUnlock()

	for n, e := range p.m {
		for v, p := range e {
			if err := f(labels.Label{Name: n, Value: v}, newListPostings(p...)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Add a label set to the postings index.
func (p *MemPostings) Add(id storage.SeriesRef, lset labels.Labels) {
	p.mtx.Lock()

	lset.Range(func(l labels.Label) {
		p.addFor(id, l)
	})
	p.addFor(id, allPostingsKey)

	p.mtx.Unlock()
}

func (p *MemPostings) addFor(id storage.SeriesRef, l labels.Label) {
	nm, ok := p.m[l.Name]
	if !ok {
		nm = map[string][]storage.SeriesRef{}
		p.m[l.Name] = nm
	}
	list := append(nm[l.Value], id)
	nm[l.Value] = list

	if !p.ordered {
		return
	}
	// There is no guarantee that no higher ID was inserted before as they may
	// be generated independently before adding them to postings.
	// We repair order violations on insert. The invariant is that the first n-1
	// items in the list are already sorted.
	for i := len(list) - 1; i >= 1; i-- {
		if list[i] >= list[i-1] {
			break
		}
		list[i], list[i-1] = list[i-1], list[i]
	}
}

// ExpandPostings returns the postings expanded as a slice.
func ExpandPostings(p Postings) (res []storage.SeriesRef, err error) {
	for p.Next() {
		res = append(res, p.At())
	}
	return res, p.Err()
}

// Postings provides iterative access over a postings list.
type Postings interface {
	// Next advances the iterator and returns true if another value was found.
	Next() bool

	// Seek advances the iterator to value v or greater and returns
	// true if a value was found.
	Seek(v storage.SeriesRef) bool

	// At returns the value at the current iterator position.
	At() storage.SeriesRef

	// Err returns the last error of the iterator.
	Err() error
}

// errPostings is an empty iterator that always errors.
type errPostings struct {
	err error
}

func (e errPostings) Next() bool                  { return false }
func (e errPostings) Seek(storage.SeriesRef) bool { return false }
func (e errPostings) At() storage.SeriesRef       { return 0 }
func (e errPostings) Err() error                  { return e.err }

var emptyPostings = errPostings{}

// EmptyPostings returns a postings list that's always empty.
// NOTE: Returning EmptyPostings sentinel when Postings struct has no postings is recommended.
// It triggers optimized flow in other functions like Intersect, Without etc.
func EmptyPostings() Postings {
	return emptyPostings
}

// IsEmptyPostingsType returns true if the postings are an empty postings list.
// When this function returns false, it doesn't mean that the postings isn't empty
// (it could be an empty intersection of two non-empty postings, for example).
func IsEmptyPostingsType(p Postings) bool {
	return p == emptyPostings
}

// ErrPostings returns new postings that immediately error.
func ErrPostings(err error) Postings {
	return errPostings{err}
}

// Intersect returns a new postings list over the intersection of the
// input postings.
func Intersect(its ...Postings) Postings {
	if len(its) == 0 {
		return EmptyPostings()
	}
	if len(its) == 1 {
		return its[0]
	}
	for _, p := range its {
		if p == EmptyPostings() {
			return EmptyPostings()
		}
	}

	return newIntersectPostings(its...)
}

type intersectPostings struct {
	arr []Postings
	cur storage.SeriesRef
}

func newIntersectPostings(its ...Postings) *intersectPostings {
	return &intersectPostings{arr: its}
}

func (it *intersectPostings) At() storage.SeriesRef {
	return it.cur
}

func (it *intersectPostings) doNext() bool {
Loop:
	for {
		for _, p := range it.arr {
			if !p.Seek(it.cur) {
				return false
			}
			if p.At() > it.cur {
				it.cur = p.At()
				continue Loop
			}
		}
		return true
	}
}

func (it *intersectPostings) Next() bool {
	for _, p := range it.arr {
		if !p.Next() {
			return false
		}
		if p.At() > it.cur {
			it.cur = p.At()
		}
	}
	return it.doNext()
}

func (it *intersectPostings) Seek(id storage.SeriesRef) bool {
	it.cur = id
	return it.doNext()
}

func (it *intersectPostings) Err() error {
	for _, p := range it.arr {
		if p.Err() != nil {
			return p.Err()
		}
	}
	return nil
}

// Merge returns a new iterator over the union of the input iterators.
func Merge(its ...Postings) Postings {
	if len(its) == 0 {
		return EmptyPostings()
	}
	if len(its) == 1 {
		return its[0]
	}

	p, ok := newMergedPostings(its)
	if !ok {
		return EmptyPostings()
	}
	return p
}

type postingsHeap []Postings

func (h postingsHeap) Len() int           { return len(h) }
func (h postingsHeap) Less(i, j int) bool { return h[i].At() < h[j].At() }
func (h *postingsHeap) Swap(i, j int)     { (*h)[i], (*h)[j] = (*h)[j], (*h)[i] }

func (h *postingsHeap) Push(x interface{}) {
	*h = append(*h, x.(Postings))
}

func (h *postingsHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

type mergedPostings struct {
	h           postingsHeap
	initialized bool
	cur         storage.SeriesRef
	err         error
}

func newMergedPostings(p []Postings) (m *mergedPostings, nonEmpty bool) {
	ph := make(postingsHeap, 0, len(p))

	for _, it := range p {
		// NOTE: mergedPostings struct requires the user to issue an initial Next.
		if it.Next() {
			ph = append(ph, it)
		} else {
			if it.Err() != nil {
				return &mergedPostings{err: it.Err()}, true
			}
		}
	}

	if len(ph) == 0 {
		return nil, false
	}
	return &mergedPostings{h: ph}, true
}

func (it *mergedPostings) Next() bool {
	if it.h.Len() == 0 || it.err != nil {
		return false
	}

	// The user must issue an initial Next.
	if !it.initialized {
		heap.Init(&it.h)
		it.cur = it.h[0].At()
		it.initialized = true
		return true
	}

	for {
		cur := it.h[0]
		if !cur.Next() {
			heap.Pop(&it.h)
			if cur.Err() != nil {
				it.err = cur.Err()
				return false
			}
			if it.h.Len() == 0 {
				return false
			}
		} else {
			// Value of top of heap has changed, re-heapify.
			heap.Fix(&it.h, 0)
		}

		if it.h[0].At() != it.cur {
			it.cur = it.h[0].At()
			return true
		}
	}
}

func (it *mergedPostings) Seek(id storage.SeriesRef) bool {
	if it.h.Len() == 0 || it.err != nil {
		return false
	}
	if !it.initialized {
		if !it.Next() {
			return false
		}
	}
	for it.cur < id {
		cur := it.h[0]
		if !cur.Seek(id) {
			heap.Pop(&it.h)
			if cur.Err() != nil {
				it.err = cur.Err()
				return false
			}
			if it.h.Len() == 0 {
				return false
			}
		} else {
			// Value of top of heap has changed, re-heapify.
			heap.Fix(&it.h, 0)
		}

		it.cur = it.h[0].At()
	}
	return true
}

func (it mergedPostings) At() storage.SeriesRef {
	return it.cur
}

func (it mergedPostings) Err() error {
	return it.err
}

// Without returns a new postings list that contains all elements from the full list that
// are not in the drop list.
func Without(full, drop Postings) Postings {
	if full == EmptyPostings() {
		return EmptyPostings()
	}

	if drop == EmptyPostings() {
		return full
	}
	return newRemovedPostings(full, drop)
}

type removedPostings struct {
	full, remove Postings

	cur storage.SeriesRef

	initialized bool
	fok, rok    bool
}

func newRemovedPostings(full, remove Postings) *removedPostings {
	return &removedPostings{
		full:   full,
		remove: remove,
	}
}

func (rp *removedPostings) At() storage.SeriesRef {
	return rp.cur
}

func (rp *removedPostings) Next() bool {
	if !rp.initialized {
		rp.fok = rp.full.Next()
		rp.rok = rp.remove.Next()
		rp.initialized = true
	}
	for {
		if !rp.fok {
			return false
		}

		if !rp.rok {
			rp.cur = rp.full.At()
			rp.fok = rp.full.Next()
			return true
		}

		fcur, rcur := rp.full.At(), rp.remove.At()
		if fcur < rcur {
			rp.cur = fcur
			rp.fok = rp.full.Next()

			return true
		} else if rcur < fcur {
			// Forward the remove postings to the right position.
			rp.rok = rp.remove.Seek(fcur)
		} else {
			// Skip the current posting.
			rp.fok = rp.full.Next()
		}
	}
}

func (rp *removedPostings) Seek(id storage.SeriesRef) bool {
	if rp.cur >= id {
		return true
	}

	rp.fok = rp.full.Seek(id)
	rp.rok = rp.remove.Seek(id)
	rp.initialized = true

	return rp.Next()
}

func (rp *removedPostings) Err() error {
	if rp.full.Err() != nil {
		return rp.full.Err()
	}

	return rp.remove.Err()
}

// ListPostings implements the Postings interface over a plain list.
type ListPostings struct {
	list []storage.SeriesRef
	cur  storage.SeriesRef
}

func NewListPostings(list []storage.SeriesRef) Postings {
	return newListPostings(list...)
}

func newListPostings(list ...storage.SeriesRef) *ListPostings {
	return &ListPostings{list: list}
}

func (it *ListPostings) At() storage.SeriesRef {
	return it.cur
}

func (it *ListPostings) Next() bool {
	if len(it.list) > 0 {
		it.cur = it.list[0]
		it.list = it.list[1:]
		return true
	}
	it.cur = 0
	return false
}

func (it *ListPostings) Seek(x storage.SeriesRef) bool {
	// If the current value satisfies, then return.
	if it.cur >= x {
		return true
	}
	if len(it.list) == 0 {
		return false
	}

	// Do binary search between current position and end.
	i := sort.Search(len(it.list), func(i int) bool {
		return it.list[i] >= x
	})
	if i < len(it.list) {
		it.cur = it.list[i]
		it.list = it.list[i+1:]
		return true
	}
	it.list = nil
	return false
}

func (it *ListPostings) Err() error {
	return nil
}

// BigEndianPostings implements the Postings interface over a byte stream of
// big endian numbers.
type BigEndianPostings struct {
	list []byte
	cur  uint32
}

func NewBigEndianPostings(list []byte) *BigEndianPostings {
	return &BigEndianPostings{list: list}
}

func (it *BigEndianPostings) At() storage.SeriesRef {
	return storage.SeriesRef(it.cur)
}

func (it *BigEndianPostings) Next() bool {
	if len(it.list) >= 4 {
		it.cur = binary.BigEndian.Uint32(it.list)
		it.list = it.list[4:]
		return true
	}
	return false
}

func (it *BigEndianPostings) Seek(x storage.SeriesRef) bool {
	if storage.SeriesRef(it.cur) >= x {
		return true
	}

	num := len(it.list) / 4
	// Do binary search between current position and end.
	i := sort.Search(num, func(i int) bool {
		return binary.BigEndian.Uint32(it.list[i*4:]) >= uint32(x)
	})
	if i < num {
		j := i * 4
		it.cur = binary.BigEndian.Uint32(it.list[j:])
		it.list = it.list[j+4:]
		return true
	}
	it.list = nil
	return false
}

func (it *BigEndianPostings) Err() error {
	return nil
}

// seriesRefSlice attaches the methods of sort.Interface to []storage.SeriesRef, sorting in increasing order.
type seriesRefSlice []storage.SeriesRef

func (x seriesRefSlice) Len() int           { return len(x) }
func (x seriesRefSlice) Less(i, j int) bool { return x[i] < x[j] }
func (x seriesRefSlice) Swap(i, j int)      { x[i], x[j] = x[j], x[i] }

type ShardedPostings struct {
	p                    Postings
	minOffset, maxOffset uint64
	initialized          bool
}

// Note: shardedPostings can technically return more series than just those that correspond to
// the requested shard. This is because we do fingperint/offset sampling in TSDB so we won't know exactly
// which offsets to start/end at, but will likely buffer a little on each end, so they still need to be
// checked for shard inclusiveness.
// For example (below), given a shard, we'll likely return a slight superset of offsets surrounding the shard.
// ---[shard0]--- # Shard membership
// -[--shard0--]- # Series returned by shardedPostings
func NewShardedPostings(p Postings, fpFilter FingerprintFilter, offsets FingerprintOffsets) *ShardedPostings {
	minVal, maxVal := offsets.Range(fpFilter)
	return &ShardedPostings{
		p:         p,
		minOffset: minVal,
		maxOffset: maxVal,
	}
}

// Next advances the iterator and returns true if another value was found.
func (sp *ShardedPostings) Next() bool {
	// fast forward to the point we know we'll have to start checking
	if !sp.initialized {
		sp.initialized = true
		// Underlying bigEndianPostings doesn't play nice with Seek(0)
		// so we first advance manually once
		if ok := sp.p.Next(); !ok {
			return false
		}
		return sp.Seek(0)
	}
	ok := sp.p.Next()
	if !ok {
		return false
	}

	if sp.p.At() >= storage.SeriesRef(sp.maxOffset) {
		return false
	}

	return true
}

// Seek advances the iterator to value v or greater and returns
// true if a value was found.
func (sp *ShardedPostings) Seek(v storage.SeriesRef) (res bool) {
	if v >= storage.SeriesRef(sp.maxOffset) {
		return false
	}
	if v < storage.SeriesRef(sp.minOffset) {
		v = storage.SeriesRef(sp.minOffset)
	}
	return sp.p.Seek(v)
}

// At returns the value at the current iterator position.
func (sp *ShardedPostings) At() storage.SeriesRef {
	return sp.p.At()
}

// Err returns the last error of the iterator.
func (sp *ShardedPostings) Err() (err error) {
	return sp.p.Err()
}
