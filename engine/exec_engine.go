package engine

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/concourse/atc"
	"github.com/concourse/atc/db"
	"github.com/concourse/atc/exec"
	"github.com/pivotal-golang/lager"
	"github.com/tedsuo/ifrit"
)

type execMetadata struct {
	Plan atc.Plan
}

type execEngine struct {
	factory         exec.Factory
	delegateFactory BuildDelegateFactory
	db              EngineDB
}

func NewExecEngine(factory exec.Factory, delegateFactory BuildDelegateFactory, db EngineDB) Engine {
	return &execEngine{
		factory:         factory,
		delegateFactory: delegateFactory,
		db:              db,
	}
}

func (engine *execEngine) Name() string {
	return "exec.v1"
}

func (engine *execEngine) CreateBuild(model db.Build, plan atc.Plan) (Build, error) {
	return &execBuild{
		buildID:  model.ID,
		db:       engine.db,
		factory:  engine.factory,
		delegate: engine.delegateFactory.Delegate(model.ID),
		metadata: execMetadata{
			Plan: plan,
		},

		signals: make(chan os.Signal, 1),
	}, nil
}

func (engine *execEngine) LookupBuild(model db.Build) (Build, error) {
	var metadata execMetadata
	err := json.Unmarshal([]byte(model.EngineMetadata), &metadata)
	if err != nil {
		return nil, err
	}

	return &execBuild{
		buildID:  model.ID,
		db:       engine.db,
		factory:  engine.factory,
		delegate: engine.delegateFactory.Delegate(model.ID),
		metadata: metadata,

		signals: make(chan os.Signal, 1),
	}, nil
}

type execBuild struct {
	buildID int
	db      EngineDB

	factory  exec.Factory
	delegate BuildDelegate

	signals chan os.Signal

	metadata execMetadata
}

func (build *execBuild) Metadata() string {
	payload, err := json.Marshal(build.metadata)
	if err != nil {
		panic("failed to marshal build metadata: " + err.Error())
	}

	return string(payload)
}

func (build *execBuild) Abort() error {
	build.signals <- os.Kill
	return nil
}

func (build *execBuild) Resume(logger lager.Logger) {
	step := build.buildStep(build.metadata.Plan, logger)
	source := step.Using(&exec.NoopArtifactSource{})

	defer source.Release()

	process := ifrit.Background(source)

	exited := process.Wait()

	for {
		select {
		case err := <-exited:
			build.delegate.Finish(logger.Session("finish"), err)
			return

		case sig := <-build.signals:
			process.Signal(sig)

			if sig == os.Kill {
				build.delegate.Aborted(logger)
			}
		}
	}
}

func (build *execBuild) Hijack(spec atc.HijackProcessSpec, io HijackProcessIO) (HijackedProcess, error) {
	ioConfig := exec.IOConfig{
		Stdin:  io.Stdin,
		Stdout: io.Stdout,
		Stderr: io.Stderr,
	}

	return build.factory.Hijack(build.executeSessionID(), ioConfig, spec)
}

func (build *execBuild) buildStep(plan atc.Plan, logger lager.Logger) exec.Step {
	if plan.Aggregate != nil {
		logger = logger.Session("aggregate")

		step := exec.Aggregate{}
		for name, innerPlan := range *plan.Aggregate {
			step[name] = build.buildStep(innerPlan, logger.Session(name))
		}

		return step
	}

	if plan.Compose != nil {
		return exec.Compose(
			build.buildStep(plan.Compose.A, logger),
			build.buildStep(plan.Compose.B, logger),
		)
	}

	if plan.Conditional != nil {
		logger = logger.Session("conditional", lager.Data{
			"on": plan.Conditional.Conditions,
		})

		return exec.Conditional{
			Conditions: plan.Conditional.Conditions,
			Step:       build.buildStep(plan.Conditional.Plan, logger),
		}
	}

	if plan.Execute != nil {
		logger = logger.Session("execute")

		var configSource exec.BuildConfigSource
		if plan.Execute.Config != nil && plan.Execute.ConfigPath != "" {
			configSource = exec.MergedConfigSource{
				A: exec.FileConfigSource{plan.Execute.ConfigPath},
				B: exec.StaticConfigSource{*plan.Execute.Config},
			}
		} else if plan.Execute.Config != nil {
			configSource = exec.StaticConfigSource{*plan.Execute.Config}
		} else if plan.Execute.ConfigPath != "" {
			configSource = exec.FileConfigSource{plan.Execute.ConfigPath}
		} else {
			return exec.Identity{}
		}

		return build.factory.Execute(
			build.executeSessionID(),
			build.delegate.ExecutionDelegate(logger),
			exec.Privileged(plan.Execute.Privileged),
			configSource,
		)
	}

	if plan.Get != nil {
		logger = logger.Session("get", lager.Data{
			"name": plan.Get.Name,
		})

		return build.factory.Get(
			build.inputSessionID(plan.Get.Name),
			build.delegate.InputDelegate(logger, *plan.Get),
			atc.ResourceConfig{
				Name:   plan.Get.Resource,
				Type:   plan.Get.Type,
				Source: plan.Get.Source,
			},
			plan.Get.Params,
			plan.Get.Version,
		)
	}

	if plan.Put != nil {
		logger = logger.Session("put", lager.Data{
			"name": plan.Put.Resource,
		})

		return build.factory.Put(
			build.outputSessionID(plan.Put.Resource),
			build.delegate.OutputDelegate(logger, *plan.Put),
			atc.ResourceConfig{
				Name:   plan.Put.Resource,
				Type:   plan.Put.Type,
				Source: plan.Put.Source,
			},
			plan.Put.Params,
		)
	}

	return exec.Identity{}
}

func (build *execBuild) executeSessionID() exec.SessionID {
	return exec.SessionID(fmt.Sprintf("build-%d-execute", build.buildID))
}

func (build *execBuild) inputSessionID(inputName string) exec.SessionID {
	return exec.SessionID(fmt.Sprintf("build-%d-input-%s", build.buildID, inputName))
}

func (build *execBuild) outputSessionID(outputName string) exec.SessionID {
	return exec.SessionID(fmt.Sprintf("build-%d-output-%s", build.buildID, outputName))
}
