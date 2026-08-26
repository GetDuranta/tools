package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/GetDuranta/tools/internal/devenvgateway"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	runtimeConfig, err := devenvgateway.ParseRuntimeConfig(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	awsConfig, err := config.LoadDefaultConfig(ctx, config.WithRegion(runtimeConfig.Region))
	if err != nil {
		return err
	}
	store := devenvgateway.NewDynamoStore(dynamodb.NewFromConfig(awsConfig), runtimeConfig.TableName)
	verifier := devenvgateway.NewALBOIDCVerifier(runtimeConfig.Region, runtimeConfig.ALBSignerARN,
		runtimeConfig.ALBClientID, runtimeConfig.OwnerNamespace)
	verifier.TrustEmailClaim = runtimeConfig.TrustEmailClaim
	controlPlane, err := devenvgateway.NewSigV4ControlPlane(runtimeConfig.ControlAPIURL,
		awsConfig.Credentials, runtimeConfig.Region, nil)
	if err != nil {
		return err
	}
	handler, err := devenvgateway.NewHandler(devenvgateway.HandlerConfig{
		Store: store, Verifier: verifier, ControlPlane: controlPlane,
		ControlHost: runtimeConfig.ControlHost, PreviewSuffix: runtimeConfig.PreviewSuffix,
		SessionTTL: runtimeConfig.SessionTTL, CodeTTL: time.Minute, ExternalScheme: "https",
		Logger: logger, Upstreams: runtimeConfig.Upstreams,
	})
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr: runtimeConfig.ListenAddress, Handler: handler, ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10,
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	errorsChannel := make(chan error, 1)
	go func() {
		logger.Info("gateway listening", "address", runtimeConfig.ListenAddress)
		errorsChannel <- server.ListenAndServe()
	}()
	select {
	case err = <-errorsChannel:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return server.Shutdown(shutdownContext)
	}
}
