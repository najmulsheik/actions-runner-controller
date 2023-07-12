/*
Copyright 2021 The actions-runner-controller authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/actions/actions-runner-controller/build"
	"github.com/actions/actions-runner-controller/github/actions"
	"github.com/actions/actions-runner-controller/logging"
	"github.com/go-logr/logr"
	"github.com/kelseyhightower/envconfig"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/net/http/httpproxy"
)

type RunnerScaleSetListenerConfig struct {
	ConfigureUrl                string `split_words:"true"`
	AppID                       int64  `split_words:"true"`
	AppInstallationID           int64  `split_words:"true"`
	AppPrivateKey               string `split_words:"true"`
	Token                       string `split_words:"true"`
	EphemeralRunnerSetNamespace string `split_words:"true"`
	EphemeralRunnerSetName      string `split_words:"true"`
	MaxRunners                  int    `split_words:"true"`
	MinRunners                  int    `split_words:"true"`
	RunnerScaleSetId            int    `split_words:"true"`
	ServerRootCA                string `split_words:"true"`
	LogLevel                    string `split_words:"true"`
	LogFormat                   string `split_words:"true"`
}

func main() {
	var rc RunnerScaleSetListenerConfig
	if err := envconfig.Process("github", &rc); err != nil {
		fmt.Fprintf(os.Stderr, "Error: processing environment variables for RunnerScaleSetListenerConfig: %v\n", err)
		os.Exit(1)
	}

	logLevel := string(logging.LogLevelDebug)
	if rc.LogLevel != "" {
		logLevel = rc.LogLevel
	}

	logFormat := string(logging.LogFormatText)
	if rc.LogFormat != "" {
		logFormat = rc.LogFormat
	}

	logger, err := logging.NewLogger(logLevel, logFormat)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: creating logger: %v\n", err)
		os.Exit(1)
	}

	// Validate all inputs
	if err := validateConfig(&rc); err != nil {
		logger.Error(err, "Inputs validation failed")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 2)

	go func() {
		opts := runOptions{
			serviceOptions: []func(*Service){
				WithLogger(logger),
			},
		}
		opts.serviceOptions = append(opts.serviceOptions, WithPrometheusMetrics(rc))

		if err := run(ctx, rc, logger, opts); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	go func() {
		reg := prometheus.NewRegistry()
		reg.MustRegister(
			availableJobs,
			acquiredJobs,
			assignedJobs,
			runningJobs,
			registeredRunners,
			busyRunners,
			desiredRunners,
			idleRunners,
			availableJobsTotal,
			acquiredJobsTotal,
			assignedJobsTotal,
			startedJobsTotal,
			completedJobsTotal,
			jobQueueDurationSeconds,
			jobStartupDurationSeconds,
			jobExecutionDurationSeconds,
		)

		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))

		srv := http.Server{
			Addr:    ":8888",
			Handler: mux,
		}

		go func() {
			<-ctx.Done()
			srv.Shutdown(context.Background())
		}()

		logger.Info("Starting metrics server", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	err1 := <-errCh
	stop()
	if err1 != nil {
		logger.Error(err1, "First error emitted")
	}
	err2 := <-errCh
	if err2 != nil {
		logger.Error(err2, "Second error emitted")
	}
	if err1 != nil || err2 != nil {
		os.Exit(1)
	}
}

type runOptions struct {
	serviceOptions []func(*Service)
}

func run(ctx context.Context, rc RunnerScaleSetListenerConfig, logger logr.Logger, opts runOptions) error {
	// Create root context and hook with sigint and sigterm
	creds := &actions.ActionsAuth{}
	if rc.Token != "" {
		creds.Token = rc.Token
	} else {
		creds.AppCreds = &actions.GitHubAppAuth{
			AppID:             rc.AppID,
			AppInstallationID: rc.AppInstallationID,
			AppPrivateKey:     rc.AppPrivateKey,
		}
	}

	actionsServiceClient, err := newActionsClientFromConfig(
		rc,
		creds,
		actions.WithLogger(logger),
		actions.WithUserAgent(fmt.Sprintf("actions-runner-controller/%s", build.Version)),
	)
	if err != nil {
		return fmt.Errorf("failed to create an Actions Service client: %w", err)
	}

	// Create message listener
	autoScalerClient, err := NewAutoScalerClient(ctx, actionsServiceClient, &logger, rc.RunnerScaleSetId)
	if err != nil {
		return fmt.Errorf("failed to create a message listener: %w", err)
	}
	defer autoScalerClient.Close()

	// Create kube manager and scale controller
	kubeManager, err := NewKubernetesManager(&logger)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes manager: %w", err)
	}

	scaleSettings := &ScaleSettings{
		Namespace:    rc.EphemeralRunnerSetNamespace,
		ResourceName: rc.EphemeralRunnerSetName,
		MaxRunners:   rc.MaxRunners,
		MinRunners:   rc.MinRunners,
	}

	service, err := NewService(ctx, autoScalerClient, kubeManager, scaleSettings, opts.serviceOptions...)
	if err != nil {
		return fmt.Errorf("failed to create new service: %v", err)
	}

	// Start listening for messages
	if err = service.Start(); err != nil {
		return fmt.Errorf("failed to start message queue listener: %w", err)
	}
	return nil
}

func validateConfig(config *RunnerScaleSetListenerConfig) error {
	if len(config.ConfigureUrl) == 0 {
		return fmt.Errorf("GitHubConfigUrl is not provided")
	}

	if len(config.EphemeralRunnerSetNamespace) == 0 || len(config.EphemeralRunnerSetName) == 0 {
		return fmt.Errorf("EphemeralRunnerSetNamespace '%s' or EphemeralRunnerSetName '%s' is missing", config.EphemeralRunnerSetNamespace, config.EphemeralRunnerSetName)
	}

	if config.RunnerScaleSetId == 0 {
		return fmt.Errorf("RunnerScaleSetId '%d' is missing", config.RunnerScaleSetId)
	}

	if config.MaxRunners < config.MinRunners {
		return fmt.Errorf("MinRunners '%d' cannot be greater than MaxRunners '%d'", config.MinRunners, config.MaxRunners)
	}

	hasToken := len(config.Token) > 0
	hasPrivateKeyConfig := config.AppID > 0 && config.AppPrivateKey != ""

	if !hasToken && !hasPrivateKeyConfig {
		return fmt.Errorf("GitHub auth credential is missing, token length: '%d', appId: '%d', installationId: '%d', private key length: '%d", len(config.Token), config.AppID, config.AppInstallationID, len(config.AppPrivateKey))
	}

	if hasToken && hasPrivateKeyConfig {
		return fmt.Errorf("only one GitHub auth method supported at a time. Have both PAT and App auth: token length: '%d', appId: '%d', installationId: '%d', private key length: '%d", len(config.Token), config.AppID, config.AppInstallationID, len(config.AppPrivateKey))
	}

	return nil
}

func newActionsClientFromConfig(config RunnerScaleSetListenerConfig, creds *actions.ActionsAuth, options ...actions.ClientOption) (*actions.Client, error) {
	if config.ServerRootCA != "" {
		systemPool, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("failed to load system cert pool: %w", err)
		}
		pool := systemPool.Clone()
		ok := pool.AppendCertsFromPEM([]byte(config.ServerRootCA))
		if !ok {
			return nil, fmt.Errorf("failed to parse root certificate")
		}

		options = append(options, actions.WithRootCAs(pool))
	}

	proxyFunc := httpproxy.FromEnvironment().ProxyFunc()
	options = append(options, actions.WithProxy(func(req *http.Request) (*url.URL, error) {
		return proxyFunc(req.URL)
	}))

	return actions.NewClient(config.ConfigureUrl, creds, options...)
}
