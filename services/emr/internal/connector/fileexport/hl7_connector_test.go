package fileexport

import (
	"context"
	"path"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/egress"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/hl7v2"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/hl7v2/hl7test"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
)

func (s *server) hl7Connector() *FileExport {
	return NewHL7(func(ctx context.Context, cfg connector.Config, o emrurl.Origin, fp, u string, a []ssh.AuthMethod) (*ssh.Client, error) {
		s.dials.Add(1)
		return egress.SSH(ctx, cfg, s.proxy, proxyKey, o, fp, u, a)
	})
}

func (s *server) hl7Config() connector.Config {
	cfg := hl7Config()
	cfg.Settings["host_key_fingerprint"] = ssh.FingerprintSHA256(s.hostKey.PublicKey())
	return cfg
}

// The hl7v2_file connector on the file_export transport: files load in
// name order and move to processed/ byte for byte; a half-written file
// (a .part, or an FHS without its FTS) is left; a file refused whole goes
// to rejected/ and stores nothing; a misnamed file is counted; skipped
// messages are counted and the file still loads.
func TestHL7FetchOverSFTP(t *testing.T) {
	s := newServer(t)
	f1 := hl7test.File(hl7v2.FileName(hat, 1), hat, hl7test.ORU("C1", "O1", hat, hl7test.Potassium(hat)), hl7test.ADT("C2", hat))
	f2 := hl7test.File(hl7v2.FileName(hat, 2), hat, hl7test.RDE("C3", "R1", hat))
	bad := hl7test.File(hl7v2.FileName(hat, 3), hat, strings.Replace(hl7test.ORU("C4", "O4", hat), "|C4|", "||", 1))
	half := []byte(hl7v2.FileHeader("x", hat) + hl7test.ORU("C5", "O5", hat, hl7test.Potassium(hat)))
	s.put(hl7v2.FileName(hat, 1), f1)
	s.put(hl7v2.FileName(hat, 2), f2)
	s.put(hl7v2.FileName(hat, 3), bad)
	s.put(hl7v2.FileName(hat, 4), half)
	s.put(hl7v2.FileName(hat, 5)+".part", f1)
	s.put("results-20261127T101500Z.csv", []byte("x\n"))
	c := s.hl7Connector()
	if r := c.Test(ctx(t), s.hl7Config(), s.creds()); !r.OK {
		t.Fatalf("test %+v", r)
	}
	sk := &memSink{}
	cur, err := c.Fetch(ctx(t), s.hl7Config(), s.creds(), nil, sk)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.list(Processed); len(got) != 2 || got[0] != hl7v2.FileName(hat, 1) || got[1] != hl7v2.FileName(hat, 2) {
		t.Fatalf("processed %v", got)
	}
	if string(s.read(path.Join(Processed, hl7v2.FileName(hat, 1)))) != string(f1) {
		t.Fatal("changed when moved")
	}
	if got := s.list(Rejected); len(got) != 1 || got[0] != hl7v2.FileName(hat, 3) {
		t.Fatalf("rejected %v", got)
	}
	left := s.list("")
	if len(left) != 3 {
		t.Fatalf("left %v", left)
	}
	kinds := map[string]int{}
	for _, r := range sk.recs {
		kinds[r.Kind]++
	}
	if kinds["observation"] != 1 || kinds["medication"] != 1 || kinds["patient"] != 2 || len(sk.rejected) != 0 {
		t.Fatalf("records %v, %d rejected", kinds, len(sk.rejected))
	}
	// Set aside: the refused file, the half-written one, the misnamed one.
	if c.SetAside(cur) != 3 || !c.CaughtUp(cur) {
		t.Fatalf("set aside %d", c.SetAside(cur))
	}
	if _, ok := c.NewFileAt(cur); !ok {
		t.Fatal("no new file time")
	}
	if c.Metrics.RecordsSkipped.Load() != 1 || c.Metrics.FilesLoaded.Load() != 2 || c.Metrics.FilesRejected.Load() != 1 {
		t.Fatalf("metrics skipped %d loaded %d rejected %d", c.Metrics.RecordsSkipped.Load(), c.Metrics.FilesLoaded.Load(),
			c.Metrics.FilesRejected.Load())
	}
	// Finished later: read on the next run.
	s.put(hl7v2.FileName(hat, 4), append(half, []byte(hl7v2.FileTrailer(1))...))
	sk2 := &memSink{}
	if _, err := c.Fetch(ctx(t), s.hl7Config(), s.creds(), nil, sk2); err != nil {
		t.Fatal(err)
	}
	if len(s.list(Processed)) != 3 {
		t.Fatalf("finished file not loaded: %v", s.list(""))
	}
}

// A dead letter keeps identity only, and the rest of the file loads.
func TestHL7DeadLettersOverSFTP(t *testing.T) {
	s := newServer(t)
	k := hl7test.Potassium(hat)
	k.Status = "Q"
	s.put(hl7v2.FileName(hat, 1), hl7test.File("f", hat, hl7test.ORU("C1", "O1", hat, k, hl7test.Potassium(hat.Add(time.Second)))))
	c := s.hl7Connector()
	sk := &memSink{}
	if _, err := c.Fetch(ctx(t), s.hl7Config(), s.creds(), nil, sk); err != nil {
		t.Fatal(err)
	}
	if len(sk.rejected) != 1 || sk.rejected[0].Kind != "observation" || len(s.list(Processed)) != 1 {
		t.Fatalf("rejected %+v", sk.rejected)
	}
}
