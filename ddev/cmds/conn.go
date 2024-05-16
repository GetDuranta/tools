package cmds

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
)

type GlobalConfig struct {
	Profile string
	Region  string
}

func loadConfig(cfg *GlobalConfig) (aws.Config, error) {
	var opts []func(*config.LoadOptions) error
	if cfg.Profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(cfg.Profile))
	}
	if cfg.Region != "" {
		opts = append(opts, config.WithRegion(cfg.Region))
	}
	awsConfig, err := config.LoadDefaultConfig(context.TODO(), opts...)
	if err != nil {
		return aws.Config{}, err
	}
	return awsConfig, nil
}
