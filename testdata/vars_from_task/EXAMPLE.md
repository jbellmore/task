# Dynamic Variables from Task Execution

This feature allows you to set **task-level** variables from the output of other tasks, similar to how `sh:` works but using the internal task execution mechanism.

**Important**: The `task:` syntax is only supported for task-level variables, not taskfile-level variables. For taskfile-level variables, use `sh:` instead to avoid circular dependencies.

## Basic Example

```yaml
version: '3'

tasks:
  generate-value:
    cmds:
      - echo "hello-from-task"
    silent: true

  use-var:
    vars:
      MY_VAR:
        task: generate-value
    cmds:
      - echo "Value is {{.MY_VAR}}"
```

## Task with Dependencies

When you execute a task for a variable, all its dependencies run as well:

```yaml
version: '3'

tasks:
  setup:
    cmds:
      - echo "Setting up..."
    silent: true

  generate-with-deps:
    deps:
      - setup
    cmds:
      - echo "value-with-deps"
    silent: true

  use-task-with-deps:
    vars:
      RESULT:
        task: generate-with-deps
    cmds:
      - echo "Got result: {{.RESULT}}"
```

The output will capture both the dependency output and the main task output.

## Multiple Variables from Tasks

```yaml
version: '3'

tasks:
  get-version:
    cmds:
      - echo "1.0.0"
    silent: true

  get-commit:
    cmds:
      - git rev-parse --short HEAD
    silent: true

  build:
    vars:
      VERSION:
        task: get-version
      COMMIT:
        task: get-commit
    cmds:
      - go build -ldflags="-X main.Version={{.VERSION}} -X main.Commit={{.COMMIT}}" main.go
```

## Comparison with sh:

### Using sh: (shells out)
```yaml
vars:
  GIT_COMMIT:
    sh: git log -n 1 --format=%h
```

### Using task: (uses internal task execution)
```yaml
vars:
  GIT_COMMIT:
    task: get-commit
```

The `task:` approach allows you to:
- Reuse existing task definitions
- Leverage task dependencies
- Apply task features like preconditions, status checks, etc.
- Keep logic in tasks rather than inline shell commands
