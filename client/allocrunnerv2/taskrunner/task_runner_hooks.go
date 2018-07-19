package taskrunner

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/nomad/client/allocrunnerv2/interfaces"
	"github.com/hashicorp/nomad/client/allocrunnerv2/taskrunner/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// initHooks intializes the tasks hooks.
func (tr *TaskRunner) initHooks() {
	hookLogger := tr.logger.Named("task_hook")
	task := tr.Task()

	// Create the task directory hook. This is run first to ensure the
	// directoy path exists for other hooks.
	tr.runnerHooks = []interfaces.TaskHook{
		newValidateHook(tr.clientConfig, hookLogger),
		newTaskDirHook(tr, hookLogger),
		newArtifactHook(tr, hookLogger),
		newShutdownDelayHook(task.ShutdownDelay, hookLogger),
	}

	// If Vault is enabled, add the hook
	if task.Vault != nil {
		tr.runnerHooks = append(tr.runnerHooks, newVaultHook(&vaultHookConfig{
			vaultStanza: task.Vault,
			client:      tr.vaultClient,
			events:      tr,
			lifecycle:   tr,
			updater:     tr,
			logger:      hookLogger,
			alloc:       tr.Alloc(),
			task:        tr.taskName,
		}))
	}

	// If there are templates is enabled, add the hook
	if len(task.Templates) != 0 {
		tr.runnerHooks = append(tr.runnerHooks, newTemplateHook(&templateHookConfig{
			logger:       hookLogger,
			lifecycle:    tr,
			events:       tr,
			templates:    task.Templates,
			clientConfig: tr.clientConfig,
			envBuilder:   tr.envBuilder,
		}))
	}
}

// prestart is used to run the runners prestart hooks.
func (tr *TaskRunner) prestart() error {
	//XXX is this necessary? maybe we should have a generic cancelletion
	//    method instead of peeking into the alloc
	// Determine if the allocation is terminaland we should avoid running
	// prestart hooks.
	alloc := tr.Alloc()
	if alloc.TerminalStatus() {
		tr.logger.Trace("skipping prestart hooks since allocation is terminal")
		return nil
	}

	if tr.logger.IsTrace() {
		start := time.Now()
		tr.logger.Trace("running prestart hooks", "start", start)
		defer func() {
			end := time.Now()
			tr.logger.Trace("finished prestart hooks", "end", end, "duration", end.Sub(start))
		}()
	}

	for _, hook := range tr.runnerHooks {
		pre, ok := hook.(interfaces.TaskPrestartHook)
		if !ok {
			tr.logger.Trace("skipping non-prestart hook", "name", hook.Name())
			continue
		}

		name := pre.Name()
		// Build the request
		req := interfaces.TaskPrestartRequest{
			Task:    tr.Task(),
			TaskDir: tr.taskDir.Dir,
			TaskEnv: tr.envBuilder.Build(),
		}

		tr.localStateLock.RLock()
		origHookState := tr.localState.Hooks[name]
		tr.localStateLock.RUnlock()
		if origHookState != nil && origHookState.PrestartDone {
			tr.logger.Trace("skipping done prestart hook", "name", pre.Name())
			continue
		}

		req.VaultToken = tr.getVaultToken()

		// Time the prestart hook
		var start time.Time
		if tr.logger.IsTrace() {
			start = time.Now()
			tr.logger.Trace("running prestart hook", "name", name, "start", start)
		}

		// Run the prestart hook
		var resp interfaces.TaskPrestartResponse
		if err := pre.Prestart(tr.ctx, &req, &resp); err != nil {
			return structs.WrapRecoverable(fmt.Sprintf("prestart hook %q failed: %v", name, err), err)
		}

		// Store the hook state
		{
			tr.localStateLock.Lock()
			hookState, ok := tr.localState.Hooks[name]
			if !ok {
				hookState = &state.HookState{}
				tr.localState.Hooks[name] = hookState
			}

			if resp.HookData != nil {
				hookState.Data = resp.HookData
				hookState.PrestartDone = resp.Done
			}
			tr.localStateLock.Unlock()

			// Store and persist local state if the hook state has changed
			if !hookState.Equal(origHookState) {
				tr.localState.Hooks[name] = hookState
				if err := tr.persistLocalState(); err != nil {
					return err
				}
			}
		}

		// Store the environment variables returned by the hook
		if len(resp.Env) != 0 {
			tr.envBuilder.SetGenericEnv(resp.Env)
		}

		if tr.logger.IsTrace() {
			end := time.Now()
			tr.logger.Trace("finished prestart hooks", "name", name, "end", end, "duration", end.Sub(start))
		}
	}

	return nil
}

// poststart is used to run the runners poststart hooks.
func (tr *TaskRunner) poststart() error {
	if tr.logger.IsTrace() {
		start := time.Now()
		tr.logger.Trace("running poststart hooks", "start", start)
		defer func() {
			end := time.Now()
			tr.logger.Trace("finished poststart hooks", "end", end, "duration", end.Sub(start))
		}()
	}

	for _, hook := range tr.runnerHooks {
		post, ok := hook.(interfaces.TaskPoststartHook)
		if !ok {
			continue
		}

		name := post.Name()
		var start time.Time
		if tr.logger.IsTrace() {
			start = time.Now()
			tr.logger.Trace("running poststart hook", "name", name, "start", start)
		}

		req := interfaces.TaskPoststartRequest{}
		var resp interfaces.TaskPoststartResponse
		// XXX We shouldn't exit on the first one
		if err := post.Poststart(tr.ctx, &req, &resp); err != nil {
			return fmt.Errorf("poststart hook %q failed: %v", name, err)
		}

		if tr.logger.IsTrace() {
			end := time.Now()
			tr.logger.Trace("finished poststart hooks", "name", name, "end", end, "duration", end.Sub(start))
		}
	}

	return nil
}

// stop is used to run the stop hooks.
func (tr *TaskRunner) stop() error {
	if tr.logger.IsTrace() {
		start := time.Now()
		tr.logger.Trace("running stop hooks", "start", start)
		defer func() {
			end := time.Now()
			tr.logger.Trace("finished stop hooks", "end", end, "duration", end.Sub(start))
		}()
	}

	for _, hook := range tr.runnerHooks {
		post, ok := hook.(interfaces.TaskStopHook)
		if !ok {
			continue
		}

		name := post.Name()
		var start time.Time
		if tr.logger.IsTrace() {
			start = time.Now()
			tr.logger.Trace("running stop hook", "name", name, "start", start)
		}

		req := interfaces.TaskStopRequest{}
		var resp interfaces.TaskStopResponse
		// XXX We shouldn't exit on the first one
		if err := post.Stop(tr.ctx, &req, &resp); err != nil {
			return fmt.Errorf("stop hook %q failed: %v", name, err)
		}

		if tr.logger.IsTrace() {
			end := time.Now()
			tr.logger.Trace("finished stop hooks", "name", name, "end", end, "duration", end.Sub(start))
		}
	}

	return nil
}

// update is used to run the runners update hooks.
func (tr *TaskRunner) updateHooks() {
	if tr.logger.IsTrace() {
		start := time.Now()
		tr.logger.Trace("running update hooks", "start", start)
		defer func() {
			end := time.Now()
			tr.logger.Trace("finished update hooks", "end", end, "duration", end.Sub(start))
		}()
	}

	for _, hook := range tr.runnerHooks {
		upd, ok := hook.(interfaces.TaskUpdateHook)
		if !ok {
			tr.logger.Trace("skipping non-update hook", "name", hook.Name())
			continue
		}

		name := upd.Name()

		// Build the request
		req := interfaces.TaskUpdateRequest{
			VaultToken: tr.getVaultToken(),
		}

		// Time the update hook
		var start time.Time
		if tr.logger.IsTrace() {
			start = time.Now()
			tr.logger.Trace("running update hook", "name", name, "start", start)
		}

		// Run the update hook
		var resp interfaces.TaskUpdateResponse
		if err := upd.Update(tr.ctx, &req, &resp); err != nil {
			tr.logger.Error("update hook failed", "name", name, "error", err)
		}

		if tr.logger.IsTrace() {
			end := time.Now()
			tr.logger.Trace("finished update hooks", "name", name, "end", end, "duration", end.Sub(start))
		}
	}
}

// kill is used to run the runners kill hooks.
func (tr *TaskRunner) kill() {
	if tr.logger.IsTrace() {
		start := time.Now()
		tr.logger.Trace("running kill hooks", "start", start)
		defer func() {
			end := time.Now()
			tr.logger.Trace("finished kill hooks", "end", end, "duration", end.Sub(start))
		}()
	}

	for _, hook := range tr.runnerHooks {
		upd, ok := hook.(interfaces.TaskKillHook)
		if !ok {
			tr.logger.Trace("skipping non-kill hook", "name", hook.Name())
			continue
		}

		name := upd.Name()

		// Time the update hook
		var start time.Time
		if tr.logger.IsTrace() {
			start = time.Now()
			tr.logger.Trace("running kill hook", "name", name, "start", start)
		}

		// Run the update hook
		req := interfaces.TaskKillRequest{}
		var resp interfaces.TaskKillResponse
		if err := upd.Kill(context.Background(), &req, &resp); err != nil {
			tr.logger.Error("kill hook failed", "name", name, "error", err)
		}

		if tr.logger.IsTrace() {
			end := time.Now()
			tr.logger.Trace("finished kill hooks", "name", name, "end", end, "duration", end.Sub(start))
		}
	}
}

/*
TR Hooks:

> @schmichael
Task Validate:
Require:  Client config, task definiton
Return: error
Implement: Prestart

> DONE
Task Dir Build:
Requires: Folder structure, driver isolation, client config
Return env, error
Implement: Prestart

> @alex
Vault: Task, RPC to talk to server to derive token, Node SecretID
Return vault token (Call a setter), error, env
Implement: Prestart

> @alex
Consul Template:
Require: Task, alloc directory, way to signal/restart task, updates when vault token changes
Return env, error
Implement: Prestart and Update (for new Vault token) and Destroy

> @schmichael
Consul Service Reg:
Require: Task, interpolation/ENV
Return: error
Implement: Poststart, Update, Kill, Exited

> @alex
Dispatch Payload:
Require: Alloc
Return error
Implement: Prestart

> @schmichael
Artifacts:
Require: Folder structure, task, interpolation/ENV
Return: error
Implement: Prestart
*/