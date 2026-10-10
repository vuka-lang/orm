package orm_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/vuka-lang/orm"
	"github.com/vuka-lang/orm/ormtest"
)

// Member is a table between two others, keyed by both.
type Member struct {
	GroupID int64 `gorm:"column:group_id;primaryKey"`
	UserID  int64 `gorm:"column:user_id;primaryKey"`
}

// Account has one Settings row holding its key (GORM's has-one).
type Account struct {
	ID       int64
	Name     string
	Settings *Settings `gorm:"foreignKey:AccountID"`
}

type Settings struct {
	ID        int64
	AccountID int64
	Theme     string
}

var (
	Members   = orm.For[Member]()
	Accounts  = orm.For[Account]()
	Settings_ = orm.For[Settings]()
)

func TestCompositeKey(t *testing.T) {
	ctx := ormtest.Open(t, Members)
	if err := Members.BulkCreate(ctx, []*Member{{GroupID: 1, UserID: 7}, {GroupID: 1, UserID: 8}, {GroupID: 2, UserID: 7}}); err != nil {
		t.Fatal(err)
	}
	if n, err := Members.Filter(orm.Q{"group_id": 1}).Delete(ctx); err != nil || n != 2 {
		t.Fatalf("delete = %d, %v", n, err)
	}
	if err := Members.Save(ctx, &Member{GroupID: 2, UserID: 7}); err == nil || !strings.Contains(err.Error(), "primary key") {
		t.Fatalf("saved a row of a composite key: %v", err)
	}
	left, _ := Members.Values[int64]("user_id").All(ctx)
	if !slices.Equal(left, []int64{7}) {
		t.Fatalf("left %v", left)
	}
}

func TestHasOne(t *testing.T) {
	ctx := ormtest.Open(t, Accounts, Settings_)
	_ = Accounts.BulkCreate(ctx, []*Account{{Name: "ali"}, {Name: "neema"}})
	_ = Settings_.Create(ctx, &Settings{AccountID: 1, Theme: "dark"})
	got, err := Accounts.PrefetchRelated("settings").OrderBy("id").All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Settings == nil || got[0].Settings.Theme != "dark" || got[1].Settings != nil {
		t.Fatalf("has-one = %+v, %+v", got[0].Settings, got[1].Settings)
	}
	names, err := Accounts.Filter(orm.Q{"settings__theme": "dark"}).Values[string]("name").All(ctx)
	if err != nil || !slices.Equal(names, []string{"ali"}) {
		t.Fatalf("lookup across a has-one = %v, %v", names, err)
	}
}
