package orm

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
)

// ModelInfo is a model as a Router sees it: its Go type, its table and
// the databases it declares (Meta's DB, Read, Write and Mirror, with
// For's On and Mirror over them).
type ModelInfo struct {
	Name  string // the Go type's name
	Table string
	Type  reflect.Type
	Meta  Meta
}

// Router picks the databases of models' queries at run time, Django's
// database routers: a tenant's database from the context, an analytics
// model on its own server. Register routers on a Databases (Routers, or
// AddRouter); the first giving an opinion wins.
//
// ReadDB and WriteDB name the database a model's reads and writes go to,
// "" for no opinion. AllowRelation says whether a relation between two
// models may be declared, AllowMigrate whether a model's table is
// migrated on a database: being bools, they can't abstain, so every
// router must allow (embed BaseRouter to allow by default).
//
// A router's choice yields to orm.Using; it wins over a model's Meta and
// a table's Route. Migrations run before main: a router that should move
// a model's migrations is registered on Default at init.
type Router interface {
	ReadDB(ctx context.Context, m ModelInfo) string
	WriteDB(ctx context.Context, m ModelInfo) string
	AllowRelation(a, b ModelInfo) bool
	AllowMigrate(db string, m ModelInfo) bool
}

// BaseRouter is a Router with no opinion that allows everything: embed it
// and override what the router decides.
type BaseRouter struct{}

func (BaseRouter) ReadDB(context.Context, ModelInfo) string  { return "" }
func (BaseRouter) WriteDB(context.Context, ModelInfo) string { return "" }
func (BaseRouter) AllowRelation(a, b ModelInfo) bool         { return true }
func (BaseRouter) AllowMigrate(string, ModelInfo) bool       { return true }

// AddRouter adds r after the routers s has.
func (s *Databases) AddRouter(r Router) *Databases {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routers = append(slices.Clip(s.routers), r)
	return s
}

func (s *Databases) routerList() []Router {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.routers
}

// dbFor is the database name a model's reads (or writes) go to by s's
// routers, the model's Meta, then its table's Route; "" the default.
func (s *Databases) dbFor(ctx context.Context, mi ModelInfo, write bool) string {
	for _, r := range s.routerList() {
		n := r.ReadDB(ctx, mi)
		if write {
			n = r.WriteDB(ctx, mi)
		}
		if n != "" {
			return n
		}
	}
	if write {
		if n := cmp.Or(mi.Meta.Write, mi.Meta.DB); n != "" {
			return n
		}
	} else if n := cmp.Or(mi.Meta.Read, mi.Meta.DB); n != "" {
		return n
	}
	if r, ok := s.route(mi.Table); ok && r.DB != "" {
		return r.DB
	}
	return ""
}

// mirrorFor is where a model's writes are repeated: its Meta's (For's)
// Mirror, else its table's Route's.
func (s *Databases) mirrorFor(mi ModelInfo) string {
	if mi.Meta.Mirror != "" {
		return mi.Meta.Mirror
	}
	if r, ok := s.route(mi.Table); ok {
		return r.Mirror
	}
	return ""
}

// infoOf is the ModelInfo of the model type t: its own manager's, else
// what its Meta declares.
func infoOf(t reflect.Type) ModelInfo {
	if v, ok := homes.Load(t); ok {
		if m, ok := v.(homeEntry).m.(interface{ Info() ModelInfo }); ok {
			return m.Info()
		}
	}
	mi := ModelInfo{Name: t.Name(), Type: t}
	if m, err := modelOf(t, "", ""); err == nil {
		mi.Table, mi.Meta = m.Table, m.meta
	}
	return mi
}

// primaryKey marks a context whose reads go to the write database.
type primaryKey struct{}

// Primary has ctx's reads go where the models write: never to a replica,
// nor a read database a Meta or router names. For a read that must see a
// write just made outside a Scope.
func Primary(ctx context.Context) context.Context {
	return context.WithValue(ctx, primaryKey{}, true)
}

// scope is a request's writes, as Scope records them.
type scope struct {
	mu     sync.Mutex
	tables map[string]string // table → the database it was written on
	dbs    map[string]bool
}

type scopeKey struct{}

// Scope starts a read-your-writes scope on ctx, a request's: once a
// query with it (or a context made from it) writes a model, its reads of
// that model go to the database written, not a replica or a read
// database; raw reads of a database written go to its primary.
// Middleware starts one per request (ormweb does). ReadYourWrites(false)
// on the Databases turns it off.
func Scope(ctx context.Context) context.Context {
	return context.WithValue(ctx, scopeKey{}, &scope{tables: map[string]string{}, dbs: map[string]bool{}})
}

func scopeOf(ctx context.Context) *scope {
	sc, _ := ctx.Value(scopeKey{}).(*scope)
	return sc
}

func (sc *scope) wrote(table, db string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if table != "" {
		sc.tables[table] = db
	}
	sc.dbs[db] = true
}

func (sc *scope) written(table string) (string, bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	db, ok := sc.tables[table]
	return db, ok
}

func (sc *scope) wroteDB(db string) bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.dbs[db]
}

// resolve is the database a statement goes to, as ctx sees it: WithDB's;
// else Using's, the routers', the model's Meta, its table's Route or the
// default — the read side (a replica when the database has some) or the
// write side, which a transaction, a Scope that wrote the model or
// Primary take reads to as well. Writes take part in ctx's Atomic (whose
// first write picks its database), and a Scope records them. mi is nil
// for raw SQL, routed by name alone.
func resolve(ctx context.Context, s *Databases, mi *ModelInfo, name string, write bool) (conn, error) {
	if d, ok := ctx.Value(dbKey{}).(*DB); ok {
		return dbConnTo(ctx, d, name, write, false)
	}
	sc := scopeOf(ctx)
	if !s.ryw() {
		sc = nil
	}
	pinned := write || ctx.Value(primaryKey{}) != nil || ctx.Value(atomicKey{}) != nil
	if n, ok := ctx.Value(usingKey{}).(string); ok {
		name = n
	} else if mi != nil {
		if !pinned && sc != nil {
			if db, ok := sc.written(mi.Table); ok {
				name, pinned = db, true
			}
		}
		if name == "" {
			name = s.dbFor(ctx, *mi, write || pinned)
		}
	}
	d, err := s.Lookup(name)
	if err != nil {
		return conn{}, err
	}
	if !pinned && sc != nil && sc.wroteDB(d.name) {
		pinned = true
	}
	if write && sc != nil {
		table := ""
		if mi != nil {
			table = mi.Table
		}
		sc.wrote(table, d.name)
	}
	return dbConnTo(ctx, d, d.name, write, !pinned)
}

// dbConnTo is d for a statement: inside ctx's transaction on it, or a
// write binding ctx's Atomic to it; a read on a healthy replica when
// replicaOK.
func dbConnTo(ctx context.Context, d *DB, name string, write, replicaOK bool) (conn, error) {
	if write && d.readOnly {
		return conn{}, fmt.Errorf("orm: %s is read-only", dbLabel(cmp.Or(name, d.name)))
	}
	c := on(ctx, d)
	c.name, c.read = cmp.Or(d.name, name), !write
	if c.run != d.sql {
		return c, nil // in a transaction
	}
	if f, ok := ctx.Value(atomicKey{}).(*frame); ok && write {
		st, err := f.bind(d)
		if err != nil {
			return conn{}, err
		}
		c.run = st.tx
		return c, nil
	}
	if replicaOK && !write && d.sup != nil {
		if r := d.sup.replica(); r != nil {
			c.run, c.db, c.rep = r.sql, r.sql, r
		}
	}
	return c, nil
}

// relationCheck is what's wrong with the relations of the models routed
// in s: a relation between models on two databases that isn't CrossDB,
// or one a router refuses.
func (s *Databases) relationCheck(models []ModelInfo) []error {
	var errs []error
	ctx := context.Background()
	for _, a := range models {
		m, err := modelOf(a.Type, "", "")
		if err != nil {
			continue
		}
		for _, r := range m.relList {
			if r.Kind != relFK && r.Kind != relM2M {
				continue
			}
			b := infoOf(r.Target)
			for _, rt := range s.routerList() {
				if !rt.AllowRelation(a, b) {
					errs = append(errs, fmt.Errorf("orm: a router refuses the relation %s.%s to %s", a.Name, r.Name, b.Name))
				}
			}
			da, db := s.dbFor(ctx, a, true), s.dbFor(ctx, b, true)
			if s.canonical(da) != s.canonical(db) && !r.crossDB() {
				errs = append(errs, fmt.Errorf("orm: %s.%s relates %s on %s to %s on %s: mark it CrossDB (@orm.Rel{CrossDB: true}, orm:\"crossdb\") or put both on one database", a.Name, r.Name, a.Name, dbLabel(da), b.Name, dbLabel(db)))
			}
		}
	}
	return errs
}

// canonical is the declared name "" (the default) stands for.
func (s *Databases) canonical(name string) string {
	if name != "" {
		return name
	}
	if d, err := s.Lookup(""); err == nil {
		return d.name
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sp := range s.specs {
		if sp.def {
			return sp.name
		}
	}
	return ""
}

// models is every model the program has a manager of: Register's, and
// For's and Objects's own managers.
func knownModels() []ModelInfo {
	seen := map[reflect.Type]bool{}
	var out []ModelInfo
	add := func(m AnyManager) {
		mi, ok := m.(interface{ Info() ModelInfo })
		if !ok || m.Err() != nil {
			return
		}
		i := mi.Info()
		if i.Type != nil && !seen[i.Type] {
			seen[i.Type] = true
			out = append(out, i)
		}
	}
	for _, m := range registered() {
		add(m)
	}
	homes.Range(func(_, v any) bool {
		if m, ok := v.(homeEntry).m.(AnyManager); ok {
			add(m)
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Check is what keeps s from serving the program's models: a Meta, For
// option, Route or connection handle naming a database s doesn't declare,
// and relations across databases that aren't CrossDB or a router
// refuses. Start runs it.
func (s *Databases) Check() error {
	declared := map[string]bool{}
	for _, n := range s.Names() {
		declared[n] = true
	}
	s.mu.RLock()
	for _, sp := range s.specs {
		declared[sp.name] = true
	}
	s.mu.RUnlock()
	var errs []error
	need := func(name, who string) {
		if name != "" && !declared[name] {
			errs = append(errs, fmt.Errorf("orm: %s names database %q, which isn't declared (have %s)", who, name, strings.Join(sortedKeys(declared), ", ")))
		}
	}
	models := knownModels()
	for _, m := range models {
		need(m.Meta.DB, m.Name+"'s Meta.DB")
		need(m.Meta.Read, m.Name+"'s Meta.Read")
		need(m.Meta.Write, m.Name+"'s Meta.Write")
		need(m.Meta.Mirror, m.Name+"'s Meta.Mirror")
	}
	s.routes.Range(func(k, v any) bool {
		r := v.(Route)
		need(r.DB, "the route of table "+k.(string))
		need(r.Mirror, "the route of table "+k.(string))
		return true
	})
	for _, h := range connHandles() {
		need(h.name, "connection handle "+h.t.String())
	}
	if len(declared) > 0 {
		errs = append(errs, s.relationCheck(models)...)
	}
	return errors.Join(errs...)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, cmp.Or(k, `""`))
	}
	slices.Sort(out)
	return out
}
