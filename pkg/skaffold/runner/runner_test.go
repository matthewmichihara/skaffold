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

package runner

import (
	"context"
	"fmt"
	"io"
	"testing"

	"k8s.io/client-go/tools/clientcmd/api"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/build"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/config"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/deploy"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/deploy/helm"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/deploy/kubectl"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/deploy/kustomize"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/filemon"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/graph"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/runner/runcontext"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/schema/defaults"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/schema/latest"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/sync"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/tag"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/test"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/util"
	"github.com/GoogleContainerTools/skaffold/testutil"
)

type Actions struct {
	Built    []string
	Synced   []string
	Tested   []string
	Deployed []string
}

type TestBench struct {
	buildErrors   []error
	syncErrors    []error
	testErrors    []error
	deployErrors  []error
	namespaces    []string
	userIntents   []func(*intents)
	intents       *intents
	intentTrigger bool

	devLoop        func(context.Context, io.Writer, func() error) error
	firstMonitor   func(bool) error
	cycles         int
	currentCycle   int
	currentActions Actions
	actions        []Actions
	tag            int
}

func NewTestBench() *TestBench {
	return &TestBench{}
}

func (t *TestBench) WithBuildErrors(buildErrors []error) *TestBench {
	t.buildErrors = buildErrors
	return t
}

func (t *TestBench) WithSyncErrors(syncErrors []error) *TestBench {
	t.syncErrors = syncErrors
	return t
}

func (t *TestBench) WithDeployErrors(deployErrors []error) *TestBench {
	t.deployErrors = deployErrors
	return t
}

func (t *TestBench) WithDeployNamespaces(ns []string) *TestBench {
	t.namespaces = ns
	return t
}

func (t *TestBench) WithTestErrors(testErrors []error) *TestBench {
	t.testErrors = testErrors
	return t
}

func (t *TestBench) TestDependencies() ([]string, error)              { return nil, nil }
func (t *TestBench) Dependencies() ([]string, error)                  { return nil, nil }
func (t *TestBench) Cleanup(ctx context.Context, out io.Writer) error { return nil }
func (t *TestBench) Prune(ctx context.Context, out io.Writer) error   { return nil }

func (t *TestBench) enterNewCycle() {
	t.actions = append(t.actions, t.currentActions)
	t.currentActions = Actions{}
}

func (t *TestBench) Build(_ context.Context, _ io.Writer, _ tag.ImageTags, artifacts []*latest.Artifact) ([]graph.Artifact, error) {
	if len(t.buildErrors) > 0 {
		err := t.buildErrors[0]
		t.buildErrors = t.buildErrors[1:]
		if err != nil {
			return nil, err
		}
	}

	t.tag++

	var builds []graph.Artifact
	for _, artifact := range artifacts {
		builds = append(builds, graph.Artifact{
			ImageName: artifact.ImageName,
			Tag:       fmt.Sprintf("%s:%d", artifact.ImageName, t.tag),
		})
	}

	t.currentActions.Built = findTags(builds)
	return builds, nil
}

func (t *TestBench) Sync(_ context.Context, item *sync.Item) error {
	if len(t.syncErrors) > 0 {
		err := t.syncErrors[0]
		t.syncErrors = t.syncErrors[1:]
		if err != nil {
			return err
		}
	}

	t.currentActions.Synced = []string{item.Image}
	return nil
}

func (t *TestBench) Test(_ context.Context, _ io.Writer, artifacts []graph.Artifact) error {
	if len(t.testErrors) > 0 {
		err := t.testErrors[0]
		t.testErrors = t.testErrors[1:]
		if err != nil {
			return err
		}
	}

	t.currentActions.Tested = findTags(artifacts)
	return nil
}

func (t *TestBench) Deploy(_ context.Context, _ io.Writer, artifacts []graph.Artifact) ([]string, error) {
	if len(t.deployErrors) > 0 {
		err := t.deployErrors[0]
		t.deployErrors = t.deployErrors[1:]
		if err != nil {
			return nil, err
		}
	}

	t.currentActions.Deployed = findTags(artifacts)
	return t.namespaces, nil
}

func (t *TestBench) Render(context.Context, io.Writer, []graph.Artifact, bool, string) error {
	return nil
}

func (t *TestBench) Actions() []Actions {
	return append(t.actions, t.currentActions)
}

func (t *TestBench) WatchForChanges(ctx context.Context, out io.Writer, doDev func() error) error {
	// don't actually call the monitor here, because extra actions would be added
	if err := t.firstMonitor(true); err != nil {
		return err
	}

	t.intentTrigger = true
	for _, intent := range t.userIntents {
		intent(t.intents)
		if err := t.devLoop(ctx, out, doDev); err != nil {
			return err
		}
	}

	t.intentTrigger = false
	for i := 0; i < t.cycles; i++ {
		t.enterNewCycle()
		t.currentCycle = i
		if err := t.devLoop(ctx, out, doDev); err != nil {
			return err
		}
	}
	return nil
}

func (t *TestBench) LogWatchToUser(_ io.Writer) {}

func findTags(artifacts []graph.Artifact) []string {
	var tags []string
	for _, artifact := range artifacts {
		tags = append(tags, artifact.Tag)
	}
	return tags
}

func (r *SkaffoldRunner) WithMonitor(m filemon.Monitor) *SkaffoldRunner {
	r.monitor = m
	return r
}

type triggerState struct {
	build  bool
	sync   bool
	deploy bool
}

func createRunner(t *testutil.T, testBench *TestBench, monitor filemon.Monitor, artifacts []*latest.Artifact, autoTriggers *triggerState) *SkaffoldRunner {
	if autoTriggers == nil {
		autoTriggers = &triggerState{true, true, true}
	}
	cfg := &latest.SkaffoldConfig{
		Pipeline: latest.Pipeline{
			Build: latest.BuildConfig{
				TagPolicy: latest.TagPolicy{
					// Use the fastest tagger
					ShaTagger: &latest.ShaTagger{},
				},
				Artifacts: artifacts,
			},
			Deploy: latest.DeployConfig{StatusCheckDeadlineSeconds: 60},
		},
	}
	defaults.Set(cfg)
	defaults.SetDefaultDeployer(cfg)
	runCtx := &runcontext.RunContext{
		Pipelines: runcontext.NewPipelines([]latest.Pipeline{cfg.Pipeline}),
		Opts: config.SkaffoldOptions{
			Trigger:           "polling",
			WatchPollInterval: 100,
			AutoBuild:         autoTriggers.build,
			AutoSync:          autoTriggers.sync,
			AutoDeploy:        autoTriggers.deploy,
		},
	}
	runner, err := NewForConfig(runCtx)
	t.CheckNoError(err)

	runner.builder = testBench
	runner.syncer = testBench
	runner.tester = testBench
	runner.deployer = testBench
	runner.listener = testBench
	runner.monitor = monitor

	testBench.devLoop = func(ctx context.Context, out io.Writer, doDev func() error) error {
		if err := monitor.Run(true); err != nil {
			return err
		}
		return doDev()
	}

	testBench.firstMonitor = func(bool) error {
		// default to noop so we don't add extra actions
		return nil
	}

	return runner
}

func TestNewForConfig(t *testing.T) {
	tests := []struct {
		description      string
		pipeline         latest.Pipeline
		shouldErr        bool
		cacheArtifacts   bool
		expectedBuilder  build.BuilderMux
		expectedTester   test.Tester
		expectedDeployer deploy.Deployer
	}{
		{
			description: "local builder config",
			pipeline: latest.Pipeline{
				Build: latest.BuildConfig{
					TagPolicy: latest.TagPolicy{ShaTagger: &latest.ShaTagger{}},
					BuildType: latest.BuildType{
						LocalBuild: &latest.LocalBuild{},
					},
				},
				Deploy: latest.DeployConfig{
					DeployType: latest.DeployType{
						KubectlDeploy: &latest.KubectlDeploy{},
					},
				},
			},
			expectedTester:   &test.FullTester{},
			expectedDeployer: &kubectl.Deployer{},
		},
		{
			description: "gcb config",
			pipeline: latest.Pipeline{
				Build: latest.BuildConfig{
					TagPolicy: latest.TagPolicy{ShaTagger: &latest.ShaTagger{}},
					BuildType: latest.BuildType{
						GoogleCloudBuild: &latest.GoogleCloudBuild{},
					},
				},
				Deploy: latest.DeployConfig{
					DeployType: latest.DeployType{
						KubectlDeploy: &latest.KubectlDeploy{},
					},
				},
			},
			expectedTester:   &test.FullTester{},
			expectedDeployer: &kubectl.Deployer{},
		},
		{
			description: "cluster builder config",
			pipeline: latest.Pipeline{
				Build: latest.BuildConfig{
					TagPolicy: latest.TagPolicy{ShaTagger: &latest.ShaTagger{}},
					BuildType: latest.BuildType{
						Cluster: &latest.ClusterDetails{Timeout: "100s"},
					},
				},
				Deploy: latest.DeployConfig{
					DeployType: latest.DeployType{
						KubectlDeploy: &latest.KubectlDeploy{},
					},
				},
			},
			expectedTester:   &test.FullTester{},
			expectedDeployer: &kubectl.Deployer{},
		},
		{
			description: "bad tagger config",
			pipeline: latest.Pipeline{
				Build: latest.BuildConfig{
					TagPolicy: latest.TagPolicy{},
					BuildType: latest.BuildType{
						LocalBuild: &latest.LocalBuild{},
					},
				},
				Deploy: latest.DeployConfig{
					DeployType: latest.DeployType{
						KubectlDeploy: &latest.KubectlDeploy{},
					},
				},
			},
			shouldErr: true,
		},
		{
			description:      "unknown builder and tagger",
			pipeline:         latest.Pipeline{},
			shouldErr:        true,
			expectedTester:   &test.FullTester{},
			expectedDeployer: &kubectl.Deployer{},
		},
		{
			description: "no artifacts, cache",
			pipeline: latest.Pipeline{
				Build: latest.BuildConfig{
					TagPolicy: latest.TagPolicy{ShaTagger: &latest.ShaTagger{}},
					BuildType: latest.BuildType{
						LocalBuild: &latest.LocalBuild{},
					},
				},
				Deploy: latest.DeployConfig{
					DeployType: latest.DeployType{
						KubectlDeploy: &latest.KubectlDeploy{},
					},
				},
			},
			expectedTester:   &test.FullTester{},
			expectedDeployer: &kubectl.Deployer{},
			cacheArtifacts:   true,
		},
		{
			description: "multiple deployers",
			pipeline: latest.Pipeline{
				Build: latest.BuildConfig{
					TagPolicy: latest.TagPolicy{ShaTagger: &latest.ShaTagger{}},
					BuildType: latest.BuildType{
						LocalBuild: &latest.LocalBuild{},
					},
				},
				Deploy: latest.DeployConfig{
					DeployType: latest.DeployType{
						KubectlDeploy:   &latest.KubectlDeploy{},
						KustomizeDeploy: &latest.KustomizeDeploy{},
						HelmDeploy:      &latest.HelmDeploy{},
					},
				},
			},
			expectedTester: &test.FullTester{},
			expectedDeployer: deploy.DeployerMux([]deploy.Deployer{
				&helm.Deployer{},
				&kubectl.Deployer{},
				&kustomize.Deployer{},
			}),
		},
	}
	for _, test := range tests {
		testutil.Run(t, test.description, func(t *testutil.T) {
			t.SetupFakeKubernetesContext(api.Config{CurrentContext: "cluster1"})
			t.Override(&util.DefaultExecCommand, testutil.CmdRunWithOutput(
				"helm version --client", `version.BuildInfo{Version:"v3.0.0"}`).
				AndRunWithOutput("kubectl version --client -ojson", "v1.5.6"))

			runCtx := &runcontext.RunContext{
				Pipelines: runcontext.NewPipelines([]latest.Pipeline{test.pipeline}),
				Opts: config.SkaffoldOptions{
					Trigger: "polling",
				},
			}

			cfg, err := NewForConfig(runCtx)
			t.CheckError(test.shouldErr, err)

			t.CheckError(test.shouldErr, err)
			if cfg != nil {
				b, _t, d := WithTimings(&test.expectedBuilder, test.expectedTester, test.expectedDeployer, test.cacheArtifacts)

				if test.shouldErr {
					t.CheckError(true, err)
				} else {
					t.CheckNoError(err)
					t.CheckTypeEquality(b, cfg.builder)
					t.CheckTypeEquality(_t, cfg.tester)
					t.CheckTypeEquality(d, cfg.deployer)
				}
			}
		})
	}
}

func TestTriggerCallbackAndIntents(t *testing.T) {
	var tests = []struct {
		description          string
		autoBuild            bool
		autoSync             bool
		autoDeploy           bool
		expectedBuildIntent  bool
		expectedSyncIntent   bool
		expectedDeployIntent bool
	}{
		{
			description:          "default",
			autoBuild:            true,
			autoSync:             true,
			autoDeploy:           true,
			expectedBuildIntent:  true,
			expectedSyncIntent:   true,
			expectedDeployIntent: true,
		},
		{
			description:          "build trigger in api mode",
			autoBuild:            false,
			autoSync:             true,
			autoDeploy:           true,
			expectedBuildIntent:  false,
			expectedSyncIntent:   true,
			expectedDeployIntent: true,
		},
		{
			description:          "deploy trigger in api mode",
			autoBuild:            true,
			autoSync:             true,
			autoDeploy:           false,
			expectedBuildIntent:  true,
			expectedSyncIntent:   true,
			expectedDeployIntent: false,
		},
		{
			description:          "sync trigger in api mode",
			autoBuild:            true,
			autoSync:             false,
			autoDeploy:           true,
			expectedBuildIntent:  true,
			expectedSyncIntent:   false,
			expectedDeployIntent: true,
		},
	}

	for _, test := range tests {
		testutil.Run(t, test.description, func(t *testutil.T) {
			opts := config.SkaffoldOptions{
				Trigger:           "polling",
				WatchPollInterval: 100,
				AutoBuild:         test.autoBuild,
				AutoSync:          test.autoSync,
				AutoDeploy:        test.autoDeploy,
			}
			pipeline := latest.Pipeline{
				Build: latest.BuildConfig{
					TagPolicy: latest.TagPolicy{ShaTagger: &latest.ShaTagger{}},
					BuildType: latest.BuildType{
						LocalBuild: &latest.LocalBuild{},
					},
				},
				Deploy: latest.DeployConfig{
					DeployType: latest.DeployType{
						KubectlDeploy: &latest.KubectlDeploy{},
					},
				},
			}
			r, _ := NewForConfig(&runcontext.RunContext{
				Opts:      opts,
				Pipelines: runcontext.NewPipelines([]latest.Pipeline{pipeline}),
			})

			r.intents.resetBuild()
			r.intents.resetSync()
			r.intents.resetDeploy()

			t.CheckDeepEqual(test.expectedBuildIntent, r.intents.build)
			t.CheckDeepEqual(test.expectedSyncIntent, r.intents.sync)
			t.CheckDeepEqual(test.expectedDeployIntent, r.intents.deploy)
		})
	}
}
