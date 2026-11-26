// Copyright 2026 colonel-byte
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	zarflog "github.com/zarf-dev/zarf/src/pkg/logger"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/deploy"
)

// runDeploy is the deploy Job's entry point (docs/dev/architecture.md §6 step 6): read the
// mounted spec, deploy against the cluster KUBECONFIG points at, and report through the
// termination message. It exits the process - non-zero on failure, so the Job's Failed
// condition and the message agree.
func runDeploy(args []string) {
	flags := flag.NewFlagSet("deploy", flag.ExitOnError)
	specPath := flags.String("spec", "", "path of the mounted deploy-spec file")
	cachePath := flags.String("cache-path", "", "mount path of the zarf-cache volume")
	terminationLog := flags.String("termination-log", "/dev/termination-log",
		"where the kubelet reads the termination message")
	if err := flags.Parse(args); err != nil {
		os.Exit(2)
	}

	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("deploy")
	// SIGTERM is pod deletion; a canceled context lets the SDK abort mid-deploy instead of
	// being killed holding helm locks.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	// The SDK logs through its own context logger, which discards without one; route it to
	// stderr so the deploy narrative lands in the pod log.
	ctx = zarflog.WithContext(ctx, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	result := deploy.Result{}
	spec, err := deploy.Load(*specPath)
	if err == nil {
		// Before any pull: the credentials and CA, if the controller mounted them.
		err = deploy.Reachability(*specPath, *cachePath)
	}
	if err != nil {
		result.Error = err.Error()
	} else {
		result = deploy.Run(ctx, spec, *cachePath)
	}

	if err := deploy.WriteResult(*terminationLog, result); err != nil {
		log.Error(err, "unable to write the termination message")
	}
	if !result.Succeeded {
		log.Info("deploy failed", "error", result.Error)
		os.Exit(1)
	}
	log.Info("deploy succeeded", "package", result.Package, "name", result.Name, "version", result.Version)
	os.Exit(0)
}
