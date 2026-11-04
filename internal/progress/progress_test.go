package progress

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// clock is a fake time, moved by hand.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) add(d time.Duration)     { c.t = c.t.Add(d) }
func fake(p *Printer, c *clock) *Printer { p.now, p.tick = c.now, time.Hour; return p }

func TestPlain(t *testing.T) {
	var out bytes.Buffer
	c := &clock{}
	p := fake(New(&out, false, false), c)
	task := p.Start("Compiling the migration tool")
	task.Detail("never shown")
	c.add(1200 * time.Millisecond)
	task.Done("Compiled the migration tool")
	p.Start("Applying 0002_posts on the default database").Fail()
	want := "Compiling the migration tool…\n✓ Compiled the migration tool  1.2s\n" +
		"Applying 0002_posts on the default database…\n✗ Applying 0002_posts on the default database  0.0s\n"
	if out.String() != want {
		t.Fatalf("got %q\nwant %q", out.String(), want)
	}
	if strings.ContainsAny(out.String(), "\r\x1b") {
		t.Fatal("plain output redraws")
	}
}

func TestLive(t *testing.T) {
	var out bytes.Buffer
	c := &clock{}
	p := fake(New(&out, true, false), c)
	task := p.Start("Compiling the migration tool")
	task.Hint(10*time.Second, "(the first build after an update can take a minute)")
	c.add(11 * time.Second)
	p.mu.Lock()
	p.frame++
	p.draw()
	p.mu.Unlock()
	if !strings.HasSuffix(out.String(), "\r\x1b[K⠙ Compiling the migration tool  11.0s  (the first build after an update can take a minute)") {
		t.Fatalf("redraw: %q", out.String())
	}
	out.Reset()
	release := p.Hold()
	if out.String() != "\r\x1b[K" {
		t.Fatalf("hold left the line: %q", out.String())
	}
	out.Reset()
	release()
	p.Write([]byte("go: downloading example.com/x v1.0.0\npart"))
	out.Reset()
	task.Done("Compiled the migration tool")
	if out.String() != "\r\x1b[K✓ Compiled the migration tool  11.0s\n" {
		t.Fatalf("done: %q", out.String())
	}

	out.Reset()
	task = p.Start("Applying 0001_initial on the default database")
	task.Detail("statement 2/3")
	if !strings.HasSuffix(out.String(), "Applying 0001_initial on the default database  0.0s  statement 2/3") {
		t.Fatalf("detail: %q", out.String())
	}
	out.Reset()
	task.Fail()
	if out.String() != "\r\x1b[K✗ Applying 0001_initial on the default database  0.0s\n" {
		t.Fatalf("fail: %q", out.String())
	}
}

func TestQuiet(t *testing.T) {
	var out bytes.Buffer
	p := New(&out, true, true)
	task := p.Start("Compiling the migration tool")
	p.Println("✓ Applied 2 migrations")
	task.Done("Compiled the migration tool")
	p.Start("Applying").Fail()
	if out.Len() != 0 {
		t.Fatalf("quiet printed %q", out.String())
	}
	p.Write([]byte("build error\n"))
	if out.String() != "build error\n" {
		t.Fatalf("quiet dropped another program's output: %q", out.String())
	}
}

func TestDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{400 * time.Millisecond: "0.4s", 21100 * time.Millisecond: "21.1s", 65 * time.Second: "1m5s"} {
		if got := Duration(d); got != want {
			t.Errorf("Duration(%v) = %s, want %s", d, got, want)
		}
	}
}
