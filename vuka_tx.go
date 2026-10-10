package orm

import (
	"context"
	"fmt"
	"reflect"
	"unsafe"

	"github.com/vuka-lang/vuka"
)

// Transaction is a Vuka call decorator running the function in a
// transaction (Atomic): committed when it succeeds, rolled back when it
// returns an error or an Err Result, or panics. Its context argument is
// the transaction's, so the queries it makes with it take part; nested,
// it is a savepoint. The first write picks the database (the written
// model's); @orm.TxOn("analytics") beside it names one, @orm.Retry(3)
// retries deadlocks.
//
//	@orm.Transaction
//	func publish(ctx context.Context, id int64) error {
//		p := Post.Objects.Get(ctx, Post.ID.Eq(id))?
//		p.Published = Some(time.Now())
//		return p.Save(ctx)
//	}
func Transaction(c *vuka.Call) {
	i := ctxArg(c)
	if i < 0 {
		panic(fmt.Sprintf("orm: %s is a transaction but takes no context.Context", c.Name))
	}
	var opts []TxOption
	var on TxOn
	if c.Attr(&on) {
		opts = append(opts, on)
	}
	var retry Retry
	if c.Attr(&retry) {
		opts = append(opts, retry)
	}
	outer := c.Args[i]
	err := Atomic(c.Context(), func(ctx context.Context) error {
		c.Args[i] = ctx
		c.Next()
		return callErr(c)
	}, opts...)
	c.Args[i] = outer
	if err != nil && callErr(c) == nil { // the commit failed, or no database
		failCall(c, err)
	}
}

// ctxArg is the index of the call's context.Context argument, -1 for none.
func ctxArg(c *vuka.Call) int {
	for i, a := range c.Args {
		if _, ok := a.(context.Context); ok {
			return i
		}
	}
	return -1
}

type resulter interface {
	IsErr() bool
	Err() error
}

// callErr is the call's failure: its error result, or an Err Result's.
func callErr(c *vuka.Call) error {
	if err := c.Err(); err != nil {
		return err
	}
	for _, r := range c.Results {
		if x, ok := r.(resulter); ok && x.IsErr() {
			return x.Err()
		}
	}
	return nil
}

// failCall makes the call fail with err: its error result, or its Result
// an Err.
func failCall(c *vuka.Call, err error) {
	if len(c.Results) == 0 {
		panic(err)
	}
	for i, r := range c.Results {
		if _, ok := r.(error); ok || r == nil && i == len(c.Results)-1 {
			defer func() {
				if recover() != nil {
					panic(err)
				}
			}()
			c.SetErr(err)
			return
		}
		if _, ok := r.(resulter); ok {
			v := reflect.New(reflect.TypeOf(r)).Elem() // a zero Result: its err field set below
			f := v.FieldByName("err")
			reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Set(reflect.ValueOf(&err).Elem())
			c.Results[i] = v.Interface()
			return
		}
	}
	panic(err)
}

// OnCommit runs fn once ctx's transaction commits — at once outside one;
// a rolled back transaction never runs it, nor a savepoint's rolled back
// part. The way to send mail or enqueue work (an outbox) only for writes
// that stay.
func OnCommit(ctx context.Context, fn func(ctx context.Context)) error {
	if f, ok := ctx.Value(atomicKey{}).(*frame); ok {
		f.after = append(f.after, fn)
		return nil
	}
	db, err := defaultDB(ctx)
	if err != nil {
		return err
	}
	afterCommit(ctx, db.sql, fn)
	return nil
}
