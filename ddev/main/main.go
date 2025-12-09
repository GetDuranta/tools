package main

import (
	"log/slog"
	"os"
	"tools/ddev/cmds"

	"github.com/spf13/cobra"
)

func makeCompletionCmd() *cobra.Command {
	var completionCmd = &cobra.Command{
		Use:   "completion",
		Short: "Generates bash/zsh completion scripts",
		Long: `To load completion run
        . <(completion)
        To configure your bash or zsh shell to load completions for each session add to your
        # ~/.bashrc or ~/.profile
        . <(completion)
        `,
		Run: func(cmd *cobra.Command, args []string) {
			zsh, err := cmd.Flags().GetBool("zsh")
			if err != nil {
				panic(err.Error())
			}
			if zsh {
				_ = cmd.Parent().GenZshCompletion(os.Stdout)
			} else {
				_ = cmd.Parent().GenBashCompletion(os.Stdout)
			}
		},
	}
	completionCmd.Flags().BoolP("zsh", "z", false, "Generate ZSH completion")

	return completionCmd
}

func main() {
	cfg := cmds.GlobalConfig{}

	var rootCmd = &cobra.Command{
		Use: "ddev",
	}

	rootCmd.PersistentFlags().StringVarP(&cfg.Profile, "profile", "p", "",
		"set the AWS profile to use")
	rootCmd.PersistentFlags().StringVarP(&cfg.Region, "region", "r", "",
		"override the region in profile")

	rootCmd.AddCommand(cmds.GetShellCommand(&cfg))
	rootCmd.AddCommand(cmds.GetSqlCommand(&cfg))
	rootCmd.AddCommand(cmds.GetMigrateCommand(&cfg))
	rootCmd.AddCommand(makeCompletionCmd())

	if err := rootCmd.Execute(); err != nil {
		slog.Error("Failed to run the task", "error", err)
		os.Exit(1)
	}
}
