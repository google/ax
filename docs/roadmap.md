# Roadmap

## 1. Stabilize Core Specs

Stabilize the `ax.io/v1alpha1` declarative schemas and lifecycle contracts across the API, CLI, and controller:

| Resource | Scope |
|---|---|
| **`Task`** | Lifecycle phases, status conditions, resource limits, token/timeout budgets, and approval policies. |
| **`Workspace`** | Git checkouts, static/registry MCP servers, skill registries, mount paths, and goal overrides. |
| **`Model`** | Provider bindings, model identifiers, sampling parameters, and credential secret references. |
| **`Sandbox` / `SandboxConfig`** | Runtime isolation backends, kernel/syscall constraints, filesystem mounts, and security profiles. |


## 2. Actor Architecture

Evolve how AX maps tasks onto Agent Substrate actors to improve security boundaries, cluster utilization, and stateful workflows:

- **Migration to the New Actor**: Migrate `ax-controller` and the Substrate integration layer (`internal/substrate`) to the new Agent Substrate Actor API and lifecycle model.
- **Splitting Task Workspace Setup into a Separate Actor**: Decouple maiden workspace initialization (Git repository cloning, MCP and skill materialization, and goal-driven bootstrap) from the primary task runtime by executing setup in a dedicated setup actor before handing off the prepared workspace state to the task actor.
- **Minimally Privileged Policies**: Apply strict least-privilege policies tailored independently to the workspace setup actor and the task execution actor—scoping repository/registry credentials and setup egress exclusively to the initialization phase while enforcing minimal runtime permissions, network egress, and capabilities on the task actor.
- **Idleness Detection and Automatic Suspension for Density**: Continuously monitor actor activity (process execution, I/O, network traffic, and active gRPC/SSH sessions) to detect idle tasks and automatically trigger `SuspendActor` checkpointing, reclaiming worker CPU and memory to maximize cluster density.
- **Stateful Task Branching**: Enable branching (forking) a running or suspended `Task`—including its checkpointed memory and workspace filesystem state—into multiple parallel tasks to explore speculative execution paths concurrently.


## 3. Agentic Environment

Make workspace synthesis and in-sandbox agent harnesses dynamic and extensible:

- **Dynamic Agentic Environment Curation**: Enhance goal-driven workspace preparation (`Workspace.spec.goal` and `Task.spec.workspace.goal`) to dynamically inspect repository contents, resolve toolchains, discover relevant MCP servers and skills from registries, and curate a verified working environment automatically.
- **Allow Customization**: Decouple the built-in workspace bootstrap and coding agent harness so users can configure custom agent runtimes, system instructions, tool policies, and model configurations.


## 4. Networking, Identity, Governance & Observability

Harden perimeter security, enterprise governance, and platform observability for production deployments:

- **SPIFFE Identity for Tasks**: Issue cryptographic workload identities (SPIFFE IDs / X.509-SVIDs) to every `Task` actor, enabling zero-trust mutual TLS (mTLS) authentication across tasks, MCP servers, and internal services.
- **Google Platform Requirements for Governance**: Meet baseline Google platform requirements for MCP and skill registry integration, access control, policy enforcement, budget guardrails, and audit compliance.
- **Telemetry and Trajectory Collection**: Automatically collect and export OpenTelemetry metrics, distributed traces, and structured agent trajectories (prompts, model responses, tool calls, process executions, and lifecycle transitions) at the runner layer without requiring custom instrumentation in user containers.


## 5. Documentation

Expand documentation in `docs/` to cover architecture rationale and extensibility guides:

- **Layering with Substrate**: Document the architectural division of responsibilities between AX and Agent Substrate, answering major questions.
- **Custom Images**: Provide guides and conventions for authoring, building, and configuring custom container images for `Task` workloads.
- **Custom Runners**: Document the internal contract of `ax-task-runner` more comprehensively.
