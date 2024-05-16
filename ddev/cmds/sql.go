package cmds

import (
	"context"
	_ "embed"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/rds/auth"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/frioux/shellquote"
	"github.com/spf13/cobra"
	"os"
	"os/exec"
	"path"
	"syscall"
)

//go:embed "rds-ca-certs.pem"
var rdsCaCertificates string

type SqlConfig struct {
	Username    string
	Readonly    bool
	PrintScript bool
}

func GetSqlCommand(globalCfg *GlobalConfig) *cobra.Command {
	sqlConfig := SqlConfig{}

	var sqlCommand = &cobra.Command{
		Use:   "sql <db-cluster-name> <database-name> [args...]",
		Short: "connect to the RDS database",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster := args[0]
			dbName := args[1]

			if cluster == "" {
				return fmt.Errorf("database name is required")
			}

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
			if sqlConfig.Readonly {
				if aws.ToString(db.ReaderEndpoint) == "" {
					return fmt.Errorf("no read-only endpoint available")
				}
				endpoint = fmt.Sprintf("%s:%d", *db.ReaderEndpoint, *db.Port)
			}

			dbToken, err := auth.BuildAuthToken(
				context.TODO(), endpoint, awsConfig.Region, sqlConfig.Username, awsConfig.Credentials)
			if err != nil {
				return fmt.Errorf("failed to create authentication token: %v", err)
			}

			dsn := fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslrootcert=%s sslmode=verify-full",
				*db.Endpoint, *db.Port, sqlConfig.Username, dbName, certPath,
			)

			if sqlConfig.PrintScript {
				fmt.Printf("export PGPASSWORD=\"" + dbToken + "\"\n")
				fmt.Printf("export DSN=\"" + dsn + "\"\n")
				return nil
			}

			binary, err := exec.LookPath("psql")
			if err != nil {
				return fmt.Errorf("psql command not found: %v", err)
			}
			env := os.Environ()
			env = append(env, "PGPASSWORD="+dbToken)

			psqlArgs := []string{"psql", dsn}
			if len(args) > 2 {
				psqlArgs = append(psqlArgs, args[2:]...)
			}
			err = syscall.Exec(binary, psqlArgs, env)
			if err != nil {
				return fmt.Errorf("failed to run psql: %v", err)
			}
			return nil
		},
	}

	sqlCommand.Flags().StringVarP(&sqlConfig.Username, "username", "u", "postgres",
		"username for the connection")
	sqlCommand.Flags().BoolVarP(&sqlConfig.Readonly, "readonly", "o", false,
		"connect to the read-only endpoint")
	sqlCommand.Flags().BoolVar(&sqlConfig.PrintScript, "print-script", false,
		"print the connection script")
	sqlCommand.Flags().SetInterspersed(false)

	return sqlCommand
}

func ensureCaCerts() (string, error) {
	homedir, err := os.UserHomeDir()
	if err != nil {
		homedir = os.TempDir()
	}
	awsCfgDir := path.Join(homedir, ".aws")
	err = os.MkdirAll(awsCfgDir, 0755)
	if err != nil {
		return "", err
	}

	caBundle := path.Join(awsCfgDir, "rds-ca-certs.pem")
	err = os.WriteFile(caBundle, []byte(rdsCaCertificates), 0644)
	if err != nil {
		return "", err
	}

	caBundle, err = shellquote.Quote([]string{caBundle})
	if err != nil {
		return "", err
	}
	return caBundle, nil
}
