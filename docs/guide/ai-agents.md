---
description: "Give an AI coding agent what it needs to configure and deploy statica: an installable skill, the documentation as Markdown, and the checks that verify its work."
---

# Configure statica with an AI agent

An AI coding agent can write the configuration and the deployment of **statica** for you. This page lists what the documentation site publishes for agents and how to give it to yours.

## Install the skill

The skill `deploy-statica` is a procedure in the [Agent Skills](https://agentskills.io/) format: the facts to gather, the complete example to start from, the rules that prevent the usual mistakes, and the checks to run before reporting the deployment as done. Install it with the [`skills`](https://github.com/vercel-labs/skills) tool, which asks which of your agents gets it:

```sh
npx skills add https://gtriggiano.github.io/statica
```

To install it by hand, save the file in the skills directory of your agent, for example `.claude/skills/` for Claude Code or `.agents/skills/` for the tools that read that directory:

```sh
mkdir -p .agents/skills/deploy-statica
curl -fsSL -o .agents/skills/deploy-statica/SKILL.md \
  https://gtriggiano.github.io/statica/.well-known/agent-skills/deploy-statica/SKILL.md
```

Then ask for what you want, for example "serve the bucket `example-websites` as `www.example.com` with statica on our Kubernetes cluster". The skill describes the latest release, and the site lists it in `https://gtriggiano.github.io/statica/.well-known/agent-skills/index.json` for tools that discover skills.

## Point an agent at the documentation

Without the skill, give the agent the index of the documentation, `https://gtriggiano.github.io/statica/llms.txt`. It follows the [llms.txt](https://llmstxt.org/) format and links every page as Markdown: each page of this site has a Markdown version at the same address with `.md` in place of `.html`, with the commands and the image tag of its release written out.

## Check what the agent wrote

An agent's configuration is checked like any other, and these checks need no trust in the agent:

- `statica config validate` rejects an unknown field, a wrong type and an invalid value, offline ([Check a configuration](./configuration.md#check-a-configuration)).
- The [JSON Schema](./configuration.md#validate-in-your-editor) of the configuration file gives an editor or a YAML validator the field names, types and allowed values.
- `statica config check-origin` reads one object with the credentials of the environment, and names the cause when it fails ([Verify it](./s3-providers.md#verify-it)).
- `/readyz` on the admin listener tells whether an instance serves traffic ([Health checks and readiness](./health-checks.md)).

Review what the checks cannot see: the addresses in `server.trusted_proxies`, the exposure of the admin port, and the permissions of the credentials ([Security model](../architecture/security.md)).
