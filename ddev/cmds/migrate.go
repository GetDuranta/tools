package cmds

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/rds/auth"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/golang-migrate/migrate/v4"
	migpgx "github.com/golang-migrate/migrate/v4/database/pgx"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // Import for the Postgres driver registration
	_ "github.com/golang-migrate/migrate/v4/source/file"     // Import for the file driver
	"github.com/spf13/cobra"
)

func GetMigrateCommand(globalCfg *GlobalConfig) *cobra.Command {
	var username string

	var sqlCommand = &cobra.Command{
		Use:   "migrate <source path> <db-cluster-name> <database-name> [args...]",
		Short: "migrate the RDS database using Go Migration",
		Args:  cobra.MinimumNArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			sourcePath := args[0]
			cluster := args[1]
			dbName := args[2]

			awsConfig, err := loadConfig(globalCfg)
			if err != nil {
				return err
			}

			certPath, err := ensureCaCerts()
			if err != nil {
				return fmt.Errorf("failed to prepare the CA bundle: %v", err)
			}

			conn := rds.NewFromConfig(awsConfig)
			cls, err := conn.DescribeDBClusters(context.TODO(), &rds.DescribeDBClustersInput{
				DBClusterIdentifier: aws.String(cluster),
				IncludeShared:       aws.Bool(true),
			})
			if err != nil {
				return fmt.Errorf("failed to get the database %v", err)
			}
			if len(cls.DBClusters) == 0 {
				return fmt.Errorf("failed to find the database %s", cluster)
			}
			db := cls.DBClusters[0]

			endpoint := fmt.Sprintf("%s:%d", *db.Endpoint, *db.Port)

			dbToken, err := auth.BuildAuthToken(
				context.TODO(), endpoint, awsConfig.Region, username, awsConfig.Credentials)
			if err != nil {
				return fmt.Errorf("failed to create authentication token: %v", err)
			}

			dsn := fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslrootcert=%s sslmode=verify-full password='%s'",
				*db.Endpoint, *db.Port, username, dbName, certPath, dbToken,
			)

			database, err := sql.Open("pgx", dsn)
			if err != nil {
				return fmt.Errorf("failed to open the driver: %w", err)
			}

			migrator, err := migpgx.WithInstance(database, &migpgx.Config{
				DatabaseName: dbName,
				SchemaName:   "public",
			})
			if err != nil {
				return fmt.Errorf("failed to create the migrator, %w", err)
			}

			sourceUrl := fmt.Sprintf("file://%v", sourcePath)
			m, err := migrate.NewWithDatabaseInstance(sourceUrl, "pgx", migrator)
			if err != nil {
				return fmt.Errorf("failed to read the migration status: %w", err)
			}
			m.Log = &Log{verbose: true}

			err = m.Up()
			if errors.Is(err, migrate.ErrNoChange) {
				version, _, _ := m.Version()
				slog.Info("There are no changes to apply", "cur_version", version)
				return nil
			}
			if err != nil {
				return fmt.Errorf("failed to run the migrations: %w", err)
			}
			version, _, _ := m.Version()
			slog.Info("Applied the changes", "new_version", version)

			return nil
		},
	}

	sqlCommand.Flags().StringVarP(&username, "username", "u", "postgres",
		"username for the connection")
	sqlCommand.Flags().SetInterspersed(false)

	return sqlCommand
}
