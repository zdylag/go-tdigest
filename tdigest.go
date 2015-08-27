// Package tdigest provides a highly accurate mergeable data-structure
// for quantile estimation.
package tdigest

import (
	"fmt"
	"math"
	"math/rand"
)

type centroid struct {
	mean  float64
	count uint32
}

func newCentroid(mean float64, count uint32) *centroid {
	return &centroid{mean: mean, count: count}
}

func (c centroid) String() string {
	return fmt.Sprintf("C<m=%.6f,c=%d>", c.mean, c.count)
}

func (c centroid) Equals(other *centroid) bool {
	return c.mean == other.mean && c.count == other.count
}

func (c *centroid) Update(x float64, weight uint32) {
	c.count += weight
	c.mean += float64(weight) * (x - c.mean) / float64(c.count)
}

func centroidLess(p, q interface{}) bool {
	res := p.(*centroid).mean < q.(*centroid).mean
	return res
}

func centroidLessOrEquals(p, q interface{}) bool {
	a := p.(*centroid).mean
	b := q.(*centroid).mean
	return a <= b
}

// TDigest is a quantile summary structure
type TDigest struct {
	summary     *summary
	compression float64
	count       uint32
}

// New creates a new digest.
// The compression parameter rules the threshold in which samples are
// merged together - the more often distinct samples are merged the more
// precision is lost. Compression should be tuned according to your data
// distribution, but a value of 10 is often good enough. A higher
// compression value means holding more centroids in memory, which means
// a bigger serialization payload and higher memory footprint.
func New(compression float64) *TDigest {
	tdigest := TDigest{compression: compression, summary: newSummary(uint(compression) * 20), count: 0}
	return &tdigest
}

// Percentile returns the desired percentile estimation.
// Values of p must be between 0 and 1 (inclusive), will panic otherwise.
func (t *TDigest) Percentile(p float64) float64 {
	if p < 0 || p > 1 {
		panic("Percentiles must be between 0 and 1 (inclusive)")
	}

	if t.summary.Len() == 0 {
		return math.NaN()
	} else if t.summary.Len() == 1 {
		return t.summary.Min().mean
	}

	p *= float64(t.count)
	var total float64
	i := 0

	found := false
	var result float64

	t.summary.IterInOrderWith(func(item interface{}) bool {
		k := float64(item.(*centroid).count)

		if p < total+k {
			if i == 0 || i+1 == t.summary.Len() {
				result = item.(*centroid).mean
				found = true
				return false
			}
			succ, pred := t.successorAndPredecessorItems(item.(*centroid))
			delta := (succ.mean - pred.mean) / 2
			result = item.(*centroid).mean + ((p-total)/k-0.5)*delta
			found = true
			return false
		}

		i++
		total += k
		return true
	})

	if found {
		return result
	}
	return t.summary.Max().mean
}

// Add registers a new sample in the digest.
// It's the main entry point for the digest and very likely the only
// method to be used for collecting samples. The count parameter is for
// when you are registering a sample that occurred multiple times - the
// most common value for this is 1.
func (t *TDigest) Add(value float64, count uint32) error {

	if count == 0 {
		return fmt.Errorf("Illegal datapoint <value: %.4f, count: %d>", value, count)
	}

	t.count += count

	c := newCentroid(value, count)

	if t.summary.Len() == 0 {
		t.addCentroid(c)
		return nil
	}

	candidates := t.findNearestCentroids(c)

	for len(candidates) > 0 && count > 0 {
		j := rand.Intn(len(candidates))
		chosen := candidates[j]

		quantile := t.computeCentroidQuantile(chosen)

		if float64(chosen.count+count) > t.threshold(quantile) {
			candidates = append(candidates[:j], candidates[j+1:]...)
			continue
		}

		deltaW := math.Min(t.threshold(quantile)-float64(chosen.count), float64(count))
		t.updateCentroid(chosen, value, uint32(deltaW))
		count -= uint32(deltaW)

		candidates = append(candidates[:j], candidates[j+1:]...)
	}

	if count > 0 {
		t.addCentroid(newCentroid(value, count))
	}

	if float64(t.summary.Len()) > 20*t.compression {
		t.Compress()
	}

	return nil
}

// Compress tries to reduce the number of individual centroids stored
// in the digest.
// Compression trades off accuracy for performance and happens
// automatically after a certain amount of distinct samples have been
// stored.
func (t *TDigest) Compress() {
	if t.summary.Len() <= 1 {
		return
	}

	oldTree := t.summary
	t.summary = newSummary(uint(t.compression) * 20)

	nodes := oldTree.Data()
	shuffle(nodes)

	for _, item := range nodes {
		t.Add(item.mean, item.count)
	}
}

// Merge joins a given digest into itself.
// Merging is useful when you have multiple TDigest instances running
// in separate threads and you want to compute quantiles over all the
// samples. This is particularly important on a scatter-gather/map-reduce
// scenario.
func (t *TDigest) Merge(other *TDigest) {
	if other.summary.Len() == 0 {
		return
	}

	nodes := other.summary.Data()
	shuffle(nodes)

	for _, item := range nodes {
		t.Add(item.mean, item.count)
	}
}

func shuffle(data []*centroid) {
	for i := len(data) - 1; i > 1; i-- {
		other := rand.Intn(i + 1)
		tmp := data[other]
		data[other] = data[i]
		data[i] = tmp
	}
}

func (t TDigest) String() string {
	return fmt.Sprintf("TD<compression=%.2f, count=%d, centroids=%d>", t.compression, t.count, t.summary.Len())
}

func (t *TDigest) updateCentroid(c *centroid, mean float64, weight uint32) {
	deleted := t.summary.Delete(c)

	if deleted == nil {
		panic(fmt.Sprintf("Trying to update a centroid that doesn't exist: %s. %s", c, t))
	}

	deleted.Update(mean, weight)
	t.addCentroid(deleted)
}

func (t *TDigest) threshold(q float64) float64 {
	return (4 * float64(t.count) * q * (1 - q)) / t.compression
}

func (t *TDigest) computeCentroidQuantile(c *centroid) float64 {
	var cumSum uint32

	t.summary.IterInOrderWith(func(i interface{}) bool {
		if !centroidLess(i.(*centroid), c) {
			return false
		}

		cumSum += i.(*centroid).count

		return true
	})

	return (float64(c.count)/2.0 + float64(cumSum)) / float64(t.count)
}

func (t *TDigest) addCentroid(c *centroid) {
	current := t.summary.Find(c)

	if current != nil {
		t.summary.Delete(current)
		c.Update(current.mean, current.count)
	}

	t.summary.Add(c)
}

func (t *TDigest) findNearestCentroids(c *centroid) []*centroid {
	ceil, floor := t.ceilingAndFloorItems(c)

	if ceil == nil && floor == nil {
		panic("findNearestCentroids called on an empty tree")
	}

	if ceil == nil {
		return []*centroid{floor}
	}

	if floor == nil {
		return []*centroid{ceil}
	}

	if math.Abs(floor.mean-c.mean) < math.Abs(ceil.mean-c.mean) {
		return []*centroid{floor}
	} else if math.Abs(floor.mean-c.mean) == math.Abs(ceil.mean-c.mean) && !floor.Equals(ceil) {
		return []*centroid{floor, ceil}
	} else {
		return []*centroid{ceil}
	}
}

func (t *TDigest) getSurroundingWith(c *centroid, cmp func(a, b interface{}) bool) (*centroid, *centroid) {
	var ceiling, floor *centroid = nil, nil

	t.summary.IterInOrderWith(func(item interface{}) bool {
		if ceiling == nil && cmp(c, item) {
			ceiling = item.(*centroid)
		}
		if cmp(item, c) {
			floor = item.(*centroid)
		}
		return true
	})
	return ceiling, floor
}

func (t *TDigest) ceilingAndFloorItems(c *centroid) (*centroid, *centroid) {
	// ceiling => smallest key greater than or equals to key
	// floor   => greatest key less than or equals to key
	return t.getSurroundingWith(c, centroidLessOrEquals)
}

func (t *TDigest) successorAndPredecessorItems(c *centroid) (*centroid, *centroid) {
	// FIXME This can be way cheaper if done directly on the tree nodes
	return t.getSurroundingWith(c, centroidLess)
}
