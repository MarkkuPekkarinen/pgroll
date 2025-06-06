// SPDX-License-Identifier: Apache-2.0

package benchmarks

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/oapi-codegen/nullable"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

const (
	unitRowsPerSecond       = "rows/s"
	unitExecutionsPerSecond = "executions/s"
)

var (
	rowCounts = []int{10_000, 100_000, 300_000}
	reports   = newReports()
)

func TestMain(m *testing.M) {
	testutils.SharedTestMain(m, func() (err error) {
		// Only run in GitHub actions
		if os.Getenv("GITHUB_ACTIONS") != "true" {
			return nil
		}

		w, err := os.Create(fmt.Sprintf("benchmark_result_%s.json", getPostgresVersion()))
		if err != nil {
			return fmt.Errorf("creating report file: %w", err)
		}
		defer func() {
			err = w.Close()
		}()

		encoder := json.NewEncoder(w)
		if err := encoder.Encode(reports); err != nil {
			return fmt.Errorf("encoding report file: %w", err)
		}
		return nil
	})
}

func BenchmarkBackfill(b *testing.B) {
	ctx := context.Background()
	testSchema := testutils.TestSchema()
	var opts []roll.Option

	for _, rowCount := range rowCounts {
		b.Run(strconv.Itoa(rowCount), func(b *testing.B) {
			testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(b, testSchema, opts, func(mig *roll.Roll, db *sql.DB) {
				b.Cleanup(func() {
					require.NoError(b, mig.Close())
				})

				setupInitialTable(b, ctx, testSchema, mig, db, rowCount)
				b.ResetTimer()

				// Backfill
				b.StartTimer()
				require.NoError(b, mig.Start(ctx, &migAlterColumn, backfill.NewConfig()))
				require.NoError(b, mig.Complete(ctx))
				b.StopTimer()
				b.Logf("Backfilled %d rows in %s", rowCount, b.Elapsed())
				rowsPerSecond := float64(rowCount) / b.Elapsed().Seconds()
				b.ReportMetric(rowsPerSecond, unitRowsPerSecond)

				addRowsPerSecond(b, rowCount, rowsPerSecond)
			})
		})
	}
}

// Benchmark the difference between updating all rows with and without an update trigger in place
func BenchmarkWriteAmplification(b *testing.B) {
	ctx := context.Background()
	testSchema := testutils.TestSchema()
	var opts []roll.Option

	assertRowCount := func(tb testing.TB, db *sql.DB, rowCount int) {
		tb.Helper()

		var count int
		err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE name = 'person'").Scan(&count)
		require.NoError(b, err)
		require.Equal(b, rowCount, count)
	}

	b.Run("NoTrigger", func(b *testing.B) {
		for _, rowCount := range rowCounts {
			b.Run(strconv.Itoa(rowCount), func(b *testing.B) {
				testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(b, testSchema, opts, func(mig *roll.Roll, db *sql.DB) {
					setupInitialTable(b, ctx, testSchema, mig, db, rowCount)
					b.Cleanup(func() {
						require.NoError(b, mig.Close())
						assertRowCount(b, db, rowCount)
					})

					b.ResetTimer()

					// Update the name in all rows
					b.StartTimer()
					_, err := db.ExecContext(ctx, `UPDATE users SET name = 'person'`)
					require.NoError(b, err)
					b.StopTimer()
					rowsPerSecond := float64(rowCount) / b.Elapsed().Seconds()
					b.ReportMetric(rowsPerSecond, unitRowsPerSecond)

					addRowsPerSecond(b, rowCount, rowsPerSecond)
				})
			})
		}
	})

	b.Run("WithTrigger", func(b *testing.B) {
		for _, rowCount := range rowCounts {
			b.Run(strconv.Itoa(rowCount), func(b *testing.B) {
				testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(b, testSchema, opts, func(mig *roll.Roll, db *sql.DB) {
					setupInitialTable(b, ctx, testSchema, mig, db, rowCount)

					// Start the migration
					require.NoError(b, mig.Start(ctx, &migAlterColumn, backfill.NewConfig()))
					b.Cleanup(func() {
						// Finish the migration
						require.NoError(b, mig.Complete(ctx))
						require.NoError(b, mig.Close())
						assertRowCount(b, db, rowCount)
					})

					b.ResetTimer()

					// Update the name in all rows
					b.StartTimer()
					_, err := db.ExecContext(ctx, `UPDATE users SET name = 'person'`)
					require.NoError(b, err)
					b.StopTimer()
					rowsPerSecond := float64(rowCount) / b.Elapsed().Seconds()
					b.ReportMetric(rowsPerSecond, unitRowsPerSecond)

					addRowsPerSecond(b, rowCount, rowsPerSecond)
				})
			})
		}
	})
}

func BenchmarkReadSchema(b *testing.B) {
	ctx := context.Background()
	testSchema := testutils.TestSchema()
	var opts []roll.Option

	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(b, testSchema, opts, func(mig *roll.Roll, db *sql.DB) {
		b.Cleanup(func() {
			require.NoError(b, mig.Close())
		})

		setupInitialTable(b, ctx, testSchema, mig, db, 1)
		b.ResetTimer()

		// We don't want this benchmark to test the network so instead we run the actual function in a tight
		// loop within a single execution.
		executions := 10000
		q := fmt.Sprintf(`SELECT %s.read_schema($1) FROM generate_series(1, $2);`, pq.QuoteIdentifier(mig.State().Schema()))
		_, err := db.ExecContext(ctx, q, testSchema, executions)
		b.StopTimer()
		require.NoError(b, err)
		perSecond := float64(executions) / b.Elapsed().Seconds()
		b.ReportMetric(perSecond, unitExecutionsPerSecond)

		reports.AddReport(BenchmarkReport{
			Name:     b.Name(),
			Unit:     unitExecutionsPerSecond,
			RowCount: executions,
			Result:   perSecond,
		})
	})
}

func setupInitialTable(tb testing.TB, ctx context.Context, testSchema string, mig *roll.Roll, db *sql.DB, rowCount int) {
	tb.Helper()

	seed := func(tb testing.TB, rowCount int, db *sql.DB) {
		tx, err := db.Begin()
		require.NoError(tb, err)
		defer tx.Rollback()

		stmt, err := tx.PrepareContext(ctx, pq.CopyInSchema(testSchema, "users", "name"))
		require.NoError(tb, err)

		for i := 0; i < rowCount; i++ {
			_, err = stmt.ExecContext(ctx, nil)
			require.NoError(tb, err)
		}

		_, err = stmt.ExecContext(ctx)
		require.NoError(tb, err)
		require.NoError(tb, tx.Commit())
	}

	// Setup
	require.NoError(tb, mig.Start(ctx, &migCreateTable, backfill.NewConfig()))
	require.NoError(tb, mig.Complete(ctx))
	seed(tb, rowCount, db)
}

func addRowsPerSecond(b *testing.B, rowCount int, perSecond float64) {
	reports.AddReport(BenchmarkReport{
		Name:     b.Name(),
		Unit:     unitRowsPerSecond,
		RowCount: rowCount,
		Result:   perSecond,
	})
}

// Simple table with a nullable `name` field.
var migCreateTable = migrations.Migration{
	Name: "01_create_table",
	Operations: migrations.Operations{
		&migrations.OpCreateTable{
			Name: "users",
			Columns: []migrations.Column{
				{
					Name: "id",
					Type: "serial",
					Pk:   true,
				},
				{
					Name:     "name",
					Type:     "varchar(255)",
					Nullable: true,
					Unique:   false,
				},
			},
		},
	},
}

// Alter the table to make the name field not null and backfill the old name fields with
// `placeholder`.
var migAlterColumn = migrations.Migration{
	Name: "02_alter_column",
	Operations: migrations.Operations{
		&migrations.OpAlterColumn{
			Table:    "users",
			Column:   "name",
			Up:       "SELECT CASE WHEN name IS NULL THEN 'placeholder' ELSE name END",
			Down:     "user_name",
			Comment:  nullable.NewNullableWithValue("the name of the user"),
			Nullable: ptr(false),
		},
	},
}

func ptr[T any](x T) *T { return &x }

func getPostgresVersion() string {
	return os.Getenv("POSTGRES_VERSION")
}

func newReports() *BenchmarkReports {
	return &BenchmarkReports{
		GitSHA:          os.Getenv("GITHUB_SHA"),
		PostgresVersion: getPostgresVersion(),
		Timestamp:       time.Now().Unix(),
		Reports:         []BenchmarkReport{},
	}
}

type BenchmarkReports struct {
	GitSHA          string
	PostgresVersion string
	Timestamp       int64
	Reports         []BenchmarkReport
}

func (r *BenchmarkReports) AddReport(report BenchmarkReport) {
	r.Reports = append(r.Reports, report)
}

type BenchmarkReport struct {
	Name     string
	RowCount int
	Unit     string
	Result   float64
}
