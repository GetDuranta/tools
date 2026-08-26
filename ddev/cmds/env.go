package cmds

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/GetDuranta/tools/internal/devenv"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/spf13/cobra"
)

type envClient struct {
	baseURL string
	config  aws.Config
	http    *http.Client
}

func GetEnvCommand(global *GlobalConfig) *cobra.Command {
	command := &cobra.Command{Use: "env", Short: "manage disposable EC2 development environments"}
	command.AddCommand(envCreateCommand(global), envListCommand(global), envGetCommand(global),
		envStartCommand(global), envSimpleMutationCommand(global, "stop"), envExtendCommand(global),
		envArchiveCommand(global), envSimpleMutationCommand(global, "delete"), envOpenCommand(global),
		envSSHCommand(global), envCheckpointCommand(global))
	return command
}

func envCreateCommand(global *GlobalConfig) *cobra.Command {
	var profile, visibility, checkpoint string
	var includeUntracked bool
	var wait bool
	command := &cobra.Command{
		Use: "create <name>", Args: cobra.ExactArgs(1), Short: "upload this checkout and create an environment",
		RunE: func(cmd *cobra.Command, args []string) error {
			runtimeProfile := devenv.Profile(profile)
			if err := runtimeProfile.Validate(); err != nil {
				return err
			}
			previewVisibility := devenv.Visibility(visibility)
			if err := previewVisibility.Validate(); err != nil {
				return err
			}
			client, err := newEnvClient(cmd.Context(), global)
			if err != nil {
				return err
			}
			artifact, err := buildSourceArtifact(includeUntracked, runtimeProfile == devenv.ProfileGPUCVML)
			if err != nil {
				return err
			}
			defer artifact.Cleanup()
			var upload devenv.SourceUpload
			if err = client.do(cmd.Context(), http.MethodPost, "/v1/source-uploads", nil, "", &upload); err != nil {
				return err
			}
			if err = uploadSource(cmd.Context(), upload, artifact.Path); err != nil {
				return err
			}
			request := devenv.CreateRequest{
				Name: args[0], Profile: runtimeProfile, Visibility: previewVisibility,
				FromCheckpointID: checkpoint,
				Source:           devenv.Source{Repository: artifact.Repository, Ref: artifact.Commit, BundleKey: upload.BundleKey},
			}
			var result devenv.MutationResult
			if err = client.do(cmd.Context(), http.MethodPost, "/v1/environments", request, randomKey(), &result); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", result.Environment.ID, result.Environment.State, result.Operation.ID)
			if !wait {
				return nil
			}
			return waitForEnvironment(cmd, client, result.Operation.ID, result.Environment.ID, true)
		},
	}
	command.Flags().StringVar(&profile, "runtime-profile", string(devenv.ProfileStandard), "standard or gpu-cvml")
	command.Flags().StringVar(&visibility, "visibility", string(devenv.VisibilityOrganization), "organization or restricted")
	command.Flags().StringVar(&checkpoint, "from-checkpoint", "", "checkpoint to restore")
	command.Flags().BoolVar(&includeUntracked, "include-untracked", false, "include non-ignored untracked files; inspect them for secrets first")
	command.Flags().BoolVar(&wait, "wait", true, "wait until the environment is ready and print its browser link")
	return command
}

func envListCommand(global *GlobalConfig) *cobra.Command {
	return &cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := newEnvClient(cmd.Context(), global)
		if err != nil {
			return err
		}
		var response struct {
			Environments []devenv.Environment `json:"environments"`
		}
		if err = client.do(cmd.Context(), http.MethodGet, "/v1/environments", nil, "", &response); err != nil {
			return err
		}
		for _, environment := range response.Environments {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", environment.ID, environment.State,
				environment.Profile, environment.Name)
		}
		return nil
	}}
}

func envGetCommand(global *GlobalConfig) *cobra.Command {
	return &cobra.Command{Use: "get <environment-id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := newEnvClient(cmd.Context(), global)
		if err != nil {
			return err
		}
		var environment devenv.Environment
		if err = client.do(cmd.Context(), http.MethodGet, "/v1/environments/"+url.PathEscape(args[0]), nil, "", &environment); err != nil {
			return err
		}
		return writeJSON(cmd.OutOrStdout(), environment)
	}}
}

func envStartCommand(global *GlobalConfig) *cobra.Command {
	var profile string
	command := &cobra.Command{Use: "start <environment-id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		request := devenv.StartRequest{}
		if profile != "" {
			value := devenv.Profile(profile)
			request.Profile = &value
		}
		return runEnvMutation(cmd, global, args[0], "start", request)
	}}
	command.Flags().StringVar(&profile, "runtime-profile", "", "runtime profile for an archived environment")
	return command
}

func envExtendCommand(global *GlobalConfig) *cobra.Command {
	var hours int
	command := &cobra.Command{Use: "extend <environment-id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return runEnvMutation(cmd, global, args[0], "extend", devenv.ExtendRequest{Hours: hours})
	}}
	command.Flags().IntVar(&hours, "hours", 4, "idle lease extension from 1 to 4 hours")
	return command
}

func envArchiveCommand(global *GlobalConfig) *cobra.Command {
	var name string
	var pinned bool
	command := &cobra.Command{Use: "archive <environment-id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return runEnvMutation(cmd, global, args[0], "archive", devenv.ArchiveRequest{CheckpointName: name, Pinned: pinned})
	}}
	command.Flags().StringVar(&name, "checkpoint-name", "", "human-readable checkpoint name")
	command.Flags().BoolVar(&pinned, "pin", false, "disable automatic checkpoint expiry")
	return command
}

func envSimpleMutationCommand(global *GlobalConfig, action string) *cobra.Command {
	return &cobra.Command{Use: action + " <environment-id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		method := http.MethodPost
		path := "/v1/environments/" + url.PathEscape(args[0]) + ":" + action
		if action == "delete" {
			method = http.MethodDelete
			path = "/v1/environments/" + url.PathEscape(args[0])
		}
		client, err := newEnvClient(cmd.Context(), global)
		if err != nil {
			return err
		}
		var result devenv.MutationResult
		if err = client.do(cmd.Context(), method, path, nil, randomKey(), &result); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", result.Environment.State, result.Operation.ID)
		return nil
	}}
}

func envOpenCommand(global *GlobalConfig) *cobra.Command {
	return &cobra.Command{Use: "open <environment-id>", Aliases: []string{"url"}, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := newEnvClient(cmd.Context(), global)
		if err != nil {
			return err
		}
		var link devenv.BrowserLink
		if err = client.do(cmd.Context(), http.MethodPost, "/v1/environments/"+url.PathEscape(args[0])+":browser-link", nil, "", &link); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), link.URL)
		return nil
	}}
}

func envSSHCommand(global *GlobalConfig) *cobra.Command {
	return &cobra.Command{Use: "ssh <environment-id>", Aliases: []string{"shell"}, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := newEnvClient(cmd.Context(), global)
		if err != nil {
			return err
		}
		id := url.PathEscape(args[0])
		var refreshed devenv.Environment
		if err = client.do(cmd.Context(), http.MethodPost, "/internal/v1/environments/"+id+":activity",
			devenv.ActivityRequest{Kind: "terminal"}, "", &refreshed); err != nil {
			return err
		}
		if refreshed.InstanceID == "" {
			return errors.New("environment has no running instance")
		}
		arguments := []string{}
		profile, region := envProfileRegion(global)
		if profile != "" {
			arguments = append(arguments, "--profile", profile)
		}
		if region != "" {
			arguments = append(arguments, "--region", region)
		}
		arguments = append(arguments, "ssm", "start-session", "--target", refreshed.InstanceID,
			"--document-name", "AWS-StartInteractiveCommand", "--parameters", "command=cd /workspace/repo && exec bash -l")
		process := exec.CommandContext(cmd.Context(), "aws", arguments...)
		process.Stdin, process.Stdout, process.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
		done := make(chan struct{})
		defer close(done)
		go terminalHeartbeat(cmd, client, args[0], done)
		return process.Run()
	}}
}

func envCheckpointCommand(global *GlobalConfig) *cobra.Command {
	command := &cobra.Command{Use: "checkpoint", Short: "list and delete workspace checkpoints"}
	command.AddCommand(
		&cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newEnvClient(cmd.Context(), global)
			if err != nil {
				return err
			}
			var response struct {
				Checkpoints []devenv.Checkpoint `json:"checkpoints"`
			}
			if err = client.do(cmd.Context(), http.MethodGet, "/v1/checkpoints", nil, "", &response); err != nil {
				return err
			}
			for _, checkpoint := range response.Checkpoints {
				expires := "never"
				if checkpoint.ExpiresAt != nil {
					expires = checkpoint.ExpiresAt.Format(time.RFC3339)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\tpinned=%t\texpires=%s\n",
					checkpoint.ID, checkpoint.State, checkpoint.Name, checkpoint.Pinned, expires)
			}
			return nil
		}},
		&cobra.Command{Use: "delete <checkpoint-id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newEnvClient(cmd.Context(), global)
			if err != nil {
				return err
			}
			var operation devenv.Operation
			if err = client.do(cmd.Context(), http.MethodDelete, "/v1/checkpoints/"+url.PathEscape(args[0]),
				nil, randomKey(), &operation); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), operation.ID)
			return nil
		}},
	)
	return command
}

func waitForEnvironment(cmd *cobra.Command, client *envClient, operationID, environmentID string,
	printLink bool) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		var operation devenv.Operation
		if err := client.do(cmd.Context(), http.MethodGet, "/v1/operations/"+url.PathEscape(operationID),
			nil, "", &operation); err != nil {
			return err
		}
		switch operation.Status {
		case devenv.OperationSucceeded:
			if !printLink {
				return nil
			}
			var link devenv.BrowserLink
			if err := client.do(cmd.Context(), http.MethodPost,
				"/v1/environments/"+url.PathEscape(environmentID)+":browser-link", nil, "", &link); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), link.URL)
			return nil
		case devenv.OperationFailed:
			return fmt.Errorf("environment operation failed: %s", operation.Error)
		}
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-ticker.C:
		}
	}
}

func terminalHeartbeat(cmd *cobra.Command, client *envClient, environmentID string, done <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-cmd.Context().Done():
			return
		case <-ticker.C:
			var environment devenv.Environment
			if err := client.do(cmd.Context(), http.MethodPost,
				"/internal/v1/environments/"+url.PathEscape(environmentID)+":activity",
				devenv.ActivityRequest{Kind: "terminal"}, "", &environment); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to refresh environment lease: %v\n", err)
			}
		}
	}
}

func runEnvMutation(cmd *cobra.Command, global *GlobalConfig, id, action string, body any) error {
	client, err := newEnvClient(cmd.Context(), global)
	if err != nil {
		return err
	}
	var result devenv.MutationResult
	if err = client.do(cmd.Context(), http.MethodPost,
		"/v1/environments/"+url.PathEscape(id)+":"+action, body, randomKey(), &result); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", result.Environment.State, result.Operation.ID)
	return nil
}

func newEnvClient(ctx context.Context, global *GlobalConfig) (*envClient, error) {
	profile, region := envProfileRegion(global)
	options := []func(*config.LoadOptions) error{config.WithRegion(region)}
	if profile != "" {
		options = append(options, config.WithSharedConfigProfile(profile))
	}
	awsConfig, err := config.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, err
	}
	baseURL := strings.TrimRight(global.EnvAPIURL, "/")
	if baseURL == "" {
		baseURL = strings.TrimRight(os.Getenv("DURANTA_DEV_ENV_API_URL"), "/")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return nil, errors.New("set --env-api-url or DURANTA_DEV_ENV_API_URL to the HTTPS control API")
	}
	return &envClient{baseURL: baseURL, config: awsConfig, http: &http.Client{Timeout: 15 * time.Minute}}, nil
}

func (c *envClient) do(ctx context.Context, method, path string, requestBody any, idempotency string, response any) error {
	var raw []byte
	var err error
	if requestBody != nil {
		raw, err = json.Marshal(requestBody)
		if err != nil {
			return err
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	request.Header.Set("accept", "application/json")
	if requestBody != nil {
		request.Header.Set("content-type", "application/json")
	}
	if idempotency != "" {
		request.Header.Set("Idempotency-Key", idempotency)
	}
	digest := sha256.Sum256(raw)
	credentials, err := c.config.Credentials.Retrieve(ctx)
	if err != nil {
		return err
	}
	if err = v4.NewSigner().SignHTTP(ctx, credentials, request, hex.EncodeToString(digest[:]),
		"execute-api", c.config.Region, time.Now().UTC()); err != nil {
		return err
	}
	result, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer result.Body.Close()
	if result.StatusCode < 200 || result.StatusCode > 299 {
		message, _ := io.ReadAll(io.LimitReader(result.Body, 64<<10))
		return fmt.Errorf("control API returned %s: %s", result.Status, strings.TrimSpace(string(message)))
	}
	if response == nil {
		return nil
	}
	return json.NewDecoder(result.Body).Decode(response)
}

func uploadSource(ctx context.Context, upload devenv.SourceUpload, artifact string) error {
	file, err := os.Open(artifact)
	if err != nil {
		return err
	}
	defer file.Close()
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, upload.URL, file)
	if err != nil {
		return err
	}
	for key, value := range upload.Headers {
		request.Header.Set(key, value)
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	request.ContentLength = info.Size()
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return fmt.Errorf("source upload returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func envProfileRegion(global *GlobalConfig) (string, string) {
	profile, region := global.Profile, global.Region
	if profile == "" {
		profile = "be-dev"
	}
	if region == "" {
		region = "us-west-2"
	}
	return profile, region
}

func randomKey() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw)
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
