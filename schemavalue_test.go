package orm_test

import (
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"

	"github.com/vuka-lang/orm"
	"github.com/vuka-lang/orm/ormtest"
)

// TicketStatus is a name on the default schema and a code on the old one.
type TicketStatus string

const (
	TicketOpen   TicketStatus = "OPEN"
	TicketClosed TicketStatus = "CLOSED"
)

var ticketCodes = map[TicketStatus]int64{TicketOpen: 1, TicketClosed: 2}

func (s TicketStatus) ValueFor(names string) (driver.Value, error) {
	if names != "old" {
		return string(s), nil
	}
	c, ok := ticketCodes[s]
	if !ok {
		return nil, fmt.Errorf("no code for status %q", s)
	}
	return c, nil
}

// ScanFor reads a code on the old schema and a name elsewhere: it is told
// which, never guesses.
func (s *TicketStatus) ScanFor(names string, src any) error {
	if names != "old" {
		switch v := src.(type) {
		case []byte:
			*s = TicketStatus(v)
		case string:
			*s = TicketStatus(v)
		default:
			return fmt.Errorf("status %v is no name", src)
		}
		return nil
	}
	code, ok := src.(int64)
	if !ok {
		return fmt.Errorf("status %v is no code", src)
	}
	for name, c := range ticketCodes {
		if c == code {
			*s = name
			return nil
		}
	}
	return fmt.Errorf("unknown status code %d", code)
}

type Ticket struct {
	ID     int64
	Status TicketStatus `orm:"type:varchar(32)" old:"type:integer"`
}

var (
	Tickets    = orm.For[Ticket]()
	OldTickets = orm.For[Ticket](orm.Names("old"), orm.Table("tickets_old"))
)

func TestSchemaValuer(t *testing.T) {
	ctx := ormtest.Open(t, Tickets, OldTickets)
	for _, m := range []*orm.Manager[Ticket]{Tickets, OldTickets} {
		if err := m.Create(ctx, &Ticket{Status: TicketOpen}); err != nil {
			t.Fatal(err)
		}
		if err := m.Create(ctx, &Ticket{Status: TicketClosed}); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Filter(orm.Q{"status": TicketClosed}).Update(ctx, orm.Set{"status": TicketOpen}); err != nil {
			t.Fatal(err)
		}
		open, err := m.Filter(orm.Q{"status__in": []TicketStatus{TicketOpen}}).Count(ctx)
		if err != nil || open != 2 {
			t.Fatalf("open = %d, %v", open, err)
		}
		rows, err := m.OrderBy("id").All(ctx)
		if err != nil || len(rows) != 2 || rows[0].Status != TicketOpen {
			t.Fatalf("read %v, %v", rows, err)
		}
	}
	codes, err := orm.Raw[int64](ctx, "SELECT status FROM tickets_old ORDER BY id")
	if err != nil || len(codes) != 2 || codes[0] != 1 || codes[1] != 1 {
		t.Fatalf("stored codes %v, %v", codes, err)
	}
	names, err := orm.Raw[string](ctx, "SELECT status FROM tickets ORDER BY id")
	if err != nil || len(names) != 2 || names[0] != "OPEN" {
		t.Fatalf("stored names %v, %v", names, err)
	}

	// Every way of reading gives the name, on both schemas.
	for _, m := range []*orm.Manager[Ticket]{Tickets, OldTickets} {
		st, err := m.OrderBy("id").Values[TicketStatus]("status").All(ctx)
		if err != nil || len(st) != 2 || st[0] != TicketOpen {
			t.Fatalf("Values[TicketStatus] = %v, %v", st, err)
		}
		list, err := m.OrderBy("id").Values[[]any]("id", "status").All(ctx)
		if err != nil || list[0][1] != TicketOpen {
			t.Fatalf("Values[[]any] = %v, %v", list, err)
		}
		type row struct {
			ID     int64
			Status TicketStatus
		}
		rs, err := m.OrderBy("id").Values[row]().All(ctx)
		if err != nil || rs[0].Status != TicketOpen {
			t.Fatalf("Values[row] = %v, %v", rs, err)
		}
	}
	raw, err := orm.RawOn[TicketStatus](ctx, orm.DBSchema{Names: "old"}, "SELECT status FROM tickets_old ORDER BY id")
	if err != nil || len(raw) != 2 || raw[0] != TicketOpen {
		t.Fatalf("RawOn[TicketStatus] = %v, %v", raw, err)
	}

	type plain struct {
		ID     int64
		Status string
	}
	for _, c := range []struct {
		name string
		run  func() error
		want string
	}{
		{"Values into a string", func() error { _, err := OldTickets.Values[string]("status").All(ctx); return err },
			`orm: Values reads "status" into string, but the field is orm_test.TicketStatus, whose value differs per schema: read it into orm_test.TicketStatus`},
		{"Values into a plain struct", func() error { _, err := OldTickets.Values[plain]().All(ctx); return err },
			"read it into orm_test.TicketStatus"},
		{"Values into an int", func() error { _, err := OldTickets.Values[int64]("status").All(ctx); return err },
			"into int64"},
		{"an int condition", func() error { _, err := OldTickets.Filter(orm.Q{"status": 2}).Count(ctx); return err },
			"Ticket.Status is orm_test.TicketStatus, whose value differs per schema: pass orm_test.TicketStatus, not int (2)"},
		{"a string list", func() error {
			_, err := Tickets.Filter(orm.Q{"status__in": []string{"OPEN"}}).Count(ctx)
			return err
		}, "not string (OPEN)"},
		{"a string update", func() error {
			_, err := OldTickets.Filter(orm.Q{"id": 1}).Update(ctx, orm.Set{"status": "CLOSED"})
			return err
		},
			"not string (CLOSED)"},
		{"a value with no code", func() error { return OldTickets.Create(ctx, &Ticket{Status: "LOST"}) },
			`Ticket.Status: no code for status "LOST"`},
	} {
		if err := c.run(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}
