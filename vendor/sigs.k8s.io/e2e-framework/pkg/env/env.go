/*
Copyright 2021 The Kubernetes Authors.

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

// Package env exposes types to create type `Environment` used to run
// feature tests.
package env

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	log "k8s.io/klog/v2"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/pkg/internal/types"
)

type (
	Environment = types.Environment
	Func        = types.EnvFunc
	FeatureFunc = types.FeatureEnvFunc

	actionRole uint8
)

type testEnv struct {
	ctx     context.Context
	cfg     *envconf.Config
	actions []action
	rnd     rand.Source
}

// New creates a test environment with no config attached.
func New() types.Environment {
	return newTestEnv()
}

func NewParallel() types.Environment {
	return newTestEnvWithParallel()
}

// NewWithConfig creates an environment using an Environment Configuration value
func NewWithConfig(cfg *envconf.Config) types.Environment {
	env := newTestEnv()
	env.cfg = cfg
	return env
}

// NewWithKubeConfig creates an environment using an Environment Configuration value
// and the given kubeconfig.
func NewWithKubeConfig(kubeconfigfile string) types.Environment {
	env := newTestEnv()
	cfg := envconf.NewWithKubeConfig(kubeconfigfile)
	env.cfg = cfg
	return env
}

// NewInClusterConfig creates an environment using an Environment Configuration value
// and assumes an in-cluster kubeconfig.
func NewInClusterConfig() types.Environment {
	env := newTestEnv()
	cfg := envconf.NewWithKubeConfig("")
	env.cfg = cfg
	return env
}

// NewWithContext creates a new environment with the provided context and config.
func NewWithContext(ctx context.Context, cfg *envconf.Config) (types.Environment, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context is nil")
	}
	if cfg == nil {
		return nil, fmt.Errorf("environment config is nil")
	}
	return &testEnv{ctx: ctx, cfg: cfg}, nil
}

func newTestEnv() *testEnv {
	return &testEnv{
		ctx: context.Background(),
		cfg: envconf.New(),
		rnd: rand.NewSource(time.Now().UnixNano()),
	}
}

func newTestEnvWithParallel() *testEnv {
	return &testEnv{
		ctx: context.Background(),
		cfg: envconf.New().WithParallelTestEnabled(),
	}
}

// WithContext returns a new environment with the context set to ctx.
// Argument ctx cannot be nil
func (e *testEnv) WithContext(ctx context.Context) types.Environment {
	if ctx == nil {
		panic("nil context") // this should never happen
	}
	env := &testEnv{
		ctx: ctx,
		cfg: e.cfg,
	}
	env.actions = append(env.actions, e.actions...)
	return env
}

// Setup registers environment operations that are executed once
// prior to the environment being ready and prior to any test.
func (e *testEnv) Setup(funcs ...Func) types.Environment {
	if len(funcs) == 0 {
		return e
	}
	e.actions = append(e.actions, action{role: roleSetup, funcs: funcs})
	return e
}

// BeforeEachTest registers environment funcs that are executed
// before each Env.Test(...)
func (e *testEnv) BeforeEachTest(funcs ...types.TestEnvFunc) types.Environment {
	if len(funcs) == 0 {
		return e
	}
	e.actions = append(e.actions, action{role: roleBeforeTest, testFuncs: funcs})
	return e
}

// BeforeEachFeature registers step functions that are executed
// before each Feature is tested during env.Test call.
func (e *testEnv) BeforeEachFeature(funcs ...FeatureFunc) types.Environment {
	if len(funcs) == 0 {
		return e
	}
	e.actions = append(e.actions, action{role: roleBeforeFeature, featureFuncs: funcs})
	return e
}

// AfterEachFeature registers step functions that are executed
// after each feature is tested during an env.Test call.
func (e *testEnv) AfterEachFeature(funcs ...FeatureFunc) types.Environment {
	if len(funcs) == 0 {
		return e
	}
	e.actions = append(e.actions, action{role: roleAfterFeature, featureFuncs: funcs})
	return e
}

// AfterEachTest registers environment funcs that are executed
// after each Env.Test(...).
func (e *testEnv) AfterEachTest(funcs ...types.TestEnvFunc) types.Environment {
	if len(funcs) == 0 {
		return e
	}
	e.actions = append(e.actions, action{role: roleAfterTest, testFuncs: funcs})
	return e
}

// panicOnMissingContext is used to check if the test Env has a non-nil context setup
// and fail fast if the context has not already been set
func (e *testEnv) panicOnMissingContext() {
	if e.ctx == nil {
		panic("context not set") // something is terribly wrong.
	}
}

// processTestActions is used to run a series of test action that were configured as
// BeforeEachTest or AfterEachTest
func (e *testEnv) processTestActions(t *testing.T, actions []action) {
	var err error
	for _, action := range actions {
		if e.ctx, err = action.runWithT(e.ctx, e.cfg, t); err != nil {
			t.Fatalf("BeforeEachTest failure: %s", err)
		}
	}
}

// processTestFeature is used to trigger the execution of the actual feature. This function wraps the entire
// workflow of orchestrating the feature execution be running the action configured by BeforeEachFeature /
// AfterEachFeature.
func (e *testEnv) processTestFeature(t *testing.T, featureName string, feature types.Feature) {
	var err error

	// execute each feature
	beforeFeatureActions := e.getBeforeFeatureActions()
	afterFeatureActions := e.getAfterFeatureActions()

	for _, action := range beforeFeatureActions {
		if e.ctx, err = action.runWithFeature(e.ctx, e.cfg, t, deepCopyFeature(feature)); err != nil {
			t.Fatalf("BeforeEachTest failure: %s", err)
		}
	}

	// execute feature test
	e.ctx = e.execFeature(e.ctx, t, featureName, feature)

	// execute beforeFeature actions
	for _, action := range afterFeatureActions {
		if e.ctx, err = action.runWithFeature(e.ctx, e.cfg, t, deepCopyFeature(feature)); err != nil {
			t.Fatalf("BeforeEachTest failure: %s", err)
		}
	}
}

// processTests is a wrapper function that can be invoked by either Test or TestInParallel methods.
// Depending on the configuration of if the parallel tests are enabled or not, this will change the
// nature of how the test gets executed.
//
// In case if the parallel run of test features are enabled, this function will invoke the processTestFeature
// as a go-routine to get them to run in parallel
func (e *testEnv) processTests(t *testing.T, enableParallelRun bool, testFeatures ...types.Feature) {
	e.panicOnMissingContext()
	if len(testFeatures) == 0 {
		t.Log("No test testFeatures provided, skipping test")
		return
	}
	beforeTestActions := e.getBeforeTestActions()
	afterTestActions := e.getAfterTestActions()

	e.processTestActions(t, beforeTestActions)

	runInParallel := e.cfg.ParallelTestEnabled() && enableParallelRun

	if runInParallel {
		log.V(4).Info("Running test features in parallel")
	}

	var wg sync.WaitGroup
	for i, feature := range testFeatures {
		featName := feature.Name()
		if featName == "" {
			featName = fmt.Sprintf("Feature-%d", i+1)
		}
		if runInParallel {
			wg.Add(1)
			go func(w *sync.WaitGroup) {
				defer w.Done()
				e.processTestFeature(t, featName, feature)
			}(&wg)
		} else {
			e.processTestFeature(t, featName, feature)
		}
	}
	if runInParallel {
		wg.Wait()
	}
	e.processTestActions(t, afterTestActions)
}

// TestInParallel executes a series a feature tests from within a
// TestXXX function in parallel
//
// Feature setups and teardowns are executed at the same *testing.T
// contextual level as the "test" that invoked this method. Assessments
// are executed as a subtests of the feature.  This approach allows
// features/assessments to be filtered using go test -run flag.
//
// Feature tests will have access to and able to update the context
// passed to it.
//
// BeforeTest and AfterTest operations are executed before and after
// the feature is tested respectively.
//
// BeforeTest and AfterTest operations are run in series of the entire
// set of features being passed to this call while the feature themselves
// are executed in parallel to avoid duplication of action that might happen
// in BeforeTest and AfterTest actions
func (e *testEnv) TestInParallel(t *testing.T, testFeatures ...types.Feature) {
	e.processTests(t, true, testFeatures...)
}

// Test executes a feature test from within a TestXXX function.
//
// Feature setups and teardowns are executed at the same *testing.T
// contextual level as the "test" that invoked this method. Assessments
// are executed as a subtests of the feature.  This approach allows
// features/assessments to be filtered using go test -run flag.
//
// Feature tests will have access to and able to update the context
// passed to it.
//
// BeforeTest and AfterTest operations are executed before and after
// the feature is tested respectively.
func (e *testEnv) Test(t *testing.T, testFeatures ...types.Feature) {
	e.processTests(t, false, testFeatures...)
}

// Finish registers funcs that are executed at the end of the
// test suite.
func (e *testEnv) Finish(funcs ...Func) types.Environment {
	if len(funcs) == 0 {
		return e
	}

	e.actions = append(e.actions, action{role: roleFinish, funcs: funcs})
	return e
}

// Run is to launch the test suite from a TestMain function.
// It will run m.Run() and exercise all test functions in the
// package.  This method will all Env.Setup operations prior to
// starting the tests and run all Env.Finish operations after
// before completing the suite.
//
func (e *testEnv) Run(m *testing.M) int {
	if e.ctx == nil {
		panic("context not set") // something is terribly wrong.
	}

	setups := e.getSetupActions()
	// fail fast on setup, upon err exit
	var err error
	for _, setup := range setups {
		// context passed down to each setup
		if e.ctx, err = setup.run(e.ctx, e.cfg); err != nil {
			log.Fatal(err)
		}
	}

	exitCode := m.Run() // exec test suite

	finishes := e.getFinishActions()
	// attempt to gracefully clean up.
	// Upon error, log and continue.
	for _, fin := range finishes {
		// context passed down to each finish step
		if e.ctx, err = fin.run(e.ctx, e.cfg); err != nil {
			log.V(2).ErrorS(err, "Finish action handlers")
		}
	}

	return exitCode
}

func (e *testEnv) getActionsByRole(r actionRole) []action {
	if e.actions == nil {
		return nil
	}

	var result []action
	for _, a := range e.actions {
		if a.role == r {
			result = append(result, a)
		}
	}

	return result
}

func (e *testEnv) getSetupActions() []action {
	return e.getActionsByRole(roleSetup)
}

func (e *testEnv) getBeforeTestActions() []action {
	return e.getActionsByRole(roleBeforeTest)
}

func (e *testEnv) getBeforeFeatureActions() []action {
	return e.getActionsByRole(roleBeforeFeature)
}

func (e *testEnv) getAfterFeatureActions() []action {
	return e.getActionsByRole(roleAfterFeature)
}

func (e *testEnv) getAfterTestActions() []action {
	return e.getActionsByRole(roleAfterTest)
}

func (e *testEnv) getFinishActions() []action {
	return e.getActionsByRole(roleFinish)
}

func (e *testEnv) execFeature(ctx context.Context, t *testing.T, featName string, f types.Feature) context.Context {
	// feature-level subtest
	t.Run(featName, func(t *testing.T) {
		// skip feature which matches with --skip-feature
		if e.cfg.SkipFeatureRegex() != nil && e.cfg.SkipFeatureRegex().MatchString(featName) {
			t.Skipf(`Skipping feature "%s": name matched`, featName)
		}

		// skip feature which does not match with --feature
		if e.cfg.FeatureRegex() != nil && !e.cfg.FeatureRegex().MatchString(featName) {
			t.Skipf(`Skipping feature "%s": name not matched`, featName)
		}

		// skip if labels does not match
		// run tests if --labels values matches the feature labels
		for k, v := range e.cfg.Labels() {
			if f.Labels()[k] != v {
				t.Skipf(`Skipping feature "%s": unmatched label "%s=%s"`, featName, k, f.Labels()[k])
			}
		}

		// skip running a feature if labels matches with --skip-labels
		for k, v := range e.cfg.SkipLabels() {
			if f.Labels()[k] == v {
				t.Skipf(`Skipping feature "%s": matched label provided in --skip-lables "%s=%s"`, featName, k, f.Labels()[k])
			}
		}

		// setups run at feature-level
		setups := features.GetStepsByLevel(f.Steps(), types.LevelSetup)
		for _, setup := range setups {
			ctx = setup.Func()(ctx, t, e.cfg)
		}

		// assessments run as feature/assessment sub level
		assessments := features.GetStepsByLevel(f.Steps(), types.LevelAssess)

		for i, assess := range assessments {
			assessName := assess.Name()
			if assessName == "" {
				assessName = fmt.Sprintf("Assessment-%d", i+1)
			}
			t.Run(assessName, func(t *testing.T) {
				// skip assessments which matches with --skip-assessments
				if e.cfg.SkipAssessmentRegex() != nil && e.cfg.SkipAssessmentRegex().MatchString(assess.Name()) {
					t.Skipf(`Skipping assessment "%s": name matched`, assess.Name())
				}

				// skip assessments which does not matches with --assess
				if e.cfg.AssessmentRegex() != nil && !e.cfg.AssessmentRegex().MatchString(assess.Name()) {
					t.Skipf(`Skipping assessment "%s": name not matched`, assess.Name())
				}
				ctx = assess.Func()(ctx, t, e.cfg)
			})
		}

		// teardowns run at feature-level
		teardowns := features.GetStepsByLevel(f.Steps(), types.LevelTeardown)
		for _, teardown := range teardowns {
			ctx = teardown.Func()(ctx, t, e.cfg)
		}
	})

	return ctx
}

// deepCopyFeature just copies the values from the Feature but creates a deep
// copy to avoid mutation when we just want an informational copy.
func deepCopyFeature(f types.Feature) types.Feature {
	fcopy := features.New(f.Name())
	for k, v := range f.Labels() {
		fcopy = fcopy.WithLabel(k, v)
	}
	f.Steps()
	for _, step := range f.Steps() {
		fcopy = fcopy.WithStep(step.Name(), step.Level(), nil)
	}
	return fcopy.Feature()
}
