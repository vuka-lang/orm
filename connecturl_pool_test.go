package orm_test

import (
	"context"
	"testing"

	"github.com/vuka-lang/orm/v2"
)

// ConnectURL reads pool_* parameters as the pool's and keeps them out of the
// driver's DSN, so a URL carrying them (as a framework's DATABASE_URL may)
// opens cleanly instead of the driver rejecting an unknown parameter.
func TestConnectURLPoolParams(t *testing.T) {
	dir := t.TempDir()
	db, err := orm.ConnectURL("sqlite://" + dir + "/app.db?pool_max=7&pool_max_lifetime=30m&pool_max_idle=3")
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL().Close()
	if err := db.SQL().PingContext(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if got := db.SQL().Stats().MaxOpenConnections; got != 7 {
		t.Errorf("pool_max not applied: MaxOpenConnections = %d, want 7", got)
	}
}
