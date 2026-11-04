// Package progress shows what the orm command and its migration tool are
// doing while they work. On a terminal a task is one line redrawn in place,
// a spinner and the time so far, ending as "✓ <what it did>  1.2s";
// elsewhere (CI, a pipe) it is a line when it starts and one when it ends.
package progress

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Printer writes the tasks to w. Its methods are safe to call from several
// goroutines; one task runs at a time.
type Printer struct {
	w     io.Writer
	live  bool
	quiet bool
	now   func() time.Time
	tick  time.Duration

	mu      sync.Mutex
	task    *Task
	frame   int
	drawn   bool // the live line is on screen
	held    bool
	stop    chan struct{}
	partial []byte // Write's unfinished line
}

// New is a printer on w: live redraws the running task in place (w is a
// terminal), quiet prints no progress at all.
func New(w io.Writer, live, quiet bool) *Printer {
	return &Printer{w: w, live: live && !quiet, quiet: quiet, now: time.Now, tick: 100 * time.Millisecond}
}

// Task is a piece of work being shown.
type Task struct {
	p         *Printer
	title     string
	detail    string
	hint      string
	hintAfter time.Duration
	start     time.Time
}

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Start shows a task running, titled as what it does ("Compiling the
// migration tool"), ending any still shown.
func (p *Printer) Start(title string) *Task {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.task != nil {
		p.finish("")
	}
	t := &Task{p: p, title: title, start: p.now()}
	p.task = t
	switch {
	case p.quiet:
	case p.live:
		p.frame = 0
		p.draw()
		p.stop = make(chan struct{})
		go p.spin(p.stop)
	default:
		fmt.Fprintf(p.w, "%s…\n", title)
	}
	return t
}

func (p *Printer) spin(stop chan struct{}) {
	tk := time.NewTicker(p.tick)
	defer tk.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tk.C:
			p.mu.Lock()
			p.frame++
			p.draw()
			p.mu.Unlock()
		}
	}
}

// draw redraws the live line; p.mu is held.
func (p *Printer) draw() {
	t := p.task
	if !p.live || t == nil || p.held {
		return
	}
	d := p.now().Sub(t.start)
	line := fmt.Sprintf("\r\x1b[K%s %s  %s", spinner[p.frame%len(spinner)], t.title, Duration(d))
	if t.detail != "" {
		line += "  " + t.detail
	}
	if t.hint != "" && d >= t.hintAfter {
		line += "  " + t.hint
	}
	io.WriteString(p.w, line)
	p.drawn = true
}

// clear takes the live line off the screen; p.mu is held.
func (p *Printer) clear() {
	if p.drawn {
		io.WriteString(p.w, "\r\x1b[K")
		p.drawn = false
	}
}

// Detail sets what the task is at ("statement 2/5"), shown on a terminal.
func (t *Task) Detail(s string) {
	t.p.mu.Lock()
	defer t.p.mu.Unlock()
	t.detail = s
	if t.p.task == t {
		t.p.draw()
	}
}

// Hint adds s to the line once the task has run longer than after, on a
// terminal.
func (t *Task) Hint(after time.Duration, s string) {
	t.p.mu.Lock()
	defer t.p.mu.Unlock()
	t.hint, t.hintAfter = s, after
}

// Done ends the task as done: "✓ <title>  1.2s", title being what it did
// ("Compiled the migration tool").
func (t *Task) Done(title string) {
	t.p.mu.Lock()
	defer t.p.mu.Unlock()
	if t.p.task == t {
		t.p.finish("✓ " + title)
	}
}

// Fail ends the task as failed: "✗ <its title>  1.2s"; the error is the
// caller's to print.
func (t *Task) Fail() {
	t.p.mu.Lock()
	defer t.p.mu.Unlock()
	if t.p.task == t {
		t.p.finish("✗ " + t.title)
	}
}

// finish ends the task with line, or no line; p.mu is held.
func (p *Printer) finish(line string) {
	t := p.task
	p.task = nil
	if p.stop != nil {
		close(p.stop)
		p.stop = nil
	}
	p.clear()
	if line != "" && !p.quiet {
		fmt.Fprintf(p.w, "%s  %s\n", line, Duration(p.now().Sub(t.start)))
	}
}

// Println prints a line of progress (a summary, a statement run) above the
// running task; nothing when quiet.
func (p *Printer) Println(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.quiet {
		return
	}
	p.clear()
	io.WriteString(p.w, strings.TrimRight(s, "\n")+"\n")
	p.draw()
}

// Write writes another program's output above the running task, whole
// lines at a time; it is shown even when quiet.
func (p *Printer) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.partial = append(p.partial, b...)
	i := strings.LastIndexByte(string(p.partial), '\n')
	if i < 0 {
		return len(b), nil
	}
	p.clear()
	_, err := p.w.Write(p.partial[:i+1])
	p.partial = append(p.partial[:0], p.partial[i+1:]...)
	p.draw()
	return len(b), err
}

// Hold takes the running task's line off the screen until release is
// called, for a question asked on the terminal.
func (p *Printer) Hold() (release func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.held = true
	p.clear()
	return func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.held = false
		p.draw()
	}
}

// Duration is d as progress shows it: 0.4s, 21.1s, 1m5s.
func Duration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return d.Round(time.Second).String()
}
