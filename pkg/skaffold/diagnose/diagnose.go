/*
Copyright 2019 The Skaffold Authors

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

package diagnose

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"time"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/build"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/color"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/docker"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/filemon"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/graph"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/schema/latest"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/sync"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/util"
)

type Config interface {
	docker.Config

	GetPipelines() []latest.Pipeline
	Artifacts() []*latest.Artifact
}

func CheckArtifacts(ctx context.Context, cfg Config, out io.Writer) error {
	for _, p := range cfg.GetPipelines() {
		for _, artifact := range p.Build.Artifacts {
			color.Default.Fprintf(out, "\n%s: %s\n", typeOfArtifact(artifact), artifact.ImageName)

			if artifact.DockerArtifact != nil {
				size, err := sizeOfDockerContext(ctx, artifact, cfg)
				if err != nil {
					return fmt.Errorf("computing the size of the Docker context: %w", err)
				}

				fmt.Fprintf(out, " - Size of the context: %vbytes\n", size)
			}

			timeDeps1, deps, err := timeToListDependencies(ctx, artifact, cfg)
			if err != nil {
				return fmt.Errorf("listing artifact dependencies: %w", err)
			}
			timeDeps2, _, err := timeToListDependencies(ctx, artifact, cfg)
			if err != nil {
				return fmt.Errorf("listing artifact dependencies: %w", err)
			}

			fmt.Fprintln(out, " - Dependencies:", len(deps), "files")
			fmt.Fprintf(out, " - Time to list dependencies: %v (2nd time: %v)\n", timeDeps1, timeDeps2)

			timeSyncMap1, err := timeToConstructSyncMap(artifact, cfg)
			if err != nil {
				if _, isNotSupported := err.(build.ErrSyncMapNotSupported); !isNotSupported {
					return fmt.Errorf("construct artifact dependencies: %w", err)
				}
			}
			timeSyncMap2, err := timeToConstructSyncMap(artifact, cfg)
			if err != nil {
				if _, isNotSupported := err.(build.ErrSyncMapNotSupported); !isNotSupported {
					return fmt.Errorf("construct artifact dependencies: %w", err)
				}
			} else {
				fmt.Fprintf(out, " - Time to construct sync-map: %v (2nd time: %v)\n", timeSyncMap1, timeSyncMap2)
			}

			timeMTimes1, err := timeToComputeMTimes(deps)
			if err != nil {
				return fmt.Errorf("computing modTimes: %w", err)
			}
			timeMTimes2, err := timeToComputeMTimes(deps)
			if err != nil {
				return fmt.Errorf("computing modTimes: %w", err)
			}

			fmt.Fprintf(out, " - Time to compute mTimes on dependencies: %v (2nd time: %v)\n", timeMTimes1, timeMTimes2)
		}
	}
	return nil
}

func typeOfArtifact(a *latest.Artifact) string {
	switch {
	case a.DockerArtifact != nil:
		return "Docker artifact"
	case a.BazelArtifact != nil:
		return "Bazel artifact"
	case a.JibArtifact != nil:
		return "Jib artifact"
	case a.KanikoArtifact != nil:
		return "Kaniko artifact"
	case a.CustomArtifact != nil:
		return "Custom artifact"
	case a.BuildpackArtifact != nil:
		return "Buildpack artifact"
	default:
		panic("Unknown artifact")
	}
}

func timeToListDependencies(ctx context.Context, a *latest.Artifact, cfg Config) (string, []string, error) {
	start := time.Now()
	g := graph.ToArtifactGraph(cfg.Artifacts())
	sourceDependencies := graph.NewTransitiveSourceDependenciesCache(cfg, nil, g)
	paths, err := sourceDependencies.ResolveForArtifact(ctx, a)
	return util.ShowHumanizeTime(time.Since(start)), paths, err
}

func timeToConstructSyncMap(a *latest.Artifact, cfg docker.Config) (string, error) {
	start := time.Now()
	_, err := sync.SyncMap(a, cfg)
	return util.ShowHumanizeTime(time.Since(start)), err
}

func timeToComputeMTimes(deps []string) (string, error) {
	start := time.Now()

	if _, err := filemon.Stat(func() ([]string, error) { return deps, nil }); err != nil {
		return "nil", fmt.Errorf("computing modTimes: %w", err)
	}
	return util.ShowHumanizeTime(time.Since(start)), nil
}

func sizeOfDockerContext(ctx context.Context, a *latest.Artifact, cfg docker.Config) (int64, error) {
	buildCtx, buildCtxWriter := io.Pipe()
	go func() {
		err := docker.CreateDockerTarContext(ctx, buildCtxWriter, docker.NewBuildConfig(
			a.Workspace, a.ImageName, a.DockerArtifact.DockerfilePath, nil), cfg)
		if err != nil {
			buildCtxWriter.CloseWithError(fmt.Errorf("creating docker context: %w", err))
			return
		}
		buildCtxWriter.Close()
	}()

	return io.Copy(ioutil.Discard, buildCtx)
}
