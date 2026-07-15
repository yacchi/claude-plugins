# Claude Plugins Marketplace

A curated collection of Claude Code plugins for productivity and development workflows.

## Installation

Add this marketplace to Claude Code:

```bash
/plugin marketplace add yacchi/claude-plugins
```

Or via git URL:

```bash
/plugin marketplace add https://github.com/yacchi/claude-plugins.git
```

## Available Plugins

| Plugin | Description | Version |
|--------|-------------|---------|
| [hello-world](./plugins/hello-world) | A simple example plugin demonstrating the plugin structure | 1.0.0 |
| [orchestra](./plugins/orchestra) | Cost-tiered multi-agent orchestration: expensive instructor, cheap workers, adversarial verifiers | 0.1.0 |

## Usage

After adding the marketplace, you can:

1. **Browse plugins**: `/plugin`
2. **Install a plugin**: `/plugin install <plugin-name>@yacchi-plugins`
3. **List marketplaces**: `/plugin marketplace list`

## Contributing

### Adding a Plugin

1. Fork this repository
2. Create a new plugin directory under `plugins/<your-plugin-name>/`
3. Add a `plugin.json` manifest file:

```json
{
  "name": "your-plugin-name",
  "description": "Brief description of your plugin",
  "version": "1.0.0",
  "author": {
    "name": "Your Name"
  }
}
```

4. Add your plugin files (commands, agents, hooks, MCP servers)
5. Update `.claude-plugin/marketplace.json` to include your plugin
6. Submit a pull request

### Plugin Structure

```
plugins/your-plugin-name/
├── plugin.json           # Required: Plugin manifest
├── commands/             # Optional: Slash commands
│   └── example.md
├── agents/               # Optional: Agent definitions
│   └── example-agent.md
└── hooks.json            # Optional: Hook configurations
```

## Local Development

Test plugins locally before submitting:

```bash
# Add local marketplace
/plugin marketplace add ./

# Install and test your plugin
/plugin install your-plugin@yacchi-plugins
```

## License

MIT