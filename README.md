# Meta Harness

Meta Harness allows building Agent Harnesses from pieces. It helps to run primarily async agents that are harnessed with a specific model, tools, skills and a sandbox environment.

It is not meant to be useful without some coding, you have to write the agent itself in the end and deploy. That is part of the fun.

Meta harness is not executable by itself, see [examples](./examples) for how to assemble agents with it.

See [STACK.md](./STACK.md) for the tech stack and [TOOLS.md](./TOOLS.md) for how tools are written and why the tool plumbing is shaped the way it is.

Because the only place this library's design is visible is somebody else's `main`, [docs/api-design.md](./docs/api-design.md) records the bar every exported surface is held to, and [docs/sandbox.md](./docs/sandbox.md) follows one command from a tool call into a container and back.

