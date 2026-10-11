package orm

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Pool tunes a declared database's connections and the supervisor
// keeping them. A zero field takes its default; a negative MaxOpen,
// MaxIdle, MaxLifetime or MaxIdleTime is no limit (no idle connections
// kept, for MaxIdle). Give it as a Declare
// option, or as URL parameters (pool_max, pool_max_idle, pool_min_idle,
// pool_max_lifetime, pool_max_idle_time, pool_acquire, pool_health,
// breaker_failures, breaker_cooldown, startup_wait), which are taken off
// the URL before the driver sees it.
type Pool struct {
	MaxOpen int // connections open at most (20; negative: no limit)
	MaxIdle int // idle connections kept (5; negative: none)
	// MinIdle is the idle connections opened at start and kept open
	// while the database is up.
	MinIdle int
	// MaxLifetime retires a connection after it (1h; negative: never),
	// less up to a fifth at random so they don't all reconnect at once.
	MaxLifetime time.Duration
	MaxIdleTime time.Duration // closes a connection idle this long (negative or 0: never)
	// Acquire bounds the wait for a connection when MaxOpen are in use
	// (database/sql waits as long as the context lets it): past it the
	// statement fails Unavailable. 0 waits.
	Acquire time.Duration
	// Health is how often the supervisor pings the database and its
	// replicas (10s).
	Health  time.Duration
	Breaker Breaker
	// StartupWait is how long Start waits for the database to answer
	// (5s); past it the program starts with it Down and the supervisor
	// keeps reconnecting. Negative doesn't wait.
	StartupWait time.Duration
}

// Breaker is the circuit breaker on a database's statements: after
// Failures connection errors in a row (5) it opens, and statements fail
// at once, Unavailable, for Cooldown (5s); then one statement probes, and
// its success closes it. Only connection errors count — a refused or
// reset connection, a timeout, a dropped one — never a query's own.
type Breaker struct {
	Failures int
	Cooldown time.Duration
}

func (p Pool) apply(c *databaseConfig) {
	o := &c.pool
	set := func(dst *int, v int) {
		if v != 0 {
			*dst = v
		}
	}
	dur := func(dst *time.Duration, v time.Duration) {
		if v != 0 {
			*dst = v
		}
	}
	set(&o.MaxOpen, p.MaxOpen)
	set(&o.MaxIdle, p.MaxIdle)
	set(&o.MinIdle, p.MinIdle)
	set(&o.Breaker.Failures, p.Breaker.Failures)
	dur(&o.MaxLifetime, p.MaxLifetime)
	dur(&o.MaxIdleTime, p.MaxIdleTime)
	dur(&o.Acquire, p.Acquire)
	dur(&o.Health, p.Health)
	dur(&o.Breaker.Cooldown, p.Breaker.Cooldown)
	dur(&o.StartupWait, p.StartupWait)
}

// Pool defaults: a cap on connections, so a burst of requests can't
// exhaust the server's.
const (
	DefaultMaxOpen     = 20
	DefaultMaxIdle     = 5
	DefaultMaxLifetime = time.Hour
)

func (p Pool) withDefaults() Pool {
	switch {
	case p.MaxOpen == 0:
		p.MaxOpen = DefaultMaxOpen
	case p.MaxOpen < 0:
		p.MaxOpen = 0
	}
	switch {
	case p.MaxIdle == 0:
		p.MaxIdle = DefaultMaxIdle
	case p.MaxIdle < 0:
		p.MaxIdle = 0
	}
	switch {
	case p.MaxLifetime == 0:
		p.MaxLifetime = DefaultMaxLifetime
	case p.MaxLifetime < 0:
		p.MaxLifetime = 0
	}
	p.MaxIdleTime = max(p.MaxIdleTime, 0)
	p.MaxIdle = max(p.MaxIdle, p.MinIdle)
	if p.MaxOpen > 0 {
		p.MaxIdle = min(p.MaxIdle, p.MaxOpen)
	}
	if p.Health == 0 {
		p.Health = 10 * time.Second
	}
	if p.Breaker.Failures == 0 {
		p.Breaker.Failures = 5
	}
	if p.Breaker.Cooldown == 0 {
		p.Breaker.Cooldown = 5 * time.Second
	}
	if p.StartupWait == 0 {
		p.StartupWait = 5 * time.Second
	}
	return p
}

// State is a supervised database's health.
type State int32

const (
	// Connecting: it hasn't answered yet.
	Connecting State = iota
	// Up: it answers, and so do its replicas.
	Up
	// Degraded: it answers, but a replica doesn't, or the breaker is
	// probing it.
	Degraded
	// Down: it doesn't answer — the breaker is open, or it never answered
	// within StartupWait. The supervisor keeps reconnecting.
	Down
)

func (s State) String() string {
	switch s {
	case Connecting:
		return "connecting"
	case Up:
		return "up"
	case Degraded:
		return "degraded"
	case Down:
		return "down"
	}
	return fmt.Sprintf("State(%d)", int32(s))
}

func (s State) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

func (s *State) UnmarshalText(b []byte) error {
	for st := Connecting; st <= Down; st++ {
		if st.String() == string(b) {
			*s = st
			return nil
		}
	}
	return fmt.Errorf("orm: no state %q", b)
}

// StateEvent is a change of a database's state, or of one of its
// replicas' health (Replica set, State Up or Down), as ObserveState sees
// it.
type StateEvent struct {
	DB       string
	Replica  string // the replica's URL, its password hidden
	From, To State
	Err      error // what failed, for a change for the worse
}

var stateHooks struct {
	mu   sync.Mutex
	next int
	fns  map[int]func(StateEvent)
}

// ObserveState calls fn on each change of a supervised database's state:
// for metrics and alerts (the supervisor logs them too). It returns what
// stops it.
func ObserveState(fn func(StateEvent)) (stop func()) {
	stateHooks.mu.Lock()
	defer stateHooks.mu.Unlock()
	if stateHooks.fns == nil {
		stateHooks.fns = map[int]func(StateEvent){}
	}
	id := stateHooks.next
	stateHooks.next++
	stateHooks.fns[id] = fn
	return func() {
		stateHooks.mu.Lock()
		delete(stateHooks.fns, id)
		stateHooks.mu.Unlock()
	}
}

func emitState(e StateEvent) {
	stateHooks.mu.Lock()
	fns := make([]func(StateEvent), 0, len(stateHooks.fns))
	for _, fn := range stateHooks.fns {
		fns = append(fns, fn)
	}
	stateHooks.mu.Unlock()
	for _, fn := range fns {
		fn(e)
	}
}

// ErrorClass is what an error says about the connection it came on.
type ErrorClass int

const (
	// OtherError: the database answered (a bad query, a constraint).
	OtherError ErrorClass = iota
	// ConnError: the connection failed — refused, reset, timed out,
	// dropped. The breaker counts these; reads are retried once.
	ConnError
	// RetryableError: the database aborted the transaction for a
	// deadlock or a serialization failure; Retry runs it again.
	RetryableError
)

// DriverHooks is what a driver package teaches the ORM about its driver:
// how to put a user and password into a DSN (for Credentials), and how
// to read its errors.
type DriverHooks struct {
	Credentials func(dsn, user, password string) (string, error)
	Classify    func(err error) ErrorClass // OtherError for one it doesn't know
}

var hooks struct {
	mu sync.RWMutex
	by map[string]DriverHooks
}

// RegisterDriverHooks gives the ORM a driver's hooks for a dialect; the
// driver packages register theirs.
func RegisterDriverHooks(dialect string, h DriverHooks) {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	if hooks.by == nil {
		hooks.by = map[string]DriverHooks{}
	}
	hooks.by[DialectFor(dialect).Name()] = h
}

func driverHooks(dialect string) DriverHooks {
	hooks.mu.RLock()
	defer hooks.mu.RUnlock()
	return hooks.by[DialectFor(dialect).Name()]
}

// Classify is what err says about its connection: the drivers' hooks'
// reading, else the ORM's own of database/sql's, the network's and the
// databases' codes.
func Classify(err error) ErrorClass { return classify(err) }

func classify(err error) ErrorClass {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, driver.ErrSkip) {
		return OtherError
	}
	hooks.mu.RLock()
	for _, h := range hooks.by {
		if h.Classify != nil {
			if c := h.Classify(err); c != OtherError {
				hooks.mu.RUnlock()
				return c
			}
		}
	}
	hooks.mu.RUnlock()
	var coded interface{ SQLState() string }
	if errors.As(err, &coded) {
		switch st := coded.SQLState(); {
		case st == "40001", st == "40P01":
			return RetryableError
		case strings.HasPrefix(st, "08"), st == "57P01", st == "57P02", st == "57P03":
			return ConnError
		}
		return OtherError
	}
	var ne net.Error
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) || errors.As(err, &ne) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNABORTED) {
		return ConnError
	}
	msg := err.Error()
	for _, s := range []string{"Error 1213", "Error 1205", "SQLITE_BUSY", "SQLITE_LOCKED", "database is locked", "database table is locked"} {
		if strings.Contains(msg, s) {
			return RetryableError
		}
	}
	for _, s := range []string{"connection refused", "connection reset", "broken pipe", "bad connection", "invalid connection", "server has gone away", "unexpected EOF"} {
		if strings.Contains(msg, s) {
			return ConnError
		}
	}
	return OtherError
}

// breaker is a database's circuit breaker; see Breaker.
type breaker struct {
	mu       sync.Mutex
	cfg      Breaker
	failures int
	open     bool
	openedAt time.Time
	probing  bool
}

// allow is whether a statement may go: always while closed; while open,
// one probe once the cooldown passed.
func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return true
	}
	if b.probing || time.Since(b.openedAt) < b.cfg.Cooldown {
		return false
	}
	b.probing = true
	return true
}

// success closes the breaker; it says whether that changed it.
func (b *breaker) success() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	was := b.open
	b.failures, b.open, b.probing = 0, false, false
	return was
}

// failure counts a connection error, opening the breaker at the
// threshold (at once while probing); it says whether that opened it.
func (b *breaker) failure() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.open {
		b.openedAt, b.probing = time.Now(), false
		return false
	}
	if b.failures >= b.cfg.Failures {
		b.open, b.openedAt = true, time.Now()
		return true
	}
	return false
}

func (b *breaker) status() (string, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.open && (b.probing || time.Since(b.openedAt) >= b.cfg.Cooldown):
		return "half-open", b.failures
	case b.open:
		return "open", b.failures
	}
	return "closed", b.failures
}

// supervisor keeps a declared database's pool: its state, breaker,
// health pings and reconnects, replicas, MinIdle, and a graceful Close.
type supervisor struct {
	name    string
	label   string
	db      *sql.DB
	pool    Pool
	slow    time.Duration
	timeout time.Duration
	log     *slog.Logger

	br       breaker
	gen      atomic.Int64 // connections of an earlier generation are dropped
	state    atomic.Int32
	everUp   atomic.Bool
	waited   atomic.Bool // StartupWait passed
	lastErr  atomic.Pointer[string]
	up       chan struct{}
	upOnce   sync.Once
	kick     chan struct{}
	replicas []*replica
	closing  atomic.Bool
	stop     context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.Mutex // state changes, logged in order
}

// replica is a database's read replica.
type replica struct {
	url     string // its password hidden
	sql     *sql.DB
	healthy atomic.Bool
}

func newSupervisor(name string, db *sql.DB, p Pool, log *slog.Logger) *supervisor {
	s := &supervisor{name: name, label: dbLabel(name), db: db, pool: p, log: log, up: make(chan struct{}), kick: make(chan struct{}, 1)}
	s.br.cfg = p.Breaker
	return s
}

// start runs the health loop until stop or Close.
func (s *supervisor) start() {
	if s == nil || s.stop != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.stop = cancel
	s.wg.Go(func() { s.loop(ctx) })
}

func (s *supervisor) loop(ctx context.Context) {
	fails := 0
	for {
		wait := s.pool.Health
		if err := s.check(ctx); err != nil {
			// Reconnecting: exponential backoff with jitter, 100ms to 30s.
			base := min(30*time.Second, 100*time.Millisecond<<min(fails, 9))
			wait = base/2 + time.Duration(rand.Int64N(int64(base/2)+1))
			fails++
		} else {
			fails = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-s.kick:
		}
	}
}

// check pings the database and its replicas, and tops up MinIdle.
func (s *supervisor) check(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, min(s.pool.Health, 5*time.Second))
	err := s.db.PingContext(pctx)
	cancel()
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		s.failed(err)
	} else {
		s.answered()
	}
	for _, r := range s.replicas {
		rctx, cancel := context.WithTimeout(ctx, min(s.pool.Health, 5*time.Second))
		rerr := r.sql.PingContext(rctx)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		s.replicaHealth(r, rerr)
	}
	if err == nil && s.pool.MinIdle > 0 {
		s.warm(ctx)
	}
	return err
}

// warm opens idle connections up to MinIdle.
func (s *supervisor) warm(ctx context.Context) {
	if s.db.Stats().Idle >= s.pool.MinIdle {
		return
	}
	// Held at once, MinIdle connections are that many open; back in the
	// pool, that many idle.
	need := s.pool.MinIdle
	if s.pool.MaxOpen > 0 {
		need = min(need, s.pool.MaxOpen)
	}
	var held []*sql.Conn
	for range need {
		c, err := s.db.Conn(ctx)
		if err != nil {
			break
		}
		held = append(held, c)
	}
	for _, c := range held {
		c.Close()
	}
}

// answered notes the database answering.
func (s *supervisor) answered() {
	s.br.success()
	s.everUp.Store(true)
	s.upOnce.Do(func() { close(s.up) })
	s.update(nil)
}

// failed notes a connection error.
func (s *supervisor) failed(err error) {
	msg := err.Error()
	s.lastErr.Store(&msg)
	if s.br.failure() {
		select {
		case s.kick <- struct{}{}: // reconnect from now
		default:
		}
	}
	s.update(err)
}

func (s *supervisor) replicaHealth(r *replica, err error) {
	if err == nil {
		if !r.healthy.Swap(true) {
			s.logState(StateEvent{DB: s.name, Replica: r.url, From: Down, To: Up})
			s.update(nil)
		}
		return
	}
	if r.healthy.Swap(false) {
		s.logState(StateEvent{DB: s.name, Replica: r.url, From: Up, To: Down, Err: err})
		s.update(err)
	}
}

// compute is the database's state now.
func (s *supervisor) compute() State {
	if !s.everUp.Load() {
		if s.waited.Load() {
			return Down
		}
		return Connecting
	}
	switch st, _ := s.br.status(); st {
	case "open":
		return Down
	case "half-open":
		return Degraded
	}
	for _, r := range s.replicas {
		if !r.healthy.Load() {
			return Degraded
		}
	}
	return Up
}

// update moves the state to what compute says, logging a change.
func (s *supervisor) update(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	to := s.compute()
	from := State(s.state.Swap(int32(to)))
	if from != to {
		s.logState(StateEvent{DB: s.name, From: from, To: to, Err: err})
	}
}

func (s *supervisor) logState(e StateEvent) {
	if s.log != nil {
		args := []any{"db", s.name, "from", e.From.String(), "to", e.To.String()}
		if e.Replica != "" {
			args = append(args, "replica", e.Replica)
		}
		lvl := slog.LevelInfo
		if e.Err != nil {
			args, lvl = append(args, "err", e.Err.Error()), slog.LevelWarn
		}
		s.log.Log(context.Background(), lvl, "orm: database state", args...)
	}
	emitState(e)
}

// replica is a healthy replica, the least busy, or nil for none.
func (s *supervisor) replica() *replica {
	var best *replica
	inUse := 0
	for _, r := range s.replicas {
		if !r.healthy.Load() {
			continue
		}
		if n := r.sql.Stats().InUse; best == nil || n < inUse {
			best, inUse = r, n
		}
	}
	return best
}

func (s *supervisor) unavailable(why string) error {
	return &Error{Kind: Unavailable, Message: fmt.Sprintf("orm: %s is unavailable: %s", s.label, why)}
}

// admit is whether a statement may go: not while closing, nor while the
// breaker is open.
func (s *supervisor) admit() error {
	if s.closing.Load() {
		return s.unavailable("it is closing")
	}
	if !s.br.allow() {
		msg := "the circuit breaker is open"
		if e := s.lastErr.Load(); e != nil {
			msg += " (" + *e + ")"
		}
		return s.unavailable(msg)
	}
	return nil
}

// record notes a statement's outcome: a connection error toward the
// breaker (and the older connections dropped), anything else the
// database answering.
func (s *supervisor) record(err error) {
	if classify(err) == ConnError {
		s.gen.Add(1)
		s.failed(err)
		return
	}
	if was := s.br.success(); was || !s.everUp.Load() {
		s.answered()
	}
}

func (s *supervisor) slowLog(ctx context.Context, name, q string, d time.Duration) {
	if s.slow > 0 && d >= s.slow && s.log != nil {
		if len(q) > 500 {
			q = q[:500] + "…"
		}
		s.log.WarnContext(ctx, "orm: slow query", "db", name, "dur", d, "sql", q)
	}
}

// acquire is a connection of db within Acquire, else Unavailable.
func (s *supervisor) acquire(ctx context.Context, db *sql.DB) (*sql.Conn, error) {
	actx, cancel := context.WithTimeout(ctx, s.pool.Acquire)
	defer cancel()
	c, err := db.Conn(actx)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return nil, s.unavailable(fmt.Sprintf("no free connection within %v (all %d in use)", s.pool.Acquire, s.pool.MaxOpen))
	}
	return c, err
}

// timeoutKey carries WithTimeout's duration.
type timeoutKey struct{}

// Timeout bounds a statement: a Declare option (each statement of the
// database), an argument of Raw, RawRow and Exec (that one), or
// WithTimeout's (each statement of a context).
type Timeout time.Duration

func (t Timeout) apply(c *databaseConfig) { c.timeout = time.Duration(t) }

// WithTimeout bounds each statement ctx's queries run by d, over their
// database's Timeout.
func WithTimeout(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, timeoutKey{}, d)
}

// SlowQuery logs (warn, through the Databases' Logger) each statement of
// the database taking d or longer: a Declare option.
type SlowQuery time.Duration

func (q SlowQuery) apply(c *databaseConfig) { c.slow = time.Duration(q) }

// statementCtx is ctx bounded by the statement's timeout; the cancel
// for an exec, nil for a query (whose rows outlive the call: its timer
// is let run out).
func (c conn) statementCtx(ctx context.Context, query bool) (context.Context, context.CancelFunc) {
	d, _ := ctx.Value(timeoutKey{}).(time.Duration)
	if d == 0 && c.src != nil && c.src.sup != nil {
		d = c.src.sup.timeout
	}
	if d <= 0 {
		return ctx, func() {}
	}
	tctx, cancel := context.WithTimeout(ctx, d)
	if query {
		context.AfterFunc(tctx, cancel)
		return tctx, func() {}
	}
	return tctx, cancel
}

// execRaw runs an exec as the database's supervisor has it.
func (c conn) execRaw(ctx context.Context, q string, args []any) (sql.Result, error) {
	ctx, cancel := c.statementCtx(ctx, false)
	defer cancel()
	s := c.sup()
	if s == nil {
		return c.run.ExecContext(ctx, q, args...)
	}
	if err := s.admit(); err != nil {
		return nil, err
	}
	start := time.Now()
	var res sql.Result
	var err error
	if db, ok := c.run.(*sql.DB); ok && s.pool.Acquire > 0 {
		var cn *sql.Conn
		if cn, err = s.acquire(ctx, db); err == nil {
			res, err = cn.ExecContext(ctx, q, args...)
			cn.Close()
		}
	} else {
		res, err = c.run.ExecContext(ctx, q, args...)
	}
	if KindOf(err) != Unavailable {
		s.record(err)
	}
	s.slowLog(ctx, c.name, q, time.Since(start))
	return res, err
}

// queryRaw runs a query as the database's supervisor has it: a read
// failing on a dropped connection is retried once on a fresh one (on
// the primary, for a replica's, which is ejected).
func (c conn) queryRaw(ctx context.Context, q string, args []any) (*sql.Rows, error) {
	ctx, _ = c.statementCtx(ctx, true)
	s := c.sup()
	if s == nil {
		return c.run.QueryContext(ctx, q, args...)
	}
	if c.rep == nil {
		if err := s.admit(); err != nil {
			return nil, err
		}
	}
	start := time.Now()
	rows, err := s.query(ctx, c.run, q, args)
	if _, pooled := c.run.(*sql.DB); err != nil && pooled && c.read && classify(err) == ConnError {
		if c.rep != nil {
			s.replicaHealth(c.rep, err)
			c.run, c.rep = s.db, nil
			if aerr := s.admit(); aerr != nil {
				return nil, aerr
			}
		} else {
			s.gen.Add(1)
		}
		rows, err = s.query(ctx, c.run, q, args)
	}
	if c.rep == nil && KindOf(err) != Unavailable {
		s.record(err)
	}
	s.slowLog(ctx, c.name, q, time.Since(start))
	return rows, err
}

func (s *supervisor) query(ctx context.Context, run runner, q string, args []any) (*sql.Rows, error) {
	db, ok := run.(*sql.DB)
	if !ok || s.pool.Acquire <= 0 {
		return run.QueryContext(ctx, q, args...)
	}
	cn, err := s.acquire(ctx, db)
	if err != nil {
		return nil, err
	}
	rows, err := cn.QueryContext(ctx, q, args...)
	if err != nil {
		cn.Close()
		return nil, err
	}
	go cn.Close() // returns it to the pool once the rows are closed
	return rows, nil
}

func (c conn) sup() *supervisor {
	if c.src == nil {
		return nil
	}
	return c.src.sup
}

// begin is a transaction on d: admitted by its breaker, its connection
// acquired within Acquire; end returns the connection.
func (d *DB) begin(ctx context.Context) (*sql.Tx, func(), error) {
	s := d.sup
	if s == nil {
		tx, err := d.sql.BeginTx(ctx, nil)
		return tx, func() {}, err
	}
	if err := s.admit(); err != nil {
		return nil, nil, err
	}
	if s.pool.Acquire <= 0 {
		tx, err := d.sql.BeginTx(ctx, nil)
		s.record(err)
		return tx, func() {}, err
	}
	cn, err := s.acquire(ctx, d.sql)
	if err != nil {
		return nil, nil, err
	}
	tx, err := cn.BeginTx(ctx, nil)
	s.record(err)
	if err != nil {
		cn.Close()
		return nil, nil, err
	}
	return tx, func() { cn.Close() }, nil
}

// commit commits tx, noting a dropped connection.
func (s *supervisor) commit(tx *sql.Tx) error {
	err := tx.Commit()
	if s != nil && err != nil {
		s.record(err)
	}
	return err
}

// close stops new statements, waits for those running (their
// connections back in the pool) until ctx is done, and closes the pools.
func (s *supervisor) close(ctx context.Context) error {
	s.closing.Store(true)
	if s.stop != nil {
		s.stop()
	}
	s.wg.Wait()
	busy := func() bool {
		n := s.db.Stats().InUse
		for _, r := range s.replicas {
			n += r.sql.Stats().InUse
		}
		return n > 0
	}
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	var waitErr error
	for busy() {
		select {
		case <-ctx.Done():
			waitErr = fmt.Errorf("orm: closing %s with statements still running: %w", s.label, ctx.Err())
		case <-t.C:
			continue
		}
		break
	}
	errs := []error{waitErr, s.db.Close()}
	for _, r := range s.replicas {
		errs = append(errs, r.sql.Close())
	}
	return errors.Join(errs...)
}

// ReplicaStats is a replica's health and pool.
type ReplicaStats struct {
	URL     string      `json:"url"`
	Healthy bool        `json:"healthy"`
	Pool    sql.DBStats `json:"pool"`
}

// DBStats is a database's pool, as Stats reports it.
type DBStats struct {
	Name       string         `json:"name"`
	Dialect    string         `json:"dialect"`
	Default    bool           `json:"default"`
	Supervised bool           `json:"supervised"` // declared (Declare), not added
	State      State          `json:"state"`
	Breaker    string         `json:"breaker"` // closed, open, half-open
	Failures   int            `json:"failures"`
	LastError  string         `json:"last_error,omitempty"`
	Pool       sql.DBStats    `json:"pool"`
	Replicas   []ReplicaStats `json:"replicas,omitempty"`
}

// Stats is a database's pool in Default: database/sql's statistics, its
// state, breaker and replicas.
func Stats(name string) (DBStats, error) { return Default.StatsOf(name) }

// StatsOf is the database name's pool ("" the default).
func (s *Databases) StatsOf(name string) (DBStats, error) {
	d, err := s.Lookup(name)
	if err != nil {
		return DBStats{}, err
	}
	def, _ := s.Lookup("")
	return d.stats(d == def), nil
}

// Stats is every database's pool, by name.
func (s *Databases) Stats() []DBStats {
	var out []DBStats
	for _, n := range s.Names() {
		if st, err := s.StatsOf(n); err == nil {
			out = append(out, st)
		}
	}
	return out
}

func (d *DB) stats(def bool) DBStats {
	st := DBStats{Name: d.name, Dialect: d.dialect.Name(), Default: def, Pool: d.sql.Stats(), State: Up, Breaker: "closed"}
	s := d.sup
	if s == nil {
		return st
	}
	st.Supervised, st.State = true, State(s.state.Load())
	st.Breaker, st.Failures = s.br.status()
	if e := s.lastErr.Load(); e != nil {
		st.LastError = *e
	}
	for _, r := range s.replicas {
		st.Replicas = append(st.Replicas, ReplicaStats{URL: r.url, Healthy: r.healthy.Load(), Pool: r.sql.Stats()})
	}
	return st
}

// State is the database's state: Up for one that isn't supervised.
func (d *DB) State() State {
	if d.sup == nil {
		return Up
	}
	return State(d.sup.state.Load())
}

// connector opens a supervised database's connections: with fresh
// credentials when it has Credentials, its session settings run on each,
// each wrapped to retire on its jittered lifetime or once its generation
// passes.
type connector struct {
	sup     *supervisor
	drv     driver.Driver
	dsn     string
	dialect string
	inner   driver.Connector // without credentials
	creds   func(ctx context.Context) (user, password string, err error)
	session []string
}

func (c *connector) Driver() driver.Driver { return c.drv }

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	inner := c.inner
	if c.creds != nil {
		user, pass, err := c.creds(ctx)
		if err != nil {
			return nil, fmt.Errorf("orm: credentials for %s: %w", c.sup.label, err)
		}
		h := driverHooks(c.dialect)
		if h.Credentials == nil {
			return nil, fmt.Errorf("orm: the %s driver takes no credentials: blank-import its orm package (github.com/vuka-lang/orm/%s)", c.dialect, c.dialect)
		}
		dsn, err := h.Credentials(c.dsn, user, pass)
		if err != nil {
			return nil, err
		}
		if c.dialect == "mysql" {
			dsn = MySQLDSN(dsn)
		}
		if inner, err = openConnector(c.drv, dsn); err != nil {
			return nil, err
		}
	}
	cn, err := inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	for _, q := range c.session {
		if err := execDriver(ctx, cn, q); err != nil {
			cn.Close()
			return nil, fmt.Errorf("orm: %s: session setting %q: %w", c.sup.label, q, err)
		}
	}
	w := &wconn{Conn: cn, sup: c.sup, gen: c.sup.gen.Load()}
	if lt := c.sup.pool.MaxLifetime; lt > 0 {
		w.deadline = time.Now().Add(jitteredLifetime(lt, rand.Float64()))
	}
	return w, nil
}

// jitteredLifetime is a connection's lifetime: d less up to a fifth, by
// r in [0, 1).
func jitteredLifetime(d time.Duration, r float64) time.Duration {
	return d - time.Duration(float64(d)*0.2*r)
}

type dsnConnector struct {
	drv driver.Driver
	dsn string
}

func (c dsnConnector) Connect(context.Context) (driver.Conn, error) { return c.drv.Open(c.dsn) }
func (c dsnConnector) Driver() driver.Driver                        { return c.drv }

func openConnector(drv driver.Driver, dsn string) (driver.Connector, error) {
	if dc, ok := drv.(driver.DriverContext); ok {
		return dc.OpenConnector(dsn)
	}
	return dsnConnector{drv, dsn}, nil
}

func execDriver(ctx context.Context, cn driver.Conn, q string) error {
	if e, ok := cn.(driver.ExecerContext); ok {
		if _, err := e.ExecContext(ctx, q, nil); !errors.Is(err, driver.ErrSkip) {
			return err
		}
	}
	st, err := cn.Prepare(q)
	if err != nil {
		return err
	}
	defer st.Close()
	if se, ok := st.(driver.StmtExecContext); ok {
		_, err = se.ExecContext(ctx, nil)
	} else {
		_, err = st.Exec(nil) //nolint:staticcheck // drivers without StmtExecContext
	}
	return err
}

// wconn is a supervised database's connection: the driver's, retiring
// once its lifetime or generation passes or it failed, passing every
// optional interface through.
type wconn struct {
	driver.Conn
	sup      *supervisor
	gen      int64
	deadline time.Time
	bad      atomic.Bool
}

func (c *wconn) note(err error) error {
	if classify(err) == ConnError {
		c.bad.Store(true)
	}
	return err
}

func (c *wconn) IsValid() bool {
	if c.bad.Load() || c.gen < c.sup.gen.Load() || !c.deadline.IsZero() && time.Now().After(c.deadline) {
		return false
	}
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (c *wconn) ResetSession(ctx context.Context) error {
	if !c.IsValid() {
		return driver.ErrBadConn
	}
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return c.note(r.ResetSession(ctx))
	}
	return nil
}

func (c *wconn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return c.note(p.Ping(ctx))
	}
	return nil
}

func (c *wconn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if e, ok := c.Conn.(driver.ExecerContext); ok {
		r, err := e.ExecContext(ctx, q, args)
		return r, c.note(err)
	}
	return nil, driver.ErrSkip
}

func (c *wconn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if e, ok := c.Conn.(driver.QueryerContext); ok {
		r, err := e.QueryContext(ctx, q, args)
		return r, c.note(err)
	}
	return nil, driver.ErrSkip
}

func (c *wconn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		st, err := p.PrepareContext(ctx, q)
		return st, c.note(err)
	}
	st, err := c.Conn.Prepare(q)
	return st, c.note(err)
}

func (c *wconn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		tx, err := b.BeginTx(ctx, opts)
		return tx, c.note(err)
	}
	if opts.Isolation != 0 || opts.ReadOnly {
		return nil, errors.New("orm: the driver takes no transaction options")
	}
	tx, err := c.Conn.Begin() //nolint:staticcheck // drivers without ConnBeginTx
	return tx, c.note(err)
}

func (c *wconn) CheckNamedValue(nv *driver.NamedValue) error {
	if n, ok := c.Conn.(driver.NamedValueChecker); ok {
		return n.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}
