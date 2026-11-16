package proptest

import (
	"fmt"
	"slices"
	"sync"
	"testing"
)

func TestParse(t *testing.T) {
	for v, want := range map[string]Shard{"": {0, 1}, "1/1": {0, 1}, "1/3": {0, 3}, "3/3": {2, 3}} {
		got, err := Parse(v)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %v, %v; want %v", v, got, err, want)
		}
	}
	for _, v := range []string{"0/3", "4/3", "1/0", "-1/2", "2", "a/b", "1/", "/2", "1/2/3"} {
		if _, err := Parse(v); err == nil {
			t.Errorf("Parse(%q) accepted", v)
		}
	}
	if s := (Shard{1, 4}).String(); s != "2/4" {
		t.Errorf("String: %s", s)
	}
}

// The shards of a split run every case exactly once between them.
func TestShardsCoverEveryCaseOnce(t *testing.T) {
	for n := 1; n <= 7; n++ {
		for cases := 0; cases <= 23; cases++ {
			seen := make([]int, cases)
			for i := 0; i < n; i++ {
				for k := 0; k < cases; k++ {
					if (Shard{i, n}).Has(k) {
						seen[k]++
					}
				}
			}
			for k, c := range seen {
				if c != 1 {
					t.Fatalf("%d shards, %d cases: case %d ran %d times", n, cases, k, c)
				}
			}
		}
	}
}

func TestCasesRunsTheShardsCasesInParallel(t *testing.T) {
	t.Setenv(Env, "2/3")
	var mu sync.Mutex
	var got []int
	var tally Tally
	ran := Cases(t, 10, func(k int) string { return fmt.Sprint("k=", k) }, func(t *testing.T, k int) {
		mu.Lock()
		got = append(got, k)
		mu.Unlock()
		tally.Add("cases", 1)
		tally.Add("sum", k)
	})
	slices.Sort(got)
	off := Offset(t.Name(), 3)
	var want []int
	sum := 0
	for k := 0; k < 10; k++ {
		if (k+off)%3 == 1 {
			want = append(want, k)
			sum += k
		}
	}
	if !slices.Equal(got, want) || ran != len(want) {
		t.Fatalf("ran %d: %v, want %v", ran, got, want)
	}
	if tally.Get("cases") != len(want) || tally.Get("sum") != sum || tally.Get("none") != 0 {
		t.Fatalf("tally: %v", tally.n)
	}
}

// With any offset, the shards still run every case exactly once.
func TestOffsetKeepsEveryCaseOnce(t *testing.T) {
	for _, name := range []string{"TestA", "TestP2Realistic", "x"} {
		for n := 1; n <= 5; n++ {
			off := Offset(name, n)
			if off < 0 || off >= n || off != Offset(name, n) {
				t.Fatalf("Offset(%q, %d) = %d", name, n, off)
			}
			for k := 0; k < 13; k++ {
				c := 0
				for i := 0; i < n; i++ {
					if (Shard{i, n}).Has(k + off) {
						c++
					}
				}
				if c != 1 {
					t.Fatalf("%q, %d shards: case %d in %d shards", name, n, k, c)
				}
			}
		}
	}
}

func TestCasesUnsetRunsAll(t *testing.T) {
	t.Setenv(Env, "")
	var tally Tally
	if ran := Cases(t, 5, func(k int) string { return fmt.Sprint(k) }, func(t *testing.T, k int) { tally.Add("n", 1) }); ran != 5 || tally.Get("n") != 5 {
		t.Fatalf("ran %d, counted %d", ran, tally.Get("n"))
	}
}

// A malformed PROPERTY_SHARD fails the test rather than running nothing.
func TestFromEnvRefusesMalformed(t *testing.T) {
	t.Setenv(Env, "5/4")
	ft := &fatalTB{TB: t}
	func() {
		defer func() { _ = recover() }()
		FromEnv(ft)
	}()
	if !ft.failed {
		t.Fatal("FromEnv accepted 5/4")
	}
}

// fatalTB records Fatal and stops the caller, as testing does, without
// failing the outer test.
type fatalTB struct {
	testing.TB
	failed bool
}

func (f *fatalTB) Helper() {}
func (f *fatalTB) Fatal(args ...any) {
	f.failed = true
	panic("fatal")
}
