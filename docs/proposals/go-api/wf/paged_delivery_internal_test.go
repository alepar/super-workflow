package wf

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

func TestPagedOutputFailureDoesNotCredit(t *testing.T) {
	d := foldDef()
	j := MemJournal()
	now := time.Now()
	e := NewEngine(j, nil, EngineOptions{Defs: []*Def{d}, PagedPrompts: true, Clock: func() time.Time { return now }}).(*Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.HandOut(run, 1)
	if err != nil || len(ins) != 1 {
		t.Fatalf("handout: %v %v", ins, err)
	}
	failed := errors.New("broken output")
	_, err = e.deliverPage(run, ins[0].Name, "", harness.Caller{}, PromptPageRequest{Page: 1}, func(string) error { return failed })
	if !errors.Is(err, failed) {
		t.Fatalf("write error: %v", err)
	}
	err = e.withDryRun(run, func(c *cycle) error {
		_, u, ok := c.st.unitByName(ins[0].Name)
		if !ok {
			t.Fatal("unit gone")
		}
		if u.Packet != nil || len(u.PagesFetched) > 0 || u.State != UnitHanded {
			t.Fatal("failed write changed ownership or coverage")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.PromptPage(run, ins[0].Name, "", harness.Caller{}, PromptPageRequest{Page: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestPageEnvelopeIndependentLimits(t *testing.T) {
	for _, body := range []string{strings.Repeat("x\n", 1000), strings.Repeat("界🙂\t", 2000), strings.Repeat("longword", 2000)} {
		env := harness.PromptEnvelope{Bytes: 1024, Lines: 25, Columns: 50}
		pages, _, err := pageText(body, "Contract", env, func(int, string) string { return "wf prompt unit --page 2" }, "nonce", "", false)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pages {
			if len(p) > env.Bytes || strings.Count(p, "\n") > env.Lines {
				t.Fatalf("page exceeds independent envelope: %d bytes %d lines", len(p), strings.Count(p, "\n"))
			}
		}
	}
}

func TestPromptPageFoldRejectsForeignPacketAndRange(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		r := newIssuedRun(t)
		name := r.handOut()
		r.must(JournalAttemptStarted, AttemptStartedData{Name: name})
		packet := &PromptPacket{Run: r.recs[0].Run, Unit: name, Attempt: r.fold().insts[""].Attempt, Pages: []string{"one"}, Codes: []string{"code"}, Digest: "digest"}
		page := 2
		if foreign {
			page = 1
			packet.Run = "other-run"
		}
		err := r.add(JournalPromptFetched, PromptFetchedData{Name: name, Packet: packet, PacketDigest: "digest", Page: page})
		if !errors.Is(err, ErrCorruptJournal) {
			t.Fatalf("invalid coverage accepted (foreign=%v): %v", foreign, err)
		}
	}
}

type pageOutputFailure struct{ short, flush bool }

func (w pageOutputFailure) Write(p []byte) (int, error) {
	if w.short {
		return len(p) / 2, nil
	}
	if w.flush {
		return len(p), nil
	}
	return 0, errors.New("write failed")
}
func (w pageOutputFailure) Flush() error {
	if w.flush {
		return errors.New("flush failed")
	}
	return nil
}

func TestPagedCLIOutputFailureDoesNotCredit(t *testing.T) {
	for _, writer := range []pageOutputFailure{{}, {short: true}, {flush: true}} {
		d := foldDef()
		j := MemJournal()
		e := NewEngine(j, nil, EngineOptions{Defs: []*Def{d}, PagedPrompts: true}).(*Engine)
		run, err := e.Run(d, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		ins, err := e.HandOut(run, 1)
		if err != nil {
			t.Fatal(err)
		}
		c := &cli{stdout: writer, stderr: io.Discard, env: func(string) string { return "" }}
		if code := c.promptPage(e, "prompt", run, ins[0].Name, &flags{page: 1}); code == 0 {
			t.Fatal("failed transport returned success")
		}
		unlock, err := j.Lock(run)
		if err != nil {
			t.Fatal(err)
		}
		records, err := j.Records(run)
		unlock()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range records {
			if r.Kind == JournalPromptFetched {
				t.Fatal("failed output earned fetch credit")
			}
		}
	}
}

// A printed command is never wrapped or cut: a page keeps it one line,
// however wide, and that line alone may pass the page's columns.
func TestAPagedCommandStaysOneLine(t *testing.T) {
	command := workspaceCommand("", "/w", "/bin/super wf done --claim "+strings.Repeat("c", 140)+" --state-dir /s")
	text := "Intro line.\n" + command + "\n" + strings.Repeat("word ", 60)
	pages, _, err := pageText(text, "contract", harness.PromptEnvelope{Bytes: 3072, Lines: 150, Columns: 100}, func(int, string) string { return command }, "nonce", "", true)
	if err != nil {
		t.Fatal(err)
	}
	whole := strings.Join(pages, "\n")
	if strings.Count(whole, command) < 2 || strings.Contains(whole, " \\\n") {
		t.Fatalf("the command was wrapped or cut:\n%s", whole)
	}
	for _, p := range pages {
		for _, line := range strings.Split(p, "\n") {
			if len(line) > 100 && !isCommandLine(line, "") {
				t.Fatalf("a prose line passed the columns: %q", line)
			}
		}
	}
}
