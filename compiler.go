package task

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-task/task/v3/internal/env"
	"github.com/go-task/task/v3/internal/execext"
	"github.com/go-task/task/v3/internal/filepathext"
	"github.com/go-task/task/v3/internal/logger"
	"github.com/go-task/task/v3/internal/templater"
	"github.com/go-task/task/v3/internal/version"
	"github.com/go-task/task/v3/taskfile/ast"
)

type Compiler struct {
	Dir            string
	Entrypoint     string
	UserWorkingDir string

	TaskfileEnv  *ast.Vars
	TaskfileVars *ast.Vars

	Logger *logger.Logger

	// TaskRunner is a function that can execute a task and return its output
	TaskRunner func(ctx context.Context, taskName string, output io.Writer) error

	dynamicCache   map[string]string
	muDynamicCache sync.Mutex
}

func (c *Compiler) GetTaskfileVariables() (*ast.Vars, error) {
	return c.getVariables(nil, nil, true)
}

func (c *Compiler) GetVariables(t *ast.Task, call *Call) (*ast.Vars, error) {
	return c.getVariables(t, call, true)
}

func (c *Compiler) FastGetVariables(t *ast.Task, call *Call) (*ast.Vars, error) {
	return c.getVariables(t, call, false)
}

func (c *Compiler) getVariables(t *ast.Task, call *Call, evaluateShVars bool) (*ast.Vars, error) {
	result := env.GetEnviron()
	specialVars, err := c.getSpecialVars(t, call)
	if err != nil {
		return nil, err
	}
	for k, v := range specialVars {
		result.Set(k, ast.Var{Value: v})
	}

	getRangeFunc := func(dir string) func(k string, v ast.Var) error {
		return func(k string, v ast.Var) error {
			cache := &templater.Cache{Vars: result}
			// Replace values
			newVar := templater.ReplaceVar(v, cache)
			// If the variable should not be evaluated, but is nil, set it to an empty string
			// This stops empty interface errors when using the templater to replace values later
			if !evaluateShVars && newVar.Value == nil {
				result.Set(k, ast.Var{Value: ""})
				return nil
			}
			// If the variable should not be evaluated and it is set, we can set it and return
			if !evaluateShVars {
				result.Set(k, ast.Var{Value: newVar.Value})
				return nil
			}
			// Now we can check for errors since we've handled all the cases when we don't want to evaluate
			if err := cache.Err(); err != nil {
				return err
			}
			// If the variable is already set, we can set it and return
			if newVar.Value != nil || (newVar.Sh == nil && newVar.Task == nil) {
				result.Set(k, ast.Var{Value: newVar.Value})
				return nil
			}
			// If the variable is dynamic, we need to resolve it first
			static, err := c.HandleDynamicVar(newVar, dir, env.GetFromVars(result))
			if err != nil {
				return err
			}
			result.Set(k, ast.Var{Value: static})
			return nil
		}
	}
	rangeFunc := getRangeFunc(c.Dir)

	var taskRangeFunc func(k string, v ast.Var) error
	if t != nil {
		// NOTE(@andreynering): We're manually joining these paths here because
		// this is the raw task, not the compiled one.
		cache := &templater.Cache{Vars: result}
		dir := templater.Replace(t.Dir, cache)
		if err := cache.Err(); err != nil {
			return nil, err
		}
		dir = filepathext.SmartJoin(c.Dir, dir)
		taskRangeFunc = getRangeFunc(dir)
	}

	for k, v := range c.TaskfileEnv.All() {
		if err := rangeFunc(k, v); err != nil {
			return nil, err
		}
	}
	for k, v := range c.TaskfileVars.All() {
		if err := rangeFunc(k, v); err != nil {
			return nil, err
		}
	}
	if t != nil {
		for k, v := range t.IncludeVars.All() {
			if err := rangeFunc(k, v); err != nil {
				return nil, err
			}
		}
		for k, v := range t.IncludedTaskfileVars.All() {
			if err := taskRangeFunc(k, v); err != nil {
				return nil, err
			}
		}
	}

	if t == nil || call == nil {
		return result, nil
	}

	for k, v := range call.Vars.All() {
		if err := rangeFunc(k, v); err != nil {
			return nil, err
		}
	}
	for k, v := range t.Vars.All() {
		if err := taskRangeFunc(k, v); err != nil {
			return nil, err
		}
	}

	return result, nil
}

func (c *Compiler) HandleDynamicVar(v ast.Var, dir string, e []string) (string, error) {
	c.muDynamicCache.Lock()
	defer c.muDynamicCache.Unlock()

	// Determine the cache key and type of dynamic variable
	var cacheKey string
	var isShVar, isTaskVar bool

	if v.Sh != nil && *v.Sh != "" {
		cacheKey = "sh:" + *v.Sh
		isShVar = true
	} else if v.Task != nil && *v.Task != "" {
		cacheKey = "task:" + *v.Task
		isTaskVar = true
	} else {
		// If the variable is not dynamic or it is empty, return an empty string
		return "", nil
	}

	if c.dynamicCache == nil {
		c.dynamicCache = make(map[string]string, 30)
	}
	if result, ok := c.dynamicCache[cacheKey]; ok {
		return result, nil
	}

	// NOTE(@andreynering): If a var have a specific dir, use this instead
	if v.Dir != "" {
		dir = v.Dir
	}

	var stdout bytes.Buffer
	var result string

	if isShVar {
		opts := &execext.RunCommandOptions{
			Command: *v.Sh,
			Dir:     dir,
			Stdout:  &stdout,
			Stderr:  c.Logger.Stderr,
			Env:     e,
		}
		if err := execext.RunCommand(context.Background(), opts); err != nil {
			return "", fmt.Errorf(`task: Command "%s" failed: %s`, opts.Command, err)
		}

		// Trim a single trailing newline from the result to make most command
		// output easier to use in shell commands.
		result = strings.TrimSuffix(stdout.String(), "\r\n")
		result = strings.TrimSuffix(result, "\n")

		c.Logger.VerboseErrf(logger.Magenta, "task: dynamic variable: %q result: %q\n", *v.Sh, result)
	} else if isTaskVar {
		if c.TaskRunner == nil {
			return "", fmt.Errorf("task: cannot execute task %q for variable: TaskRunner not configured. This is a configuration error - the TaskRunner should be set during compiler initialization", *v.Task)
		}

		if err := c.TaskRunner(context.Background(), *v.Task, &stdout); err != nil {
			return "", fmt.Errorf(`task: failed to get output from task "%s": %w`, *v.Task, err)
		}

		// Trim a single trailing newline from the result to make most command
		// output easier to use in shell commands.
		result = strings.TrimSuffix(stdout.String(), "\r\n")
		result = strings.TrimSuffix(result, "\n")

		c.Logger.VerboseErrf(logger.Magenta, "task: dynamic variable from task: %q result: %q\n", *v.Task, result)
	}

	c.dynamicCache[cacheKey] = result

	return result, nil
}

// ResetCache clear the dynamic variables cache
func (c *Compiler) ResetCache() {
	c.muDynamicCache.Lock()
	defer c.muDynamicCache.Unlock()

	c.dynamicCache = nil
}

func (c *Compiler) getSpecialVars(t *ast.Task, call *Call) (map[string]string, error) {
	allVars := map[string]string{
		"TASK_EXE":         filepath.ToSlash(os.Args[0]),
		"ROOT_TASKFILE":    filepathext.SmartJoin(c.Dir, c.Entrypoint),
		"ROOT_DIR":         c.Dir,
		"USER_WORKING_DIR": c.UserWorkingDir,
		"TASK_VERSION":     version.GetVersion(),
	}
	if t != nil {
		allVars["TASK"] = t.Task
		allVars["TASK_DIR"] = filepathext.SmartJoin(c.Dir, t.Dir)
		allVars["TASKFILE"] = t.Location.Taskfile
		allVars["TASKFILE_DIR"] = filepath.Dir(t.Location.Taskfile)
	} else {
		allVars["TASK"] = ""
		allVars["TASK_DIR"] = ""
		allVars["TASKFILE"] = ""
		allVars["TASKFILE_DIR"] = ""
	}
	if call != nil {
		allVars["ALIAS"] = call.Task
	} else {
		allVars["ALIAS"] = ""
	}

	return allVars, nil
}
