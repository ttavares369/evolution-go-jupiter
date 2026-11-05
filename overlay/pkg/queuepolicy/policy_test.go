package queuepolicy

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestPolicyExplicitAllowlistValidation(t *testing.T) {
	for _, value := range []string{"not-a-uuid", "00000000-0000-0000-0000-000000000000", "10000000-0000-4000-8000-000000000001,", "10000000-0000-4000-8000-00000000000A"} {
		if _, err := Initialize(context.Background(), nil, value); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("invalid list accepted: %q", value)
		}
	}
}

func TestPolicyDefaultOffHasNoDDLAndObservesNewTable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery("SELECT to_regclass").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	check, err := Initialize(context.Background(), db, "")
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT to_regclass").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	if exclusive, err := check(context.Background(), "fixture"); err != nil || exclusive {
		t.Fatal("default off changed")
	}
	mock.ExpectQuery("SELECT to_regclass").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery("SELECT EXISTS").WithArgs("fixture").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	if exclusive, err := check(context.Background(), "fixture"); err != nil || !exclusive {
		t.Fatal("activation in another producer ignored")
	}
	mock.ExpectQuery("SELECT to_regclass").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	if exclusive, err := check(context.Background(), "fixture"); !errors.Is(err, ErrUnavailable) || exclusive {
		t.Fatal("disappeared policy table silently opened legacy")
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyFailuresNeverOpenLegacy(t *testing.T) {
	gate.RLock()
	old := gate.check
	gate.RUnlock()
	defer func() { gate.Lock(); gate.check = old; gate.Unlock() }()
	gate.Lock()
	gate.check = nil
	gate.Unlock()
	if !errors.Is(Legacy("fixture"), ErrUnavailable) {
		t.Fatal("missing runtime installation opened legacy")
	}
	for _, check := range []Checker{nil, func(context.Context, string) (bool, error) { return false, errors.New("private database error") }} {
		Install(check)
		if !errors.Is(Legacy("fixture"), ErrUnavailable) {
			t.Fatal("failed policy opened legacy")
		}
		if exclusive, err := Exclusive(context.Background(), "fixture"); err == nil || exclusive {
			t.Fatal("failed gate advertised exclusivity")
		}
	}
	Install(func(_ context.Context, id string) (bool, error) { return id == "exclusive", nil })
	if Legacy("legacy") != nil || !errors.Is(Legacy("exclusive"), ErrExclusive) {
		t.Fatal("UUID policy not isolated")
	}
}
