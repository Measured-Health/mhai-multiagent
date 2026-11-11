//go:build integration

package testutil

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Run's status is the command's, on every call. testcontainers' Exec alone
// reported exit 0 for about 1 grep miss in 1200, which once failed the
// encryption-at-rest test on main with nothing stored in clear.
func TestNATSRunReportsTheCommandsStatus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	n, stop, err := NewNATS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 150; i++ {
				want := (g + i) % 3
				out, code, err := n.Run(ctx, `printf 'line'; exit "$1"`, strconv.Itoa(want))
				if err != nil {
					t.Error(err)
					return
				}
				if code != want || out != "line" {
					t.Errorf("Run = %q, %d; want %q, %d", out, code, "line", want)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// The store scan finds a needle in clear under /data and names the file,
// and finds nothing once it is gone (the scan itself works).
func TestNATSStoreFilesContainingFindsPlaintext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	n, stop, err := NewNATS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	const needle = "synthetic-plaintext-needle"
	if files, err := n.StoreFilesContaining(ctx, needle); err != nil || len(files) != 0 {
		t.Fatalf("before: %v, %v", files, err)
	}
	if _, code, err := n.Run(ctx, `mkdir -p /data/probe && printf 'x%sy' "$1" > /data/probe/f.blk`, needle); err != nil || code != 0 {
		t.Fatalf("plant: %d, %v", code, err)
	}
	files, err := n.StoreFilesContaining(ctx, needle)
	if err != nil || len(files) != 1 || files[0] != "/data/probe/f.blk" {
		t.Fatalf("planted needle: %v, %v", files, err)
	}
	if _, code, err := n.Run(ctx, `rm -r /data/probe`); err != nil || code != 0 {
		t.Fatalf("remove: %d, %v", code, err)
	}
	if files, err := n.StoreFilesContaining(ctx, needle); err != nil || len(files) != 0 {
		t.Fatalf("after: %v, %v", files, err)
	}
}
