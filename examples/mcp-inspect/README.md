# mcp-inspect

Dials an MCP server and prints what it advertises.

Reflection is the one part of an agent's tool set a consumer cannot read in
their own source: what reached the model came from a server, at runtime. This is
what you run when it has gone wrong.

```sh
go run . lightpanda mcp
go run . https://example.com/mcp
```

An argument starting with `http://` or `https://` is an endpoint; anything else
is a command to run and speak to over stdio.

It prints the negotiated protocol version, every exposed tool with its required
arguments, anything that was skipped and why, and the byte size each definition
adds to a prompt — the column nobody measures until their system prompt has
doubled. `lightpanda mcp` costs about 10 KB for twenty tools, which is the
argument for the declared door in `mcp/lightpanda`.
