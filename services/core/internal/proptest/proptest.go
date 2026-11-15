// Package proptest runs a property test's cases in parallel and splits them
// across CI jobs, without changing which cases there are.
//
// A property draws its cases (seeded worlds, random histories) in a fixed
// order. Cases runs each one as a parallel subtest, so a job uses every CPU
// it has; with PROPERTY_SHARD=i/n (1 ≤ i ≤ n) it runs only the cases whose
// index k has (k + o) mod n = i-1, so n jobs together run every case exactly
// once. The offset o comes from the test's name, so that the first cases of
// different properties (the costly few of a small property) don't all land
// in the first shard.
// Unset, it runs them all, as a developer's go test does.
//
// Test code only: nothing outside _test.go files imports it.
package proptest

import (
	"fmt"
	"hash/fnv"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Env is the variable that names the shard.
const Env = "PROPERTY_SHARD"

// Shard is one of N slices of every property's cases; I is 0-based.
type Shard struct{ I, N int }

// Has reports whether case k is this shard's.
func (s Shard) Has(k int) bool { return k%s.N == s.I }

// String is the shard as PROPERTY_SHARD writes it.
func (s Shard) String() string { return fmt.Sprintf("%d/%d", s.I+1, s.N) }

// Parse reads "i/n" with 1 ≤ i ≤ n; "" is the whole set (1/1). Anything
// else is an error, never a silent "all" or "none".
func Parse(v string) (Shard, error) {
	if v == "" {
		return Shard{0, 1}, nil
	}
	a, b, ok := strings.Cut(v, "/")
	i, err1 := strconv.Atoi(a)
	n, err2 := strconv.Atoi(b)
	if !ok || err1 != nil || err2 != nil || n < 1 || i < 1 || i > n {
		return Shard{}, fmt.Errorf("proptest: %s=%q: want i/n with 1 <= i <= n", Env, v)
	}
	return Shard{i - 1, n}, nil
}

// FromEnv is the shard PROPERTY_SHARD names. A malformed value fails the
// test: a typo must not quietly run nothing.
func FromEnv(t testing.TB) Shard {
	t.Helper()
	s, err := Parse(os.Getenv(Env))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Cases runs body for each of the n cases that is this shard's, each in a
// parallel subtest named name(k), and returns once all have finished, with
// how many ran. body runs concurrently with other cases: anything it adds
// up must go through a Tally or a lock, and the cases must be drawn before
// Cases is called so that each case is the same whatever the shard.
func Cases(t *testing.T, n int, name func(k int) string, body func(t *testing.T, k int)) int {
	t.Helper()
	s := FromEnv(t)
	off := Offset(t.Name(), s.N)
	ran := 0
	t.Run("cases", func(t *testing.T) {
		for k := 0; k < n; k++ {
			if !s.Has(k + off) {
				continue
			}
			ran++
			t.Run(name(k), func(t *testing.T) {
				t.Parallel()
				body(t, k)
			})
		}
	})
	if s.N > 1 {
		t.Logf("shard %s: %d of %d cases", s, ran, n)
	}
	return ran
}

// Offset is a test's shift of its cases among n shards: the same in every
// job, so each case still has exactly one shard.
func Offset(name string, n int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return int(h.Sum32() % uint32(n))
}

// Tally is a set of counters the parallel cases add to.
type Tally struct {
	mu sync.Mutex
	n  map[string]int
}

// Add adds v to the counter name.
func (c *Tally) Add(name string, v int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	c.n[name] += v
}

// Get is the counter name.
func (c *Tally) Get(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[name]
}
