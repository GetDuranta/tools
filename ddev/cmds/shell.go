package cmds

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/frioux/shellquote"
	"github.com/spf13/cobra"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

type EcsConfig struct {
	TaskIndex int32
}

func GetShellCommand(globalCfg *GlobalConfig) *cobra.Command {
	ecsCfg := EcsConfig{}

	var shellCmd = &cobra.Command{
		Use:   "shell <cluster-name> <container-name> (cmd...|/bin/sh)",
		Args:  cobra.MinimumNArgs(2),
		Short: "allows to log into the running ECS tasks",
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster := args[0]
			containerName := args[1]
			if cluster == "" {
				return fmt.Errorf("cluster name is required")
			}

			awsConfig, err := loadConfig(globalCfg)
			if err != nil {
				return err
			}

			conn := ecs.NewFromConfig(awsConfig)
			tasks, err := conn.ListTasks(context.TODO(), &ecs.ListTasksInput{
				Cluster: aws.String(cluster),
			})
			if err != nil {
				return err
			}
			if len(tasks.TaskArns) == 0 {
				return fmt.Errorf("no tasks found in the cluster")
			}

			var idx int32
			if ecsCfg.TaskIndex != -1 {
				idx = ecsCfg.TaskIndex
			}
			if idx > int32(len(tasks.TaskArns)) {
				return fmt.Errorf("task index out of range")
			}

			parts := strings.Split(tasks.TaskArns[idx], "/")
			taskId := parts[len(parts)-1]

			// Describe containers within the task
			desc, err := conn.DescribeTasks(context.TODO(), &ecs.DescribeTasksInput{
				Cluster: aws.String(cluster),
				Tasks:   []string{taskId},
			})
			if err != nil {
				return err
			}

			if len(desc.Tasks) == 0 || len(desc.Tasks[0].Containers) == 0 {
				return fmt.Errorf("no containers found in the task")
			}

			if len(desc.Tasks[0].Containers) > 1 && containerName == "" {
				return fmt.Errorf("multiple containers found in the task, please specify the container name")
			} else {
				containerName = *desc.Tasks[0].Containers[0].Name
			}

			var quoted string
			if len(args) < 3 {
				quoted = "/bin/sh"
			} else {
				quoted, err = shellquote.Quote(args[2:])
				if err != nil {
					return err
				}
			}

			awsArgs := []string{"aws"}
			if globalCfg.Profile != "" {
				awsArgs = append(awsArgs, "--profile", globalCfg.Profile)
			}
			if globalCfg.Region != "" {
				awsArgs = append(awsArgs, "--region", globalCfg.Region)
			}
			awsArgs = append(awsArgs, "ecs", "execute-command", "--cluster", cluster, "--task", taskId,
				"--container", containerName, "--interactive",
				"--command", quoted)

			fmt.Printf("Invoking: aws %s\n", strings.Join(awsArgs, " "))

			binary, err := exec.LookPath("aws")
			if err != nil {
				return fmt.Errorf("aws command not found: %v", err)
			}
			env := os.Environ()
			err = syscall.Exec(binary, awsArgs, env)

			if err != nil {
				return fmt.Errorf("failed to run the AWS command: %v", err)
			}

			return nil
		},
	}

	shellCmd.Flags().Int32VarP(&ecsCfg.TaskIndex, "task-index", "i", -1,
		"log into the specific task index")
	shellCmd.Flags().SetInterspersed(false)

	return shellCmd
}
