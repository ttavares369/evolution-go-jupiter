package queuepolicy

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

const activationTestID = "10000000-0000-4000-8000-000000000001"

func TestActivationCanonicalUUIDBeforeSQL(t *testing.T) {
	for _, id := range []string{"", "not-uuid", "00000000-0000-0000-0000-000000000000", "10000000-0000-4000-8000-00000000000A"} {
		if err := Activate(context.Background(), nil, id); !errors.Is(err, ErrInvalidInstance) {
			t.Fatalf("invalid UUID accepted: %q %v", id, err)
		}
	}
	if err := Activate(context.Background(), nil, activationTestID); !errors.Is(err, ErrUnavailable) {
		t.Fatal("nil DB accepted")
	}
}

func TestActivationEverySQLBoundaryFailsClosed(t *testing.T) {
	for _, phase := range []string{"begin", "lock", "missing", "instance-query", "wrong-instance", "ddl", "insert", "commit", "success"} {
		t.Run(phase, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			private := errors.New("private-driver-token-sentinel")
			expected := ErrUnavailable
			if phase == "begin" {
				mock.ExpectBegin().WillReturnError(private)
			} else {
				mock.ExpectBegin()
				if phase == "lock" {
					mock.ExpectExec(regexp.QuoteMeta(exclusivePolicyLock)).WillReturnError(private)
				} else {
					mock.ExpectExec(regexp.QuoteMeta(exclusivePolicyLock)).WillReturnResult(sqlmock.NewResult(0, 1))
					lookup := mock.ExpectQuery("SELECT id FROM instances WHERE id=\\$1 FOR KEY SHARE").WithArgs(activationTestID)
					switch phase {
					case "missing":
						lookup.WillReturnError(sql.ErrNoRows)
						expected = ErrInstanceNotFound
					case "instance-query":
						lookup.WillReturnError(private)
					case "wrong-instance":
						lookup.WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("other"))
					default:
						lookup.WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(activationTestID))
						ddl := mock.ExpectExec(regexp.QuoteMeta(exclusiveTableDDL))
						if phase == "ddl" {
							ddl.WillReturnError(private)
						} else {
							ddl.WillReturnResult(sqlmock.NewResult(0, 0))
							insert := mock.ExpectExec("INSERT INTO queue_v1_exclusive_instances.*ON CONFLICT DO NOTHING").WithArgs(activationTestID)
							if phase == "insert" {
								insert.WillReturnError(private)
							} else {
								insert.WillReturnResult(sqlmock.NewResult(0, 1))
								if phase == "commit" {
									mock.ExpectCommit().WillReturnError(private)
								} else {
									mock.ExpectCommit()
									expected = nil
								}
							}
						}
					}
				}
				if phase != "commit" && phase != "success" {
					mock.ExpectRollback()
				}
			}
			err = Activate(context.Background(), db, activationTestID)
			if expected == nil && err != nil || expected != nil && !errors.Is(err, expected) {
				t.Fatalf("unexpected result: %v", err)
			}
			if err != nil && err.Error() == private.Error() {
				t.Fatal("driver secret escaped")
			}
			if err = mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConfigurationAndAdminSharePolicyTransaction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery("SELECT to_regclass").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(exclusivePolicyLock)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(exclusiveTableDDL)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO queue_v1_exclusive_instances.*ON CONFLICT DO NOTHING").WithArgs(activationTestID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if _, err = Initialize(context.Background(), db, activationTestID); err != nil {
		t.Fatal(err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
